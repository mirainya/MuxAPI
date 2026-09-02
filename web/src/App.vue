<script setup>
// 管理后台页面壳：集中管理页面状态、轮询、表单动作和管理 API 调用。
// 大型派生视图使用 computed，跨页面异步请求使用 epoch 防止旧响应覆盖新状态。
import { ref, reactive, onMounted, onUnmounted, computed, watch, nextTick } from 'vue'
import { useRoute } from 'vue-router'
import Icon from './Icon.vue'
import Fence from './Fence.vue'
import FancySelect from './FancySelect.vue'
import UpstreamPicker from './UpstreamPicker.vue'
import Chart from './Chart.vue'
import ThemePicker from './ThemePicker.vue'
import RoutingView from './RoutingView.vue'
import { api } from './api.js'
import { useLogs } from './composables/useLogs.js'
import { useMonitorViews } from './composables/useMonitorViews.js'

const route = useRoute()
const pageNames = new Set(['overview', 'groups', 'upstreams', 'monitors', 'logs', 'routing', 'settings'])
const page = computed(() => pageNames.has(String(route.name)) ? String(route.name) : 'overview')
const detailGroup = ref(null)     // 进入分组详情时设置

// 后端资源缓存；详情页成员与密钥只对应 detailGroup。
const groups = ref([])
const upstreams = ref([])         // 全局上游池
const members = ref([])           // 当前详情分组的成员
const keys = ref([])              // 当前详情分组的密钥
const monitors = ref([])          // 监控项（含探测快照）
const tags = ref([])              // 上游管理标签
const err = ref('')
const errStatus = ref(0)
const errorInfo = computed(() => {
  const raw = String(err.value || '').trim()
  const status = errStatus.value || (/^\d{3}$/.test(raw) ? Number(raw) : 0)
  if (status === 401) return { title: '登录已失效', detail: '请重新输入管理 Token。', code: 'HTTP 401' }
  if (status === 403) return { title: '当前操作无权限', detail: '请检查管理 Token 的权限。', code: 'HTTP 403' }
  if (status === 404) return { title: '请求的资源不存在', detail: '页面数据可能已被删除或移动。', code: 'HTTP 404' }
  if (status >= 500) return { title: '服务暂时不可用', detail: '无法读取最新数据，请稍后重试。', code: `HTTP ${status}` }
  if (/服务器响应超时|请求超时/i.test(raw)) {
    return { title: '服务器响应较慢', detail: '本次读取已停止，请重新加载。', code: '' }
  }
  if (/failed to fetch|networkerror|network request failed/i.test(raw)) {
    return { title: '无法连接服务', detail: '请检查网络或服务运行状态后重试。', code: '' }
  }
  return { title: '操作未完成', detail: raw || '请求未能完成，请稍后重试。', code: status ? `HTTP ${status}` : '' }
})
function clearError() { err.value = ''; errStatus.value = 0 }
const loggedIn = ref(!!api.getToken())
const loginForm = reactive({ token: api.getToken() })
// 页面级加载状态：首次进入或切换页面时保留稳定布局，避免慢查询期间出现大片空白。
const pageLoading = ref(!!loggedIn.value)
const pageLoadingLabel = ref('正在加载页面')
let pageLoadEpoch = 0
const overviewStats = ref({})
const overviewLoading = ref(false)
const overviewSummary = ref(null)
const overviewSummaryLoading = ref(false)
const overviewTrendWindow = ref('24h')
const overviewTrendTagID = ref(0)
const overviewTrends = ref(null)
const overviewTrendLoading = ref(false)
const overviewTrendError = ref('')
const overviewBalanceCurrency = ref('')
let overviewSummaryEpoch = 0
let overviewTrendEpoch = 0

async function loadGroups() { groups.value = (await api.groups()) || [] }
async function loadUpstreams() { upstreams.value = (await api.upstreams()) || [] }
async function loadOverviewUpstreams() { upstreams.value = (await api.overviewUpstreams()) || [] }
async function loadMonitors() { monitors.value = (await api.monitors()) || [] }
async function loadTags() { tags.value = (await api.tags()) || [] }

async function loadOverviewTrends() {
  const epoch = ++overviewTrendEpoch
  overviewTrendLoading.value = true
  overviewTrendError.value = ''
  try {
    const data = await api.overviewTrends({ window: overviewTrendWindow.value, tag_id: overviewTrendTagID.value })
    if (epoch !== overviewTrendEpoch) return
    overviewTrends.value = data || null
    const currencies = (data?.balances || []).map(series => series.currency)
    if (!currencies.includes(overviewBalanceCurrency.value)) overviewBalanceCurrency.value = currencies[0] || ''
  } catch (e) {
    if (epoch === overviewTrendEpoch) {
      overviewTrendError.value = e.status === 404
        ? '当前服务端版本不含趋势接口，请重新编译并重启 MuxAPI'
        : String(e.message || e)
    }
  } finally {
    if (epoch === overviewTrendEpoch) overviewTrendLoading.value = false
  }
}

async function loadOverviewSummary() {
  const epoch = ++overviewSummaryEpoch
  overviewSummaryLoading.value = true
  try {
    const data = await api.overviewSummary()
    if (epoch === overviewSummaryEpoch) overviewSummary.value = data || null
  } catch (e) {
    if (epoch !== overviewSummaryEpoch) return
    if (e.status === 404) {
      const today = new Date()
      today.setHours(0, 0, 0, 0)
      try {
        const stats = await api.logStats({ since: Math.floor(today.getTime() / 1000) })
        if (epoch === overviewSummaryEpoch) {
          overviewSummary.value = { today_requests: Number(stats?.total) || 0, week_cost: null }
        }
      } catch {
        if (epoch === overviewSummaryEpoch) overviewSummary.value = null
      }
    } else if (!overviewSummary.value) {
      overviewSummary.value = null
    }
  } finally {
    if (epoch === overviewSummaryEpoch) overviewSummaryLoading.value = false
  }
}

function changeOverviewTag(value) {
  overviewTrendTagID.value = Number(value) || 0
  guard(loadOverviewTrends)
}

// 总览只读取需要处理的运行数据；不复用请求记录页的筛选状态。
async function loadOverview() {
  overviewLoading.value = true
  const since = Math.floor(Date.now() / 1000) - 24 * 60 * 60
  try {
    // 统计与标签很快返回，先结束页面级加载；上游和监控列表经远程库查询，完成后再渐进填充。
    const [stats, tagList] = await Promise.all([
      api.logStats({ since }), api.tags(),
    ])
    overviewStats.value = stats || {}
    tags.value = tagList || []
    void loadOverviewUpstreams().catch(() => {})
    void loadMonitors().catch(() => {})
    // 趋势查询较重，不阻塞总览基础内容显示；趋势区域保留自己的加载状态。
    void loadOverviewTrends()
    void loadOverviewSummary()
  } finally {
    overviewLoading.value = false
  }
}

async function refreshOverviewData() {
  await Promise.all([loadOverviewTrends(), loadOverviewSummary()])
}

// 单项「立即探测」：探完用返回的快照原地更新该卡片
// 用 Set 记录正在探测的卡片 id，支持多卡并发互不串台
const probing = reactive(new Set())
const probingChannels = reactive(new Set())
const removingChannels = reactive(new Set())
const recoveringUpstreams = reactive(new Set())
const recoveringModels = reactive(new Set())
const refreshingBilling = reactive(new Set())
// 计费比对明细：展开的上游 id、各上游选中的窗口、已取回的区间比对结果。
// 明细放内联面板而非原生 title——余额、缺价模型、价目版本这些要能选中复制、
// 触屏可看，原生 tooltip 做不到。
const billingDetailOpen = reactive(new Set())
const billingWindowFor = reactive({})
const billingRangeAudit = reactive({})
const billingRangeLoading = reactive(new Set())
async function probeOne(m) {
  probing.add(m.id)
  try {
    const sn = await api.probeMonitor(m.id)
    m.snapshot = sn
    const source = monitors.value.find(item => item.id === m.id)
    if (source && source !== m) source.snapshot = sn
  } finally { probing.delete(m.id) }
}
async function probeChannel(channel) {
  const items = channel.monitors.filter(item => item.enabled && item.upstream?.enabled)
  if (!items.length) return
  probingChannels.add(channel.id)
  try {
    // 单个模型失败不应中止整个渠道的检测，确保渠道状态由完整模型集合计算。
    for (const item of items) {
      try { await probeOne(item) } catch { /* 结果会由该模型的历史状态保留 */ }
    }
  } finally {
    probingChannels.delete(channel.id)
  }
}

const {
  primaryTagFor, auxiliaryTagsFor, tagGroupKey, tagGroupName,
  monitorItems, monitorChannels, monitorSearch, monitorStatusFilter,
  monitorTagFilter, collapsedMonitorTags, monitorStatusOptions, monitorTagOptions,
  monitorSections, monitorVisibleCount, summary, toggleMonitorTag, matrix, ovSummary,
  cellDrawerId, cellDrawer, openCell, closeCell, probeCell,
} = useMonitorViews({ monitors, upstreams, tags, probeOne })
// 分组详情加载守卫：每次切换详情/返回都自增 epoch，
// 参数化加载(members/keys)返回时校验 epoch 未变才写入，避免快速切换短暂错配。
let loadEpoch = 0
async function loadMembers(gid) {
  const ep = loadEpoch
  const data = (await api.members(gid)) || []
  if (ep === loadEpoch) members.value = data
}
const mhTitle = mh => mh.model + ' · 当前渠道暂不支持；点击立即恢复'
async function loadDetail(gid) {
  const ep = loadEpoch
  await loadMembers(gid)
  const ks = (await api.keys(gid)) || []
  if (ep === loadEpoch) keys.value = ks
}

// guard 统一展示接口错误，并在管理凭证失效时返回登录页。
async function guard(fn) {
  clearError()
  try { await fn() } catch (e) {
    errStatus.value = Number(e.status) || 0
    if (e.status === 401) {
      loggedIn.value = false
      api.clearToken()
      err.value = '未授权，请输入管理 Token'
      return
    }
    err.value = String(e.message || e)
  }
}

const appVersion = ref('')
onMounted(() => {
  fetch('/admin/version').then(r => r.json()).then(d => { appVersion.value = d.version || 'dev' }).catch(() => {})
  if (loggedIn.value) activatePage(page.value)
})

// 看板自动刷新：探测间隔 5min，这里每 60s 拉一次快照即可，离开即停
let monTimer = null
function startMonPoll() {
  stopMonPoll()
  monTimer = setInterval(() => { loadMonitors().catch(() => {}) }, 60000)
}
function stopMonPoll() { if (monTimer) { clearInterval(monTimer); monTimer = null } }
let overviewTimer = null
function startOverviewPoll() {
  stopOverviewPoll()
  overviewTimer = setInterval(() => { loadOverview().catch(() => {}) }, 60000)
}
function stopOverviewPoll() { if (overviewTimer) { clearInterval(overviewTimer); overviewTimer = null } }

// 运行时状态轮询：分组列表/分组详情/上游池页，每 8s 刷新健康，离开即停。
let rtTimer = null
function startRtPoll(fn) {
  stopRtPoll()
  rtTimer = setInterval(() => { fn().catch(() => {}) }, 8000)
}
function stopRtPoll() { if (rtTimer) { clearInterval(rtTimer); rtTimer = null } }
function stopAllPoll() { stopMonPoll(); stopOverviewPoll(); stopRtPoll(); stopLogPoll() }
onUnmounted(() => {
  stopAllPoll()
  abortUpstreamTestRequests?.()
  abortGroupTestRequests?.()
})

function activatePage(p) {
  const epoch = ++pageLoadEpoch
  pageLoading.value = true
  pageLoadingLabel.value = ({ overview: '正在读取总览', groups: '正在读取分组', upstreams: '正在读取上游池', monitors: '正在读取监控', logs: '正在读取请求记录', settings: '正在读取设置' })[p] || '正在加载页面'
  detailGroup.value = null
  window.scrollTo({ top: 0, behavior: 'instant' })
  cellDrawerId.value = null; logDetail.value = null
  loadEpoch++   // 离开详情，作废在途的 members/keys 加载
  stopAllPoll()
  guard(async () => {
    if (p === 'overview') { await loadOverview(); startOverviewPoll() }
    else if (p === 'groups') { await loadGroups(); startRtPoll(loadGroups) }
    else if (p === 'upstreams') { await loadTags(); await loadGroups(); await loadUpstreams(); startRtPoll(loadUpstreams) }
    else if (p === 'monitors') { await loadTags(); await loadUpstreams(); await loadMonitors(); startMonPoll() }
    else if (p === 'logs') { await loadLogOptions(); await loadLogs(true); startLogPoll() }
    else if (p === 'settings') { await loadSettings(); await Promise.all([loadBackupConfig(), loadBackupSchedule(), loadBackups(), loadUpstreams(), loadMappings()]) }
  }).finally(() => {
    if (epoch === pageLoadEpoch) pageLoading.value = false
  })
}
function retryCurrentView() {
  if (pageLoading.value) return
  if (detailGroup.value) openDetail(detailGroup.value)
  else activatePage(page.value)
}
watch(() => route.name, () => {
  if (loggedIn.value) activatePage(page.value)
})
function openDetail(g) {
  const epoch = ++pageLoadEpoch
  pageLoading.value = true
  pageLoadingLabel.value = '正在读取分组详情'
  detailGroup.value = g
  loadEpoch++   // 切入新分组详情，作废上一组在途加载
  stopAllPoll()
  guard(async () => { await loadUpstreams(); await loadDetail(g.id); startRtPoll(() => loadMembers(g.id)) }).finally(() => {
    if (epoch === pageLoadEpoch) pageLoading.value = false
  })
}
function backToGroups() {
  const epoch = ++pageLoadEpoch
  pageLoading.value = true
  pageLoadingLabel.value = '正在读取分组'
  detailGroup.value = null; loadEpoch++; stopAllPoll()
  guard(async () => { await loadGroups(); startRtPoll(loadGroups) }).finally(() => {
    if (epoch === pageLoadEpoch) pageLoading.value = false
  })
}

// 上游池里未加入当前分组的（供"添加成员"下拉）
const memberIds = computed(() => new Set(members.value.map(m => m.upstream_id)))
const addable = computed(() => upstreams.value.filter(u => !memberIds.value.has(u.id)))

const pages = {
  overview: { title: '总览', desc: '优先处理余额、计费与服务异常' },
  groups: { title: '分组管理', desc: '每个分组是一个独立的调度池，拥有自己的上游与接入密钥' },
  upstreams: { title: '上游池', desc: '按主标签管理全局渠道，并用普通标签快速筛选' },
  monitors: { title: '监控看板', desc: '按主标签组织模型探测卡片与运行时健康' },
  logs: { title: '请求记录', desc: '每一次转发请求的真实去向：模型 → 选中渠道 → 状态 → 延迟' },
  routing: { title: '路由决策', desc: '实时查看路由引擎的选路逻辑与候选成本对比' },
  settings: { title: '设置', desc: '运行时配置，保存后即时生效（无需重启）' },
}

// --- 弹窗状态 ---
const dlg = reactive({ type: '', form: {} })
const upstreamDraft = ref(null)
const dialogSaving = ref(false)
const upstreamVendor = ref('')
const upstreamImportText = ref('')
const upstreamImportMessage = ref('')
const upstreamImportError = ref(false)
const upstreamBaseURLDirty = ref(false)
const upstreamFormGroupSearch = ref('')
const upstreamFormGroupChoices = computed(() => {
  const selected = new Set(upstreamFormGroupIDs.value)
  const query = upstreamFormGroupSearch.value.trim().toLowerCase()
  return groups.value.filter(g => !selected.has(g.id) && (!query || g.name.toLowerCase().includes(query)))
})
const tagManagerSearch = ref('')
function closeDlg({ preserveDraft = false } = {}) {
  const closingType = dlg.type
  if (preserveDraft && closingType === 'upstream' && !dlg.form.id) {
    upstreamDraft.value = {
      form: { ...dlg.form, tag_ids: [...(dlg.form.tag_ids || [])] },
      groupIDs: [...upstreamFormGroupIDs.value],
      vendor: upstreamVendor.value,
      importText: upstreamImportText.value,
      importMessage: upstreamImportMessage.value,
      importError: upstreamImportError.value,
      baseURLDirty: upstreamBaseURLDirty.value,
    }
  } else if (!preserveDraft && closingType === 'upstream') {
    upstreamDraft.value = null
  }
  dlg.type = ''
  upstreamVendor.value = ''
  upstreamImportText.value = ''
  upstreamImportMessage.value = ''
  upstreamImportError.value = false
  upstreamBaseURLDirty.value = false
  upstreamFormGroupSearch.value = ''
  tagManagerSearch.value = ''
}

// 表单保存统一显示进行中状态，避免重复点击和无反馈等待。
async function guardDialogSave(fn) {
  if (dialogSaving.value) return
  dialogSaving.value = true
  try { await guard(fn) } finally { dialogSaving.value = false }
}

// 通用确认弹窗：confirm(消息, 危险操作回调)
const confirmState = reactive({ show: false, msg: '', onOk: null })
function ask(msg, onOk) { confirmState.show = true; confirmState.msg = msg; confirmState.onOk = onOk }
function confirmOk() { confirmState.show = false; confirmState.onOk?.() }

function newGroup() { dlg.type = 'group'; dlg.form = { name: '', description: '', max_multiplier: '' } }
function editGroup(g) { dlg.type = 'group'; dlg.form = { id: g.id, name: g.name, description: g.description, max_multiplier: g.max_multiplier ?? '' } }
function saveGroup() {
  guardDialogSave(async () => {
    const rawMultiplier = String(dlg.form.max_multiplier ?? '').trim()
    const maxMultiplier = rawMultiplier === '' ? null : Number(rawMultiplier)
    if (maxMultiplier !== null && (!Number.isFinite(maxMultiplier) || maxMultiplier <= 0)) throw new Error('最大计费倍率必须大于 0')
    const f = { ...dlg.form, max_multiplier: maxMultiplier }
    if (f.id) await api.updateGroup(f.id, f)
    else await api.createGroup(f)
    closeDlg(); await loadGroups()
  })
}

const protocolOptions = [
  { value: 'passthrough', label: '透传' },
  { value: 'openai', label: 'OpenAI Chat Completions' },
  { value: 'openai-response', label: 'OpenAI Responses' },
  { value: 'claude', label: 'Anthropic Messages' },
  { value: 'codex', label: 'Codex Responses' },
  { value: 'gemini', label: 'Google Gemini' },
]
// 厂商预设：选中后自动填 base_url 与协议；custom 表示手动输入。
const vendorPresets = [
  { value: 'openrouter', label: 'OpenRouter', base_url: 'https://openrouter.ai/api', protocol: 'openai' },
  { value: 'deepseek', label: 'DeepSeek', base_url: 'https://api.deepseek.com', protocol: 'openai' },
  { value: 'xai', label: 'Grok (xAI)', base_url: 'https://api.x.ai', protocol: 'openai' },
  { value: 'moonshot', label: 'Moonshot (Kimi)', base_url: 'https://api.moonshot.cn', protocol: 'openai' },
  { value: 'dashscope', label: '阿里百炼 (Qwen)', base_url: 'https://dashscope.aliyuncs.com/compatible-mode', protocol: 'openai' },
  { value: 'openai', label: 'OpenAI', base_url: 'https://api.openai.com', protocol: 'openai' },
  { value: 'anthropic', label: 'Anthropic', base_url: 'https://api.anthropic.com', protocol: 'claude' },
  { value: 'codex', label: 'Codex (OpenAI Responses)', base_url: 'https://api.openai.com', protocol: 'codex' },
  { value: 'custom', label: '自定义…', base_url: '', protocol: '' },
]
function applyVendorPreset(value) {
  const preset = vendorPresets.find(p => p.value === value)
  if (!preset || dlg.form.id) return // 编辑已有上游时不覆盖
  // 预设只接管空地址或仍由预设填充的地址，避免覆盖用户已经手动输入的内容。
  if (preset.base_url && (!String(dlg.form.base_url || '').trim() || !upstreamBaseURLDirty.value)) dlg.form.base_url = preset.base_url
  if (preset.protocol) dlg.form.protocol = preset.protocol
}
function markUpstreamBaseURLDirty() { upstreamBaseURLDirty.value = true }

function importedValue(source, keys) {
  for (const key of keys) {
    if (source && source[key] != null && String(source[key]).trim() !== '') return String(source[key]).trim()
  }
  return ''
}
function importedProtocol(value) {
  const normalized = String(value || '').trim().toLowerCase()
  return ({
    'openai-chat': 'openai',
    'openai_chat': 'openai',
    chat: 'openai',
    anthropic: 'claude',
    claude: 'claude',
    responses: 'openai-response',
    'openai-responses': 'openai-response',
    codex: 'codex',
    passthrough: 'passthrough',
    relay: 'passthrough',
  })[normalized] || (protocolOptions.some(option => option.value === normalized) ? normalized : '')
}
function parseUpstreamImport(raw) {
  const text = String(raw || '').trim()
  if (!text) throw new Error('请先粘贴上游配置')
  if (text.startsWith('{') || text.startsWith('[')) {
    let parsed
    try { parsed = JSON.parse(text) } catch { throw new Error('JSON 格式无效') }
    const source = Array.isArray(parsed) ? parsed[0] : parsed
    if (!source || typeof source !== 'object' || Array.isArray(source)) throw new Error('JSON 需要是上游对象')
    return source
  }
  const lines = text.split(/\r?\n/).map(line => line.trim()).filter(line => line && !line.startsWith('#'))
  const envValues = {}
  for (const line of lines) {
    const match = line.match(/^([a-z][a-z0-9_.-]*)\s*=\s*(.*)$/i)
    if (match) envValues[match[1].toLowerCase()] = match[2].trim().replace(/^['"]|['"]$/g, '')
  }
  if (Object.keys(envValues).length) return envValues
  const parts = lines[0].split(/\s*\|\s*|\t+/).map(value => value.trim()).filter(Boolean)
  if (parts.length >= 3) return { name: parts[0], base_url: parts[1], api_key: parts.slice(2).join(' | ') }
  if (parts.length === 2) {
    return /^https?:\/\//i.test(parts[0])
      ? { base_url: parts[0], api_key: parts[1] }
      : { name: parts[0], base_url: parts[1] }
  }
  if (/^https?:\/\//i.test(parts[0])) return { base_url: parts[0] }
  throw new Error('格式无法识别，请使用 名称 | 地址 | API Key')
}
function importUpstreamConfig() {
  upstreamImportError.value = false
  try {
    const source = parseUpstreamImport(upstreamImportText.value)
    const values = {
      name: importedValue(source, ['name', 'title', 'channel_name']),
      base_url: importedValue(source, ['base_url', 'baseUrl', 'url', 'endpoint']),
      api_key: importedValue(source, ['api_key', 'apiKey', 'key', 'token']),
      proxy: importedValue(source, ['proxy', 'proxy_url', 'proxyUrl']),
      protocol: importedProtocol(importedValue(source, ['protocol', 'type'])),
    }
    const changed = []
    if (values.name) { dlg.form.name = values.name; changed.push('名称') }
    if (values.base_url) { dlg.form.base_url = values.base_url; upstreamBaseURLDirty.value = true; changed.push('base_url') }
    if (values.api_key) { dlg.form.api_key = values.api_key; changed.push('api_key') }
    if (values.proxy) { dlg.form.proxy = values.proxy; changed.push('代理') }
    if (values.protocol) { dlg.form.protocol = values.protocol; changed.push('协议') }
    if (!changed.length) throw new Error('没有找到可导入的名称、地址或密钥字段')
    upstreamImportMessage.value = `已填充：${changed.join('、')}`
  } catch (error) {
    upstreamImportError.value = true
    upstreamImportMessage.value = error.message || '导入失败'
  }
}
const upstreamFormGroupIDs = ref([])
function toggleUpstreamFormGroup(id) {
  const selected = new Set(upstreamFormGroupIDs.value)
  selected.has(id) ? selected.delete(id) : selected.add(id)
  upstreamFormGroupIDs.value = [...selected]
}
const protocolLabels = Object.fromEntries(protocolOptions.map(option => [option.value, option.label]))
function protocolLabel(protocol) { return protocolLabels[protocol || 'passthrough'] || protocol }
const billingTypeOptions = [
  { value: 'none', label: '不采集' },
  { value: 'auto', label: '自动探测' },
  { value: 'sub2api', label: 'Sub2API' },
  { value: 'newapi', label: 'New API' },
]
const billingTypeLabels = Object.fromEntries(billingTypeOptions.map(option => [option.value, option.label]))
function billingTypeLabel(value) { return billingTypeLabels[value || 'none'] || value }
const cacheModeOptions = [
  { value: 'auto', label: '自动学习' },
  { value: 'enabled', label: '支持缓存' },
  { value: 'disabled', label: '不使用缓存' },
]
const cacheModeLabels = Object.fromEntries(cacheModeOptions.map(option => [option.value, option.label]))
function cacheModeLabel(value) { return cacheModeLabels[value || 'auto'] || value }
function billingAmount(item) {
  const state = item.billing
  if (state?.unlimited) return '无限额度'
  if (state?.remaining == null) return '余额 —'
  const currency = state.currency || 'USD'
  const amount = Number(state.remaining)
  try {
    const formatted = new Intl.NumberFormat('zh-CN', {
      style: 'currency', currency, currencyDisplay: 'narrowSymbol',
      minimumFractionDigits: 2, maximumFractionDigits: 4,
    }).format(Math.abs(amount))
    return amount < 0 ? `欠费 ${formatted}` : formatted
  } catch {
    const formatted = `${Math.abs(amount).toFixed(4)} ${currency}`
    return amount < 0 ? `欠费 ${formatted}` : formatted
  }
}
function billingMultiplier(item) {
  const value = item.billing?.effective_multiplier ?? item.billing?.group_multiplier
  if (value == null) return '倍率 —'
  return formatMultiplier(value)
}
function formatMultiplier(value) { return `${Number(value).toFixed(4).replace(/0+$/, '').replace(/\.$/, '')}x` }
function billingStatusText(item) {
  return ({ ok: '正常', partial: '部分数据', error: '失败', pending: '待采集' }[item.billing?.status] || '待采集')
}
function billingStatusClass(item) { return item.billing?.status || 'pending' }
const billingAuditReasons = {
  insufficient_snapshots: '本窗口内不足两次成功采集',
  counter_reset: '累计计费值已重置，等待下一采集窗口',
  multiplier_unavailable: '平台未提供有效计费倍率',
  pricing_catalog_unavailable: 'LiteLLM 价格目录尚不可用',
  pricing_query_failed: '本地价格查询失败',
  request_usage_incomplete: '成功请求缺少 Usage，费用数据不足',
  model_price_unavailable: '部分模型缺少 LiteLLM 价格',
  actual_cost_unavailable: '平台未提供实际费用累计值或余额变化',
  actual_cost_exceeded: '实际扣费超出「平台自报原价 × 倍率」容差',
  actual_cost_exceeded_local_basis: '实际扣费高于本地价目估算，但平台未提供自报原价，无法据此判定多收',
  multiplier_changed: '窗口内倍率有调整，偏差含调整时点落差，不作超收判定',
  catalog_cost_exceeded: '平台自报原价明显高于公共价目表估算',
}
// 本地价目估算为何不可信。只影响价目核对轨道，不阻塞计费核对。
const billingLocalPricingReasons = {
  pricing_catalog_unavailable: 'LiteLLM 价格目录尚不可用',
  request_usage_incomplete: '部分请求缺少 Usage',
  model_price_unavailable: '部分模型缺少 LiteLLM 价格',
}
// 计费核对基准：reported 用平台自报原价(不受本地价表漂移影响)；
// local 是平台未提供原价时的降级，结论会被价表差异污染。
const billingBasisLabels = { reported: '基准：平台自报原价', local: '基准：本地价目表（降级）' }
function billingCost(item, value) {
  if (value == null) return '—'
  const currency = item.billing?.currency || 'USD'
  try {
    return new Intl.NumberFormat('zh-CN', {
      style: 'currency', currency, currencyDisplay: 'narrowSymbol',
      minimumFractionDigits: 2, maximumFractionDigits: 4,
    }).format(Number(value))
  } catch {
    return `${Number(value).toFixed(4)} ${currency}`
  }
}
function billingAuditText(item) {
  const audit = item.billing?.audit
  if (!audit) return ''
  if (audit.status === 'pending') return '费用比对 · 待采集'
  if (audit.theoretical_cost == null) return `理论 — · 实际 ${billingCost(item, audit.actual_cost)}`
  const prefix = audit.status === 'warning' ? '异常 · ' : ''
  // catalog_cost_exceeded 是价目轨道告警（本地估算 vs 上游报价），不是计费轨道
  if (audit.reason === 'catalog_cost_exceeded' && audit.list_cost != null && audit.reported_list_cost != null) {
    return `${prefix}本地 ${billingCost(item, audit.list_cost)} · 报价 ${billingCost(item, audit.reported_list_cost)}`
  }
  // 标签跟随实际基准：reported=平台自报原价，local=本地价目表（降级）
  const label = audit.billing_basis === 'local' ? '理论(本地)' : '理论'
  return `${prefix}${label} ${billingCost(item, audit.theoretical_cost)} · 实际 ${billingCost(item, audit.actual_cost)}`
}
function billingPricingText(item) {
  const audit = item.billing?.audit
  if (!audit) return ''
  const parts = []
  if (audit.pricing_source) parts.push(audit.pricing_source)
  if (audit.price_coverage != null) parts.push(`覆盖 ${(Number(audit.price_coverage) * 100).toFixed(0)}%`)
  // 有 reason 就显示：unavailable 是阻塞原因，ok 也可能带保留说明
  if (audit.reason) parts.push(billingAuditReasons[audit.reason] || audit.reason)
  return parts.join(' · ')
}
// 区间比对：默认 24h。单个采集间隔的结论会被下一轮覆盖，且样本量太小，
// 故明细面板一律展示聚合窗口，行内摘要保留即时值。
async function loadBillingRange(item, window) {
  const key = window || billingWindowFor[item.id] || '24h'
  billingWindowFor[item.id] = key
  billingRangeLoading.add(item.id)
  try {
    const payload = await api.upstreamBillingAudit(item.id, key)
    if (payload) {
      billingRangeAudit[item.id] = payload.audit || null
      if (payload.window) billingWindowFor[item.id] = payload.window
    }
  } finally {
    billingRangeLoading.delete(item.id)
  }
}
function toggleBillingDetail(item) {
  if (billingDetailOpen.has(item.id)) {
    billingDetailOpen.delete(item.id)
    return
  }
  billingDetailOpen.add(item.id)
  if (!billingRangeAudit[item.id]) guard(() => loadBillingRange(item))
}
const billingWindowOptions = [
  { key: '1h', label: '1 小时' },
  { key: '24h', label: '24 小时' },
  { key: '7d', label: '7 天' },
]
// 明细行：只保留需要留存/追溯的项，短暂状态留在行内摘要。
function billingDetailRows(item) {
  const audit = billingRangeAudit[item.id]
  if (!audit) return []
  const rows = []
  const push = (label, value) => { if (value) rows.push({ label, value }) }
  push('比对区间', audit.snapshot_count > 1
    ? `${sinceText(audit.from_at)} → ${sinceText(audit.to_at)}（${audit.snapshot_count} 次采集）`
    : '本窗口内采集次数不足')
  push('本地价目估算', audit.list_cost != null ? billingCost(item, audit.list_cost) : '')
  push('平台自报原价', audit.reported_list_cost != null ? billingCost(item, audit.reported_list_cost) : '')
  if (audit.catalog_deviation != null) {
    const rate = audit.catalog_deviation_rate != null
      ? `（${(Number(audit.catalog_deviation_rate) * 100).toFixed(1)}%）` : ''
    push('价目差异', `${billingCost(item, audit.catalog_deviation)}${rate}`)
  }
  push('理论扣费', audit.theoretical_cost != null ? billingCost(item, audit.theoretical_cost) : '')
  push('实际扣费', audit.actual_cost != null
    ? `${billingCost(item, audit.actual_cost)}${audit.actual_source === 'balance' ? '（按余额变化推算）' : ''}` : '')
  if (audit.deviation != null) {
    const rate = audit.deviation_rate != null ? `（${(Number(audit.deviation_rate) * 100).toFixed(1)}%）` : ''
    push('计费偏差', `${billingCost(item, audit.deviation)}${rate}`)
  }
  push('余额减少', audit.balance_spent != null ? billingCost(item, audit.balance_spent) : '')
  push('计费倍率', audit.expected_multiplier != null
    ? `${audit.expected_multiplier}${audit.multiplier_changed ? '（区间内有调整）' : ''}` : '')
  push('实测倍率', audit.observed_multiplier != null ? Number(audit.observed_multiplier).toFixed(4) : '')
  push('比对基准', billingBasisLabels[audit.billing_basis] || '')
  push('本地估算受限', billingLocalPricingReasons[audit.local_pricing_reason] || audit.local_pricing_reason || '')
  push('价格覆盖', audit.price_coverage != null
    ? `${audit.priced_request_count || 0}/${audit.request_count || 0}（${(Number(audit.price_coverage) * 100).toFixed(1)}%）` : '')
  push('缺少 Usage', audit.missing_usage_count ? `${audit.missing_usage_count} 条成功请求` : '')
  push('缺少价格', audit.missing_models?.length ? audit.missing_models.join('、') : '')
  push('价目来源', audit.pricing_source
    ? `${audit.pricing_source}${audit.pricing_version ? ` (${audit.pricing_version.slice(0, 12)})` : ''}` : '')
  return rows
}
function billingMeta(item) {
  return [billingTypeLabel(item.billing_type), cacheModeLabel(item.cache_mode), item.billing?.billing_group, billingStatusText(item)].filter(Boolean).join(' · ')
}
function billingTitle(item) {
  return [billingMeta(item), item.billing?.error, item.billing?.refreshed_at ? `更新于 ${sinceText(item.billing.refreshed_at)}` : '尚未采集'].filter(Boolean).join('\n')
}
function upstreamHref(value) {
  try {
    const url = new URL(String(value || '').trim())
    return url.protocol === 'http:' || url.protocol === 'https:' ? url.href : ''
  } catch {
    return ''
  }
}
const primaryTagOptions = computed(() => [
  { value: 0, label: '未分类' },
  ...tags.value.map(tag => ({ value: tag.id, label: tag.name })),
])
const tagFilterOptions = computed(() => [
  { value: '', label: '普通标签：全部' },
  ...tags.value.map(tag => ({ value: String(tag.id), label: tag.name })),
])
const batchTagOptions = computed(() => [
  { value: '', label: '选择标签' },
  ...primaryTagOptions.value,
])
const upstreamFormSelectedTags = computed(() => {
  const selected = new Set((dlg.form.tag_ids || []).map(Number))
  const primary = Number(dlg.form.primary_tag_id) || 0
  return tags.value.filter(tag => selected.has(tag.id) && tag.id !== primary)
})
const upstreamFormTagOptions = computed(() => {
  const primary = Number(dlg.form.primary_tag_id) || 0
  return tags.value.filter(tag => tag.id !== primary).map(tag => ({ value: tag.id, label: tag.name, color: tag.color }))
})
function newUpstream() {
  const draft = upstreamDraft.value
  upstreamDraft.value = null
  upstreamFormGroupIDs.value = draft?.groupIDs ? [...draft.groupIDs] : []
  upstreamFormGroupSearch.value = ''
  upstreamVendor.value = draft?.vendor || ''
  upstreamImportText.value = draft?.importText || ''
  upstreamImportMessage.value = draft?.importMessage || ''
  upstreamImportError.value = draft?.importError || false
  upstreamBaseURLDirty.value = draft?.baseURLDirty || false
  dlg.type = 'upstream'
  dlg.form = draft?.form
    ? { ...draft.form, tag_ids: [...(draft.form.tag_ids || [])] }
    : { name: '', base_url: '', api_key: '', proxy: '', protocol: 'passthrough', billing_type: 'none', cache_mode: 'auto', credit_ratio: 1, enabled: true, channel_probe: false, primary_tag_id: 0, tag_ids: [] }
}
function editUpstream(u) {
  upstreamDraft.value = null
  upstreamFormGroupIDs.value = []
  upstreamFormGroupSearch.value = ''
  upstreamVendor.value = ''
  upstreamImportText.value = ''
  upstreamImportMessage.value = ''
  upstreamImportError.value = false
  upstreamBaseURLDirty.value = true
  dlg.type = 'upstream'
  dlg.form = { ...u, protocol: u.protocol || 'passthrough', billing_type: u.billing_type || 'none', cache_mode: u.cache_mode || 'auto', credit_ratio: u.credit_ratio || 1, api_key: '', primary_tag_id: u.primary_tag_id || 0, tag_ids: [...(u.tag_ids || [])] }
}
function saveUpstream() {
  guardDialogSave(async () => {
    const primaryTagID = Number(dlg.form.primary_tag_id) || 0
    const tagIDs = [...new Set((dlg.form.tag_ids || []).map(Number).filter(id => id && id !== primaryTagID))]
    const f = { ...dlg.form, primary_tag_id: primaryTagID, tag_ids: tagIDs, credit_ratio: Number(dlg.form.credit_ratio) || 1 }
    if (f.id) await api.updateUpstream(f.id, f)
    else {
      if (upstreamFormGroupIDs.value.length) f.group_ids = upstreamFormGroupIDs.value.map(Number).filter(Boolean)
      await api.createUpstream(f)
    }
    closeDlg(); await loadUpstreams()
  })
}
function delUpstream(u) {
  ask(`删除上游「${u.name}」？将同时从所有分组移除。`, () =>
    guard(async () => { await api.deleteUpstream(u.id); await loadUpstreams() }))
}

async function recoverUpstream(item) {
  const id = Number(item.id || item.upstream_id)
  if (!id || recoveringUpstreams.has(id)) return
  recoveringUpstreams.add(id)
  try {
    await api.recoverUpstream(id)
    await loadUpstreams()
    if (detailGroup.value) await loadMembers(detailGroup.value.id)
    flash(`已恢复「${item.name || item.upstream_name || ('#' + id)}」`)
  } finally {
    recoveringUpstreams.delete(id)
  }
}

async function recoverUpstreamModel(item, modelHealth) {
  const id = Number(item.id || item.upstream_id)
  const model = String(modelHealth?.model || '').trim()
  const key = `${id}:${model}`
  if (!id || !model || recoveringModels.has(key)) return
  recoveringModels.add(key)
  try {
    await api.recoverUpstreamModel(id, model)
    item.model_health = (item.model_health || []).filter(entry => entry.model !== model)
    flash(`已恢复「${model}」`)
  } finally {
    recoveringModels.delete(key)
  }
}

async function refreshUpstreamBilling(item) {
  if (!item.id || item.billing_type === 'none' || refreshingBilling.has(item.id)) return
  refreshingBilling.add(item.id)
  try {
    const state = await api.refreshUpstreamBilling(item.id)
    item.billing = state
    if (state.status === 'error') throw new Error(state.error || '计费数据采集失败')
    flash(`已刷新「${item.name}」的计费数据`)
  } finally {
    refreshingBilling.delete(item.id)
  }
}

// setBillingMultiplierPrompt 手动录入倍率，等价于一次人工探测。
// 下次 auto-refresh(约10分钟)若从扣费日志取到 group_ratio 会覆盖此值。
async function setBillingMultiplierPrompt(item) {
  if (!item.id || item.billing_type === 'none') return
  const current = Number(item.billing?.effective_multiplier ?? item.billing?.group_multiplier ?? 1)
  const input = window.prompt(
    `手动录入「${item.name}」倍率(等同一次探测结果，下次自动刷新可能覆盖):`,
    String(current)
  )
  if (input == null) return
  const value = Number(String(input).trim())
  if (!Number.isFinite(value) || value <= 0) {
    flash('倍率必须是大于 0 的数字', 'error')
    return
  }
  const state = await api.setUpstreamBillingMultiplier(item.id, value)
  item.billing = state
  flash(`已录入「${item.name}」倍率 ${value}`)
}

const upstreamRunFilter = ref('')
const upstreamEnabledFilter = ref('')
const upstreamTagFilters = reactive(new Set())   // 多选标签筛选（替换旧的两个下拉）
const upstreamProtocolFilter = ref('')
const upstreamSearch = ref('')
const upstreamPageSize = ref(20)
const upstreamCurrentPage = ref(1)
const upstreamSelected = reactive(new Set())
const upstreamBatchTagID = ref('')
const collapsedUpstreamTags = reactive(new Set())
const upstreamDragId = ref(null)
const upstreamDragOverId = ref(null)
const upstreamRunOptions = [
  { value: '', label: '运行状态：全部' },
  { value: 'CLOSED', label: '正常' },
  { value: 'HALF_OPEN', label: '半开' },
  { value: 'OPEN', label: '熔断' },
  { value: 'UNPROBED', label: '待探测' },
]
const upstreamEnabledOptions = [
  { value: '', label: '启停状态：全部' },
  { value: 'enabled', label: '启用' },
  { value: 'disabled', label: '停用' },
]
const upstreamPageSizeOptions = [
  { value: 20, label: '20 条' },
  { value: 50, label: '50 条' },
  { value: 100, label: '100 条' },
]
const upstreamPrimaryTagOptions = computed(() => [
  { value: '', label: '主标签：全部' },
  { value: 'untagged', label: '未分类' },
  ...tags.value.map(tag => ({ value: `tag-${tag.id}`, label: tag.name })),
])
const upstreamProtocolOptions = [
  { value: '', label: '协议：全部' },
  ...protocolOptions,
]

const upstreamRunValue = u => rtUnprobed(u.health) ? 'UNPROBED' : (u.health?.state || '')

// 不含标签筛选的基础过滤集合，用于算 chip bar 里各标签的命中数
const upstreamsBaseFiltered = computed(() => {
  return upstreams.value.filter(u => {
    const query = upstreamSearch.value.trim().toLowerCase()
    if (upstreamEnabledFilter.value === 'enabled' && !u.enabled) return false
    if (upstreamEnabledFilter.value === 'disabled' && u.enabled) return false
    if (upstreamRunFilter.value && upstreamRunValue(u) !== upstreamRunFilter.value) return false
    if (upstreamProtocolFilter.value && (u.protocol || 'passthrough') !== upstreamProtocolFilter.value) return false
    if (query && ![u.name, u.base_url, u.protocol, ...(u.tags || []).map(tag => tag.name)].some(v => String(v || '').toLowerCase().includes(query))) return false
    return true
  })
})

// chip bar：每个标签在当前基础过滤下的命中数
const tagChipItems = computed(() => {
  return tags.value.map(tag => ({
    ...tag,
    count: upstreamsBaseFiltered.value.filter(u => (u.tags || []).some(t => t.id === tag.id)).length,
  }))
})

const upstreamsFiltered = computed(() => {
  const tagOrder = new Map(tags.value.map((tag, index) => [tag.id, index]))
  const filtered = upstreamsBaseFiltered.value.filter(u => {
    if (upstreamTagFilters.size > 0 && !(u.tags || []).some(t => upstreamTagFilters.has(t.id))) return false
    return true
  })
  return filtered.sort((a, b) => {
    const aTag = primaryTagFor(a), bTag = primaryTagFor(b)
    return (tagOrder.get(aTag?.id) ?? Number.MAX_SAFE_INTEGER) - (tagOrder.get(bTag?.id) ?? Number.MAX_SAFE_INTEGER)
  })
})
const upstreamTotalPages = computed(() => Math.max(1, Math.ceil(upstreamsFiltered.value.length / Number(upstreamPageSize.value))))
const upstreamPageRows = computed(() => {
  const page = Math.min(upstreamCurrentPage.value, upstreamTotalPages.value)
  const offset = (page - 1) * Number(upstreamPageSize.value)
  return upstreamsFiltered.value.slice(offset, offset + Number(upstreamPageSize.value))
})
const upstreamPageSections = computed(() => {
  const sections = new Map()
  for (const item of upstreamPageRows.value) {
    const tag = primaryTagFor(item)
    const key = tagGroupKey(tag)
    if (!sections.has(key)) sections.set(key, { key, tag, name: tagGroupName(tag), rows: [] })
    sections.get(key).rows.push(item)
  }
  return [...sections.values()]
})
function buildPageItems(total, current) {
  if (total <= 7) return Array.from({ length: total }, (_, index) => ({ type: 'page', value: index + 1, key: `page-${index + 1}` }))
  let start = Math.max(2, current - 1)
  let end = Math.min(total - 1, current + 1)
  if (current <= 4) end = 5
  if (current >= total - 3) start = total - 4
  const items = [{ type: 'page', value: 1, key: 'page-1' }]
  if (start > 2) items.push({ type: 'ellipsis', key: 'ellipsis-left' })
  for (let value = start; value <= end; value++) items.push({ type: 'page', value, key: `page-${value}` })
  if (end < total - 1) items.push({ type: 'ellipsis', key: 'ellipsis-right' })
  items.push({ type: 'page', value: total, key: `page-${total}` })
  return items
}
const tagColorChoices = [
  { value: 'gray', label: '灰' }, { value: 'green', label: '绿' },
  { value: 'teal', label: '青绿' }, { value: 'cyan', label: '青' },
  { value: 'blue', label: '蓝' }, { value: 'indigo', label: '靛蓝' },
  { value: 'purple', label: '紫' }, { value: 'pink', label: '粉' },
  { value: 'red', label: '红' }, { value: 'orange', label: '橙' },
  { value: 'amber', label: '黄' }, { value: 'lime', label: '青柠' },
  { value: 'rose', label: '玫红' }, { value: 'emerald', label: '翠绿' },
  { value: 'sky', label: '天蓝' }, { value: 'violet', label: '深紫' },
  { value: 'fuchsia', label: '品红' }, { value: 'yellow', label: '金黄' },
]
const tagDraft = reactive({ id: 0, name: '', color: 'gray' })

// 每个标签被多少个上游使用（含主标签），供标签管理和 chip bar 显示
const tagUsageCount = computed(() => {
  const m = new Map()
  for (const u of upstreams.value) {
    for (const t of (u.tags || [])) {
      m.set(t.id, (m.get(t.id) || 0) + 1)
    }
  }
  return m
})

const filteredManagedTags = computed(() => {
  const query = tagManagerSearch.value.trim().toLowerCase()
  const list = query ? tags.value.filter(tag => tag.name.toLowerCase().includes(query)) : tags.value
  return list.map(tag => ({ ...tag, count: tagUsageCount.value.get(tag.id) || 0 }))
})
function resetTagDraft() { Object.assign(tagDraft, { id: 0, name: '', color: 'gray' }) }
function openTagManager() { tagManagerSearch.value = ''; dlg.type = 'tags'; dlg.form = {}; resetTagDraft() }
function editTagDraft(tag) { Object.assign(tagDraft, { id: tag.id, name: tag.name, color: tag.color }) }
function saveTag() {
  guardDialogSave(async () => {
    const payload = { name: tagDraft.name.trim(), color: tagDraft.color }
    if (!payload.name) return
    if (tagDraft.id) await api.updateTag(tagDraft.id, payload)
    else await api.createTag(payload)
    await Promise.all([loadTags(), loadUpstreams()])
    resetTagDraft()
  })
}

const groupDragId = ref(null)
const groupDragOverId = ref(null)
function onGroupDragStart(g, e) {
  groupDragId.value = g.id
  e.dataTransfer.effectAllowed = 'move'
  e.dataTransfer.setData('text/plain', String(g.id))
}
function onGroupDragOver(g, e) {
  if (g.id === groupDragId.value) return
  e.preventDefault()
  groupDragOverId.value = g.id
}
function onGroupDrop(target) {
  const from = groups.value.findIndex(g => g.id === groupDragId.value)
  const to = groups.value.findIndex(g => g.id === target.id)
  if (from < 0 || to < 0 || from === to) { onGroupDragEnd(); return }
  const arr = groups.value.slice()
  const [moved] = arr.splice(from, 1)
  arr.splice(to, 0, moved)
  groups.value = arr
  onGroupDragEnd()
  guard(() => api.reorderGroups(arr.map(g => g.id)).catch(e => { loadGroups().catch(() => {}); throw e }))
}
function onGroupDragEnd() { groupDragId.value = groupDragOverId.value = null }
function delTag(tag) {
  closeDlg()
  ask(`删除标签「${tag.name}」？上游不会被删除。`, () => guard(async () => {
    await api.deleteTag(tag.id)
    await Promise.all([loadTags(), loadUpstreams()])
    resetTagDraft()
  }))
}
const upstreamPageItems = computed(() => buildPageItems(upstreamTotalPages.value, Math.min(upstreamCurrentPage.value, upstreamTotalPages.value)))
const upstreamSelectedCount = computed(() => upstreamSelected.size)
function onUpstreamFilterChange() { upstreamCurrentPage.value = 1 }
function toggleTagFilter(tagId) {
  if (upstreamTagFilters.has(tagId)) upstreamTagFilters.delete(tagId)
  else upstreamTagFilters.add(tagId)
  onUpstreamFilterChange()
}
function clearTagFilters() { upstreamTagFilters.clear(); onUpstreamFilterChange() }

// 行内标签 picker
const inlineTagPickerUpstreamId = ref(null)
const inlineTagPickerPos = ref({ top: 0, left: 0 })
function openInlineTagPicker(id, event) {
  if (inlineTagPickerUpstreamId.value === id) { inlineTagPickerUpstreamId.value = null; return }
  const rect = event.currentTarget.getBoundingClientRect()
  inlineTagPickerPos.value = { top: rect.bottom + 6, left: rect.left }
  inlineTagPickerUpstreamId.value = id
}
async function toggleInlineTag(u, tagId) {
  const primaryId = Number(u.primary_tag_id) || 0
  if (primaryId === tagId) return               // 主标签不可从 picker 里摘除
  const current = new Set((u.tag_ids || []).map(Number))
  if (current.has(tagId)) current.delete(tagId); else current.add(tagId)
  const tagIDs = [...current].filter(id => id && id !== primaryId)
  await guard(async () => {
    await api.updateUpstream(u.id, { ...u, api_key: '', primary_tag_id: primaryId, tag_ids: tagIDs })
    await loadUpstreams()
  })
}
function closeInlineTagPicker() { inlineTagPickerUpstreamId.value = null }
onMounted(() => document.addEventListener('click', closeInlineTagPicker))
onUnmounted(() => document.removeEventListener('click', closeInlineTagPicker))
function goUpstreamPage(page) { upstreamCurrentPage.value = Math.max(1, Math.min(Number(page) || 1, upstreamTotalPages.value)) }
function toggleUpstreamSelection(id) {
  if (upstreamSelected.has(id)) upstreamSelected.delete(id)
  else upstreamSelected.add(id)
}
function toggleUpstreamSectionSelection(rows) {
  const selected = rows.length > 0 && rows.every(u => upstreamSelected.has(u.id))
  rows.forEach(u => selected ? upstreamSelected.delete(u.id) : upstreamSelected.add(u.id))
}
async function batchUpdateUpstreams(payload, message) {
  const ids = [...upstreamSelected]
  if (!ids.length) return
  await api.batchUpdateUpstreams({ ids, ...payload })
  upstreamSelected.clear()
  await loadUpstreams()
  flash(message)
}
async function applyBatchTag(mode) {
  const tagID = Number(upstreamBatchTagID.value) || 0
  const count = upstreamSelectedCount.value
  if (mode !== 'primary' && !tagID) return
  const payload = mode === 'primary' ? { primary_tag_id: tagID }
    : mode === 'add' ? { add_tag_ids: [tagID] } : { remove_tag_ids: [tagID] }
  const verb = mode === 'primary' ? '设置主标签' : mode === 'add' ? '添加标签' : '移除标签'
  await batchUpdateUpstreams(payload, `已为 ${count} 个渠道${verb}`)
  upstreamBatchTagID.value = ''
}
function toggleUpstreamTagSection(key) {
  if (collapsedUpstreamTags.has(key)) collapsedUpstreamTags.delete(key)
  else collapsedUpstreamTags.add(key)
}

function onUpstreamDragStart(u, e) {
  upstreamDragId.value = u.id
  e.dataTransfer.effectAllowed = 'move'
}
function onUpstreamDragOver(u, e) {
  e.preventDefault()
  if (u.id !== upstreamDragId.value) upstreamDragOverId.value = u.id
}
function onUpstreamDrop(target) {
  const groupKey = tagGroupKey(primaryTagFor(target))
  const visible = upstreamsFiltered.value.filter(item => tagGroupKey(primaryTagFor(item)) === groupKey)
  const from = visible.findIndex(x => x.id === upstreamDragId.value)
  const to = visible.findIndex(x => x.id === target.id)
  if (from < 0 || to < 0 || from === to) { onUpstreamDragEnd(); return }
  const reorderedVisible = visible.slice()
  const [moved] = reorderedVisible.splice(from, 1)
  reorderedVisible.splice(to, 0, moved)
  const visibleIds = new Set(visible.map(x => x.id))
  let vi = 0
  const arr = upstreams.value.map(u => visibleIds.has(u.id) ? reorderedVisible[vi++] : u)
  upstreams.value = arr
  onUpstreamDragEnd()
  guard(() => api.reorderUpstreams(arr.map(x => x.id)).catch(e => { loadUpstreams().catch(() => {}); throw e }))
}
function onUpstreamDragEnd() { upstreamDragId.value = upstreamDragOverId.value = null }

// 连通测试 + 模型列表
const testState = reactive({
  show: false, id: 0, name: '',
  modelsLoading: false, models: [], model: '', modelsErr: '',
  running: false, output: '', status: null, // status: {ok,latency_ms,code,error}
})
let testAbortController = null
let testModelsAbortController = null
function abortUpstreamTestRequests() {
  testAbortController?.abort()
  testAbortController = null
  testModelsAbortController?.abort()
  testModelsAbortController = null
}
function closeTest() {
  abortUpstreamTestRequests()
  testState.show = false
  testState.modelsLoading = false
  testState.running = false
}
// 打开测试弹窗：先拉模型列表供选择
function testUpstream(u) {
  abortUpstreamTestRequests()
  const modelsController = new AbortController()
  testModelsAbortController = modelsController
  Object.assign(testState, {
    show: true, id: u.id, name: u.name,
    modelsLoading: true, models: [], model: '', modelsErr: '',
    running: false, output: '', status: null,
  })
  api.testUpstream(u.id, modelsController.signal)
    .then(r => {
      if (modelsController.signal.aborted || testModelsAbortController !== modelsController) return
      testState.models = r.models || []
      testState.model = testState.models[0] || 'gpt-5.5'
      if (!r.ok && r.error) testState.modelsErr = r.error
    })
    .catch(e => {
      if (modelsController.signal.aborted || testModelsAbortController !== modelsController) return
      testState.modelsErr = String(e.message || e); testState.model = 'gpt-5.5'
    })
    .finally(() => {
      if (testModelsAbortController === modelsController) {
        testModelsAbortController = null
        testState.modelsLoading = false
      }
    })
}
// 真实对话测试：流式回显上游回复
async function runTest() {
  if (!testState.model || testState.running) return
  testAbortController?.abort()
  const controller = new AbortController()
  testAbortController = controller
  testState.running = true; testState.output = ''; testState.status = null
  try {
    await api.testUpstreamStream(testState.id, testState.model, e => {
      if (controller.signal.aborted || testAbortController !== controller) return
      if (e.type === 'content') testState.output += e.text
      else if (e.type === 'test_complete') testState.status = { ok: true, latency_ms: e.latency_ms }
      else if (e.type === 'error') testState.status = { ok: false, code: e.status, error: e.error, latency_ms: e.latency_ms }
    }, controller.signal)
  } catch (e) {
    if (controller.signal.aborted || testAbortController !== controller) return
    testState.status = { ok: false, error: String(e.message || e) }
  } finally {
    if (testAbortController !== controller) return
    testAbortController = null
    testState.running = false
    // 流正常结束但没收到 test_complete：
    //  - 有 content → 视为「部分成功」（连上了且有回复，只是缺收尾事件）
    //  - 完全无数据 → 不能判成功，标记失败避免假阳性
    if (!testState.status) {
      testState.status = testState.output
        ? { ok: true, partial: true }
        : { ok: false, error: '上游无任何响应（未收到内容或完成事件）' }
    }
  }
}

function delGroup(g) {
  ask(`删除分组「${g.name}」？其成员关联与密钥都会被清除。`, () =>
    guard(async () => { await api.deleteGroup(g.id); await loadGroups() }))
}

const groupTestState = reactive({
  show: false, groupName: '', keyId: 0, keyName: '', protocol: 'responses',
  modelsLoading: false, models: [], model: '', modelsError: '',
  running: false, output: '', error: '', status: 0, requestId: '', result: null,
})
let groupTestAbortController = null
let groupModelsAbortController = null
function abortGroupTestRequests() {
  groupTestAbortController?.abort()
  groupTestAbortController = null
  groupModelsAbortController?.abort()
  groupModelsAbortController = null
}
function closeGroupTest() {
  abortGroupTestRequests()
  groupTestState.show = false
  groupTestState.modelsLoading = false
  groupTestState.running = false
}
const groupTestProtocolOptions = [
  { value: 'responses', label: 'OpenAI Responses' },
  { value: 'chat', label: 'Chat Completions' },
  { value: 'claude', label: 'Anthropic Messages' },
  { value: 'gemini', label: 'Gemini GenerateContent' },
]
const groupTestModelOptions = computed(() => {
  if (groupTestState.modelsLoading) return [{ value: '', label: '加载模型中…', disabled: true }]
  const options = groupTestState.models.map(model => ({ value: model, label: model }))
  if (!options.length && groupTestState.model) options.push({ value: groupTestState.model, label: groupTestState.model })
  return options
})
function selectedGroupTestKey() { return keys.value.find(key => key.enabled && key.id === Number(groupTestState.keyId)) }
async function loadGroupTestModels(controller = new AbortController()) {
  const key = selectedGroupTestKey()
  groupModelsAbortController?.abort()
  groupModelsAbortController = controller
  groupTestState.modelsLoading = true
  groupTestState.models = []
  groupTestState.modelsError = ''
  try {
    groupTestState.models = key ? await api.groupModels(key.key, controller.signal) : []
    if (controller.signal.aborted || groupModelsAbortController !== controller) return
    groupTestState.model = groupTestState.models[0] || groupTestState.model || 'gpt-5.5'
  } catch (e) {
    if (controller.signal.aborted || groupModelsAbortController !== controller) return
    groupTestState.modelsError = String(e.message || e)
    groupTestState.model ||= 'gpt-5.5'
  } finally {
    if (groupModelsAbortController === controller) {
      groupModelsAbortController = null
      groupTestState.modelsLoading = false
    }
  }
}
function openGroupTest(key) {
  if (!key?.enabled) return
  abortGroupTestRequests()
  const modelsController = new AbortController()
  Object.assign(groupTestState, {
    show: true, groupName: detailGroup.value?.name || '', keyId: key.id, keyName: key.name || key.masked || ('#' + key.id), protocol: 'responses',
    modelsLoading: false, models: [], model: '', modelsError: '',
    running: false, output: '', error: '', status: 0, requestId: '', result: null,
  })
  loadGroupTestModels(modelsController)
}
async function waitForGroupTestLog(requestId, signal) {
  if (!requestId) return null
  const deadline = Date.now() + 10000
  for (let attempt = 0; attempt < 8 && Date.now() < deadline; attempt++) {
    if (signal?.aborted) return null
    try {
      const page = await api.logs({ q: requestId, limit: 1 }, signal, 2000)
      const entry = (page?.entries || []).find(item => item.request_id === requestId)
      if (entry) return await api.logDetail(entry.id, signal, 2000)
    } catch (e) {
      if (signal?.aborted) return null
    }
    await new Promise(resolve => setTimeout(resolve, 200))
  }
  return null
}
async function runGroupTest() {
  const key = selectedGroupTestKey()
  if (!key || !groupTestState.model || groupTestState.running) return
  groupTestAbortController?.abort()
  const controller = new AbortController()
  groupTestAbortController = controller
  Object.assign(groupTestState, { running: true, output: '', error: '', status: 0, requestId: '', result: null })
  try {
    const response = await api.testGroupStream({ key: key.key, protocol: groupTestState.protocol, model: groupTestState.model }, text => {
      if (!controller.signal.aborted && groupTestAbortController === controller) groupTestState.output += text
    }, controller.signal)
    if (controller.signal.aborted || groupTestAbortController !== controller) return
    groupTestState.status = response.status
    groupTestState.requestId = response.requestId
    groupTestState.running = false
    const result = await waitForGroupTestLog(response.requestId, controller.signal)
    if (controller.signal.aborted || groupTestAbortController !== controller) return
    groupTestState.result = result
  } catch (e) {
    if (controller.signal.aborted || groupTestAbortController !== controller) return
    groupTestState.error = String(e.message || e)
    groupTestState.status = Number(e.status) || 0
    groupTestState.requestId = e.requestId || ''
    groupTestState.running = false
    if (groupTestState.requestId) {
      const result = await waitForGroupTestLog(groupTestState.requestId, controller.signal).catch(() => null)
      if (controller.signal.aborted || groupTestAbortController !== controller) return
      groupTestState.result = result
    }
  } finally {
    if (groupTestAbortController === controller) {
      groupTestAbortController = null
      groupTestState.running = false
    }
  }
}

// 组成员
function addMember() { dlg.type = 'member'; dlg.form = { upstream_id: '', priority: 50, weight: 1 } }
function saveMember() {
  guardDialogSave(async () => {
    await api.addMember(detailGroup.value.id, { ...dlg.form, upstream_id: Number(dlg.form.upstream_id) })
    closeDlg(); await loadDetail(detailGroup.value.id)
  })
}
function editMember(m) { dlg.type = 'member'; dlg.form = { upstream_id: m.upstream_id, priority: m.priority, weight: m.weight, locked: true } }
function removeMember(m) {
  guard(async () => { await api.removeMember(detailGroup.value.id, m.upstream_id); await loadDetail(detailGroup.value.id) })
}
function toggleMember(m) {
  guard(async () => { await api.setMemberEnabled(detailGroup.value.id, m.upstream_id, !m.group_enabled); await loadDetail(detailGroup.value.id) })
}

// 密钥
const newKey = ref('')   // 生成后明文展示一次
const copied = ref(0)    // 刚点击复制的密钥 id（短暂提示用）
const logRetention = ref('')           // 请求记录保留天数
const effectiveLogRetention = ref('')
const logRetentionSource = ref('')
const alertWebhook = ref('')           // 告警 Webhook URL(空=关闭)
const alertDebounce = ref('')          // 告警去抖窗口
const effectiveAlertWebhook = ref('')
const effectiveAlertDebounce = ref('')
const alertWebhookSource = ref('')
const alertDebounceSource = ref('')
const firstResponseTimeoutSec = ref('')    // 首字节超时(秒，后端存毫秒)
const effectiveFirstResponseTimeoutMs = ref('')
const firstResponseTimeoutSource = ref('')
const failThreshold = ref('')
const cooldown = ref('')
const maxUpstreamAttempts = ref('')
const maxBodyMB = ref('')
const effectiveFailThreshold = ref('')
const effectiveCooldown = ref('')
const effectiveMaxUpstreamAttempts = ref('')
const effectiveMaxBodyBytes = ref('')
const failThresholdSource = ref('')
const cooldownSource = ref('')
const maxUpstreamAttemptsSource = ref('')
const maxBodyBytesSource = ref('')
const apiBase = location.origin    // 当前访问地址，用于展示客户端接入端点
const settingsSaved = ref(false)
const settingsSection = ref('logs')  // 设置页左锚点：logs | alert | endpoint | backup

// 备份配置
const backupConfig = ref({ endpoint: '', region: '', bucket: '', access_key_id: '', secret_key: '', prefix: '', force_path_style: false })
const backupSchedule = ref({ enabled: false, cron_expr: '0 3 * * *', retain_days: 14, retain_count: 30 })
const backupRecords = ref([])
const backupTesting = ref(false)
const backupTriggering = ref(false)
const backupLoading = ref(false)
const backupTestResult = ref(null) // null | 'ok' | 'err'
const backupTestMsg = ref('')
let _backupPollTimer = null
function startBackupPoll() {
  if (_backupPollTimer) return
  _backupPollTimer = setInterval(async () => {
    const r = await api.listBackups().catch(() => null)
    if (!r) return
    backupRecords.value = r.items || []
    if (!backupRecords.value.some(b => b.status === 'running' || b.status === 'pending')) {
      clearInterval(_backupPollTimer); _backupPollTimer = null
    }
  }, 4000)
}
function stopBackupPoll() { clearInterval(_backupPollTimer); _backupPollTimer = null }

// 模型映射
const mappings = ref([])
const mappingsLoading = ref(false)
const showNewMapping = ref(false)
const newMappingForm = reactive({ upstream_id: '', source_model: '', target_model: '' })
async function loadMappings() {
  mappingsLoading.value = true
  try { mappings.value = (await api.modelMappings()) || [] }
  finally { mappingsLoading.value = false }
}
async function saveNewMapping() {
  if (!newMappingForm.source_model.trim() || !newMappingForm.target_model.trim()) return
  await api.createModelMapping({
    upstream_id: Number(newMappingForm.upstream_id) || 0,
    source_model: newMappingForm.source_model.trim(),
    target_model: newMappingForm.target_model.trim(),
    mapping_type: 'static',
  })
  showNewMapping.value = false
  Object.assign(newMappingForm, { upstream_id: '', source_model: '', target_model: '' })
  await loadMappings()
}

async function loadBackupConfig() {
  backupConfig.value = (await api.backupConfig()) || { endpoint: '', region: '', bucket: '', access_key_id: '', secret_key: '', prefix: '', force_path_style: false }
}
async function saveBackupConfig() {
  await api.saveBackupConfig(backupConfig.value)
  await loadBackupConfig()
  settingsSaved.value = true
  setTimeout(() => { settingsSaved.value = false }, 1500)
}
async function testS3() {
  backupTesting.value = true
  backupTestResult.value = null
  try {
    const r = await api.testBackupConfig(backupConfig.value)
    backupTestResult.value = r.ok ? 'ok' : 'err'
    if (!r.ok) backupTestMsg.value = r.message
  } finally { backupTesting.value = false }
}
async function loadBackupSchedule() {
  backupSchedule.value = (await api.backupSchedule()) || { enabled: false, cron_expr: '0 3 * * *', retain_days: 14, retain_count: 30 }
}
async function saveBackupSchedule() {
  await api.saveBackupSchedule(backupSchedule.value)
  await loadBackupSchedule()
  settingsSaved.value = true
  setTimeout(() => { settingsSaved.value = false }, 1500)
}
async function triggerBackup() {
  backupTriggering.value = true
  try {
    await api.triggerBackup()
    await loadBackups()
    startBackupPoll()
  } finally { backupTriggering.value = false }
}
async function loadBackups() {
  backupLoading.value = true
  try {
    const r = await api.listBackups()
    backupRecords.value = r.items || []
    if (backupRecords.value.some(b => b.status === 'running' || b.status === 'pending')) startBackupPoll()
  } finally { backupLoading.value = false }
}
function deleteBackup(id) {
  ask('删除此备份？', () => guard(async () => { await api.deleteBackup(id); await loadBackups() }))
}
async function downloadBackup(id) {
  const r = await api.backupDownloadURL(id)
  window.open(r.url, '_blank')
}
const backupStatusClass = s => ({ pending: 'tag warn', running: 'tag on', completed: 'tag ok', failed: 'tag err' }[s] || 'tag')
const backupStatusText = s => ({ pending: '待处理', running: '进行中', completed: '已完成', failed: '失败' }[s] || s)
const fmtFileSize = b => b < 1024 ? b + 'B' : b < 1024 * 1024 ? (b / 1024).toFixed(1) + 'KB' : (b / 1024 / 1024).toFixed(1) + 'MB'
function createKey() { dlg.type = 'keygen'; dlg.form = { name: '' } }
function saveKey() {
  guardDialogSave(async () => {
    const r = await api.createKey(detailGroup.value.id, dlg.form.name || '')
    closeDlg()
    newKey.value = r.key
    await loadDetail(detailGroup.value.id)
  })
}
function toggleKey(k) {
  guard(async () => { await api.setKeyEnabled(k.id, !k.enabled); await loadDetail(detailGroup.value.id) })
}
function delKey(k) {
  ask('吊销该密钥？使用它的客户端将立即失效。', () =>
    guard(async () => { await api.deleteKey(k.id); await loadDetail(detailGroup.value.id) }))
}
function copyKey() { navigator.clipboard?.writeText(newKey.value); newKey.value = '' }
function copyText(t, id) {
  navigator.clipboard?.writeText(t)
  copied.value = id
  setTimeout(() => { if (copied.value === id) copied.value = 0 }, 1200)
}
async function loadSettings() {
  const s = await api.getSettings()
  effectiveLogRetention.value = s.effective_log_retention || ''
  effectiveAlertWebhook.value = s.effective_alert_webhook || ''
  effectiveAlertDebounce.value = s.effective_alert_debounce || ''
  logRetention.value = s.log_retention || effectiveLogRetention.value
  alertWebhook.value = s.alert_webhook || ''
  alertDebounce.value = s.alert_debounce || effectiveAlertDebounce.value
  logRetentionSource.value = s.log_retention_source || ''
  alertWebhookSource.value = s.alert_webhook_source || ''
  alertDebounceSource.value = s.alert_debounce_source || ''
  effectiveFirstResponseTimeoutMs.value = s.effective_first_response_timeout_ms || ''
  // 后端存毫秒，UI 展示秒：优先取页面设置值，否则用 effective
  firstResponseTimeoutSec.value = String(Math.round((Number(s.first_response_timeout_ms || s.effective_first_response_timeout_ms) || 120000) / 1000))
  firstResponseTimeoutSource.value = s.first_response_timeout_ms_source || ''
  effectiveFailThreshold.value = s.effective_fail_threshold || ''
  effectiveCooldown.value = s.effective_cooldown || ''
  effectiveMaxUpstreamAttempts.value = s.effective_max_upstream_attempts || ''
  effectiveMaxBodyBytes.value = s.effective_max_body_bytes || ''
  failThreshold.value = s.fail_threshold || effectiveFailThreshold.value
  cooldown.value = s.cooldown || effectiveCooldown.value
  maxUpstreamAttempts.value = s.max_upstream_attempts || effectiveMaxUpstreamAttempts.value
  maxBodyMB.value = String(Math.round((Number(s.max_body_bytes || s.effective_max_body_bytes) || 33554432) / 1048576))
  failThresholdSource.value = s.fail_threshold_source || ''
  cooldownSource.value = s.cooldown_source || ''
  maxUpstreamAttemptsSource.value = s.max_upstream_attempts_source || ''
  maxBodyBytesSource.value = s.max_body_bytes_source || ''
}
const sourceText = s => s === 'settings' ? '数据库设置' : '默认值'
// 设置页左锚点点击：滚动到对应 section 并高亮
function gotoSection(id) {
  settingsSection.value = id
}
function saveSettings() {
  guard(async () => {
    await api.saveSettings({
      log_retention: String(logRetention.value),
      alert_webhook: alertWebhook.value,
      alert_debounce: alertDebounce.value,
      first_response_timeout_ms: String((Number(firstResponseTimeoutSec.value) || 120) * 1000),
      fail_threshold: String(failThreshold.value),
      cooldown: cooldown.value,
      max_upstream_attempts: String(maxUpstreamAttempts.value),
      max_body_bytes: String(Math.round((Number(maxBodyMB.value) || 32) * 1048576)),
    })
    await loadSettings()
    settingsSaved.value = true
    setTimeout(() => { settingsSaved.value = false }, 1500)
  })
}

// 监控项状态映射
const stateLabel = s => ({ OK: '正常', DEGRADED: '降级', DOWN: '故障' }[s] || '无数据')
const channelStateLabel = s => ({ OK: '可用', DEGRADED: '波动', DOWN: '不可用', NODATA: '待检测', DISABLED: '已停用' }[s] || '待检测')
const dotClass = s => ({ OK: 'closed', DEGRADED: 'half', DOWN: 'open' }[s] || 'nodata')

// 路由熔断状态映射（成员表/上游池运行时列用）。未探测过(last_probe=0 且无请求)显示「待探测」。
const rtUnprobed = h => h && h.state === 'CLOSED' && !h.last_probe && !h.reqs
const rtLabel = h => rtUnprobed(h) ? '待探测' : ({ CLOSED: '正常', HALF_OPEN: '半开', OPEN: '熔断' }[h?.state] || '待探测')
const rtClass = h => rtUnprobed(h) ? 'nodata' : ({ CLOSED: 'closed', HALF_OPEN: 'half', OPEN: 'open' }[h?.state] || 'nodata')
const rtRate = h => (h && h.reqs) ? (h.succ_rate * 100).toFixed(0) + '%' : '—'

// 模型徽章只表示短期能力排除，不再表示独立熔断状态。
const mhClass = mh => mh?.state === 'UNSUPPORTED' ? 'open' : 'nodata'
const visibleDots = item => item?.model_health || []

// 分组卡片：只展示路由摘要；完整成员名称在分组详情页查看。
const effText = rt => (rt && rt.effective && rt.effective.length) ? rt.effective.join(' / ') : '无可用'
const groupRouteState = rt => {
  if (!rt || !rt.total) return { key: 'empty', label: '无可用渠道', detail: '没有启用成员' }
  if (!rt.effective?.length) return { key: 'down', label: '无可用渠道', detail: '当前没有可路由成员' }
  const issues = []
  if (rt.open) issues.push(`${rt.open} 个熔断`)
  if (rt.half_open) issues.push(`${rt.half_open} 个半开`)
  if (rt.multiplier_blocked) issues.push(`${rt.multiplier_blocked} 个倍率拦截`)
  if (issues.length) return { key: 'partial', label: '部分可用', detail: `生效 ${rt.effective.length} 个渠道 · ${issues.join(' · ')}` }
  return { key: 'ok', label: '可路由', detail: `生效 ${rt.effective.length} 个渠道` }
}
const resourceWidth = (enabled, total) => total ? `${Math.max(3, Math.min(100, (Number(enabled) || 0) / Number(total) * 100))}%` : '0%'
// 分组卡片成功率数值配色，阈值与栅栏一致：绿≥95 / 黄≥80 / 红<80
function rateClass(g) {
  if (!g.recent_total) return 'rate-none'
  const r = Number(g.success_rate) || 0
  return r >= 95 ? 'rate-ok' : r >= 80 ? 'rate-warn' : 'rate-bad'
}
const upName = id => upstreams.value.find(u => u.id === id)?.name || ('#' + id)
const monTitle = m => m.name || (m.upstream_name + ' · ' + m.model)
const initial = m => (m.model || '?').replace(/[^a-zA-Z0-9]/g, '').charAt(0).toUpperCase() || '?'
const sinceText = ts => {
  if (!ts) return '从未'
  const s = Math.floor(Date.now() / 1000) - ts
  if (s < 60) return s + ' 秒前'
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前'
  return Math.floor(s / 3600) + ' 小时前'
}

// 总览按照处理优先级聚合现有接口数据，正常渠道不进入此视图。
const overviewLevelRank = { critical: 0, warning: 1, info: 2 }
const overviewBillingAlerts = computed(() => {
  const now = Math.floor(Date.now() / 1000)
  const alerts = []
  for (const upstream of upstreams.value) {
    const billing = upstream.billing
    if (!billing) continue
    const remaining = Number(billing.remaining)
    const hasRemaining = billing.remaining !== null && billing.remaining !== undefined && Number.isFinite(remaining)
    const base = { upstream, name: upstream.name, to: 'upstreams', kind: 'billing' }

    if (!billing.unlimited && hasRemaining && remaining < 0) {
      alerts.push({ ...base, key: `balance-debt-${upstream.id}`, level: 'critical', title: '上游已欠费', detail: billingAmount(upstream) })
    } else if (!billing.unlimited && hasRemaining && remaining === 0) {
      alerts.push({ ...base, key: `balance-empty-${upstream.id}`, level: 'critical', title: '余额已耗尽', detail: billingAmount(upstream) })
    } else if (!billing.unlimited && hasRemaining && remaining <= 1) {
      alerts.push({ ...base, key: `balance-low-${upstream.id}`, level: 'warning', title: '余额偏低', detail: `当前 ${billingAmount(upstream)}` })
    }

    if (billing.status === 'error') {
      alerts.push({ ...base, key: `billing-error-${upstream.id}`, level: 'warning', title: '计费采集失败', detail: billing.error || '等待下次自动采集', action: 'refresh-billing' })
    } else if (billing.status === 'partial') {
      alerts.push({ ...base, key: `billing-partial-${upstream.id}`, level: 'warning', title: '计费数据不完整', detail: billing.error || '上游未返回完整计费数据' })
    }

    const refreshedAt = Number(billing.last_success_at || billing.refreshed_at)
    if (refreshedAt && now - refreshedAt > 30 * 60 && billing.status !== 'error') {
      alerts.push({ ...base, key: `billing-stale-${upstream.id}`, level: 'warning', title: '计费数据已过期', detail: `最近成功采集于 ${sinceText(refreshedAt)}` })
    }

    const audit = billing.audit
    if (audit?.status === 'warning') {
      alerts.push({ ...base, key: `audit-warning-${upstream.id}`, level: 'critical', title: '费用核对异常', detail: billingAuditReasons[audit.reason] || billingAuditText(upstream) })
    } else if (audit?.multiplier_changed) {
      alerts.push({ ...base, key: `multiplier-changed-${upstream.id}`, level: 'info', title: '计费倍率已变更', detail: billingMultiplier(upstream) })
    }
  }
  return alerts
})
const overviewServiceAlerts = computed(() => {
  const alerts = []
  for (const upstream of upstreams.value) {
    if (!upstream.enabled || !upstream.health) continue
    if (upstream.health.state === 'OPEN') {
      alerts.push({ key: `upstream-open-${upstream.id}`, level: 'critical', title: '渠道已熔断', detail: rtRate(upstream.health), name: upstream.name, upstream, to: 'upstreams', kind: 'runtime', action: 'recover-upstream' })
    } else if (upstream.health.state === 'HALF_OPEN') {
      alerts.push({ key: `upstream-half-${upstream.id}`, level: 'warning', title: '渠道恢复验证中', detail: rtRate(upstream.health), name: upstream.name, upstream, to: 'upstreams', kind: 'runtime', action: 'recover-upstream' })
    }
  }
  for (const monitor of monitorItems.value) {
    if (!monitor.enabled || !monitor.upstream?.enabled || ['OPEN', 'HALF_OPEN'].includes(monitor.upstream.health?.state)) continue
    const state = monitor.snapshot?.state
    if (state !== 'DOWN' && state !== 'DEGRADED') continue
    alerts.push({
      key: `monitor-${monitor.id}`, level: state === 'DOWN' ? 'critical' : 'warning',
      title: state === 'DOWN' ? '模型监控故障' : '模型监控降级',
      detail: monitor.model, name: monitor.upstream.name, to: 'monitors', kind: 'monitor', monitor,
    })
  }
  const failed = Number(overviewStats.value.failed) || 0
  const partial = Number(overviewStats.value.partial) || 0
  if (failed + partial > 0) {
    alerts.push({
      key: 'request-failures', level: failed > 0 ? 'critical' : 'warning', title: '近期请求异常',
      detail: `${failed} 次失败${partial ? ` · ${partial} 次流中断` : ''}`, name: '近 24 小时', to: 'logs', kind: 'request',
    })
  }
  return alerts
})
const overviewIssues = computed(() => [...overviewBillingAlerts.value, ...overviewServiceAlerts.value]
  .sort((a, b) => overviewLevelRank[a.level] - overviewLevelRank[b.level] || a.name.localeCompare(b.name)))
const overviewIssuesExpanded = ref(false)
const overviewIssueFilter = ref('all')
const overviewAlertTable = ref(null)
const overviewIssueScrollPositions = new Map()
const overviewIssueFilterDefinitions = [
  { key: 'debt', label: '欠费' },
  { key: 'low-balance', label: '余额偏低' },
  { key: 'billing-collect', label: '采集异常' },
  { key: 'billing-audit', label: '费用核对' },
  { key: 'channel', label: '渠道状态' },
  { key: 'monitor', label: '模型监控' },
  { key: 'request', label: '请求异常' },
]
function overviewIssueCategory(issue) {
  if (issue.key.startsWith('balance-debt-') || issue.key.startsWith('balance-empty-')) return 'debt'
  if (issue.key.startsWith('balance-low-')) return 'low-balance'
  if (issue.key.startsWith('billing-') && !issue.key.startsWith('billing-audit-')) return 'billing-collect'
  if (issue.key.startsWith('audit-') || issue.key.startsWith('multiplier-')) return 'billing-audit'
  if (issue.kind === 'runtime') return 'channel'
  if (issue.kind === 'monitor') return 'monitor'
  return 'request'
}
const overviewIssueFilterOptions = computed(() => {
  const counts = new Map()
  for (const issue of overviewIssues.value) {
    const key = overviewIssueCategory(issue)
    counts.set(key, (counts.get(key) || 0) + 1)
  }
  return [
    { key: 'all', label: '全部', count: overviewIssues.value.length },
    ...overviewIssueFilterDefinitions
      .map(option => ({ ...option, count: counts.get(option.key) || 0 }))
      .filter(option => option.count > 0 || option.key === overviewIssueFilter.value),
  ]
})
const overviewFilteredIssues = computed(() => overviewIssueFilter.value === 'all'
  ? overviewIssues.value
  : overviewIssues.value.filter(issue => overviewIssueCategory(issue) === overviewIssueFilter.value))
const overviewVisibleIssues = computed(() => overviewIssuesExpanded.value ? overviewFilteredIssues.value : overviewFilteredIssues.value.slice(0, 5))
const overviewMoreIssueCount = computed(() => Math.max(0, overviewFilteredIssues.value.length - 5))
const overviewIssueSummary = computed(() => ({
  critical: overviewIssues.value.filter(issue => issue.level === 'critical').length,
  warning: overviewIssues.value.filter(issue => issue.level === 'warning').length,
  info: overviewIssues.value.filter(issue => issue.level === 'info').length,
}))
async function selectOverviewIssueFilter(key) {
  if (overviewIssueFilter.value === key) return
  if (overviewAlertTable.value) {
    overviewIssueScrollPositions.set(overviewIssueFilter.value, overviewAlertTable.value.scrollTop)
  }
  overviewIssueFilter.value = key
  await nextTick()
  if (overviewAlertTable.value) overviewAlertTable.value.scrollTop = overviewIssueScrollPositions.get(key) || 0
}
async function toggleOverviewIssues() {
  const table = overviewAlertTable.value
  const keepBottom = table && table.scrollTop + table.clientHeight >= table.scrollHeight - 32
  overviewIssuesExpanded.value = !overviewIssuesExpanded.value
  await nextTick()
  if (keepBottom && overviewAlertTable.value) overviewAlertTable.value.scrollTop = overviewAlertTable.value.scrollHeight
}
const overviewIssueKindText = kind => ({ billing: '计费', runtime: '渠道', monitor: '监控', request: '请求' }[kind] || '系统')
const overviewIssueLevelText = level => ({ critical: '紧急', warning: '警告', info: '提示' }[level] || '提示')
function overviewIssueTime(issue) {
  if (issue.kind === 'billing') {
    const ts = Number(issue.upstream?.billing?.last_success_at || issue.upstream?.billing?.refreshed_at)
    return ts ? `更新于 ${sinceText(ts)}` : '尚未采集'
  }
  if (issue.kind === 'runtime') {
    const ts = Number(issue.upstream?.health?.last_probe)
    return ts ? `探测于 ${sinceText(ts)}` : '尚未探测'
  }
  if (issue.kind === 'monitor') {
    const ts = Number(issue.monitor?.snapshot?.last_ts)
    return ts ? `探测于 ${sinceText(ts)}` : '尚未探测'
  }
  return '近 24 小时'
}

const overviewWindowOptions = [
  { value: '24h', label: '24 小时' },
  { value: '7d', label: '7 天' },
  { value: '30d', label: '30 天' },
]
const overviewTrendTagOptions = computed(() => {
  const used = new Set(upstreams.value.filter(item => item.enabled && item.primary_tag_id > 0)
    .map(item => Number(item.primary_tag_id)))
  return [
    { value: 0, label: '全部标签', color: 'all' },
    ...tags.value
      .filter(tag => used.has(Number(tag.id)))
      .map(tag => ({ value: tag.id, label: tag.name, color: tag.color })),
  ]
})
const overviewSelectedTag = computed(() => tags.value.find(tag => Number(tag.id) === Number(overviewTrendTagID.value)))
const overviewTrendScopeDescription = computed(() => overviewSelectedTag.value
  ? `主标签「${overviewSelectedTag.value.name}」 · 余额按储值倍率折算并显示上游明细`
  : '全部标签 · 余额按储值倍率折算并按主标签汇总')
const overviewCurrencyOptions = computed(() => (overviewTrends.value?.balances || [])
  .map(series => ({ value: series.currency, label: series.currency })))
const overviewSelectedBalance = computed(() => {
  const list = overviewTrends.value?.balances || []
  return list.find(series => series.currency === overviewBalanceCurrency.value) || list[0] || null
})
function overviewPointLabels(points) {
  return points.map(point => {
    const date = new Date(Number(point.ts) * 1000)
    return overviewTrendWindow.value === '24h'
      ? date.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })
      : date.toLocaleDateString('zh-CN', { month: '2-digit', day: '2-digit' })
  })
}
const overviewBalanceChartLabels = computed(() => overviewPointLabels(overviewSelectedBalance.value?.points || []))
const overviewSuccessChartLabels = computed(() => overviewPointLabels(overviewTrends.value?.success || []))
const overviewBalanceData = computed(() => (overviewSelectedBalance.value?.points || [])
  .map(point => point.remaining == null ? null : Number(point.remaining)))
const overviewBalanceChannelColors = Array.from({ length: 10 }, (_, index) => `var(--chart-channel-${index + 1})`)
const overviewUpstreamByID = computed(() => new Map(upstreams.value.map(item => [Number(item.id), item])))
const overviewTagByID = computed(() => new Map(tags.value.map(item => [Number(item.id), item])))
const overviewRawBalanceDatasets = computed(() => (overviewTrends.value?.upstream_balances || [])
  .filter(series => series.currency === (overviewSelectedBalance.value?.currency || 'USD'))
  .filter(series => series.points?.some(point => point.remaining != null))
  .map((series, index) => {
    const upstream = overviewUpstreamByID.value.get(Number(series.upstream_id))
    const tag = overviewTagByID.value.get(Number(upstream?.primary_tag_id))
    return {
      label: series.name || `上游 #${series.upstream_id}`,
      lineLabel: series.name || `上游 #${series.upstream_id}`,
      legendLabel: series.name || `上游 #${series.upstream_id}`,
      data: series.points.map(point => point.remaining == null ? null : Number(point.remaining)),
      color: overviewBalanceChannelColors[index % overviewBalanceChannelColors.length],
      group: tag?.name || '未设置主标签',
      groupColor: tag?.color || 'gray',
      fill: false,
    }
  }))
const overviewBalanceDatasets = computed(() => {
  const source = overviewRawBalanceDatasets.value
  if (Number(overviewTrendTagID.value) > 0) return source

  const groups = new Map()
  for (const series of source) {
    if (!groups.has(series.group)) {
      groups.set(series.group, {
        label: series.group,
        group: '标签汇总',
        groupColor: 'gray',
        tagColor: series.groupColor,
        channelCount: 0,
        data: Array(series.data.length).fill(null),
        fill: false,
      })
    }
    const aggregate = groups.get(series.group)
    aggregate.channelCount++
    series.data.forEach((value, index) => {
      if (value == null) return
      aggregate.data[index] = (aggregate.data[index] ?? 0) + Number(value)
    })
  }
  return [...groups.values()].map((series, index) => ({
    ...series,
    label: `${series.label}（${series.channelCount}）`,
    lineLabel: series.label,
    legendLabel: series.label,
    legendMeta: `${series.channelCount} 渠道`,
    color: overviewBalanceChannelColors[index % overviewBalanceChannelColors.length],
  }))
})
const overviewSuccessData = computed(() => (overviewTrends.value?.success || [])
  .map(point => point.rate == null ? null : Number(point.rate) * 100))
const overviewSuccessDatasets = computed(() => [{
  label: '成功率',
  data: overviewSuccessData.value,
  color: 'var(--chart-primary)',
  fill: true,
  borderWidth: 2,
  pointRadius: overviewSuccessData.value.map(value => value != null && value < 99 ? 4 : 0),
  pointHoverRadius: overviewSuccessData.value.map(value => value != null && value < 99 ? 5 : 3),
  pointBackgroundColor: overviewSuccessData.value.map(value => value != null && value < 99 ? 'var(--chart-danger)' : 'var(--chart-primary)'),
  pointBorderColor: 'var(--surface)',
  pointBorderWidth: 2,
}])
const overviewSuccessChartMin = computed(() => {
  const values = overviewSuccessData.value.filter(value => value != null && Number.isFinite(value))
  if (!values.length) return 95
  return Math.max(0, Math.min(95, Math.floor(Math.min(...values) - 1)))
})
const overviewBalanceHasData = computed(() => overviewBalanceDatasets.value.length > 0)
const overviewSuccessHasData = computed(() => overviewSuccessData.value.some(value => value != null))
const overviewLatestBalance = computed(() => {
  for (let index = overviewBalanceData.value.length - 1; index >= 0; index--) {
    if (overviewBalanceData.value[index] != null) return overviewBalanceData.value[index]
  }
  return null
})
const overviewBalanceChange = computed(() => {
  const values = overviewBalanceData.value.filter(value => value != null && Number.isFinite(value))
  if (values.length < 2) return { amount: null, percent: null }
  const first = values[0]
  const amount = values[values.length - 1] - first
  return {
    amount,
    percent: first === 0 ? null : amount / Math.abs(first) * 100,
  }
})
const overviewSuccessSummary = computed(() => {
  const points = overviewTrends.value?.success || []
  const total = points.reduce((sum, point) => sum + Number(point.total || 0), 0)
  const success = points.reduce((sum, point) => sum + Number(point.success || 0), 0)
  const failed = Math.max(0, total - success)
  const anomalies = points.filter(point => point.rate != null && Number(point.rate) * 100 < 99).length
  return { total, success, failed, anomalies, rate: total ? success / total * 100 : null }
})
const overviewAvailability = computed(() => {
  const enabled = upstreams.value.filter(item => item.enabled)
  const routable = enabled.filter(item => !['OPEN', 'HALF_OPEN'].includes(item.health?.state)).length
  return { total: enabled.length, routable, rate: enabled.length ? routable / enabled.length * 100 : null }
})
const overviewWeekCost = computed(() => overviewSummary.value?.week_cost || null)
const overviewWeekCostCoverage = computed(() => {
  const coverage = Number(overviewWeekCost.value?.coverage)
  return Number.isFinite(coverage) ? `${Math.round(coverage * 100)}%` : '—'
})
const overviewShowPartial = computed(() => overviewTrendWindow.value === '24h' && Number(overviewTrendTagID.value) === 0)
const overviewPartialCount = computed(() => Number(overviewStats.value.partial) || 0)
function overviewBalanceText(value) {
  if (value == null) return '—'
  const currency = overviewSelectedBalance.value?.currency || 'USD'
  try {
    return new Intl.NumberFormat('zh-CN', { style: 'currency', currency, currencyDisplay: 'narrowSymbol', maximumFractionDigits: 2 }).format(value)
  } catch { return `${Number(value).toFixed(2)} ${currency}` }
}
function overviewSignedBalanceText(value) {
  if (value == null) return '—'
  const text = overviewBalanceText(value)
  return Number(value) > 0 ? `+${text}` : text
}
function overviewSignedPercentText(value) {
  if (value == null) return ''
  const number = Number(value)
  return `${number > 0 ? '+' : ''}${number.toFixed(1)}%`
}
function overviewPercentText(value) { return value == null ? '—' : `${Number(value).toFixed(1)}%` }
function overviewCountText(value) { return Number(value || 0).toLocaleString('zh-CN') }
function overviewMetricCountText(value) { return value == null ? '—' : Number(value).toLocaleString('zh-CN') }
function overviewUSDText(value) {
  if (value == null || !Number.isFinite(Number(value))) return '—'
  const amount = Number(value)
  return new Intl.NumberFormat('zh-CN', {
    style: 'currency', currency: 'USD', currencyDisplay: 'narrowSymbol',
    minimumFractionDigits: 2, maximumFractionDigits: Math.abs(amount) < 1 ? 4 : 2,
  }).format(amount)
}

const {
  logs, logPageSize, logCurrentPage, logLoading, logDetail, logDetailLoading, logStats,
  logCacheStats, logCacheExpanded, logCacheSummary,
  logSearch, logFTime, logFGroup, logFModel, logFStatus, logFUpstream, logFKey,
  logFEndpoint, logFErrorKind, logFStream, logFSlow, logFRetried, logAutoRefresh,
  logMoreFilters, logPageSizeOptions, logTotalPages, logPageItems,
  logGroupSelectOptions, logModelSelectOptions, logKeySelectOptions,
  logEndpointSelectOptions, logErrorSelectOptions, logUpstreamSelectOptions,
  logTimeOptions, logStatusOptions, logStreamOptions, logSlowOptions,
  loadLogs, goLogPage, onLogPageSizeChange, loadLogOptions, onLogFilterChange,
  resetLogFilters, openLogDetail, closeLogDetail, startLogPoll, stopLogPoll,
  toggleLogAutoRefresh, logActiveFilters, logAdvancedFilters, requestShort, fmtMs,
  fmtBytes, fmtNum, requestOutcomeText, requestOutcomeClass, errorKindText,
  errorSourceText, selectionText, streamStateText, outcomeText, fmtTime, fmtTimeFull, statusText,
  fmtEndpoint, clientName, cacheRateText, cacheSummary, cacheRateWidth,
} = useLogs({ page, guard })
// 监控项 CRUD
const monModels = ref([])  // 当前对话框选中渠道的可选模型（datalist）
const testModelOptions = computed(() => {
  if (testState.modelsLoading) return [{ value: '', label: '加载模型中…', disabled: true }]
  const opts = testState.models.map(m => ({ value: m, label: m }))
  if (!opts.length && testState.model) opts.push({ value: testState.model, label: testState.model })
  return opts
})
const upstreamSelectOptions = computed(() => upstreams.value.map(u => ({ value: u.id, label: `${u.name} — ${u.base_url}` })))
function loadMonModels(uid) {
  monModels.value = []
  if (!uid) return
  api.testUpstream(uid).then(r => { monModels.value = r.models || [] }).catch(() => {})
}

// 批量建监控对话框状态：拉模型/勾选/共享探测参数
const batchMon = reactive({ loading: false, error: '', models: [], picked: {}, monitored: {} })
const notice = ref('')  // 轻量成功提示，3s 自动消失
function flash(msg) { notice.value = msg; setTimeout(() => { if (notice.value === msg) notice.value = '' }, 3000) }

function openBatchMonitors(u) {
  dlg.type = 'upstream-monitors'
  dlg.form = { upstream_id: u.id, upstream_name: u.name, enabled: true, stream: false, probe_text: '', max_tokens: 0, interval_sec: 0, path: '' }
  // 该上游已建监控的模型集合（前端标“已监控”、避免重复勾选）
  batchMon.monitored = {}
  monitors.value.filter(m => m.upstream_id === u.id).forEach(m => { batchMon.monitored[m.model] = true })
  batchMon.models = []; batchMon.picked = {}; batchMon.error = ''; batchMon.loading = true
  api.testUpstream(u.id)
    .then(r => { batchMon.models = r.models || []; if (r.error) batchMon.error = r.error })
    .catch(e => { batchMon.error = String(e.message || e) })
    .finally(() => { batchMon.loading = false })
}
// 可勾选模型（排除已监控的）
const batchSelectable = computed(() => batchMon.models.filter(m => !batchMon.monitored[m]))
const batchPickedCount = computed(() => batchSelectable.value.filter(m => batchMon.picked[m]).length)
function batchToggleAll() {
  const all = batchPickedCount.value === batchSelectable.value.length && batchSelectable.value.length > 0
  batchSelectable.value.forEach(m => { batchMon.picked[m] = !all })
}
function saveBatchMonitors() {
  const models = batchSelectable.value.filter(m => batchMon.picked[m])
  if (!models.length) return
  guardDialogSave(async () => {
    const f = dlg.form
    const r = await api.createMonitorsBatch(f.upstream_id, {
      models, enabled: f.enabled, stream: f.stream, probe_text: f.probe_text,
      max_tokens: Number(f.max_tokens) || 0, interval_sec: Number(f.interval_sec) || 0, path: f.path,
    })
    closeDlg(); await loadMonitors()
    flash(`已为「${f.upstream_name}」创建 ${r.created} 个监控${r.skipped ? `，跳过 ${r.skipped} 个已存在` : ''}`)
  })
}

const monitorDragId = ref(null)
const monitorDragOverId = ref(null)
const monitorDragGroupKey = ref('')
function onMonitorDragStart(m, e) {
  monitorDragId.value = m.id
  monitorDragGroupKey.value = m.groupKey
  e.dataTransfer.effectAllowed = 'move'
  e.dataTransfer.setData('text/plain', String(m.id))
}
function onMonitorDragOver(m, e) {
  if (m.groupKey !== monitorDragGroupKey.value || m.id === monitorDragId.value) return
  e.preventDefault()
  monitorDragOverId.value = m.id
}
function onMonitorDrop(target) {
  if (target.groupKey !== monitorDragGroupKey.value) { onMonitorDragEnd(); return }
  const section = monitorSections.value.find(item => item.key === target.groupKey)
  const visible = section?.items || []
  const from = visible.findIndex(m => m.id === monitorDragId.value)
  const to = visible.findIndex(m => m.id === target.id)
  if (from < 0 || to < 0 || from === to) { onMonitorDragEnd(); return }
  const reorderedVisible = visible.slice()
  const [moved] = reorderedVisible.splice(from, 1)
  reorderedVisible.splice(to, 0, moved)
  const visibleIDs = new Set(visible.map(m => m.id))
  const rawByID = new Map(monitors.value.map(m => [m.id, m]))
  let visibleIndex = 0
  const arr = monitors.value.map(m => visibleIDs.has(m.id) ? rawByID.get(reorderedVisible[visibleIndex++].id) : m)
  monitors.value = arr
  onMonitorDragEnd()
  guard(() => api.reorderMonitors(arr.map(m => m.id)).catch(e => { loadMonitors().catch(() => {}); throw e }))
}
function onMonitorDragEnd() {
  monitorDragId.value = monitorDragOverId.value = null
  monitorDragGroupKey.value = ''
}
function newMonitor() {
  const uid = upstreams.value[0]?.id || 0
  dlg.type = 'monitor'; dlg.form = { upstream_id: uid, model: '', name: '', enabled: true, stream: false, probe_text: '', max_tokens: 0, interval_sec: 0, path: '' }
  loadMonModels(uid)
}
function editMonitor(m) {
  dlg.type = 'monitor'
  dlg.form = { id: m.id, upstream_id: m.upstream_id, model: m.model, name: m.name, enabled: m.enabled, stream: !!m.stream, probe_text: m.probe_text || '', max_tokens: m.max_tokens || 0, interval_sec: m.interval_sec || 0, path: m.path || '' }
  loadMonModels(m.upstream_id)
}
function saveMonitor() {
  guardDialogSave(async () => {
    const f = { ...dlg.form, upstream_id: Number(dlg.form.upstream_id), max_tokens: Number(dlg.form.max_tokens) || 0, interval_sec: Number(dlg.form.interval_sec) || 0 }
    if (f.id) await api.updateMonitor(f.id, f)
    else await api.createMonitor(f)
    closeDlg(); await loadMonitors()
  })
}
function delMonitor(m) {
  ask(`删除监控项「${monTitle(m)}」？`, () =>
    guard(async () => { await api.deleteMonitor(m.id); await loadMonitors() }))
}
function removeChannelMonitors(channel) {
  const items = channel.monitors || []
  if (!items.length || removingChannels.has(channel.id)) return
  ask(`移除「${channel.upstream.name}」下的 ${items.length} 个监控项？`, () =>
    guard(async () => {
      removingChannels.add(channel.id)
      try {
        for (const item of items) await api.deleteMonitor(item.id)
        await loadMonitors()
      } finally {
        removingChannels.delete(channel.id)
      }
    }))
}
function toggleMonitor(m) {
  guard(async () => { await api.updateMonitor(m.id, { ...m, enabled: !m.enabled }); await loadMonitors() })
}

// 分组内容使用真实高度过渡，避免 grid 行高动画触发连续重排造成卡顿。
const monitorMotionReduced = () => window.matchMedia?.('(prefers-reduced-motion: reduce)').matches
function clearMonitorGroupStyles(el) {
  el.style.removeProperty('height')
  el.style.removeProperty('opacity')
  el.style.removeProperty('transform')
  el.style.removeProperty('overflow')
}
function monitorGroupBeforeEnter(el) {
  el.style.overflow = 'hidden'
  el.style.height = '0px'
  el.style.opacity = '0'
  el.style.transform = 'translateY(-4px)'
}
function monitorGroupEnter(el, done) {
  if (monitorMotionReduced()) { clearMonitorGroupStyles(el); done(); return }
  requestAnimationFrame(() => {
    el.style.height = `${el.scrollHeight}px`
    el.style.opacity = '1'
    el.style.transform = 'translateY(0)'
  })
  window.setTimeout(() => { clearMonitorGroupStyles(el); done() }, 170)
}
function monitorGroupBeforeLeave(el) {
  el.style.overflow = 'hidden'
  el.style.height = `${el.scrollHeight}px`
}
function monitorGroupLeave(el, done) {
  if (monitorMotionReduced()) { clearMonitorGroupStyles(el); done(); return }
  requestAnimationFrame(() => {
    el.style.height = '0px'
    el.style.opacity = '0'
    el.style.transform = 'translateY(-4px)'
  })
  window.setTimeout(() => { clearMonitorGroupStyles(el); done() }, 170)
}

function login() {
  if (!loginForm.token.trim()) { clearError(); err.value = '请输入管理 Token'; return }
  api.setToken(loginForm.token.trim())
  loggedIn.value = true
  activatePage(page.value)
}
function logout() {
  abortUpstreamTestRequests()
  abortGroupTestRequests()
  api.clearToken()
  loggedIn.value = false
  loginForm.token = ''
  groups.value = []; upstreams.value = []; members.value = []; keys.value = []; monitors.value = []; tags.value = []
  stopBackupPoll()
  stopAllPoll()
  pageLoading.value = false
  clearError()
}
</script>

<template>
<div v-if="!loggedIn" class="login-page">
    <div class="login-card">
      <ThemePicker class="login-theme-picker" />
      <div class="logo login-logo"><Icon name="bolt" :size="22" /><span class="logo-text">MuxAPI</span></div>
      <h1>管理后台登录</h1>
      <p>输入服务器环境变量 <code>MUXAPI_TOKEN</code>。</p>
      <div class="field"><label>Token</label><input v-model="loginForm.token" type="password" placeholder="MUXAPI_TOKEN" @keyup.enter="login" autofocus /></div>
      <p v-if="err" class="err-banner">{{ errorInfo.detail }}</p>
      <button class="btn login-btn" @click="login"><Icon name="check" :size="16" />进入后台</button>
    </div>
  </div>

  <div v-else class="layout">
    <header class="app-topbar">
      <div class="app-brand"><span class="app-mark"><Icon name="bolt" :size="20" /></span><span class="logo-text">MuxAPI</span></div>
      <div class="app-page-context">
        <h1 class="app-page-title">{{ detailGroup ? detailGroup.name : pages[page].title }}</h1>
        <p class="app-page-desc">{{ detailGroup ? '管理该分组的上游成员与接入密钥' : pages[page].desc }}</p>
      </div>
      <div class="app-topbar-actions"><span v-if="appVersion" class="app-version">{{ appVersion }}</span><ThemePicker /><button class="btn-link sm" @click="logout">退出</button></div>
    </header>

    <aside class="subnav-rail">
      <nav class="subnav" aria-label="主导航">
        <RouterLink class="subnav-item" :class="{ active: page === 'overview' }" :to="{ name: 'overview' }" aria-label="总览" data-label="总览"><Icon name="bolt" :size="18" /><span>总览</span></RouterLink>
        <RouterLink class="subnav-item" :class="{ active: page === 'monitors' }" :to="{ name: 'monitors' }" aria-label="渠道监控" data-label="渠道监控"><Icon name="heart" :size="18" /><span>渠道监控</span></RouterLink>
        <RouterLink class="subnav-item" :class="{ active: page === 'logs' }" :to="{ name: 'logs' }" aria-label="请求记录" data-label="请求记录"><Icon name="refresh" :size="18" /><span>请求记录</span></RouterLink>
        <RouterLink class="subnav-item" :class="{ active: page === 'routing' }" :to="{ name: 'routing' }" aria-label="路由" data-label="路由"><Icon name="link" :size="18" /><span>路由</span></RouterLink>
        <RouterLink class="subnav-item" :class="{ active: page === 'groups' }" :to="{ name: 'groups' }" aria-label="分组管理" data-label="分组管理" @click.exact="detailGroup && backToGroups()"><Icon name="cube" :size="18" /><span>分组管理</span></RouterLink>
        <RouterLink class="subnav-item" :class="{ active: page === 'upstreams' }" :to="{ name: 'upstreams' }" aria-label="上游池" data-label="上游池"><Icon name="server" :size="18" /><span>上游池</span></RouterLink>
        <RouterLink class="subnav-item" :class="{ active: page === 'settings' }" :to="{ name: 'settings' }" aria-label="系统设置" data-label="系统设置"><Icon name="cog" :size="18" /><span>系统设置</span></RouterLink>
      </nav>
    </aside>

    <div class="main-wrap">
      <main class="main">
        <section v-if="err" class="error-toast" role="alert" aria-live="assertive">
          <span class="error-toast-icon"><Icon name="alert" :size="17" /></span>
          <span class="error-toast-copy">
            <strong>{{ errorInfo.title }}</strong>
            <span>{{ errorInfo.detail }}<small v-if="errorInfo.code">{{ errorInfo.code }}</small></span>
          </span>
          <span class="error-toast-actions">
            <button class="icon-btn error-toast-retry" type="button" :disabled="pageLoading" title="重新加载当前页" aria-label="重新加载当前页" @click="retryCurrentView"><Icon :name="pageLoading ? 'loader' : 'refresh'" :class="{ spin: pageLoading }" :size="15" /></button>
            <button class="icon-btn error-toast-close" type="button" title="关闭提示" aria-label="关闭错误提示" @click="clearError"><Icon name="x" :size="15" /></button>
          </span>
        </section>
        <p v-if="notice" class="ok-banner">{{ notice }}</p>

        <section v-if="pageLoading" class="page-loading" aria-live="polite" aria-busy="true">
          <div class="page-loading-status">
            <span class="page-loading-icon"><Icon name="loader" :size="16" /></span>
            <div><strong>{{ pageLoadingLabel }}</strong><span>正在同步服务器数据</span></div>
          </div>
          <div class="page-loading-charts" aria-hidden="true">
            <section v-for="item in 2" :key="`loading-chart-${item}`" class="loading-chart-card">
              <header class="loading-chart-head">
                <span class="loading-line loading-line-label"></span>
                <span class="loading-chip"></span>
              </header>
              <span class="loading-line loading-line-value"></span>
              <div class="loading-chart-bars">
                <i v-for="bar in 10" :key="`loading-chart-${item}-${bar}`"></i>
              </div>
            </section>
          </div>
          <div class="page-loading-list" aria-hidden="true">
            <span class="loading-line loading-line-title"></span>
            <div v-for="item in 3" :key="`loading-row-${item}`" class="loading-row">
              <i></i><span></span><small></small>
            </div>
          </div>
        </section>

        <template v-else>

        <!-- 总览：趋势先于文字，异常只保留需要处理的提示。 -->
        <template v-if="page === 'overview'">
          <section class="ov-trends">
            <header class="ov-trends-head">
              <div>
                <h2>运行趋势</h2>
                <p>{{ overviewTrendScopeDescription }}</p>
              </div>
              <div class="ov-trend-controls">
                <FancySelect v-model="overviewTrendTagID" variant="tag" searchable :options="overviewTrendTagOptions" :disabled="overviewTrendLoading" @change="changeOverviewTag" />
                <div class="ov-segmented" role="tablist" aria-label="趋势时间范围">
                  <button v-for="option in overviewWindowOptions" :key="option.value" type="button" :class="{ active: overviewTrendWindow === option.value }" :disabled="overviewTrendLoading" @click="overviewTrendWindow = option.value; guard(loadOverviewTrends)">{{ option.label }}</button>
                </div>
                <button class="icon-btn" :disabled="overviewTrendLoading || overviewSummaryLoading" title="刷新运行数据" @click="guard(refreshOverviewData)"><Icon name="refresh" :class="{ spin: overviewTrendLoading || overviewSummaryLoading }" :size="16" /></button>
              </div>
            </header>

            <div class="ov-summary-band" aria-label="运行指标" :aria-busy="overviewSummaryLoading">
              <section class="ov-summary-item request">
                <span class="ov-summary-label"><i></i>今日请求<Icon v-if="overviewSummaryLoading && !overviewSummary" name="loader" :size="12" /></span>
                <strong class="ov-summary-value">{{ overviewMetricCountText(overviewSummary?.today_requests) }}</strong>
                <small>从 00:00 起</small>
              </section>
              <section class="ov-summary-item cost">
                <span class="ov-summary-label"><i></i>本周预估费用<Icon v-if="overviewSummaryLoading && !overviewSummary" name="loader" :size="12" /></span>
                <strong class="ov-summary-value">{{ overviewUSDText(overviewWeekCost?.amount) }}</strong>
                <small>LiteLLM 价目 · 覆盖 {{ overviewWeekCostCoverage }}</small>
              </section>
              <section class="ov-summary-item availability">
                <span class="ov-summary-label"><i></i>上游可用性</span>
                <strong class="ov-summary-value">{{ overviewPercentText(overviewAvailability.rate) }}</strong>
                <small>{{ overviewAvailability.routable }} / {{ overviewAvailability.total }} 个渠道可路由</small>
              </section>
            </div>

            <div v-if="overviewTrendLoading && !overviewTrends" class="ov-trend-loading"><Icon name="loader" :size="17" />正在读取趋势</div>
            <div v-else-if="overviewTrendError" class="ov-trend-empty"><Icon name="alert" :size="17" /><span>趋势暂时不可用：{{ overviewTrendError }}</span></div>
            <div v-else class="ov-chart-grid">
              <section class="ov-chart-panel ov-chart-panel-primary">
                <header class="ov-chart-head">
                  <div class="ov-chart-heading">
                    <h3>上游余额趋势</h3>
                    <div class="ov-chart-value-row">
                      <strong>{{ overviewBalanceText(overviewLatestBalance) }}</strong>
                      <span class="ov-balance-change" :class="{ positive: overviewBalanceChange.amount > 0, negative: overviewBalanceChange.amount < 0 }">
                        <small>区间变化</small>
                        <b>{{ overviewSignedBalanceText(overviewBalanceChange.amount) }}</b>
                        <em v-if="overviewBalanceChange.percent != null">{{ overviewSignedPercentText(overviewBalanceChange.percent) }}</em>
                      </span>
                    </div>
                  </div>
                  <FancySelect v-if="overviewCurrencyOptions.length > 1" v-model="overviewBalanceCurrency" :options="overviewCurrencyOptions" />
                  <span v-else class="ov-chart-meta">{{ overviewSelectedBalance?.currency || '暂无币种' }}</span>
                </header>
                <div v-if="overviewBalanceHasData" class="ov-chart"><Chart :key="`balance-${overviewTrendTagID}-${overviewTrends?.to_at || 0}`" :labels="overviewBalanceChartLabels" :datasets="overviewBalanceDatasets" color="var(--chart-threshold)" axis-labels show-legend line-labels :fmt="overviewBalanceText" /></div>
                <div v-else class="ov-chart-empty"><Icon name="server" :size="18" />暂无余额历史</div>
                <footer class="ov-chart-foot">
                  <span><b>{{ overviewCountText(overviewTrends?.upstream_count) }}</b> 个渠道</span>
                  <span><b>{{ overviewCountText(overviewTrends?.unlimited_count) }}</b> 个无限额度</span>
                </footer>
              </section>

              <section class="ov-chart-panel ov-chart-panel-secondary">
                <header class="ov-chart-head">
                  <div class="ov-chart-heading">
                    <h3>请求成功率趋势</h3>
                    <div class="ov-chart-value-row">
                      <strong>{{ overviewPercentText(overviewSuccessSummary.rate) }}</strong>
                      <span class="ov-chart-goal"><i></i>目标 99%</span>
                    </div>
                  </div>
                </header>
                <dl class="ov-success-metrics">
                  <div><dt>请求数</dt><dd>{{ overviewCountText(overviewSuccessSummary.total) }}</dd></div>
                  <div><dt>失败数</dt><dd :class="{ alert: overviewSuccessSummary.failed > 0 }">{{ overviewCountText(overviewSuccessSummary.failed) }}</dd></div>
                  <div v-if="overviewShowPartial"><dt>流中断</dt><dd :class="{ alert: overviewPartialCount > 0 }">{{ overviewCountText(overviewPartialCount) }}</dd></div>
                </dl>
                <div v-if="overviewSuccessHasData" class="ov-chart"><Chart :key="`success-${overviewTrendTagID}-${overviewTrends?.to_at || 0}`" :labels="overviewSuccessChartLabels" :datasets="overviewSuccessDatasets" :min="overviewSuccessChartMin" :max="100" :threshold="99" threshold-label="99%" axis-labels :fmt="overviewPercentText" /></div>
                <div v-else class="ov-chart-empty"><Icon name="heart" :size="18" />暂无请求历史</div>
                <footer class="ov-chart-foot">
                  <span :class="{ alert: overviewSuccessSummary.anomalies > 0 }">{{ overviewSuccessSummary.anomalies > 0 ? `${overviewSuccessSummary.anomalies} 个时段低于目标` : '各时段均达目标' }}</span>
                  <span>按 {{ overviewTrendWindow === '24h' ? '小时' : overviewTrendWindow === '7d' ? '6 小时' : '天' }}聚合</span>
                </footer>
              </section>
            </div>
          </section>

          <section class="ov-alert-strip">
            <header class="ov-alert-head">
              <div class="ov-alert-heading">
                <div class="ov-alert-title-row"><h2>需要处理</h2><span v-if="overviewIssues.length" class="ov-alert-total">{{ overviewIssues.length }}</span></div>
                <p>{{ overviewIssues.length ? '按紧急度排列，优先显示影响路由与计费的事项' : '余额、计费与服务状态均正常' }}</p>
              </div>
              <div v-if="overviewIssues.length" class="ov-alert-summary">
                <span class="critical"><i></i><em>紧急</em><b>{{ overviewIssueSummary.critical }}</b></span>
                <span class="warning"><i></i><em>警告</em><b>{{ overviewIssueSummary.warning }}</b></span>
                <span class="info"><i></i><em>提示</em><b>{{ overviewIssueSummary.info }}</b></span>
              </div>
            </header>
            <div v-if="overviewIssues.length" class="ov-alert-filters" role="tablist" aria-label="待处理类型">
              <button v-for="option in overviewIssueFilterOptions" :key="option.key" type="button" role="tab" :aria-selected="overviewIssueFilter === option.key" :class="{ active: overviewIssueFilter === option.key }" @click="selectOverviewIssueFilter(option.key)">
                {{ option.label }}<small>{{ option.count }}</small>
              </button>
            </div>
            <div v-if="!overviewIssues.length" class="ov-alert-clear"><span><Icon name="check" :size="17" /></span><div><strong>当前无需处理</strong><small>未发现余额、计费或服务异常</small></div></div>
            <div v-else-if="!overviewFilteredIssues.length" class="ov-alert-clear"><span><Icon name="check" :size="17" /></span><div><strong>当前类型无需处理</strong><small>该分类暂未发现异常</small></div></div>
            <div v-else ref="overviewAlertTable" class="ov-alert-table" role="list" aria-label="待处理事项">
              <div :key="overviewIssueFilter" class="ov-alert-list">
                <article v-for="issue in overviewVisibleIssues" :key="issue.key" class="ov-alert-row" :class="issue.level" role="listitem">
                  <span class="ov-alert-level"><i></i>{{ overviewIssueLevelText(issue.level) }}</span>
                  <div class="ov-alert-body">
                    <span class="ov-alert-subject"><strong>{{ issue.title }}</strong><small>{{ overviewIssueKindText(issue.kind) }} · {{ issue.name }}</small></span>
                    <span class="ov-alert-detail" :title="issue.detail || ''">{{ issue.detail || '—' }}</span>
                  </div>
                  <span class="ov-alert-time">{{ overviewIssueTime(issue) }}</span>
                  <span class="ov-alert-operation">
                    <button v-if="issue.action === 'refresh-billing'" class="ov-alert-action" :disabled="refreshingBilling.has(issue.upstream.id)" @click="guard(() => refreshUpstreamBilling(issue.upstream))"><Icon name="refresh" :size="14" />刷新</button>
                    <button v-else-if="issue.action === 'recover-upstream'" class="ov-alert-action" :disabled="recoveringUpstreams.has(issue.upstream.id)" @click="guard(() => recoverUpstream(issue.upstream))"><Icon name="refresh" :size="14" />恢复</button>
                    <RouterLink v-else class="ov-alert-action" :to="{ name: issue.to }">查看<Icon name="chevron-right" :size="14" /></RouterLink>
                  </span>
                </article>
                <button v-if="overviewMoreIssueCount" class="ov-alert-expand" type="button" @click="toggleOverviewIssues">
                  {{ overviewIssuesExpanded ? '收起事项' : ('查看全部 ' + overviewFilteredIssues.length + ' 项') }}
                  <Icon name="chevron-right" :size="14" :class="{ rotated: overviewIssuesExpanded }" />
                </button>
              </div>
            </div>
          </section>
        </template>

        <!-- 分组列表 -->
        <template v-if="page === 'groups' && !detailGroup">
          <div class="toolbar">
            <div class="toolbar-left"><span class="count">{{ groups.length }} 个分组</span></div>
            <button class="btn" @click="newGroup"><Icon name="plus" :size="16" />新建分组</button>
          </div>
          <div class="cards group-cards">
            <div class="card group-card" v-for="g in groups" :key="g.id" @click="openDetail(g)"
              :class="{ dragging: groupDragId === g.id, dragover: groupDragOverId === g.id }"
              @dragover="onGroupDragOver(g, $event)" @drop="onGroupDrop(g)">
              <div class="card-head">
                <div class="gc-id">
                  <span class="gc-avatar">{{ (g.name || '?').slice(0,1) }}</span>
                  <div class="gc-titlewrap">
                    <span class="card-name">{{ g.name }}</span>
                    <p class="card-desc">{{ g.description || '无描述' }}</p>
                  </div>
                </div>
                <div class="card-actions">
                  <span class="mon-grip group-grip" draggable="true" title="拖拽调整顺序" @dragstart="onGroupDragStart(g, $event)" @dragend="onGroupDragEnd" @click.stop><Icon name="grip" :size="16" /></span>
                  <button class="icon-btn" @click.stop="editGroup(g)"><Icon name="edit" :size="16" /></button>
                  <button class="icon-btn danger" @click.stop="delGroup(g)"><Icon name="trash" :size="16" /></button>
                </div>
              </div>

              <div class="gc-route-state" :class="`is-${groupRouteState(g.runtime).key}`">
                <span class="gc-route-dot"><i></i></span>
                <span><strong>{{ groupRouteState(g.runtime).label }}</strong><small>{{ groupRouteState(g.runtime).detail }}</small></span>
              </div>

              <div class="gc-metrics">
                <div class="gc-success-metric">
                  <span>近 24h 成功率</span>
                  <b :class="rateClass(g)">{{ g.recent_total ? g.success_rate + '%' : '—' }}</b>
                </div>
                <div><span>调用</span><b>{{ g.recent_total || 0 }}</b></div>
                <div><span>延迟</span><b>{{ g.recent_total ? g.avg_latency_ms + 'ms' : '—' }}</b></div>
              </div>

              <div class="gc-resources">
                <div class="gc-resource">
                  <div><span>上游池</span><b>{{ g.enabled_upstream_count || 0 }}/{{ g.upstream_count || 0 }}</b></div>
                  <span class="gc-resource-track"><i :style="{ width: resourceWidth(g.enabled_upstream_count, g.upstream_count) }"></i></span>
                </div>
                <div class="gc-resource">
                  <div><span>接入密钥</span><b>{{ g.enabled_key_count || 0 }}/{{ g.key_count || 0 }}</b></div>
                  <span class="gc-resource-track"><i :style="{ width: resourceWidth(g.enabled_key_count, g.key_count) }"></i></span>
                </div>
              </div>

              <div class="gc-policy">
                <span v-if="g.max_multiplier" class="gc-policy-item">倍率 ≤ {{ formatMultiplier(g.max_multiplier) }}</span>
                <span v-if="g.runtime?.multiplier_blocked" class="gc-policy-item blocked">拦截 {{ g.runtime.multiplier_blocked }} 次</span>
                <span class="gc-policy-item effective" :title="effText(g.runtime)">生效 {{ g.runtime?.effective?.length || 0 }} 个渠道</span>
              </div>

              <div class="gc-trend">
                <div class="gc-trend-head"><span>24h 请求分布</span><b>{{ g.recent_total ? `${g.recent_total} 次` : '暂无请求' }}</b></div>
                <Fence :trend="g.trend || []" />
              </div>

              <div class="card-foot"><span>点击管理上游与密钥</span><Icon name="chevron-right" :size="15" /></div>
            </div>
            <div v-if="!groups.length" class="empty">还没有分组，点右上角新建一个。</div>
          </div>
        </template>

        <!-- 分组详情 -->
        <template v-else-if="detailGroup">
          <button class="btn-link" @click="backToGroups">← 返回分组列表</button>

          <div class="section-head">
            <h3 class="section-title">上游成员</h3>
            <button class="btn btn-sm" @click="addMember" :disabled="!addable.length"><Icon name="plus" :size="14" />从池中添加</button>
          </div>
          <div class="table-wrap">
            <table>
              <thead><tr><th>名称</th><th>地址</th><th>组内优先级</th><th>权重</th><th>计费倍率</th><th>运行时</th><th>成功率</th><th>延迟</th><th>组内开关</th><th>操作</th></tr></thead>
              <tbody>
                <tr v-for="m in members" :key="m.upstream_id" :class="{ 'row-eff': m.effective }">
                  <td class="cell-name">{{ m.name }}<span v-if="m.effective" class="eff-badge">生效中</span></td>
                  <td class="cell-url">
                    <a v-if="upstreamHref(m.base_url)" class="upstream-url-link" :href="upstreamHref(m.base_url)" target="_blank" rel="noopener noreferrer" :title="`打开 ${m.base_url}`">
                      <span>{{ m.base_url }}</span><Icon name="external-link" :size="13" />
                    </a>
                    <span v-else>{{ m.base_url }}</span>
                  </td>
                  <td>{{ m.priority }}</td>
                  <td>{{ m.weight }}</td>
                  <td class="member-multiplier"><span>{{ m.effective_multiplier == null ? '—' : formatMultiplier(m.effective_multiplier) }}</span><small v-if="m.multiplier_blocked">超限</small></td>
                  <td>
                    <span class="state-badge" :class="rtClass(m.health)">{{ m.enabled && m.group_enabled ? rtLabel(m.health) : '已停用' }}</span>
                    <div v-if="visibleDots(m).length" class="model-dots">
                      <button v-for="mh in visibleDots(m)" :key="mh.model" type="button" class="model-dot model-dot-action" :class="mhClass(mh)" :title="mhTitle(mh)" :disabled="recoveringModels.has(`${m.upstream_id}:${mh.model}`)" @click="guard(() => recoverUpstreamModel(m, mh))">{{ mh.model }}</button>
                    </div>
                  </td>
                  <td>{{ rtRate(m.health) }}</td>
                  <td>{{ m.health && m.health.avg_lat_ms ? m.health.avg_lat_ms + 'ms' : '—' }}</td>
                  <td>
                    <span v-if="!m.enabled" class="tag off" title="该上游已全局停用，请到上游池页启用">全局停用</span>
                    <span v-else class="tag" :class="m.group_enabled ? 'on' : 'off'">{{ m.group_enabled ? '启用' : '停用' }}</span>
                  </td>
                  <td>
                    <button v-if="m.health?.state === 'OPEN' || m.health?.state === 'HALF_OPEN'" class="icon-btn" title="手动恢复渠道" :disabled="recoveringUpstreams.has(m.upstream_id)" @click="guard(() => recoverUpstream(m))"><Icon name="refresh" :size="16" /></button>
                    <button v-if="m.enabled" class="btn-link sm" @click="toggleMember(m)">{{ m.group_enabled ? '停用' : '启用' }}</button>
                    <button class="icon-btn" @click="editMember(m)"><Icon name="edit" :size="16" /></button>
                    <button class="icon-btn danger" @click="removeMember(m)"><Icon name="trash" :size="16" /></button>
                  </td>
                </tr>
                <tr v-if="!members.length"><td colspan="10" class="empty-cell">暂无上游，从全局池添加。</td></tr>
              </tbody>
            </table>
          </div>

          <div class="section-head section-head-spaced">
            <h3 class="section-title">接入密钥</h3>
            <button class="btn btn-sm" @click="createKey"><Icon name="plus" :size="14" />生成密钥</button>
          </div>
          <p class="hint">客户端用这里的密钥访问 MuxAPI，请求即路由到本分组的上游池。密钥仅在生成时明文显示一次。</p>
          <div class="table-wrap">
            <table>
              <thead><tr><th>名称</th><th>密钥</th><th>状态</th><th>操作</th></tr></thead>
              <tbody>
                <tr v-for="k in keys" :key="k.id">
                  <td>{{ k.name || '—' }}</td>
                  <td><code class="key-cell" title="点击复制" @click="copyText(k.key, k.id)">{{ k.key }}</code><span v-if="copied === k.id" class="copy-feedback">已复制 ✓</span></td>
                  <td><span class="tag" :class="k.enabled ? 'on' : 'off'">{{ k.enabled ? '启用' : '停用' }}</span></td>
                  <td>
                    <button class="icon-btn" :disabled="!k.enabled" :title="k.enabled ? '使用此密钥测试分组' : '启用密钥后可测试'" aria-label="测试分组" @click="openGroupTest(k)"><Icon name="play" :size="16" /></button>
                    <button class="btn-link sm" @click="toggleKey(k)">{{ k.enabled ? '停用' : '启用' }}</button>
                    <button class="icon-btn danger" @click="delKey(k)"><Icon name="trash" :size="16" /></button>
                  </td>
                </tr>
                <tr v-if="!keys.length"><td colspan="4" class="empty-cell">暂无密钥，点生成一个。</td></tr>
              </tbody>
            </table>
          </div>
        </template>

        <!-- 上游池 -->
        <template v-else-if="page === 'upstreams'">
          <div class="toolbar upstream-toolbar">
            <div class="toolbar-left">
              <span class="count">{{ upstreamsFiltered.length }} / {{ upstreams.length }} 个上游</span>
              <div class="search-box upstream-search">
                <Icon class="ic" name="search" :size="16" />
                <input v-model="upstreamSearch" class="search-input" placeholder="名称、地址或标签" @input="onUpstreamFilterChange" />
              </div>
              <FancySelect v-model="upstreamProtocolFilter" :options="upstreamProtocolOptions" @change="onUpstreamFilterChange" />
              <FancySelect v-model="upstreamRunFilter" :options="upstreamRunOptions" @change="onUpstreamFilterChange" />
              <FancySelect v-model="upstreamEnabledFilter" :options="upstreamEnabledOptions" @change="onUpstreamFilterChange" />
            </div>
            <div class="toolbar-actions">
              <button class="btn btn-ghost" @click="openTagManager"><Icon name="filter" :size="16" />标签管理</button>
              <button class="btn" @click="newUpstream"><Icon name="plus" :size="16" />新增上游</button>
            </div>
          </div>
          <!-- Tag Chip Bar：多选标签筛选 -->
          <div v-if="tags.length" class="tag-chip-bar">
            <button class="tag-chip-bar-item" :class="{ active: !upstreamTagFilters.size }" @click="clearTagFilters">全部</button>
            <button v-for="item in tagChipItems" :key="item.id"
              class="tag-chip-bar-item" :class="{ 'tag-active': upstreamTagFilters.has(item.id) }"
              @click="toggleTagFilter(item.id)">
              <span class="tag-color-dot" :class="`tag-${item.color}`"></span>
              {{ item.name }}<span class="chip-count">{{ item.count }}</span>
            </button>
          </div>
          <div v-if="upstreamSelectedCount" class="upstream-batchbar">
            <b>已选 {{ upstreamSelectedCount }} 项</b>
            <button class="btn btn-sm" @click="guard(() => batchUpdateUpstreams({ enabled: true }, `已启用 ${upstreamSelectedCount} 个渠道`))"><Icon name="check" :size="15" />启用</button>
            <button class="btn btn-sm btn-ghost" @click="guard(() => batchUpdateUpstreams({ enabled: false }, `已停用 ${upstreamSelectedCount} 个渠道`))"><Icon name="x" :size="15" />停用</button>
            <span class="batch-separator"></span>
            <FancySelect v-model="upstreamBatchTagID" :options="batchTagOptions" />
            <button class="btn btn-sm btn-ghost" @click="guard(() => applyBatchTag('primary'))">设为主标签</button>
            <button class="btn btn-sm btn-ghost" :disabled="!upstreamBatchTagID" @click="guard(() => applyBatchTag('add'))">添加标签</button>
            <button class="btn btn-sm btn-ghost" :disabled="!upstreamBatchTagID" @click="guard(() => applyBatchTag('remove'))">移除标签</button>
            <button class="icon-btn batch-clear" title="取消选择" @click="upstreamSelected.clear()"><Icon name="x" :size="16" /></button>
          </div>
          <div class="upstream-tag-groups">
            <section v-for="section in upstreamPageSections" :key="section.key" class="upstream-tag-section">
              <button class="upstream-tag-head" @click="toggleUpstreamTagSection(section.key)">
                <Icon class="source-chevron" :class="{ collapsed: collapsedUpstreamTags.has(section.key) }" name="chevron-right" :size="16" />
                <span class="tag-color-dot" :class="`tag-${section.tag?.color || 'gray'}`"></span>
                <strong>{{ section.name }}</strong><span>{{ section.rows.length }} 个上游</span>
                <label class="section-select" @click.stop><input type="checkbox" :checked="section.rows.every(u => upstreamSelected.has(u.id))" @change="toggleUpstreamSectionSelection(section.rows)" />选择本组</label>
              </button>
              <div v-if="!collapsedUpstreamTags.has(section.key)" class="table-wrap upstream-table-wrap">
                <table class="upstream-table">
                  <thead><tr><th class="select-cell"><input type="checkbox" :checked="section.rows.every(u => upstreamSelected.has(u.id))" :aria-label="`选择 ${section.name}`" @change="toggleUpstreamSectionSelection(section.rows)" /></th><th></th><th>名称</th><th>标签</th><th>地址</th><th>协议</th><th>计费</th><th>运行时</th><th>成功率</th><th>操作</th></tr></thead>
                  <tbody>
                    <!-- template 包两行：明细行必须与主行同处 v-for 作用域内 -->
                    <template v-for="u in section.rows" :key="u.id">
                    <tr :class="{ disabled: !u.enabled, dragging: upstreamDragId === u.id, dragover: upstreamDragOverId === u.id }"
                      draggable="true" @dragstart="onUpstreamDragStart(u, $event)" @dragover="onUpstreamDragOver(u, $event)" @drop="onUpstreamDrop(u)" @dragend="onUpstreamDragEnd">
                      <td class="select-cell"><input type="checkbox" :checked="upstreamSelected.has(u.id)" :aria-label="`选择 ${u.name}`" @change="toggleUpstreamSelection(u.id)" /></td>
                      <td class="drag-cell"><span class="mon-grip" title="拖拽调整顺序"><Icon name="grip" :size="16" /></span></td>
                      <td class="cell-name">{{ u.name }}</td>
                      <td>
                        <div class="cell-tags-wrap" @click.stop>
                          <div class="tag-chip-row"><span v-for="tag in auxiliaryTagsFor(u)" :key="tag.id" class="manage-tag" :class="`tag-${tag.color}`">{{ tag.name }}</span><span v-if="!auxiliaryTagsFor(u).length" class="tag-empty">—</span></div>
                          <button class="inline-tag-btn" title="快速打标签" @click.stop="openInlineTagPicker(u.id, $event)"><Icon name="plus" :size="12" /></button>
                          <Teleport to="body">
                            <div v-if="inlineTagPickerUpstreamId === u.id" class="inline-tag-picker"
                              :style="{ top: inlineTagPickerPos.top + 'px', left: inlineTagPickerPos.left + 'px' }"
                              @click.stop>
                              <button v-for="tag in tags" :key="tag.id"
                                class="inline-tag-option manage-tag" :class="[`tag-${tag.color}`, { active: (u.tag_ids||[]).includes(tag.id) || u.primary_tag_id === tag.id, 'is-primary': u.primary_tag_id === tag.id }]"
                                @click="toggleInlineTag(u, tag.id)">{{ tag.name }}</button>
                              <div v-if="!tags.length" class="inline-tag-empty">暂无标签</div>
                            </div>
                          </Teleport>
                        </div>
                      </td>
                      <td class="cell-url">
                        <a v-if="upstreamHref(u.base_url)" class="upstream-url-link" :href="upstreamHref(u.base_url)" target="_blank" rel="noopener noreferrer" :title="`打开 ${u.base_url}`">
                          <span>{{ u.base_url }}</span><Icon name="external-link" :size="13" />
                        </a>
                        <span v-else>{{ u.base_url }}</span>
                      </td>
                      <td><span class="tag">{{ protocolLabel(u.protocol) }}</span></td>
                      <td class="billing-cell">
                        <span v-if="!u.billing_type || u.billing_type === 'none'" class="tag-empty">未采集</span>
                        <div v-else class="billing-summary" :class="`billing-${billingStatusClass(u)}`" :title="billingTitle(u)">
                          <div class="billing-values"><strong :class="{ 'billing-debt': Number(u.billing?.remaining) < 0 }">{{ billingAmount(u) }}</strong><span>{{ billingMultiplier(u) }}</span><button class="icon-btn billing-refresh" title="手动录入倍率(作为一次探测结果，下次自动刷新可能覆盖)" @click="guard(() => setBillingMultiplierPrompt(u))"><Icon name="pencil" :size="14" /></button><button class="icon-btn billing-refresh" title="刷新计费数据" :disabled="refreshingBilling.has(u.id)" @click="guard(() => refreshUpstreamBilling(u))"><Icon name="refresh" :size="14" /></button></div>
                          <small><i></i>{{ billingMeta(u) }}</small>
                          <div v-if="u.billing?.audit" class="billing-audit" :class="`billing-audit-${u.billing.audit.status}`">{{ billingAuditText(u) }}</div>
                          <div v-if="billingPricingText(u)" class="billing-pricing">{{ billingPricingText(u) }}</div>
                          <button class="btn-link sm billing-detail-toggle" :aria-expanded="billingDetailOpen.has(u.id)" @click="toggleBillingDetail(u)">{{ billingDetailOpen.has(u.id) ? '收起明细' : '费用明细' }}</button>
                        </div>
                      </td>
                      <td><span class="state-badge" :class="rtClass(u.health)">{{ u.enabled ? rtLabel(u.health) : '已停用' }}</span></td>
                      <td>{{ rtRate(u.health) }}</td>
                      <td>
                        <button v-if="u.health?.state === 'OPEN' || u.health?.state === 'HALF_OPEN'" class="icon-btn" title="手动恢复渠道" :disabled="recoveringUpstreams.has(u.id)" @click="guard(() => recoverUpstream(u))"><Icon name="refresh" :size="16" /></button>
                        <button class="btn-link sm" @click="testUpstream(u)">测试</button><button class="btn-link sm" @click="openBatchMonitors(u)">建监控</button>
                        <button class="icon-btn" title="编辑上游" @click="editUpstream(u)"><Icon name="edit" :size="16" /></button><button class="icon-btn danger" title="删除上游" @click="delUpstream(u)"><Icon name="trash" :size="16" /></button>
                      </td>
                    </tr>
                    <tr v-if="billingDetailOpen.has(u.id)" class="billing-detail-row">
                      <td colspan="10">
                        <div class="billing-detail">
                          <div class="billing-detail-head">
                            <strong>费用比对明细</strong>
                            <div class="billing-window-tabs" role="group" aria-label="比对区间">
                              <button v-for="opt in billingWindowOptions" :key="opt.key" type="button"
                                class="billing-window-tab" :class="{ on: (billingWindowFor[u.id] || '24h') === opt.key }"
                                :aria-pressed="(billingWindowFor[u.id] || '24h') === opt.key"
                                :disabled="billingRangeLoading.has(u.id)"
                                @click="guard(() => loadBillingRange(u, opt.key))">{{ opt.label }}</button>
                            </div>
                          </div>
                          <p v-if="billingRangeLoading.has(u.id)" class="billing-detail-hint">正在汇总…</p>
                          <template v-else-if="billingRangeAudit[u.id]">
                            <p v-if="billingRangeAudit[u.id].reason" class="billing-detail-hint" :class="`billing-audit-${billingRangeAudit[u.id].status}`">
                              {{ billingAuditReasons[billingRangeAudit[u.id].reason] || billingRangeAudit[u.id].reason }}
                            </p>
                            <dl class="billing-detail-grid">
                              <template v-for="row in billingDetailRows(u)" :key="row.label">
                                <dt>{{ row.label }}</dt><dd>{{ row.value }}</dd>
                              </template>
                            </dl>
                          </template>
                          <p v-else class="billing-detail-hint">暂无比对数据。</p>
                        </div>
                      </td>
                    </tr>
                    </template>
                  </tbody>
                </table>
              </div>
            </section>
            <div v-if="!upstreamsFiltered.length" class="empty">{{ upstreams.length ? '没有符合筛选的上游。' : '还没有上游，点右上角新增。' }}</div>
          </div>
          <div v-if="upstreamsFiltered.length" class="log-pager upstream-pager">
            <div class="log-page-size"><span>每页</span><FancySelect v-model="upstreamPageSize" :options="upstreamPageSizeOptions" @change="onUpstreamFilterChange" /></div>
            <div class="log-page-controls">
              <button class="icon-btn log-page-arrow prev" title="上一页" :disabled="upstreamCurrentPage === 1" @click="goUpstreamPage(upstreamCurrentPage - 1)"><Icon name="chevron-right" :size="15" /></button>
              <template v-for="item in upstreamPageItems" :key="item.key">
                <button v-if="item.type === 'page'" class="log-page-number" :class="{ active: item.value === upstreamCurrentPage }" @click="goUpstreamPage(item.value)">{{ item.value }}</button>
                <span v-else class="log-page-ellipsis">…</span>
              </template>
              <button class="icon-btn log-page-arrow" title="下一页" :disabled="upstreamCurrentPage === upstreamTotalPages" @click="goUpstreamPage(upstreamCurrentPage + 1)"><Icon name="chevron-right" :size="15" /></button>
            </div>
          </div>
        </template>

        <!-- 监控看板 -->
        <template v-else-if="page === 'monitors'">
          <section class="availability-overview">
            <div class="availability-heading">
              <span class="availability-mark" :class="summary.allOk ? 'closed' : (summary.down ? 'open' : 'half')"><Icon :name="summary.allOk ? 'check' : 'server'" :size="19" /></span>
              <div><h2>渠道可用性</h2><p>基于该渠道下已启用模型的探测结果汇总</p></div>
            </div>
            <div class="availability-stats" role="tablist" aria-label="渠道可用性筛选">
              <button type="button" :class="{ active: !monitorStatusFilter }" @click="monitorStatusFilter = ''"><span>监测渠道</span><b>{{ summary.total }}</b></button>
              <button type="button" class="closed" :class="{ active: monitorStatusFilter === 'OK' }" @click="monitorStatusFilter = monitorStatusFilter === 'OK' ? '' : 'OK'"><span>可用</span><b>{{ summary.up }}</b></button>
              <button type="button" class="half" :class="{ active: monitorStatusFilter === 'DEGRADED' }" @click="monitorStatusFilter = monitorStatusFilter === 'DEGRADED' ? '' : 'DEGRADED'"><span>波动</span><b>{{ summary.degraded }}</b></button>
              <button type="button" class="open" :class="{ active: monitorStatusFilter === 'DOWN' }" @click="monitorStatusFilter = monitorStatusFilter === 'DOWN' ? '' : 'DOWN'"><span>不可用</span><b>{{ summary.down }}</b></button>
              <button type="button" class="nodata" :class="{ active: monitorStatusFilter === 'NODATA' }" @click="monitorStatusFilter = monitorStatusFilter === 'NODATA' ? '' : 'NODATA'"><span>待检测</span><b>{{ summary.nodata }}</b></button>
            </div>
            <div class="availability-actions">
              <button class="icon-btn" title="刷新渠道状态" @click="guard(loadMonitors)"><Icon name="refresh" :size="16" /></button>
              <button class="icon-btn availability-add" title="新增监控" @click="newMonitor"><Icon name="plus" :size="17" /></button>
            </div>
          </section>
          <div class="monitor-toolbar">
            <div class="search-box monitor-search"><Icon class="ic" name="search" :size="16" /><input v-model="monitorSearch" class="search-input" placeholder="搜索渠道、模型或标签" /></div>
            <FancySelect v-model="monitorTagFilter" :options="monitorTagOptions" />
            <FancySelect v-model="monitorStatusFilter" :options="monitorStatusOptions" />
            <span class="monitor-result-count">{{ monitorVisibleCount }} / {{ monitorChannels.length }} 个渠道</span>
          </div>

          <div class="monitor-wall">
            <section v-for="section in monitorSections" :key="section.key" class="monitor-source-section">
              <button class="monitor-source-head" @click="toggleMonitorTag(section.key)">
                <Icon class="source-chevron" :class="{ collapsed: collapsedMonitorTags.has(section.key) }" name="chevron-right" :size="17" />
                <span class="tag-color-dot" :class="`tag-${section.tag?.color || 'gray'}`"></span>
                <strong>{{ section.name }}</strong>
                <span>{{ section.enabled }} 个监测渠道</span>
                <span v-if="section.down" class="source-stat down">{{ section.down }} 不可用</span>
                <span v-if="section.degraded" class="source-stat warn">{{ section.degraded }} 波动</span>
                <span v-if="section.nodata" class="source-stat">{{ section.nodata }} 待检测</span>
                <em>{{ section.reqs ? (section.rate * 100).toFixed(1) + '% 可用率' : '暂无探测数据' }}</em>
              </button>
              <Transition name="monitor-group" @before-enter="monitorGroupBeforeEnter" @enter="monitorGroupEnter" @before-leave="monitorGroupBeforeLeave" @leave="monitorGroupLeave">
                <div v-if="!collapsedMonitorTags.has(section.key)" class="monitor-group-content">
                  <div class="channel-card-grid availability-channel-grid">
                    <article v-for="channel in section.items" :key="channel.id" class="card channel-monitor-card availability-channel-card"
                      :class="[dotClass(channel.state), { disabled: channel.state === 'DISABLED' }]">
                  <div class="availability-channel-head">
                    <span class="channel-state-mark" :class="dotClass(channel.state)"><Icon name="server" :size="18" /></span>
                    <span class="availability-channel-id"><strong>{{ channel.upstream.name }}</strong><small>{{ channel.upstream.base_url || '未设置地址' }}</small></span>
                    <span class="availability-channel-actions">
                      <span class="state-badge" :class="dotClass(channel.state)">{{ channelStateLabel(channel.state) }}</span>
                      <button class="icon-btn availability-edit" title="编辑渠道" @click.stop="editUpstream(channel.upstream)"><Icon name="edit" :size="15" /></button>
                      <button class="icon-btn danger availability-remove" :disabled="removingChannels.has(channel.id)" title="移除该渠道的全部监控" @click.stop="removeChannelMonitors(channel)"><Icon :name="removingChannels.has(channel.id) ? 'loader' : 'trash'" :class="{ spin: removingChannels.has(channel.id) }" :size="15" /></button>
                    </span>
                  </div>
                  <div class="availability-channel-meta">
                    <span>{{ channel.enabledCount }} 个启用模型</span>
                    <span>路由 <b :class="rtClass(channel.upstream.health)">{{ channel.upstream.enabled ? rtLabel(channel.upstream.health) : '已停用' }}</b></span>
                  </div>
                  <div class="channel-primary-metric">
                    <div class="channel-primary-head"><span>24h 可用率</span><b :class="channel.reqs && channel.rate < .95 ? 'warn' : ''">{{ channel.reqs ? (channel.rate * 100).toFixed(1) + '%' : '—' }}</b></div>
                    <div class="channel-rate-track" role="progressbar" :aria-valuenow="channel.reqs ? Math.round(channel.rate * 1000) / 10 : 0" aria-valuemin="0" aria-valuemax="100"><i :class="dotClass(channel.state)" :style="{ width: channel.reqs ? Math.max(2, Math.min(100, channel.rate * 100)) + '%' : '0%' }"></i></div>
                  </div>
                  <div class="channel-secondary-metrics">
                    <div><span>请求</span><b>{{ channel.reqs || '—' }}</b></div>
                    <div><span>平均延迟</span><b>{{ channel.avgMs || channel.lastMs || '—' }}<small v-if="channel.avgMs || channel.lastMs">ms</small></b></div>
                    <div><span>最近检测</span><b>{{ sinceText(channel.lastTS) }}</b></div>
                  </div>
                  <Fence :trend="channel.trend" unit="探测" />
                  <div class="tag-chip-row monitor-card-tags"><span v-for="tag in auxiliaryTagsFor(channel.upstream)" :key="tag.id" class="manage-tag" :class="`tag-${tag.color}`">{{ tag.name }}</span></div>
                  <div class="mon-foot availability-channel-foot">
                    <button class="btn-link sm channel-detect-button" :disabled="probingChannels.has(channel.id) || !channel.enabledCount" @click="guard(() => probeChannel(channel))"><Icon :name="probingChannels.has(channel.id) ? 'loader' : 'play'" :class="{ spin: probingChannels.has(channel.id) }" :size="13" />{{ probingChannels.has(channel.id) ? '检测中…' : '立即检测' }}</button>
                    <span class="availability-detail-hint">{{ channel.monitors.length }} 个监控项</span>
                  </div>
                    </article>
                  </div>
                </div>
              </Transition>
            </section>
            <div v-if="!monitorSections.length" class="empty">没有符合条件的渠道。</div>
          </div>
        </template>

        <!-- 请求记录页：范围统计 + 服务端筛选 + 按需详情 -->
        <template v-else-if="page === 'logs'">
          <div class="log-summary">
            <div class="log-stat"><span>请求</span><b>{{ fmtNum(logStats.total) }}</b><em>{{ ((logStats.success_rate || 0) * 100).toFixed(1) }}% 成功</em></div>
            <div class="log-stat success"><span>直接成功</span><b>{{ fmtNum(logStats.direct_success) }}</b><em>未切换渠道</em></div>
            <div class="log-stat warn"><span>切换后成功</span><b>{{ fmtNum(logStats.failover_success) }}</b><em>{{ fmtNum(logStats.retried) }} 次发生重试</em></div>
            <div class="log-stat danger"><span>异常</span><b>{{ fmtNum((logStats.failed || 0) + (logStats.partial || 0)) }}</b><em>{{ fmtNum(logStats.partial) }} 次流中断</em></div>
            <div class="log-stat"><span>P95 TTFT</span><b>{{ fmtMs(logStats.p95_ttft_ms) }}</b><em>P50 {{ fmtMs(logStats.p50_ttft_ms) }}</em></div>
            <div class="log-stat"><span>P95 总耗时</span><b>{{ fmtMs(logStats.p95_duration_ms) }}</b><em>所选时间范围</em></div>
            <div class="log-stat cache"><span>缓存命中率</span><b>{{ cacheRateText(logStats) }}</b><em>{{ fmtNum(logStats.cached_tokens) }} / {{ fmtNum(logStats.cache_input_tokens) }} 输入</em></div>
            <div class="log-stat tokens"><span>Token</span><b>{{ fmtNum((logStats.input_tokens || 0) + (logStats.output_tokens || 0)) }}</b><em>入 {{ fmtNum(logStats.input_tokens) }} · 出 {{ fmtNum(logStats.output_tokens) }} · 缓存 {{ fmtNum(logStats.cached_tokens) }}</em></div>
          </div>

          <div class="log-filter-band">
            <div class="log-filter-primary">
              <div class="search-box log-search">
                <Icon class="ic" name="search" :size="16" />
                <input v-model="logSearch" class="search-input" placeholder="请求 ID、客户端或 IP" @keyup.enter="onLogFilterChange" />
              </div>
              <FancySelect v-model="logFTime" :options="logTimeOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFStatus" :options="logStatusOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFUpstream" :options="logUpstreamSelectOptions" @change="onLogFilterChange" />
              <button class="btn btn-sm log-more-filter-button" @click="logMoreFilters = !logMoreFilters"><Icon name="filter" :size="15" />筛选<span v-if="logAdvancedFilters">{{ logAdvancedFilters }}</span></button>
              <label class="log-auto"><input v-model="logAutoRefresh" type="checkbox" @change="toggleLogAutoRefresh" /><span></span>自动刷新</label>
              <span class="log-count">共 {{ fmtNum(logStats.total) }} 条</span>
              <button class="icon-btn log-refresh" :disabled="logLoading" title="刷新当前页" @click="guard(() => loadLogs(false))"><Icon name="refresh" :size="17" /></button>
            </div>
            <div class="log-filter-secondary" :class="{ open: logMoreFilters }">
              <FancySelect v-model="logFGroup" :options="logGroupSelectOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFModel" :options="logModelSelectOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFKey" :options="logKeySelectOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFEndpoint" :options="logEndpointSelectOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFErrorKind" :options="logErrorSelectOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFStream" :options="logStreamOptions" @change="onLogFilterChange" />
              <FancySelect v-model="logFSlow" :options="logSlowOptions" @change="onLogFilterChange" />
              <label class="log-check"><input v-model="logFRetried" type="checkbox" @change="onLogFilterChange" />仅看重试</label>
              <button v-if="logActiveFilters" class="btn-link sm log-reset" @click="resetLogFilters">清除 {{ logActiveFilters }} 项</button>
            </div>
          </div>

          <section v-if="logCacheStats.length" class="log-cache-panel">
            <button class="log-cache-toggle" :aria-expanded="logCacheExpanded" @click="logCacheExpanded = !logCacheExpanded">
              <span class="log-cache-title"><b>渠道缓存</b><small>{{ logCacheStats.length }} 个渠道</small></span>
              <span class="log-cache-summary">
                <span>平均 <b>{{ cacheRateText(logCacheSummary) }}</b></span>
                <span v-if="logCacheSummary.lowest" :title="logCacheSummary.lowest.upstream_name">最低 <em>{{ logCacheSummary.lowest.upstream_name }}</em> <b>{{ cacheRateText(logCacheSummary.lowest) }}</b></span>
                <span v-if="logCacheSummary.highest" :title="logCacheSummary.highest.upstream_name">最高 <em>{{ logCacheSummary.highest.upstream_name }}</em> <b>{{ cacheRateText(logCacheSummary.highest) }}</b></span>
              </span>
              <Icon class="log-cache-chevron" :class="{ expanded: logCacheExpanded }" name="chevron-right" :size="17" />
            </button>
            <div v-if="logCacheExpanded" class="table-wrap log-cache-table-wrap">
              <table class="log-cache-table">
                <thead><tr><th>渠道</th><th>有效请求</th><th>输入 Token</th><th>缓存 Token</th><th>缓存率</th></tr></thead>
                <tbody>
                  <tr v-for="channel in logCacheStats" :key="channel.upstream_id">
                    <td><b class="log-main">{{ channel.upstream_name || ('#' + channel.upstream_id) }}</b></td>
                    <td>{{ fmtNum(channel.usage_requests) }}</td>
                    <td>{{ fmtNum(channel.input_tokens) }}</td>
                    <td>{{ fmtNum(channel.cached_tokens) }}</td>
                    <td><div class="cache-rate-cell"><b>{{ cacheRateText(channel) }}</b><span><i :style="{ width: cacheRateWidth(channel) }"></i></span></div></td>
                  </tr>
                </tbody>
              </table>
            </div>
          </section>

          <div class="table-wrap log-table-wrap">
            <table class="log-table">
              <thead><tr><th>时间 / 请求</th><th>接入</th><th>客户端 / IP</th><th>端点 / 模型</th><th>路由链</th><th>结果</th><th>性能</th><th>Token</th><th>流量</th></tr></thead>
              <tbody>
                <tr v-for="l in logs" :key="l.id" class="log-row" @click="openLogDetail(l)">
                  <td>
                    <div class="log-time" :title="fmtTimeFull(l.created_at)">{{ fmtTime(l.created_at) }}</div>
                    <div class="log-id" :title="l.request_id">{{ requestShort(l.request_id) }}</div>
                  </td>
                  <td><b class="log-main">{{ l.group_name || '未知分组' }}</b><span class="log-sub">{{ l.key_name || '未知密钥' }}</span></td>
                  <td><b class="log-main" :title="l.user_agent">{{ clientName(l.user_agent) }}</b><span class="log-sub mono" :title="l.client_ip">{{ l.client_ip || '未知 IP' }}</span></td>
                  <td><b class="log-main log-endpoint">{{ fmtEndpoint(l.endpoint) }}</b><span class="log-sub log-model" :title="l.model">{{ l.model || '未知模型' }}<i v-if="l.stream">流</i></span></td>
                  <td>
                    <div v-if="l.route?.length" class="route-chain">
                      <template v-for="(step, index) in l.route" :key="step.attempt_no">
                        <Icon v-if="index" name="chevron-right" :size="12" />
                        <span class="route-step" :class="step.outcome === 'success' ? 'ok' : 'fail'" :title="`${outcomeText(step.outcome)} · ${statusText(step.status)}`">{{ step.upstream_name || ('#' + step.upstream_id) }}</span>
                      </template>
                    </div>
                    <span v-else class="log-sub">{{ l.final_upstream_name || '未选择渠道' }}</span>
                  </td>
                  <td><span class="log-status" :class="requestOutcomeClass(l)">{{ requestOutcomeText(l) }} · {{ statusText(l.status) }}</span><span v-if="l.error_kind" class="log-sub error-kind">{{ errorKindText(l.error_kind) }}</span></td>
                  <td><b class="log-metric">{{ fmtMs(l.ttft_ms) }}</b><span class="log-sub">总计 {{ fmtMs(l.duration_ms) }}</span></td>
                  <td><b class="log-metric">{{ fmtNum(l.input_tokens) }} / {{ fmtNum(l.output_tokens) }}</b><span class="log-sub">{{ cacheSummary(l) }}</span></td>
                  <td><b class="log-metric">{{ fmtBytes(l.response_bytes) }}</b><span class="log-sub">{{ streamStateText(l) }}</span></td>
                </tr>
                <tr v-if="!logs.length"><td colspan="9" class="empty-cell">{{ logLoading ? '加载中…' : '没有符合条件的请求记录。' }}</td></tr>
              </tbody>
            </table>
          </div>
          <div v-if="Number(logStats.total) > 0" class="log-pager">
            <div class="log-page-size">
              <span>每页</span>
              <FancySelect v-model="logPageSize" :options="logPageSizeOptions" :disabled="logLoading" @change="onLogPageSizeChange" />
            </div>
            <div class="log-page-controls">
              <button class="icon-btn log-page-arrow prev" title="上一页" aria-label="上一页" :disabled="logLoading || logCurrentPage === 1" @click="guard(() => goLogPage(logCurrentPage - 1))">
                <Icon name="chevron-right" :size="15" />
              </button>
              <template v-for="item in logPageItems" :key="item.key">
                <button v-if="item.type === 'page'" class="log-page-number" :class="{ active: item.value === logCurrentPage }"
                  :aria-current="item.value === logCurrentPage ? 'page' : undefined" :disabled="logLoading" @click="guard(() => goLogPage(item.value))">{{ item.value }}</button>
                <span v-else class="log-page-ellipsis">…</span>
              </template>
              <button class="icon-btn log-page-arrow" title="下一页" aria-label="下一页" :disabled="logLoading || logCurrentPage === logTotalPages" @click="guard(() => goLogPage(logCurrentPage + 1))">
                <Icon name="chevron-right" :size="15" />
              </button>
            </div>
          </div>
        </template>

        <!-- 路由决策 -->
        <template v-else-if="page === 'routing'">
          <RoutingView />
        </template>

        <!-- 设置页：页面内配置分类 -->
        <template v-else-if="page === 'settings'">
          <div class="settings-layout">
            <aside class="settings-nav">
              <div class="settings-nav-head">
                <span class="settings-nav-kicker"><i></i>CONTROL CENTER</span>
                <strong>设置中心</strong>
                <small>运行策略与数据工具</small>
              </div>
              <nav class="settings-nav-list" aria-label="设置分类">
                <button type="button" class="set-navitem" :class="{ active: settingsSection === 'logs' }" @click="gotoSection('logs')">
                  <span class="set-navicon"><Icon name="refresh" :size="16" /></span><span class="set-navcopy"><strong>日志清理</strong><small>记录保存</small></span><Icon class="set-navarrow" name="chevron-right" :size="14" />
                </button>
                <button type="button" class="set-navitem" :class="{ active: settingsSection === 'route' }" @click="gotoSection('route')">
                  <span class="set-navicon"><Icon name="link" :size="16" /></span><span class="set-navcopy"><strong>渠道路由</strong><small>失败切换</small></span><Icon class="set-navarrow" name="chevron-right" :size="14" />
                </button>
                <button type="button" class="set-navitem" :class="{ active: settingsSection === 'alert' }" @click="gotoSection('alert')">
                  <span class="set-navicon"><Icon name="alert" :size="16" /></span><span class="set-navcopy"><strong>健康告警</strong><small>Webhook 通知</small></span><Icon class="set-navarrow" name="chevron-right" :size="14" />
                </button>
                <button type="button" class="set-navitem" :class="{ active: settingsSection === 'endpoint' }" @click="gotoSection('endpoint')">
                  <span class="set-navicon"><Icon name="external-link" :size="16" /></span><span class="set-navcopy"><strong>接入地址</strong><small>客户端入口</small></span><Icon class="set-navarrow" name="chevron-right" :size="14" />
                </button>
                <button type="button" class="set-navitem" :class="{ active: settingsSection === 'backup' }" @click="gotoSection('backup')">
                  <span class="set-navicon"><Icon name="server" :size="16" /></span><span class="set-navcopy"><strong>数据备份</strong><small>S3 / R2 存储</small></span><Icon class="set-navarrow" name="chevron-right" :size="14" />
                </button>
                <button type="button" class="set-navitem" :class="{ active: settingsSection === 'mappings' }" @click="gotoSection('mappings')">
                  <span class="set-navicon"><Icon name="link" :size="16" /></span><span class="set-navcopy"><strong>模型映射</strong><small>名称转换</small></span><Icon class="set-navarrow" name="chevron-right" :size="14" />
                </button>
              </nav>
              <div class="set-navhint"><Icon name="alert" :size="14" /><span>探测间隔与路径已下放到「监控看板」逐项配置。</span></div>
            </aside>
            <div class="settings-body">
              <header class="settings-body-head">
                <div><span class="settings-body-kicker">RUNTIME CONFIGURATION</span><h2>运行设置</h2><p>修改后的参数会即时应用到新的请求。</p></div>
                <span class="settings-live"><i></i>实时生效</span>
              </header>
              <section id="set-logs" v-show="settingsSection === 'logs'" class="card settings-card">
                <div class="settings-title"><h3>日志清理</h3><p>按完成时间保留请求记录；设为 0 可永久保留完整路由与计费历史。</p></div>
                <div class="settings-fields">
                  <div class="field"><label>保留天数（0=永久）</label><input v-model="logRetention" type="number" min="0" max="365" placeholder="0" /></div>
                </div>
                <div class="settings-info">
                  <div><span>请求记录</span><b>{{ effectiveLogRetention ? effectiveLogRetention + ' 天' : '—' }}</b><em>{{ sourceText(logRetentionSource) }}</em></div>
                </div>
                <div class="settings-actions">
                  <span class="save-status" :class="{ show: settingsSaved }">已保存 ✓</span>
                  <button class="btn" @click="saveSettings"><Icon name="check" :size="16" />保存</button>
                </div>
              </section>

              <section id="set-route" v-show="settingsSection === 'route'" class="card settings-card">
                <div class="settings-title"><h3>渠道路由</h3><p>首字节前允许故障切换，流开始后保持透明传输。</p></div>
                <div class="settings-fields">
                  <div class="field"><label>首字节超时（秒）</label><input v-model="firstResponseTimeoutSec" type="number" min="1" max="600" placeholder="120" /></div>
                  <div class="field"><label>最大上游尝试数</label><input v-model="maxUpstreamAttempts" type="number" min="1" max="100" placeholder="6" /></div>
                  <div class="field"><label>熔断失败阈值</label><input v-model="failThreshold" type="number" min="1" max="100" placeholder="3" /></div>
                  <div class="field"><label>熔断冷却时间</label><input v-model="cooldown" placeholder="30s / 5m" /></div>
                  <div class="field"><label>请求体上限（MB）</label><input v-model="maxBodyMB" type="number" min="1" max="256" placeholder="32" /></div>
                </div>
                <div class="settings-info">
                  <div><span>算法</span><b>标准 P2C</b><em>渠道级</em></div>
                  <div><span>首字节超时</span><b>{{ effectiveFirstResponseTimeoutMs ? Math.round(effectiveFirstResponseTimeoutMs / 1000) + ' 秒' : '—' }}</b><em>{{ sourceText(firstResponseTimeoutSource) }}</em></div>
                  <div><span>故障切换</span><b>最多 {{ effectiveMaxUpstreamAttempts || '—' }} 个上游</b><em>{{ sourceText(maxUpstreamAttemptsSource) }}</em></div>
                  <div><span>熔断策略</span><b>{{ effectiveFailThreshold || '—' }} 次 / {{ effectiveCooldown || '—' }}</b><em>{{ sourceText(failThresholdSource || cooldownSource) }}</em></div>
                  <div><span>请求体上限</span><b>{{ effectiveMaxBodyBytes ? Math.round(effectiveMaxBodyBytes / 1048576) + ' MB' : '—' }}</b><em>{{ sourceText(maxBodyBytesSource) }}</em></div>
                </div>
                <p class="hint">收到任何响应字节前可切换渠道；开始传输后不再解析或强制等待结束事件。</p>
                <div class="settings-actions">
                  <span class="save-status" :class="{ show: settingsSaved }">已保存 ✓</span>
                  <button class="btn" @click="saveSettings"><Icon name="check" :size="16" />保存</button>
                </div>
              </section>

              <section id="set-alert" v-show="settingsSection === 'alert'" class="card settings-card">
                <div class="settings-title"><h3>健康告警</h3><p>上游渠道熔断翻转时推送 Webhook，URL 留空则关闭。</p></div>
                <div class="settings-fields">
                  <div class="field"><label>告警 Webhook</label><input v-model="alertWebhook" placeholder="https://... 留空关闭" /></div>
                  <div class="field"><label>去抖间隔</label><input v-model="alertDebounce" placeholder="60s / 5m" /></div>
                </div>
                <div class="settings-info">
                  <div><span>Webhook</span><b>{{ effectiveAlertWebhook || '已关闭' }}</b><em>{{ sourceText(alertWebhookSource) }}</em></div>
                  <div><span>去抖</span><b>{{ effectiveAlertDebounce || '—' }}</b><em>{{ sourceText(alertDebounceSource) }}</em></div>
                </div>
                <div class="settings-actions">
                  <span class="save-status" :class="{ show: settingsSaved }">已保存 ✓</span>
                  <button class="btn" @click="saveSettings"><Icon name="check" :size="16" />保存</button>
                </div>
              </section>

              <section id="set-endpoint" v-show="settingsSection === 'endpoint'" class="card settings-card">
                <div class="settings-title"><h3>接入地址</h3><p>客户端使用接入密钥访问。</p></div>
                <div class="endpoint-list">
                  <div><span>OpenAI</span><code>{{ apiBase }}/v1/chat/completions</code></div>
                  <div><span>Responses</span><code>{{ apiBase }}/v1/responses</code></div>
                  <div><span>Claude</span><code>{{ apiBase }}/v1/messages</code></div>
                  <div><span>Gemini</span><code>{{ apiBase }}/v1beta/models/&lt;model&gt;:generateContent</code></div>
                </div>
                <p class="hint">请求头：<code>Authorization: Bearer &lt;密钥&gt;</code>；Gemini SDK 也可使用 <code>x-goog-api-key</code>。</p>
              </section>

              <!-- 数据备份 -->
              <section id="set-backup" v-show="settingsSection === 'backup'" class="card settings-card">
                <div class="settings-title"><h3>数据备份</h3><p>将 PostgreSQL 数据库自动备份至 S3/R2 对象存储。</p></div>

                <h4 class="settings-subtitle">对象存储配置</h4>
                <div class="settings-fields">
                  <div class="field"><label>Endpoint</label><input v-model="backupConfig.endpoint" placeholder="https://xxx.r2.cloudflarestorage.com" /></div>
                  <div class="field"><label>Region</label><input v-model="backupConfig.region" placeholder="auto" /></div>
                  <div class="field"><label>Bucket</label><input v-model="backupConfig.bucket" placeholder="my-bucket" /></div>
                  <div class="field"><label>Access Key ID</label><input v-model="backupConfig.access_key_id" /></div>
                  <div class="field"><label>Secret Key</label><input v-model="backupConfig.secret_key" type="password" placeholder="已配置时显示占位符" /></div>
                  <div class="field"><label>前缀 (Prefix)</label><input v-model="backupConfig.prefix" placeholder="muxapi/backups/" /></div>
                  <div class="field"><label>Path Style</label>
                    <label class="settings-check">
                      <input type="checkbox" v-model="backupConfig.force_path_style" />
                      Force Path Style（部分私有部署需要）
                    </label>
                  </div>
                </div>
                <div class="settings-actions">
                  <button class="btn btn-ghost" :disabled="backupTesting" @click="guard(testS3)">{{ backupTesting ? '测试中…' : '测试连接' }}</button>
                  <span v-if="backupTestResult === 'ok'" class="backup-test-result ok">✓ 连接成功</span>
                  <span v-if="backupTestResult === 'err'" class="backup-test-result fail" :title="backupTestMsg">✗ {{ backupTestMsg }}</span>
                  <span class="save-status" :class="{ show: settingsSaved }">已保存 ✓</span>
                  <button class="btn" @click="guard(saveBackupConfig)"><Icon name="check" :size="16" />保存配置</button>
                </div>

                <h4 class="settings-subtitle spaced">定时备份</h4>
                <div class="settings-fields">
                  <div class="field"><label>启用</label>
                    <label class="settings-check">
                      <input type="checkbox" v-model="backupSchedule.enabled" />
                      启用定时备份
                    </label>
                  </div>
                  <div class="field"><label>Cron 表达式</label><input v-model="backupSchedule.cron_expr" placeholder="0 3 * * *（每天凌晨3点）" /></div>
                  <div class="field"><label>保留天数</label><input v-model.number="backupSchedule.retain_days" type="number" min="0" placeholder="14（0=不限）" /></div>
                  <div class="field"><label>最多保留份数</label><input v-model.number="backupSchedule.retain_count" type="number" min="0" placeholder="30（0=不限）" /></div>
                </div>
                <div class="settings-actions">
                  <span class="save-status" :class="{ show: settingsSaved }">已保存 ✓</span>
                  <button class="btn" @click="guard(saveBackupSchedule)"><Icon name="check" :size="16" />保存计划</button>
                </div>

                <h4 class="settings-subtitle spaced">备份记录</h4>
                <div class="settings-actions backup-record-actions">
                  <button class="btn" :disabled="backupTriggering" @click="guard(triggerBackup)">{{ backupTriggering ? '备份中…' : '立即备份' }}</button>
                  <button class="btn btn-ghost" @click="guard(loadBackups)">刷新</button>
                </div>
                <div v-if="backupLoading" class="hint">加载中…</div>
                <div v-else-if="backupRecords.length === 0" class="hint">暂无备份记录</div>
                <div v-else class="backup-table-wrap">
                  <table class="backup-table">
                    <thead><tr><th>状态</th><th>文件</th><th>大小</th><th>触发方式</th><th>开始时间</th><th>操作</th></tr></thead>
                    <tbody>
                      <tr v-for="r in backupRecords" :key="r.id">
                        <td><span :class="backupStatusClass(r.status)">{{ backupStatusText(r.status) }}</span></td>
                        <td class="col-file" :title="r.file_name">{{ r.file_name }}</td>
                        <td class="nowrap">{{ r.size_bytes ? fmtFileSize(r.size_bytes) : '—' }}</td>
                        <td>{{ r.triggered_by === 'scheduled' ? '定时' : '手动' }}</td>
                        <td class="nowrap">{{ r.started_at ? new Date(r.started_at * 1000).toLocaleString() : '—' }}</td>
                        <td class="col-ops">
                          <button v-if="r.status === 'completed'" class="btn btn-ghost btn-table" @click="guard(() => downloadBackup(r.id))">下载</button>
                          <button class="btn btn-ghost btn-table btn-table-danger" @click="deleteBackup(r.id)">删除</button>
                        </td>
                      </tr>
                    </tbody>
                  </table>
                </div>
                <div v-if="backupRecords.some(r => r.error)" class="backup-errors">
                  <div v-for="r in backupRecords.filter(r => r.error)" :key="r.id+'e'" class="hint backup-error">{{ r.file_name }}：{{ r.error }}</div>
                </div>
              </section>

              <!-- 模型映射 -->
              <section id="set-mappings" v-show="settingsSection === 'mappings'" class="card settings-card">
                <div class="settings-title"><h3>模型映射</h3><p>管理请求模型名到上游实际模型名的映射规则。自动学习的映射由前缀匹配产生，可手动覆盖或删除。</p></div>
                <div class="settings-actions mapping-actions">
                  <button class="btn btn-sm" @click="loadMappings"><Icon name="refresh" :size="14" />刷新</button>
                  <button class="btn btn-sm" @click="showNewMapping = true"><Icon name="plus" :size="14" />手动添加</button>
                </div>
                <div v-if="showNewMapping" class="mapping-form">
                  <FancySelect v-model="newMappingForm.upstream_id" :options="upstreamSelectOptions" />
                  <input v-model="newMappingForm.source_model" placeholder="请求模型名" />
                  <span class="mapping-arrow"><Icon name="chevron-right" :size="15" /></span>
                  <input v-model="newMappingForm.target_model" placeholder="上游实际模型名" />
                  <div class="mapping-form-actions"><button class="btn btn-sm" @click="saveNewMapping">保存</button><button class="btn btn-ghost btn-sm" @click="showNewMapping = false">取消</button></div>
                </div>
                <div v-if="!mappings.length && !mappingsLoading" class="hint">暂无模型映射规则</div>
                <div v-else-if="mappingsLoading" class="hint">加载中…</div>
                <div v-else class="mapping-table-wrap">
                  <table class="backup-table">
                    <thead><tr><th>上游</th><th>请求模型</th><th></th><th>实际模型</th><th>类型</th><th>过期</th><th>操作</th></tr></thead>
                    <tbody>
                      <tr v-for="m in mappings" :key="m.id">
                        <td>{{ upName(m.upstream_id) }}</td>
                        <td><code>{{ m.source_model }}</code></td>
                        <td class="mapping-table-arrow"><Icon name="chevron-right" :size="13" /></td>
                        <td><code>{{ m.target_model }}</code></td>
                        <td><span class="tag" :class="m.mapping_type === 'auto' ? 'on' : ''">{{ m.mapping_type === 'auto' ? '自动' : '手动' }}</span></td>
                        <td class="mapping-expiry">{{ m.expires_at ? new Date(m.expires_at).toLocaleDateString() : '永久' }}</td>
                        <td><button class="icon-btn danger" title="删除映射" @click="guard(async () => { await api.deleteModelMapping(m.id); await loadMappings() })"><Icon name="trash" :size="16" /></button></td>
                      </tr>
                    </tbody>
                  </table>
                </div>
              </section>
            </div>
          </div>
        </template>
        </template>
      </main>
    </div>

    <!-- 矩阵格子详情抽屉 -->
    <div class="drawer-mask" v-if="cellDrawer" @click.self="closeCell">
      <div class="drawer">
        <div class="dw-head">
          <span class="mx-dot lg" :class="dotClass(cellDrawer.snapshot.state)"></span>
          <div class="dw-id">
            <div class="dw-title">{{ cellDrawer.model }}</div>
            <div class="dw-sub">{{ cellDrawer.upstream_name }}<span v-if="cellDrawer.stream" class="tag on dw-stream-tag">流式</span></div>
          </div>
          <button class="icon-btn" @click="closeCell"><Icon name="x" :size="18" /></button>
        </div>
        <div class="dw-metrics">
          <div class="dw-m"><span>状态</span><b :class="dotClass(cellDrawer.snapshot.state)">{{ cellDrawer.enabled ? stateLabel(cellDrawer.snapshot.state) : '已停用' }}</b></div>
          <div class="dw-m"><span>成功率</span><b>{{ cellDrawer.snapshot.reqs ? (cellDrawer.snapshot.succ_rate * 100).toFixed(0) + '%' : '—' }}</b></div>
          <div class="dw-m"><span>平均延迟</span><b>{{ cellDrawer.snapshot.avg_ms || cellDrawer.snapshot.last_ms || 0 }}<small>ms</small></b></div>
          <div class="dw-m"><span>最后探测</span><b>{{ sinceText(cellDrawer.snapshot.last_ts) }}</b></div>
        </div>
        <Fence :trend="cellDrawer.snapshot.trend || []" unit="探测" />
        <div class="dw-foot">
          <div class="drawer-monitor-actions">
            <button class="btn btn-ghost" @click="toggleMonitor(cellDrawer)">{{ cellDrawer.enabled ? '停用' : '启用' }}</button>
            <button class="btn btn-ghost" @click="closeCell(); editMonitor(cellDrawer)"><Icon name="edit" :size="15" />编辑</button>
            <button class="btn" :disabled="probing.has(cellDrawer.id)" @click="guard(probeCell)">{{ probing.has(cellDrawer.id) ? '探测中…' : '立即探测' }}</button>
          </div>
        </div>
      </div>
    </div>

    <!-- 请求审计详情 -->
    <div class="drawer-mask" v-if="logDetail" @click.self="closeLogDetail">
      <aside class="drawer log-detail-drawer">
        <div class="dw-head log-detail-head">
          <span class="log-status" :class="requestOutcomeClass(logDetail)">{{ requestOutcomeText(logDetail) }} · {{ statusText(logDetail.status) }}</span>
          <div class="dw-id">
            <div class="dw-title">{{ requestShort(logDetail.request_id) }}</div>
            <div class="dw-sub">{{ fmtTimeFull(logDetail.created_at) }}</div>
          </div>
          <button class="icon-btn" title="复制请求 ID" @click="copyText(logDetail.request_id, 'request-id')"><Icon name="copy" :size="17" /></button>
          <button class="icon-btn" title="关闭" @click="closeLogDetail"><Icon name="x" :size="18" /></button>
        </div>

        <div v-if="logDetailLoading" class="log-detail-loading"><Icon name="loader" :size="18" />加载详情</div>
        <div v-else class="log-detail-scroll">
          <section class="log-detail-section">
            <h4>请求上下文</h4>
            <dl class="log-detail-grid">
              <div><dt>请求 ID</dt><dd class="mono">{{ logDetail.request_id }}</dd></div>
              <div><dt>分组 / 密钥</dt><dd>{{ logDetail.group_name || '—' }} / {{ logDetail.key_name || '—' }}</dd></div>
              <div><dt>客户端</dt><dd>{{ clientName(logDetail.user_agent) }}</dd></div>
              <div><dt>来源 IP</dt><dd class="mono">{{ logDetail.client_ip || '—' }}</dd></div>
              <div><dt>端点</dt><dd class="mono">{{ logDetail.endpoint || '—' }}</dd></div>
              <div><dt>模型</dt><dd>{{ logDetail.model || '—' }}</dd></div>
              <div><dt>请求模式</dt><dd>{{ logDetail.stream ? '流式' : '非流式' }}</dd></div>
              <div><dt>上游 Request ID</dt><dd class="mono">{{ logDetail.upstream_request_id || '—' }}</dd></div>
              <div><dt>开始</dt><dd>{{ fmtTimeFull(logDetail.created_at) }}</dd></div>
              <div><dt>完成</dt><dd>{{ fmtTimeFull(logDetail.completed_at) }}</dd></div>
              <div class="wide"><dt>User-Agent</dt><dd class="mono">{{ logDetail.user_agent || '—' }}</dd></div>
            </dl>
          </section>

          <section class="log-detail-section">
            <h4>性能与用量</h4>
            <div class="log-detail-metrics">
              <div><span>TTFT</span><b>{{ fmtMs(logDetail.ttft_ms) }}</b></div>
              <div><span>总耗时</span><b>{{ fmtMs(logDetail.duration_ms) }}</b></div>
              <div><span>请求体</span><b>{{ fmtBytes(logDetail.request_bytes) }}</b></div>
              <div><span>响应体</span><b>{{ fmtBytes(logDetail.response_bytes) }}</b></div>
              <div><span>输入 Token</span><b>{{ fmtNum(logDetail.input_tokens) }}</b></div>
              <div><span>输出 Token</span><b>{{ fmtNum(logDetail.output_tokens) }}</b></div>
              <div><span>缓存 Token / 命中率</span><b>{{ fmtNum(logDetail.cached_tokens) }} / {{ cacheRateText(logDetail) }}</b></div>
              <div><span>流结束</span><b>{{ streamStateText(logDetail) }}</b></div>
            </div>
          </section>

          <section class="log-detail-section">
            <h4>渠道尝试 <span>{{ logDetail.attempts?.length || 0 }}</span></h4>
            <div v-if="logDetail.attempts?.length" class="attempt-timeline">
              <article v-for="attempt in logDetail.attempts" :key="attempt.id" class="attempt-item">
                <div class="attempt-marker" :class="attempt.outcome === 'success' ? 'ok' : 'fail'">{{ attempt.attempt_no }}</div>
                <div class="attempt-content">
                  <header>
                    <div><b>{{ attempt.upstream_name || ('#' + attempt.upstream_id) }}</b><span>{{ selectionText(attempt.selection_reason) }} · 优先级 {{ attempt.priority || '—' }}</span></div>
                    <span class="log-status" :class="attempt.outcome === 'success' ? 'ok' : attempt.outcome === 'canceled' ? 'muted' : 'fail'">{{ outcomeText(attempt.outcome) }} · {{ statusText(attempt.status) }}</span>
                  </header>
                  <div v-if="attempt.mapped_model" class="attempt-mapping">模型映射 <code>{{ logDetail.model }} → {{ attempt.mapped_model }}</code></div>
                  <div class="attempt-facts">
                    <span>熔断 {{ attempt.health_before || '—' }} → {{ attempt.health_after || '—' }}</span>
                    <span>TTFT {{ fmtMs(attempt.ttft_ms) }}</span>
                    <span>耗时 {{ fmtMs(attempt.duration_ms) }}</span>
                    <span>响应 {{ fmtBytes(attempt.response_bytes) }}</span>
                    <span>Token {{ fmtNum(attempt.input_tokens) }} / {{ fmtNum(attempt.output_tokens) }}</span>
                    <span>{{ cacheSummary(attempt) }}</span>
                    <span v-if="attempt.stream">{{ attempt.stream_completed ? '流完成' : '未见完成事件' }} · {{ attempt.last_event || 'EOF' }}</span>
                  </div>
                  <div v-if="attempt.error_kind" class="attempt-error-meta"><b>{{ errorKindText(attempt.error_kind) }}</b><span>来源：{{ errorSourceText(attempt.error_source) }}</span></div>
                  <button v-if="attempt.error" class="log-error-block" title="复制错误" @click="copyText(attempt.error, 'attempt-' + attempt.id)">
                    <code>{{ attempt.error }}</code><Icon name="copy" :size="14" />
                  </button>
                  <div v-if="attempt.upstream_request_id" class="attempt-request-id">Upstream ID <code>{{ attempt.upstream_request_id }}</code></div>
                </div>
              </article>
            </div>
            <div v-else class="empty log-detail-empty">没有渠道尝试记录</div>
          </section>

          <section v-if="logDetail.error" class="log-detail-section">
            <h4>最终错误</h4>
            <div class="log-final-error-head"><b>{{ errorKindText(logDetail.error_kind) }}</b><span>来源：{{ errorSourceText(logDetail.error_source) }}</span></div>
            <button class="log-error-block final" title="复制错误" @click="copyText(logDetail.error, 'final-error')">
              <code>{{ logDetail.error }}</code><Icon name="copy" :size="14" />
            </button>
          </section>
        </div>
      </aside>
    </div>

    <!-- 新密钥明文展示（生成后一次性） -->
    <div class="mask" v-if="newKey" @click.self="copyKey">
      <div class="dialog">
        <h3>密钥已生成</h3>
        <p class="hint">请立即复制保存，关闭后将无法再次查看完整密钥。</p>
        <div class="key-reveal"><code>{{ newKey }}</code></div>
        <div class="dialog-foot"><button class="btn" @click="copyKey"><Icon name="check" :size="16" />复制并关闭</button></div>
      </div>
    </div>

    <div class="mask" v-if="groupTestState.show" @click.self="closeGroupTest">
      <div class="dialog group-test-dialog">
        <div class="group-test-heading">
          <h3>测试分组 · {{ groupTestState.groupName }}</h3>
          <span class="group-test-key"><small>测试密钥</small><b>{{ groupTestState.keyName }}</b></span>
        </div>
        <section class="group-test-card">
          <div class="group-test-card-head"><span>请求配置</span><small>{{ groupTestState.models.length }} 个可用模型</small></div>
          <div class="group-test-controls">
            <label><span>客户端协议</span><FancySelect v-model="groupTestState.protocol" :options="groupTestProtocolOptions" :disabled="groupTestState.running" /></label>
            <label><span>模型</span><FancySelect v-model="groupTestState.model" :options="groupTestModelOptions" :disabled="groupTestState.modelsLoading || groupTestState.running" /></label>
          </div>
          <p v-if="groupTestState.modelsError" class="test-err">{{ groupTestState.modelsError }}</p>
          <button class="btn group-test-run" :disabled="groupTestState.running || !groupTestState.model || !groupTestState.keyId" @click="runGroupTest"><Icon :name="groupTestState.running ? 'loader' : 'play'" :size="16" />{{ groupTestState.running ? '测试中…' : '开始测试' }}</button>
        </section>
        <section v-if="groupTestState.running || groupTestState.output || groupTestState.status || groupTestState.error" class="group-test-card">
          <div class="group-test-card-head">
            <span>模型响应</span>
            <div v-if="groupTestState.status || groupTestState.error" class="test-status" :class="groupTestState.error ? 'fail' : 'ok'"><Icon :name="groupTestState.error ? 'x' : 'check'" :size="16" /><span>{{ groupTestState.error ? '测试失败' : '测试通过' }}</span><small v-if="groupTestState.status">HTTP {{ groupTestState.status }}</small></div>
          </div>
          <div v-if="groupTestState.running || groupTestState.output" class="test-output group-test-output"><span>{{ groupTestState.output }}</span><span v-if="groupTestState.running" class="cursor">▋</span></div>
          <p v-if="groupTestState.error" class="test-err">{{ groupTestState.error }}</p>
        </section>
        <section v-if="groupTestState.result || groupTestState.requestId" class="group-test-card">
          <div class="group-test-card-head"><span>路由结果</span></div>
          <div v-if="groupTestState.result" class="group-test-result">
            <div class="group-test-metrics"><div><span>最终渠道</span><b>{{ groupTestState.result.final_upstream_name || '—' }}</b></div><div><span>TTFT</span><b>{{ fmtMs(groupTestState.result.ttft_ms) }}</b></div><div><span>总耗时</span><b>{{ fmtMs(groupTestState.result.duration_ms) }}</b></div></div>
            <div v-if="groupTestState.result.attempts?.length" class="group-test-route"><span v-for="attempt in groupTestState.result.attempts" :key="attempt.id" :class="attempt.outcome === 'success' ? 'ok' : 'fail'">{{ attempt.attempt_no }} · {{ attempt.upstream_name || ('#' + attempt.upstream_id) }} · {{ statusText(attempt.status) }}</span></div>
          </div>
          <div v-if="groupTestState.requestId" class="group-test-request-id"><span>请求 ID</span><code>{{ groupTestState.requestId }}</code><button class="icon-btn" title="复制请求 ID" @click="copyText(groupTestState.requestId, 'group-test-request')"><Icon :name="copied === 'group-test-request' ? 'check' : 'copy'" :size="14" /></button></div>
        </section>
        <div class="dialog-foot"><button class="btn btn-ghost" @click="closeGroupTest">关闭</button></div>
      </div>
    </div>

    <!-- 真实对话测试 -->
    <div class="mask" v-if="testState.show" @click.self="closeTest">
      <div class="dialog">
        <h3>测试上游 · {{ testState.name }}</h3>
        <p class="hint test-dialog-hint">发一条真实对话请求，验证能否端到端跑通并查看回复。</p>

        <div class="test-row">
          <FancySelect v-model="testState.model" :options="testModelOptions" :disabled="testState.modelsLoading || testState.running" />
          <button class="btn" :disabled="testState.running || !testState.model" @click="runTest">
            <Icon :name="testState.running ? 'loader' : 'play'" :size="16" />{{ testState.running ? '测试中…' : '开始测试' }}
          </button>
        </div>
        <p v-if="testState.modelsErr" class="test-err test-model-error">列模型失败：{{ testState.modelsErr }}（仍可手动测试默认模型）</p>

        <div v-if="testState.running || testState.output || testState.status" class="test-output">
          <span v-if="testState.output">{{ testState.output }}</span>
          <span v-if="testState.running" class="cursor">▋</span>
          <span v-else-if="!testState.output && testState.status?.ok" class="hint">（上游无文本输出，但连接成功）</span>
        </div>

        <div v-if="testState.status" class="test-status" :class="testState.status.ok ? 'ok' : 'fail'">
          <Icon :name="testState.status.ok ? 'check' : 'x'" :size="16" />
          <span>{{ testState.status.ok ? '测试通过' : '测试失败' }}</span>
          <small v-if="testState.status.latency_ms != null">{{ testState.status.latency_ms }}ms</small>
          <small v-if="testState.status.code">HTTP {{ testState.status.code }}</small>
        </div>
        <p v-if="testState.status && !testState.status.ok && testState.status.error" class="test-err">{{ testState.status.error }}</p>

        <div class="dialog-foot"><button class="btn btn-ghost" @click="closeTest">关闭</button></div>
      </div>
    </div>

    <!-- 确认弹窗 -->
    <div class="mask" v-if="confirmState.show" @click.self="confirmState.show = false">
      <div class="dialog dialog-sm">
        <h3>确认操作</h3>
        <p class="confirm-msg">{{ confirmState.msg }}</p>
        <div class="dialog-foot">
          <button class="btn btn-ghost" @click="confirmState.show = false">取消</button>
          <button class="btn btn-danger" @click="confirmOk"><Icon name="trash" :size="16" />确认删除</button>
        </div>
      </div>
    </div>

    <!-- 表单弹窗 -->
    <div class="mask" v-if="dlg.type" @click.self="closeDlg({ preserveDraft: true })">
      <div class="dialog" :class="[dlg.type === 'tags' ? 'tag-dialog' : (dlg.type === 'upstream' ? 'upstream-dialog' : ''), { 'dialog-saving': dialogSaving }]">
        <template v-if="dlg.type === 'group'">
          <h3>{{ dlg.form.id ? '编辑分组' : '新建分组' }}</h3>
          <div class="field"><label>名称</label><input v-model="dlg.form.name" placeholder="如 Claude 池" /></div>
          <div class="field"><label>描述</label><input v-model="dlg.form.description" placeholder="可选" /></div>
          <div class="field"><label>最大计费倍率</label><input v-model="dlg.form.max_multiplier" type="number" min="0.0001" step="0.01" placeholder="不限" /></div>
          <div class="dialog-foot"><button class="btn btn-ghost" :disabled="dialogSaving" @click="closeDlg">取消</button><button class="btn" :disabled="dialogSaving" @click="saveGroup"><Icon :name="dialogSaving ? 'loader' : 'check'" :class="{ spin: dialogSaving }" :size="16" />{{ dialogSaving ? '保存中…' : '保存' }}</button></div>
        </template>

        <template v-else-if="dlg.type === 'keygen'">
          <h3>生成接入密钥</h3>
          <div class="field"><label>名称</label><input v-model="dlg.form.name" placeholder="备注用，如「客户端A」，可留空" @keyup.enter="saveKey" /></div>
          <p class="hint hint-flush">密钥由系统生成，绑定当前分组，仅在生成后明文显示一次。</p>
          <div class="dialog-foot"><button class="btn btn-ghost" :disabled="dialogSaving" @click="closeDlg">取消</button><button class="btn" :disabled="dialogSaving" @click="saveKey"><Icon :name="dialogSaving ? 'loader' : 'plus'" :class="{ spin: dialogSaving }" :size="16" />{{ dialogSaving ? '生成中…' : '生成' }}</button></div>
        </template>

        <template v-else-if="dlg.type === 'upstream'">
          <h3>{{ dlg.form.id ? '编辑上游' : '新增上游' }}</h3>
          <section v-if="!dlg.form.id" class="upstream-quick-import">
            <div class="upstream-quick-import-head">
              <div>
                <strong><Icon name="copy" :size="14" />快捷导入</strong>
                <small>粘贴 JSON，或使用「名称 | 地址 | API Key」</small>
              </div>
              <button class="btn btn-ghost btn-sm" type="button" @click="importUpstreamConfig"><Icon name="check" :size="14" />填充表单</button>
            </div>
            <textarea v-model="upstreamImportText" rows="2" placeholder="例如：OpenRouter | https://openrouter.ai/api | sk-or-..." @keydown.ctrl.enter="importUpstreamConfig"></textarea>
            <p v-if="upstreamImportMessage" class="upstream-import-message" :class="{ error: upstreamImportError }">{{ upstreamImportMessage }}</p>
          </section>
          <div v-if="!dlg.form.id" class="field upstream-vendor-field">
            <label>厂商预设</label>
            <FancySelect v-model="upstreamVendor" :options="vendorPresets.map(p => ({ value: p.value, label: p.label }))" @change="applyVendorPreset" />
          </div>
          <div class="upstream-form-grid">
            <div class="field"><label>名称</label><input v-model="dlg.form.name" /></div>
            <div class="field"><label>base_url <small v-if="upstreamBaseURLDirty" class="field-note">已手动填写</small></label><input v-model="dlg.form.base_url" placeholder="https://..." @input="markUpstreamBaseURLDirty" /></div>
            <div class="field"><label>协议</label><FancySelect v-model="dlg.form.protocol" :options="protocolOptions" /></div>
            <div class="field"><label>api_key</label><input v-model="dlg.form.api_key" :placeholder="dlg.form.id ? '留空则不修改' : 'sk-...'" /></div>
            <div class="field"><label>代理</label><input v-model="dlg.form.proxy" placeholder="留空=直连/环境变量；如 http://127.0.0.1:7890 或 socks5://127.0.0.1:1080" /></div>
            <div class="field"><label>计费平台</label><FancySelect v-model="dlg.form.billing_type" :options="billingTypeOptions" /></div>
            <div class="field"><label>主标签</label><FancySelect v-model="dlg.form.primary_tag_id" :options="primaryTagOptions" /></div>
            <div class="field upstream-form-tags">
              <label class="field-label-row"><span>普通标签</span><small>{{ upstreamFormSelectedTags.length }} 个已选</small></label>
              <FancySelect v-model="dlg.form.tag_ids" variant="tag" multiple searchable :options="upstreamFormTagOptions" />
            </div>
            <div class="field"><label>储值倍率</label><input v-model="dlg.form.credit_ratio" type="number" step="any" min="0" placeholder="充1得N积分时填 N；默认 1" /></div>
            <div class="field upstream-form-groups" v-if="!dlg.form.id">
              <label class="field-label-row"><span>加入分组</span><small>{{ upstreamFormGroupIDs.length }} 个已选</small></label>
              <div class="tag-picker-panel">
                <div v-if="upstreamFormGroupIDs.length" class="tag-picker-selected">
                  <button v-for="gid in upstreamFormGroupIDs" :key="gid" type="button" class="manage-tag selected" :title="'移除该分组'" @click="toggleUpstreamFormGroup(gid)">
                    {{ groups.find(g => g.id === gid)?.name || gid }}<Icon name="x" :size="12" />
                  </button>
                </div>
                <div class="tag-picker-search"><Icon name="search" :size="15" /><input v-model="upstreamFormGroupSearch" placeholder="搜索分组（可多选，可留空）" /></div>
                <div class="tag-picker-options">
                  <button v-for="g in upstreamFormGroupChoices" :key="g.id" type="button" class="tag-picker-option" @click="toggleUpstreamFormGroup(g.id)">
                    <span>{{ g.name }}</span><Icon name="plus" :size="14" />
                  </button>
                  <span v-if="!groups.length" class="tag-picker-empty">还没有分组，可稍后在分组页添加成员</span>
                  <span v-else-if="!upstreamFormGroupChoices.length" class="tag-picker-empty">没有匹配的分组</span>
                </div>
              </div>
            </div>
          </div>
          <label class="check"><input type="checkbox" v-model="dlg.form.enabled" /> 启用</label>
          <div class="dialog-foot"><button class="btn btn-ghost" :disabled="dialogSaving" @click="closeDlg">取消</button><button class="btn" :disabled="dialogSaving" @click="saveUpstream"><Icon :name="dialogSaving ? 'loader' : 'check'" :class="{ spin: dialogSaving }" :size="16" />{{ dialogSaving ? '保存中…' : '保存' }}</button></div>
        </template>

        <template v-else-if="dlg.type === 'tags'">
          <h3>标签管理</h3>
          <div class="tag-manager">
            <div class="tag-manager-browser">
              <div class="tag-manager-search"><Icon name="search" :size="15" /><input v-model="tagManagerSearch" placeholder="搜索标签" /></div>
              <div class="tag-manager-list">
                <button v-for="tag in filteredManagedTags" :key="tag.id" class="tag-manager-item" :class="{ active: tagDraft.id === tag.id }" @click="editTagDraft(tag)">
                  <span class="tag-color-dot" :class="`tag-${tag.color}`"></span><span>{{ tag.name }}</span>
                  <span class="tag-count">{{ tag.count || '' }}</span>
                </button>
                <div v-if="!filteredManagedTags.length" class="tag-manager-empty">没有匹配的标签</div>
              </div>
            </div>
            <div class="tag-manager-form">
              <div class="field"><label>名称</label><input v-model="tagDraft.name" maxlength="40" placeholder="如 RelayCat、低价备用" @keyup.enter="saveTag" /></div>
              <div class="field"><label>颜色</label><div class="tag-color-picker"><button v-for="color in tagColorChoices" :key="color.value" type="button" :class="[`tag-${color.value}`, { active: tagDraft.color === color.value }]" :title="color.label" @click="tagDraft.color = color.value"><span></span></button></div></div>
              <div class="tag-manager-actions">
                <button v-if="tagDraft.id" class="btn btn-danger btn-sm" @click="delTag(tagDraft)"><Icon name="trash" :size="14" />删除</button>
                <span class="hspacer"></span>
                <button v-if="tagDraft.id" class="btn btn-ghost btn-sm" @click="resetTagDraft">新建</button>
                <button class="btn btn-sm" :disabled="dialogSaving || !tagDraft.name.trim()" @click="saveTag"><Icon :name="dialogSaving ? 'loader' : (tagDraft.id ? 'check' : 'plus')" :class="{ spin: dialogSaving }" :size="14" />{{ dialogSaving ? '保存中…' : (tagDraft.id ? '保存' : '创建') }}</button>
              </div>
            </div>
          </div>
          <div class="dialog-foot"><button class="btn btn-ghost" :disabled="dialogSaving" @click="closeDlg">关闭</button></div>
        </template>

        <template v-else-if="dlg.type === 'monitor'">
          <h3>{{ dlg.form.id ? '编辑监控项' : '新增监控项' }}</h3>
          <div class="field">
            <label>渠道</label>
            <FancySelect v-model="dlg.form.upstream_id" :options="upstreamSelectOptions" @change="loadMonModels(Number(dlg.form.upstream_id))" />
          </div>
          <div class="field">
            <label>模型</label>
            <input v-model="dlg.form.model" list="mon-models" placeholder="如 gpt-4o，可从下拉选或手填" />
            <datalist id="mon-models"><option v-for="m in monModels" :key="m" :value="m" /></datalist>
          </div>
          <div class="field"><label>备注名</label><input v-model="dlg.form.name" placeholder="可选，留空则显示「渠道 · 模型」" /></div>
          <div class="field-row">
            <div class="field"><label>探测间隔(秒)</label><input v-model="dlg.form.interval_sec" type="number" min="0" placeholder="留空/0 用默认 5 分钟" /></div>
            <div class="field"><label>max_tokens</label><input v-model="dlg.form.max_tokens" type="number" min="0" placeholder="留空/0 用默认 1" /></div>
          </div>
          <div class="field"><label>探测路径</label><input v-model="dlg.form.path" placeholder="留空用默认 /v1/chat/completions；Gemini 填 /v1beta/models/{model}:generateContent" /></div>
          <div class="field"><label>探测消息</label><input v-model="dlg.form.probe_text" placeholder="留空用默认「hi」" /></div>
          <label class="check"><input type="checkbox" v-model="dlg.form.stream" /> 流式探测（请求体加 stream:true）</label>
          <label class="check"><input type="checkbox" v-model="dlg.form.enabled" /> 启用</label>
          <div class="dialog-foot"><button class="btn btn-ghost" :disabled="dialogSaving" @click="closeDlg">取消</button><button class="btn" :disabled="dialogSaving" @click="saveMonitor"><Icon :name="dialogSaving ? 'loader' : 'check'" :class="{ spin: dialogSaving }" :size="16" />{{ dialogSaving ? '保存中…' : '保存' }}</button></div>
        </template>

        <template v-else-if="dlg.type === 'upstream-monitors'">
          <h3>为「{{ dlg.form.upstream_name }}」批量建监控</h3>
          <p class="hint">勾选要探活的模型，下方探测参数对所有选中项共享。已监控的模型自动跳过。监控只是探活子集，不影响下游 /v1/models 能看到的全部模型。</p>
          <div v-if="batchMon.loading" class="hint">正在拉取模型列表…</div>
          <div v-else-if="batchMon.error" class="err-banner">拉取模型失败：{{ batchMon.error }}</div>
          <div v-else-if="!batchMon.models.length" class="hint">该上游 /v1/models 没有返回任何模型。</div>
          <template v-else>
            <div class="batch-tools">
              <label class="check"><input type="checkbox" :checked="batchPickedCount === batchSelectable.length && batchSelectable.length > 0" @change="batchToggleAll" /> 全选可用（{{ batchPickedCount }}/{{ batchSelectable.length }}）</label>
            </div>
            <div class="batch-list">
              <label v-for="m in batchMon.models" :key="m" class="batch-item" :class="{ done: batchMon.monitored[m] }">
                <input type="checkbox" :disabled="batchMon.monitored[m]" v-model="batchMon.picked[m]" />
                <span class="bm-name">{{ m }}</span>
                <span v-if="batchMon.monitored[m]" class="tag off">已监控</span>
              </label>
            </div>
            <div class="field-row">
              <div class="field"><label>探测间隔(秒)</label><input v-model="dlg.form.interval_sec" type="number" min="0" placeholder="留空/0 用默认 5 分钟" /></div>
              <div class="field"><label>max_tokens</label><input v-model="dlg.form.max_tokens" type="number" min="0" placeholder="留空/0 用默认 1" /></div>
            </div>
            <div class="field"><label>探测路径</label><input v-model="dlg.form.path" placeholder="留空用默认 /v1/chat/completions；Gemini 填 /v1beta/models/{model}:generateContent" /></div>
            <div class="field"><label>探测消息</label><input v-model="dlg.form.probe_text" placeholder="留空用默认「hi」" /></div>
            <label class="check"><input type="checkbox" v-model="dlg.form.stream" /> 流式探测（请求体加 stream:true）</label>
            <label class="check"><input type="checkbox" v-model="dlg.form.enabled" /> 启用</label>
          </template>
          <div class="dialog-foot">
            <button class="btn btn-ghost" :disabled="dialogSaving" @click="closeDlg">取消</button>
            <button class="btn" :disabled="dialogSaving || batchPickedCount === 0" @click="saveBatchMonitors"><Icon :name="dialogSaving ? 'loader' : 'check'" :class="{ spin: dialogSaving }" :size="16" />{{ dialogSaving ? '创建中…' : `为选中 ${batchPickedCount} 个模型创建监控` }}</button>
          </div>
        </template>

        <template v-else-if="dlg.type === 'member'">
          <h3>{{ dlg.form.locked ? '调整组内策略' : '添加上游到分组' }}</h3>
          <div class="field" v-if="!dlg.form.locked">
            <label>选择上游（{{ addable.length }} 个可选）</label>
            <UpstreamPicker v-model="dlg.form.upstream_id" :upstreams="addable" />
          </div>
          <div class="field" v-else><label>上游</label><input :value="upName(dlg.form.upstream_id)" disabled /></div>
          <div class="field-row">
            <div class="field field-grow"><label>组内优先级</label><input type="number" v-model.number="dlg.form.priority" /></div>
            <div class="field field-grow"><label>权重</label><input type="number" v-model.number="dlg.form.weight" /></div>
          </div>
          <p class="hint hint-flush">优先级越小越先用；同优先级按权重分流。</p>
          <div class="dialog-foot"><button class="btn btn-ghost" :disabled="dialogSaving" @click="closeDlg">取消</button><button class="btn" :disabled="dialogSaving || !dlg.form.upstream_id" @click="saveMember"><Icon :name="dialogSaving ? 'loader' : 'check'" :class="{ spin: dialogSaving }" :size="16" />{{ dialogSaving ? '保存中…' : '保存' }}</button></div>
        </template>
      </div>
    </div>
  </div>
</template>
