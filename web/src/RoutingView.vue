<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'
import Icon from './Icon.vue'
import { api } from './api.js'
import { normalizeTs } from './api.generated.js'

const decisions = ref([])
const loading = ref(false)
const error = ref('')
const query = ref('')
const filter = ref('all')
const autoRefresh = ref(true)
const selected = ref(null)
const detail = ref(null)
const detailLoading = ref(false)
let timer = null
let loadEpoch = 0

const filters = [
  { key: 'all', label: '全部' },
  { key: 'cache', label: '缓存选路' },
  { key: 'explore', label: '探索采样' },
  { key: 'attention', label: '需关注' },
]

const visibleDecisions = computed(() => {
  const needle = query.value.trim().toLowerCase()
  return decisions.value.filter(item => {
    const text = [item.model, item.selected_upstream, item.reason, item.endpoint, item.protocol].join(' ').toLowerCase()
    if (needle && !text.includes(needle)) return false
    if (filter.value === 'cache' && !item.cache_selected) return false
    if (filter.value === 'explore' && !item.exploration) return false
    if (filter.value === 'attention' && !needsAttention(item)) return false
    return true
  })
})

const stats = computed(() => {
  const list = visibleDecisions.value
  return {
    total: list.length,
    cache: list.filter(item => item.cache_selected).length,
    savings: list.reduce((sum, item) => sum + Number(item.estimated_savings || 0), 0),
    explore: list.filter(item => item.exploration).length,
    attention: list.filter(needsAttention).length,
  }
})

const candidates = computed(() => [...(detail.value?.candidates || [])].sort((a, b) =>
  Number(b.selected) - Number(a.selected) || Number(b.eligible) - Number(a.eligible) || candidateCost(a) - candidateCost(b)))

async function load() {
  const epoch = ++loadEpoch
  loading.value = true
  error.value = ''
  try {
    const items = await api.routeDecisions({ limit: 100, include_candidates: false })
    if (epoch === loadEpoch) decisions.value = Array.isArray(items) ? items : []
  } catch (e) {
    if (epoch === loadEpoch) error.value = String(e.message || e)
  } finally {
    if (epoch === loadEpoch) loading.value = false
  }
}

async function openDetail(item) {
  selected.value = item
  detail.value = null
  detailLoading.value = true
  try { detail.value = await api.routeDecisionDetail(item.id) }
  catch (e) { detail.value = { error: String(e.message || e) } }
  finally { detailLoading.value = false }
}

function closeDetail() { selected.value = null; detail.value = null }
function stopTimer() { if (timer) clearInterval(timer); timer = null }
function restartTimer() { stopTimer(); if (autoRefresh.value) timer = setInterval(load, 15000) }
function setAutoRefresh(value) { autoRefresh.value = value; restartTimer() }
onMounted(() => { load(); restartTimer() })
onUnmounted(stopTimer)

function outcomeKey(value) {
  const key = String(value || '').toLowerCase()
  if (['success', 'partial', 'canceled', 'client_error'].includes(key)) return key
  return key ? 'failed' : 'pending'
}
function outcomeLabel(value) { return ({ success: '已完成', partial: '流中断', canceled: '已取消', client_error: '客户端错误', failed: '未完成', pending: '处理中' })[outcomeKey(value)] }
function outcomeClass(value) { return `is-${outcomeKey(value)}` }
function needsAttention(item) { return ['failed', 'partial', 'client_error'].includes(outcomeKey(item.actual_outcome)) || Number(item.confidence || 0) < .5 }

function reasonLabel(item) {
  if (item.cache_selected && Number(item.estimated_savings || 0) > 0) return '缓存命中，预计降低成本'
  if (item.exploration) return '探索采样，收集渠道样本'
  const reason = String(item.reason || '')
  if (reason.includes('ordinary input')) return '无缓存前提下选择最低价'
  if (reason.includes('lowest forecast cost')) return reason.replace('lowest forecast cost via ', '预测成本最低：').split(';')[0]
  return reason.split(';')[0] || '按当前路由策略选择'
}

function number(value) { const n = Number(value); return Number.isFinite(n) ? n : 0 }
function perRequestCost(item) { return item.selected_cost && item.forecast_requests ? number(item.selected_cost) / number(item.forecast_requests) : number(item.selected_cost) }
function candidateCost(item) { return number(item.forecast_total_cost) / (number(detail.value?.forecast_requests) || 1) }
function costWidth(item) { const max = Math.max(...candidates.value.filter(row => row.eligible).map(candidateCost), .0001); return `${Math.max(4, Math.min(100, candidateCost(item) / max * 100))}%` }
function fmtCost(value) { const n = number(value); if (!n) return '$0'; if (n < .001) return '<$0.001'; return n < 1 ? `$${n.toFixed(4)}` : `$${n.toFixed(2)}` }
function fmtNumber(value) { return number(value).toLocaleString('zh-CN') }
function fmtTokens(value) { const n = number(value); if (!n) return '—'; return n >= 1000 ? `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}K` : fmtNumber(n) }
function fmtConfidence(value) { return `${Math.round(number(value) * 100)}%` }
function confidenceClass(value) { const n = number(value); return n >= .8 ? 'high' : n >= .5 ? 'medium' : 'low' }
function formatTime(value) { const ms = normalizeTs(value); return ms ? new Date(ms).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '—' }
function timeAgo(value) { const ms = normalizeTs(value); if (!ms) return '—'; const s = Math.max(0, Math.floor((Date.now() - ms) / 1000)); if (s < 60) return `${s} 秒前`; if (s < 3600) return `${Math.floor(s / 60)} 分钟前`; if (s < 86400) return `${Math.floor(s / 3600)} 小时前`; return `${Math.floor(s / 86400)} 天前` }
</script>

<template>
  <section class="routing-page">
    <header class="routing-page-head">
      <div>
        <div class="routing-kicker"><span></span>ROUTING AUDIT</div>
        <h2>路由决策</h2>
        <p>查看每次请求的选路依据、成本预测与实际结果</p>
      </div>
      <div class="routing-head-actions">
        <label class="routing-live-toggle"><input type="checkbox" :checked="autoRefresh" @change="setAutoRefresh($event.target.checked)" /><span>自动刷新</span></label>
        <button class="icon-btn" type="button" title="刷新路由决策" :disabled="loading" @click="load"><Icon name="refresh" :class="{ spin: loading }" :size="16" /></button>
      </div>
    </header>

    <section class="routing-stats" aria-label="路由决策摘要">
      <article><i class="pink"></i><div><small>当前记录</small><strong>{{ fmtNumber(stats.total) }}</strong></div><em>最近 100 条</em></article>
      <article><i class="mint"></i><div><small>缓存选路</small><strong>{{ stats.total ? Math.round(stats.cache / stats.total * 100) : 0 }}%</strong></div><em>{{ fmtNumber(stats.cache) }} 次</em></article>
      <article><i class="amber"></i><div><small>预计节省</small><strong>{{ fmtCost(stats.savings) }}</strong></div><em>当前预测窗口</em></article>
      <article><i class="blue"></i><div><small>探索采样</small><strong>{{ fmtNumber(stats.explore) }}</strong></div><em>{{ stats.attention ? `${stats.attention} 项需关注` : '暂无异常结果' }}</em></article>
    </section>

    <section class="routing-toolbar" aria-label="路由筛选">
      <label class="routing-search"><Icon name="search" :size="16" /><input v-model="query" type="search" placeholder="搜索模型、渠道或决策理由" /><button v-if="query" type="button" aria-label="清除搜索" @click="query = ''"><Icon name="x" :size="14" /></button></label>
      <div class="routing-filter-tabs" role="tablist">
        <button v-for="item in filters" :key="item.key" type="button" role="tab" :aria-selected="filter === item.key" :class="{ active: filter === item.key }" @click="filter = item.key">{{ item.label }}<b v-if="item.key === 'attention' && stats.attention">{{ stats.attention }}</b></button>
      </div>
      <span class="routing-result-count">{{ fmtNumber(visibleDecisions.length) }} 条记录</span>
    </section>

    <div v-if="error" class="routing-inline-error" role="alert"><Icon name="alert" :size="16" /><span>{{ error }}</span><button type="button" @click="load">重新加载</button></div>
    <div v-else-if="loading && !decisions.length" class="routing-skeleton"><span v-for="item in 5" :key="item"></span></div>
    <div v-else-if="!visibleDecisions.length" class="routing-empty"><span><Icon name="link" :size="20" /></span><strong>{{ decisions.length ? '没有符合条件的记录' : '暂无路由决策记录' }}</strong><small>{{ decisions.length ? '调整搜索词或筛选条件后再试' : '请求经过智能路由后，这里会显示选路过程' }}</small></div>

    <section v-else class="routing-table-shell" aria-label="路由决策记录">
      <div class="routing-table-head"><span>时间</span><span>请求</span><span>选中渠道</span><span>决策结果</span><span>单次成本</span><span>置信度</span><span>实际状态</span><span></span></div>
      <div v-for="item in visibleDecisions" :key="item.id" class="routing-table-row" role="button" tabindex="0" @click="openDetail(item)" @keyup.enter="openDetail(item)">
        <div class="routing-time-cell"><strong>{{ timeAgo(item.created_at) }}</strong><small>{{ formatTime(item.created_at) }}</small></div>
        <div class="routing-request-cell"><strong :title="item.model">{{ item.model || '未命名模型' }}</strong><small>{{ item.protocol || '透传' }}<span v-if="item.endpoint"> · {{ item.endpoint }}</span></small></div>
        <div class="routing-channel-cell"><i></i><strong :title="item.selected_upstream">{{ item.selected_upstream || '未选中渠道' }}</strong><small v-if="item.exploration">探索采样</small><small v-else-if="item.cache_selected">缓存路径</small></div>
        <div class="routing-reason-cell"><b :class="item.cache_selected ? 'cache' : item.exploration ? 'explore' : 'direct'">{{ item.cache_selected ? '缓存' : item.exploration ? '探索' : '常规' }}</b><span :title="reasonLabel(item)">{{ reasonLabel(item) }}</span></div>
        <div class="routing-cost-cell"><strong>{{ fmtCost(perRequestCost(item)) }}</strong><small>{{ item.forecast_requests ? `${fmtNumber(item.forecast_requests)} 次预测` : '单次预测' }}</small></div>
        <div class="routing-confidence-cell"><span :class="confidenceClass(item.confidence)">{{ fmtConfidence(item.confidence) }}</span></div>
        <div class="routing-outcome-cell"><span class="routing-outcome" :class="outcomeClass(item.actual_outcome)"><i></i>{{ outcomeLabel(item.actual_outcome) }}</span></div>
        <div class="routing-row-arrow"><Icon name="chevron-right" :size="15" /></div>
      </div>
    </section>
  </section>

  <div v-if="selected" class="routing-drawer-mask" @click.self="closeDetail">
    <aside class="routing-drawer" role="dialog" aria-modal="true" aria-label="路由决策详情">
      <header class="routing-drawer-head"><div><small>决策详情 · #{{ selected.id }}</small><h3>{{ selected.model || '未命名模型' }}</h3><p>{{ formatTime(selected.created_at) }} · {{ selected.selected_upstream || '未选中渠道' }}</p></div><button class="icon-btn" type="button" title="关闭详情" @click="closeDetail"><Icon name="x" :size="17" /></button></header>
      <div v-if="detailLoading" class="routing-drawer-state"><Icon name="loader" class="spin" :size="19" /><span>正在读取候选渠道</span></div>
      <div v-else-if="detail?.error" class="routing-drawer-state error"><Icon name="alert" :size="17" /><span>{{ detail.error }}</span></div>
      <div v-else-if="detail" class="routing-drawer-body">
        <section class="routing-conclusion"><span><Icon name="check" :size="18" /></span><div><small>本次选路结论</small><strong>{{ detail.selected_upstream || selected.selected_upstream || '未选中渠道' }}</strong><p>{{ reasonLabel(detail) }}</p></div><b :class="confidenceClass(detail.confidence)">{{ fmtConfidence(detail.confidence) }} 置信度</b></section>

        <section class="routing-drawer-section"><header><h4>候选渠道成本</h4><span>{{ detail.candidate_count || candidates.length }} 个候选</span></header><div class="routing-candidate-list">
          <div v-for="item in candidates" :key="item.id || item.upstream_id" class="routing-candidate" :class="{ selected: item.selected, rejected: !item.eligible }">
            <div class="routing-candidate-top"><strong :title="item.upstream_name">{{ item.upstream_name || `渠道 #${item.upstream_id}` }}</strong><span v-if="item.selected">已选</span><span v-else-if="!item.eligible" class="rejected">已排除</span></div>
            <div v-if="item.eligible" class="routing-candidate-bar"><span><i :style="{ width: costWidth(item) }"></i></span><b>{{ fmtCost(candidateCost(item)) }}</b></div>
            <div class="routing-candidate-meta"><span v-if="item.estimated_ttft_ms">TTFT {{ Math.round(item.estimated_ttft_ms) }}ms</span><span v-if="item.success_rate">成功率 {{ Math.round(item.success_rate * 100) }}%</span><span v-if="item.cache_supported">缓存{{ item.cache_hit_rate ? ` ${Math.round(item.cache_hit_rate * 100)}%` : '可用' }}</span><em v-if="!item.eligible">{{ item.rejection_reason || '不满足当前条件' }}</em></div>
          </div>
        </div></section>

        <section class="routing-drawer-section"><header><h4>请求上下文</h4></header><dl class="routing-detail-grid"><div><dt>策略</dt><dd>{{ detail.strategy || 'cost' }}</dd></div><div><dt>协议</dt><dd>{{ detail.protocol || '—' }}</dd></div><div><dt>输入 Token</dt><dd>{{ fmtTokens(detail.estimated_input_tokens) }}</dd></div><div><dt>可复用前缀</dt><dd>{{ fmtTokens(detail.reusable_prefix_tokens) }}</dd></div><div><dt>预估输出</dt><dd>{{ fmtTokens(detail.estimated_output_tokens) }}</dd></div><div><dt>预测窗口</dt><dd>{{ detail.forecast_window_seconds ? `${Math.round(detail.forecast_window_seconds / 60)} 分钟` : '—' }}</dd></div></dl></section>

        <section class="routing-drawer-section"><header><h4>实际结果</h4><span class="routing-outcome" :class="outcomeClass(detail.actual_outcome)"><i></i>{{ outcomeLabel(detail.actual_outcome) }}</span></header><dl class="routing-detail-grid"><div><dt>实际成本</dt><dd>{{ detail.actual_cost == null ? '—' : fmtCost(detail.actual_cost) }}</dd></div><div><dt>输入 Token</dt><dd>{{ fmtTokens(detail.actual_input_tokens) }}</dd></div><div><dt>输出 Token</dt><dd>{{ fmtTokens(detail.actual_output_tokens) }}</dd></div><div><dt>缓存命中</dt><dd>{{ fmtTokens(detail.actual_cached_tokens) }}</dd></div><div><dt>缓存创建</dt><dd>{{ fmtTokens(detail.actual_cache_creation_tokens) }}</dd></div><div><dt>请求 ID</dt><dd class="routing-request-id" :title="detail.request_id">{{ detail.request_id || '—' }}</dd></div></dl></section>
      </div>
    </aside>
  </div>
</template>

<style scoped>
.routing-page { min-width: 0; display: flex; flex-direction: column; gap: 16px; padding-bottom: 28px; }
.routing-page-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 18px; padding: 4px 2px 2px; }
.routing-kicker { display: inline-flex; align-items: center; gap: 7px; color: var(--g400); font-family: var(--font-data); font-size: 9px; font-weight: 700; letter-spacing: .08em; }
.routing-kicker span { width: 7px; height: 7px; border-radius: 50%; background: var(--brand-pink); box-shadow: 0 0 0 4px color-mix(in srgb, var(--brand-pink) 14%, transparent); }
.routing-page-head h2 { margin-top: 4px; color: var(--g900); font-size: 24px; line-height: 1.2; }.routing-page-head p { margin-top: 5px; color: var(--g500); font-size: 12px; }
.routing-head-actions { display: flex; align-items: center; gap: 10px; padding-top: 8px; }.routing-live-toggle { display: inline-flex; align-items: center; gap: 7px; color: var(--g500); font-size: 12px; cursor: pointer; }.routing-live-toggle input { width: 14px; height: 14px; accent-color: var(--p500); }
.routing-stats { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 10px; }.routing-stats article { min-width: 0; display: flex; align-items: center; gap: 10px; min-height: 78px; padding: 13px 14px; border: 1px solid var(--line); border-radius: var(--r-card); background: var(--surface); box-shadow: var(--sh-card); }.routing-stats article > i { width: 8px; height: 38px; flex: none; border-radius: 5px; background: var(--brand-pink); }.routing-stats article > i.mint { background: var(--p500); }.routing-stats article > i.amber { background: var(--amber); }.routing-stats article > i.blue { background: var(--blue); }.routing-stats article div { min-width: 0; display: flex; flex-direction: column; }.routing-stats small { color: var(--g500); font-size: 11px; }.routing-stats strong { color: var(--g900); font-family: var(--font-data); font-size: 20px; line-height: 1.25; }.routing-stats em { min-width: 0; margin-left: auto; overflow: hidden; color: var(--g400); font-size: 10px; font-style: normal; text-align: right; text-overflow: ellipsis; white-space: nowrap; }
.routing-toolbar { display: flex; align-items: center; gap: 12px; min-width: 0; padding: 11px 12px; border: 1px solid var(--line); border-radius: var(--r-card); background: color-mix(in srgb, var(--surface) 90%, var(--p50)); }.routing-search { min-width: 180px; max-width: 300px; flex: 1 1 220px; display: flex; align-items: center; gap: 7px; height: 34px; padding: 0 9px; border: 1px solid var(--control-border); border-radius: var(--r-control); background: var(--surface-raised); color: var(--g400); }.routing-search:focus-within { border-color: var(--p400); box-shadow: 0 0 0 3px var(--focus-ring); }.routing-search input { min-width: 0; flex: 1; border: 0; outline: 0; background: transparent; color: var(--g800); font-size: 12px; }.routing-search button { display: grid; place-items: center; border: 0; background: transparent; color: var(--g400); cursor: pointer; }.routing-filter-tabs { display: flex; gap: 3px; padding: 3px; border-radius: var(--r-control); background: var(--g50); }.routing-filter-tabs button { display: inline-flex; align-items: center; justify-content: center; gap: 5px; height: 28px; padding: 0 10px; border: 0; border-radius: 8px; background: transparent; color: var(--g500); font-size: 11px; cursor: pointer; }.routing-filter-tabs button.active { background: var(--surface-raised); color: var(--p700); box-shadow: 0 2px 7px rgba(49,42,52,.08); font-weight: 700; }.routing-filter-tabs b { min-width: 16px; padding: 1px 4px; border-radius: 8px; background: var(--state-danger-soft); color: var(--state-danger); font-size: 9px; }.routing-result-count { flex: none; margin-left: auto; color: var(--g400); font-family: var(--font-data); font-size: 11px; }
.routing-inline-error { display: flex; align-items: center; gap: 8px; padding: 12px 14px; border: 1px solid var(--state-danger-line); border-radius: var(--r-card); background: var(--state-danger-soft); color: var(--state-danger); font-size: 12px; }.routing-inline-error span { min-width: 0; flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }.routing-inline-error button { border: 0; background: transparent; color: inherit; font-size: 11px; font-weight: 700; cursor: pointer; }
.routing-table-shell { overflow: hidden; border: 1px solid var(--line); border-radius: var(--r-card); background: var(--surface); box-shadow: var(--sh-card); }.routing-table-head, .routing-table-row { display: grid; grid-template-columns: 112px minmax(130px,1.15fr) minmax(120px,1fr) minmax(170px,1.5fr) 88px 64px 86px 24px; gap: 12px; align-items: center; }.routing-table-head { min-height: 38px; padding: 0 14px; border-bottom: 1px solid var(--line); background: var(--surface-muted); color: var(--g400); font-size: 10px; font-weight: 700; }.routing-table-row { min-height: 68px; padding: 8px 14px; border-bottom: 1px solid var(--divider); cursor: pointer; transition: background .16s, box-shadow .16s; }.routing-table-row:last-child { border-bottom: 0; }.routing-table-row:hover, .routing-table-row:focus-visible { outline: 0; background: var(--surface-hover); box-shadow: inset 3px 0 0 var(--brand-pink); }.routing-table-row > div { min-width: 0; }
.routing-time-cell,.routing-request-cell,.routing-channel-cell,.routing-cost-cell { display: flex; flex-direction: column; gap: 2px; min-width: 0; }.routing-time-cell strong { color: var(--g600); font-family: var(--font-data); font-size: 11px; }.routing-time-cell small,.routing-request-cell small,.routing-channel-cell small,.routing-cost-cell small { overflow: hidden; color: var(--g400); font-size: 10px; text-overflow: ellipsis; white-space: nowrap; }.routing-request-cell strong,.routing-channel-cell strong { overflow: hidden; color: var(--g800); font-size: 12px; text-overflow: ellipsis; white-space: nowrap; }.routing-channel-cell { position: relative; padding-left: 13px; }.routing-channel-cell > i { position: absolute; top: 5px; left: 0; width: 7px; height: 7px; border-radius: 50%; background: var(--p500); box-shadow: 0 0 0 3px var(--p50); }.routing-channel-cell small { color: var(--p700); }.routing-reason-cell { display: flex; align-items: center; gap: 7px; overflow: hidden; color: var(--g500); font-size: 11px; white-space: nowrap; }.routing-reason-cell > span { overflow: hidden; text-overflow: ellipsis; }.routing-reason-cell b { flex: none; padding: 3px 6px; border-radius: 6px; background: var(--g50); color: var(--g500); font-size: 9px; }.routing-reason-cell b.cache { background: var(--state-success-soft); color: var(--state-success); }.routing-reason-cell b.explore { background: var(--state-warning-soft); color: var(--state-warning); }.routing-cost-cell strong { color: var(--g700); font-family: var(--font-data); font-size: 12px; }.routing-confidence-cell span { display: inline-flex; padding: 3px 5px; border-radius: 6px; background: var(--state-success-soft); color: var(--state-success); font-family: var(--font-data); font-size: 10px; font-weight: 700; }.routing-confidence-cell span.medium { background: var(--state-warning-soft); color: var(--state-warning); }.routing-confidence-cell span.low { background: var(--state-danger-soft); color: var(--state-danger); }.routing-outcome { display: inline-flex; align-items: center; gap: 5px; color: var(--state-success); font-size: 10px; font-weight: 700; white-space: nowrap; }.routing-outcome i { width: 6px; height: 6px; border-radius: 50%; background: currentColor; }.routing-outcome.is-partial,.routing-outcome.is-pending { color: var(--state-warning); }.routing-outcome.is-failed,.routing-outcome.is-client_error { color: var(--state-danger); }.routing-outcome.is-canceled { color: var(--g400); }.routing-row-arrow { color: var(--g300); }.routing-table-row:hover .routing-row-arrow { color: var(--p600); transform: translateX(2px); }
.routing-empty { display: flex; flex-direction: column; align-items: center; gap: 5px; padding: 62px 20px; border: 1px dashed var(--g200); border-radius: var(--r-card); color: var(--g500); }.routing-empty > span { display: grid; place-items: center; width: 38px; height: 38px; margin-bottom: 4px; border-radius: 13px; background: var(--p50); color: var(--p600); }.routing-empty strong { color: var(--g700); font-size: 13px; }.routing-empty small { color: var(--g400); font-size: 11px; }.routing-skeleton { display: flex; flex-direction: column; gap: 1px; overflow: hidden; border: 1px solid var(--line); border-radius: var(--r-card); }.routing-skeleton span { height: 68px; background: linear-gradient(90deg,var(--surface-muted),var(--surface-hover),var(--surface-muted)); background-size: 200% 100%; animation: shimmer 1.4s infinite; }.routing-skeleton span:first-child { height: 38px; }@keyframes shimmer { to { background-position: -200% 0; } }
.routing-drawer-mask { position: fixed; inset: 0; z-index: 70; display: flex; justify-content: flex-end; background: rgba(42,35,48,.24); backdrop-filter: blur(3px); }.routing-drawer { width: min(500px,94vw); height: 100%; overflow-y: auto; border-left: 1px solid var(--line); background: var(--surface-raised); box-shadow: -14px 0 42px rgba(49,42,52,.18); animation: drawer-in .22s cubic-bezier(.22,1,.36,1); }.routing-drawer-head { display: flex; justify-content: space-between; gap: 12px; padding: 23px 22px 18px; border-bottom: 1px solid var(--divider); background: var(--surface-muted); }.routing-drawer-head small { color: var(--g400); font-family: var(--font-data); font-size: 10px; }.routing-drawer-head h3 { margin-top: 5px; overflow: hidden; color: var(--g900); font-size: 19px; text-overflow: ellipsis; white-space: nowrap; }.routing-drawer-head p { margin-top: 4px; color: var(--g500); font-size: 11px; }.routing-drawer-state { display: flex; align-items: center; justify-content: center; gap: 8px; min-height: 170px; color: var(--g500); font-size: 12px; }.routing-drawer-state.error { color: var(--state-danger); }.routing-drawer-body { display: flex; flex-direction: column; gap: 18px; padding: 18px 22px 30px; }
.routing-conclusion { display: flex; align-items: flex-start; gap: 11px; padding: 13px; border: 1px solid color-mix(in srgb,var(--p500) 24%,var(--line)); border-radius: var(--r-card); background: var(--p50); }.routing-conclusion > span { display: grid; place-items: center; width: 34px; height: 34px; flex: none; border-radius: 11px; background: var(--p500); color: var(--text-on-accent); }.routing-conclusion > div { min-width: 0; flex: 1; }.routing-conclusion small { color: var(--p700); font-size: 10px; }.routing-conclusion strong { display: block; overflow: hidden; color: var(--g900); font-size: 14px; text-overflow: ellipsis; white-space: nowrap; }.routing-conclusion p { overflow: hidden; color: var(--g600); font-size: 11px; text-overflow: ellipsis; white-space: nowrap; }.routing-conclusion > b { flex: none; padding: 4px 6px; border-radius: 6px; background: var(--state-success-soft); color: var(--state-success); font-family: var(--font-data); font-size: 9px; }.routing-conclusion > b.medium { background: var(--state-warning-soft); color: var(--state-warning); }.routing-conclusion > b.low { background: var(--state-danger-soft); color: var(--state-danger); }
.routing-drawer-section > header { display: flex; align-items: center; justify-content: space-between; gap: 10px; margin-bottom: 9px; }.routing-drawer-section h4 { color: var(--g800); font-size: 12px; }.routing-drawer-section > header > span { color: var(--g400); font-size: 10px; }.routing-candidate-list { display: flex; flex-direction: column; gap: 6px; }.routing-candidate { padding: 9px 10px; border: 1px solid var(--line); border-radius: var(--r-control); background: var(--surface); }.routing-candidate.selected { border-color: color-mix(in srgb,var(--p500) 40%,var(--line)); background: var(--p50); }.routing-candidate.rejected { opacity: .68; }.routing-candidate-top { display: flex; align-items: center; gap: 8px; }.routing-candidate-top strong { min-width: 0; overflow: hidden; color: var(--g700); font-size: 11px; text-overflow: ellipsis; white-space: nowrap; }.routing-candidate-top span { margin-left: auto; color: var(--p700); font-size: 9px; font-weight: 700; }.routing-candidate-top span.rejected { color: var(--g400); }.routing-candidate-bar { display: flex; align-items: center; gap: 9px; margin-top: 7px; }.routing-candidate-bar > span { min-width: 0; height: 7px; flex: 1; overflow: hidden; border-radius: 99px; background: var(--g100); }.routing-candidate-bar i { display: block; height: 100%; border-radius: inherit; background: var(--g300); }.routing-candidate.selected .routing-candidate-bar i { background: linear-gradient(90deg,var(--brand-pink),var(--p500)); }.routing-candidate-bar b { width: 60px; color: var(--g600); font-family: var(--font-data); font-size: 10px; text-align: right; }.routing-candidate-meta { display: flex; flex-wrap: wrap; gap: 7px 11px; margin-top: 6px; color: var(--g400); font-size: 9px; }.routing-candidate-meta em { color: var(--state-danger); font-style: normal; }.routing-detail-grid { display: grid; grid-template-columns: repeat(2,minmax(0,1fr)); gap: 1px; overflow: hidden; border: 1px solid var(--line); border-radius: var(--r-control); }.routing-detail-grid div { min-width: 0; display: flex; flex-direction: column; gap: 3px; padding: 8px 9px; background: var(--surface-muted); }.routing-detail-grid dt { color: var(--g400); font-size: 9px; }.routing-detail-grid dd { overflow: hidden; color: var(--g700); font-family: var(--font-data); font-size: 11px; font-weight: 600; text-overflow: ellipsis; white-space: nowrap; }.routing-request-id { font-size: 9px !important; }@keyframes drawer-in { from { opacity: .5; transform: translateX(28px); } }
@media (max-width: 980px) { .routing-table-head,.routing-table-row { grid-template-columns: 100px minmax(120px,1fr) minmax(110px,1fr) minmax(150px,1.3fr) 76px 58px 80px 20px; gap: 8px; padding-inline: 11px; }.routing-stats em { display: none; } }
@media (max-width: 720px) { .routing-page { gap: 12px; }.routing-page-head h2 { font-size: 21px; }.routing-live-toggle span { display: none; }.routing-stats { grid-template-columns: repeat(2,minmax(0,1fr)); gap: 8px; }.routing-stats article { min-height: 66px; }.routing-stats strong { font-size: 17px; }.routing-toolbar { flex-wrap: wrap; gap: 8px; padding: 9px; }.routing-search { max-width: none; flex-basis: 100%; }.routing-filter-tabs { flex: 1; justify-content: space-between; }.routing-filter-tabs button { flex: 1; padding-inline: 4px; font-size: 10px; }.routing-result-count { margin-left: 0; }.routing-table-head { display: none; }.routing-table-row { grid-template-columns: minmax(0,1fr) auto 17px; grid-template-areas: 'request cost arrow' 'channel outcome arrow' 'reason reason arrow'; gap: 5px 8px; min-height: 91px; padding: 11px 12px; }.routing-time-cell { display: none; }.routing-request-cell { grid-area: request; }.routing-channel-cell { grid-area: channel; }.routing-reason-cell { grid-area: reason; }.routing-cost-cell { grid-area: cost; align-items: flex-end; }.routing-confidence-cell { display: none; }.routing-outcome-cell { grid-area: outcome; }.routing-row-arrow { grid-area: arrow; align-self: center; }.routing-drawer { width: 100%; }.routing-drawer-head { padding: 18px 15px 15px; }.routing-drawer-body { padding: 14px 15px 24px; }.routing-conclusion > b { display: none; } }
</style>
