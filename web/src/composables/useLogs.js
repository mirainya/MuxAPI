import { computed, ref } from 'vue'
import { api } from '../api.js'

// 管理请求记录的筛选、分页、详情和自动刷新生命周期。
export function useLogs({ page, guard }) {
  // --- 请求记录（标准分页 + 服务端筛选）---
  const logs = ref([])
  const logPageSize = ref(20)
  const logCurrentPage = ref(1)
  const logLoading = ref(false)
  const logDetail = ref(null)
  const logDetailLoading = ref(false)
  const logStats = ref({})
  const logCacheStats = ref([])
  const logCacheExpanded = ref(false)
  const logSearch = ref('')
  const logFTime = ref('24h')
  const logFGroup = ref('')
  const logFModel = ref('')
  const logFStatus = ref('')
  const logFUpstream = ref('')
  const logFKey = ref('')
  const logFEndpoint = ref('')
  const logFErrorKind = ref('')
  const logFStream = ref('')
  const logFSlow = ref('')
  const logFRetried = ref(false)
  const logAutoRefresh = ref(false)
  const logMoreFilters = ref(false)
  const logModelOpts = ref([])
  const logGroupOpts = ref([])
  const logKeyOpts = ref([])
  const logEndpointOpts = ref([])
  const logErrorKindOpts = ref([])
  const logUpstreamOpts = ref([])
  const logPageSizeOptions = [
    { value: 20, label: '20 条' },
    { value: 50, label: '50 条' },
    { value: 100, label: '100 条' },
  ]
  const logTotalPages = computed(() => Math.max(1, Math.ceil((Number(logStats.value.total) || 0) / Number(logPageSize.value))))
  const logPageItems = computed(() => {
    const total = logTotalPages.value
    const current = Math.min(logCurrentPage.value, total)
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
  })
  const logGroupSelectOptions = computed(() => [{ value: '', label: '全部分组' }, ...logGroupOpts.value.map(g => ({ value: g, label: g }))])
  const logModelSelectOptions = computed(() => [{ value: '', label: '全部模型' }, ...logModelOpts.value.map(m => ({ value: m, label: m }))])
  const logKeySelectOptions = computed(() => [{ value: '', label: '全部密钥' }, ...logKeyOpts.value.map(k => ({ value: k, label: k }))])
  const logEndpointSelectOptions = computed(() => [{ value: '', label: '全部端点' }, ...logEndpointOpts.value.map(e => ({ value: e, label: fmtEndpoint(e) }))])
  const logErrorSelectOptions = computed(() => [{ value: '', label: '全部错误' }, ...logErrorKindOpts.value.map(e => ({ value: e, label: errorKindText(e) }))])
  const logUpstreamSelectOptions = computed(() => [{ value: '', label: '全部渠道' }, ...logUpstreamOpts.value.map(u => ({ value: u.id, label: u.name }))])
  const logTimeOptions = [
    { value: '1h', label: '最近 1 小时' },
    { value: '24h', label: '最近 24 小时' },
    { value: '7d', label: '最近 7 天' },
    { value: 'all', label: '全部记录' },
  ]
  const logStatusOptions = [
    { value: '', label: '全部状态' },
    { value: 'direct_success', label: '直接成功' },
    { value: 'failover_success', label: '切换后成功' },
    { value: 'failed', label: '失败' },
    { value: 'partial', label: '流中断' },
    { value: 'canceled', label: '客户端取消' },
    { value: 'client_error', label: '请求错误' },
  ]
  const logStreamOptions = [
    { value: '', label: '全部模式' },
    { value: 'stream', label: '流式请求' },
    { value: 'nonstream', label: '非流式请求' },
  ]
  const logSlowOptions = [
    { value: '', label: '全部耗时' },
    { value: 5000, label: '耗时 ≥ 5 秒' },
    { value: 30000, label: '耗时 ≥ 30 秒' },
    { value: 120000, label: '耗时 ≥ 2 分钟' },
  ]

  function logFilterParams() {
    const now = Math.floor(Date.now() / 1000)
    const since = ({ '1h': now - 3600, '24h': now - 86400, '7d': now - 7 * 86400 })[logFTime.value] || ''
    return {
      q: logSearch.value.trim(), since, group: logFGroup.value, model: logFModel.value,
      status: logFStatus.value, upstream_id: logFUpstream.value, key: logFKey.value,
      endpoint: logFEndpoint.value, error_kind: logFErrorKind.value, stream: logFStream.value,
      slow_ms: logFSlow.value, retried: logFRetried.value,
    }
  }

  let logLoadEpoch = 0
  // epoch 保证快速翻页或筛选时，较慢的旧请求不能覆盖最新结果。
  async function fetchLogPage(targetPage, refreshStats) {
    const epoch = ++logLoadEpoch
    logLoading.value = true
    try {
      const pageSize = Number(logPageSize.value)
      const params = { ...logFilterParams(), offset: (targetPage - 1) * pageSize, limit: pageSize }
      const [page, stats, cacheStats] = refreshStats
        ? await Promise.all([api.logs(params), api.logStats(params), api.logCacheStats(params)])
        : [await api.logs(params), null, null]
      if (epoch !== logLoadEpoch) return false
      const rows = (page && page.entries) || []
      logs.value = rows
      logCurrentPage.value = targetPage
      if (stats) logStats.value = stats
      if (cacheStats) logCacheStats.value = cacheStats
      return true
    } finally {
      if (epoch === logLoadEpoch) logLoading.value = false
    }
  }
  async function loadLogs(resetPagination = false) {
    return fetchLogPage(resetPagination ? 1 : logCurrentPage.value, true)
  }
  async function goLogPage(targetPage) {
    if (logLoading.value) return
    const normalized = Math.max(1, Math.min(Number(targetPage) || 1, logTotalPages.value))
    if (normalized === logCurrentPage.value) return
    await fetchLogPage(normalized, false)
  }
  function onLogPageSizeChange() { guard(() => loadLogs(true)) }
  async function loadLogOptions() {
    const o = await api.logOptions()
    logModelOpts.value = (o && o.models) || []
    logGroupOpts.value = (o && o.groups) || []
    logKeyOpts.value = (o && o.keys) || []
    logEndpointOpts.value = (o && o.endpoints) || []
    logErrorKindOpts.value = (o && o.error_kinds) || []
    logUpstreamOpts.value = (o && o.upstreams) || []
  }
  function onLogFilterChange() { guard(() => loadLogs(true)) }
  function resetLogFilters() {
    logSearch.value = ''; logFTime.value = '24h'; logFGroup.value = ''; logFModel.value = ''
    logFStatus.value = ''; logFUpstream.value = ''; logFKey.value = ''; logFEndpoint.value = ''
    logFErrorKind.value = ''; logFStream.value = ''; logFSlow.value = ''; logFRetried.value = false
    logMoreFilters.value = false
    onLogFilterChange()
  }
  let logDetailEpoch = 0
  async function openLogDetail(entry) {
    const epoch = ++logDetailEpoch
    logDetailLoading.value = true
    logDetail.value = entry
    try {
      const detail = await api.logDetail(entry.id)
      if (epoch === logDetailEpoch) logDetail.value = detail
    } finally {
      if (epoch === logDetailEpoch) logDetailLoading.value = false
    }
  }
  function closeLogDetail() { logDetailEpoch++; logDetail.value = null; logDetailLoading.value = false }

  let logTimer = null
  function startLogPoll() {
    stopLogPoll()
    if (!logAutoRefresh.value) return
    logTimer = setInterval(() => {
      if (page.value === 'logs' && logCurrentPage.value === 1 && !logLoading.value && !logDetail.value) loadLogs(false).catch(() => {})
    }, 10000)
  }
  function stopLogPoll() { if (logTimer) { clearInterval(logTimer); logTimer = null } }
  function toggleLogAutoRefresh() { startLogPoll() }

  const logActiveFilters = computed(() => [logSearch.value, logFGroup.value, logFModel.value, logFStatus.value,
    logFUpstream.value, logFKey.value, logFEndpoint.value, logFErrorKind.value, logFStream.value,
    logFSlow.value, logFRetried.value].filter(Boolean).length)
  const logAdvancedFilters = computed(() => [logFGroup.value, logFModel.value, logFKey.value, logFEndpoint.value,
    logFErrorKind.value, logFStream.value, logFSlow.value, logFRetried.value].filter(Boolean).length)
  const logCacheSummary = computed(() => {
    const valid = logCacheStats.value.filter(item => Number(item.input_tokens) > 0)
    if (!valid.length) return { cache_input_tokens: 0, cached_tokens: 0, cache_rate: 0, lowest: null, highest: null }
    const cacheInputTokens = valid.reduce((sum, item) => sum + (Number(item.input_tokens) || 0), 0)
    const cachedTokens = valid.reduce((sum, item) => sum + (Number(item.cached_tokens) || 0), 0)
    const byRate = valid.slice().sort((a, b) => Number(a.cache_rate) - Number(b.cache_rate))
    return {
      cache_input_tokens: cacheInputTokens,
      cached_tokens: cachedTokens,
      cache_rate: cacheInputTokens ? cachedTokens / cacheInputTokens : 0,
      lowest: byRate[0],
      highest: byRate[byRate.length - 1],
    }
  })
  const clientName = userAgent => {
    const value = String(userAgent || '').trim()
    if (!value) return '未知客户端'
    const clients = [
      [/claude(?:-code|-cli)?\/([^\s]+)/i, 'Claude CLI'],
      [/codex(?:_cli_rs|-cli)?\/([^\s]+)/i, 'Codex CLI'],
      [/openai-(?:node|python)\/([^\s]+)/i, 'OpenAI SDK'],
      [/anthropic(?:-typescript|-python)?\/([^\s]+)/i, 'Anthropic SDK'],
      [/curl\/([^\s]+)/i, 'curl'],
      [/postmanruntime\/([^\s]+)/i, 'Postman'],
      [/go-http-client\/([^\s]+)/i, 'Go HTTP'],
    ]
    for (const [pattern, name] of clients) {
      const match = value.match(pattern)
      if (match) return `${name} ${match[1]}`
    }
    const product = value.split(/\s+/, 1)[0]
    return product.length > 36 ? product.slice(0, 35) + '…' : product
  }
  const requestShort = id => id ? id.slice(0, 8) : '—'
  const fmtMs = ms => {
    const value = Number(ms) || 0
    if (!value) return '—'
    return value >= 1000 ? (value / 1000).toFixed(1) + 's' : value + 'ms'
  }
  const fmtBytes = bytes => {
    const value = Number(bytes) || 0
    if (!value) return '—'
    if (value >= 1048576) return (value / 1048576).toFixed(1) + ' MB'
    if (value >= 1024) return (value / 1024).toFixed(1) + ' KB'
    return value + ' B'
  }
  const fmtNum = value => new Intl.NumberFormat('zh-CN').format(Number(value) || 0)
  const cacheInputTokens = entry => Number(entry?.cache_input_tokens ?? entry?.input_tokens) || 0
  const cacheRateText = entry => cacheInputTokens(entry) > 0
    ? ((Number(entry.cache_rate) || 0) * 100).toFixed(1) + '%'
    : '—'
  const cacheSummary = entry => cacheInputTokens(entry) > 0
    ? `缓存 ${cacheRateText(entry)} · ${fmtNum(entry.cached_tokens)}`
    : '缓存 —'
  const cacheRateWidth = entry => Math.min(100, Math.max(0, (Number(entry?.cache_rate) || 0) * 100)).toFixed(1) + '%'
  // cache_input_tokens is normalized by the server for both legacy inclusive
  // rows and new uncached-input rows. It represents total prompt context,
  // not the amount charged at the ordinary input rate.
  const billedInputTokens = entry => Number(entry?.cache_input_tokens) ||
    (Number(entry?.input_tokens) || 0)
  // Stored by the server from the routing estimate and provider usage. A
  // missing/zero value means this request predates routing audit data.
  const tokenInflation = entry => {
    const value = Number(entry?.token_inflation)
    return Number.isFinite(value) && value > 0 ? value : 0
  }
  const tokenInflationText = entry => {
    const value = tokenInflation(entry)
    return value ? `${value.toFixed(2)}x` : '—'
  }
  const tokenInflationClass = entry => {
    const value = tokenInflation(entry)
    return value >= 1.2 ? 'high' : value > 1.05 ? 'medium' : value ? 'normal' : ''
  }
  const outcomeText = outcome => ({
    success: '成功', failed: '失败', canceled: '已取消', partial: '流中断',
    client_error: '请求错误', unsupported: '不支持', unavailable: '无可用渠道',
  }[outcome] || outcome || '未知')
  const requestOutcomeText = entry => entry.outcome === 'success' && entry.attempt_count > 1 ? '切换后成功' : outcomeText(entry.outcome)
  const requestOutcomeClass = entry => entry.outcome === 'success' ? (entry.attempt_count > 1 ? 'warn' : 'ok')
    : entry.outcome === 'canceled' || entry.outcome === 'client_error' ? 'muted' : 'fail'
  const errorKindText = kind => ({
    request_build: '请求构建失败', upstream_network: '上游网络失败', first_response_timeout: '首字节超时',
    upstream_read: '上游读取失败', model_unsupported: '模型不支持', client_request: '客户端请求错误',
    upstream_http: '上游 HTTP 错误', downstream_write: '下游写入失败', client_canceled: '客户端取消',
    empty_response: '上游空响应', error_payload: '成功状态错误体', upstream_disconnect: '上游断流',
    no_upstream: '无可用渠道', upstream_error: '上游失败', auth: '接入鉴权失败',
    request_too_large: '请求体过大', request_read: '请求体读取失败',
  }[kind] || kind || '—')
  const errorSourceText = source => ({ upstream: '上游', client: '客户端', gateway: 'MuxAPI' }[source] || source || '—')
  const selectionText = reason => {
    const map = { initial: '首次选择', failover: '故障切换', recovery_trial: '恢复验证' }
    if (map[reason]) return map[reason]
    if (!reason) return '—'
    if (reason.includes('provider cache')) return '缓存路由'
    if (reason.includes('ordinary input')) return '直连最优'
    if (reason.includes('exploration')) return '探索采样'
    return reason.split(':')[0] || reason
  }
  const streamStateText = entry => {
    if (!entry.stream) return '非流式'
    if (entry.stream_completed) return entry.last_event ? `完整 · ${entry.last_event}` : '完整结束'
    return entry.last_event ? `EOF · ${entry.last_event}` : '未见完成事件'
  }
  // 绝对时间 MM-DD HH:MM:SS（请求记录看具体时刻，不用相对）
  const fmtTime = ts => {
    if (!ts) return '—'
    const d = new Date(ts * 1000), p = n => String(n).padStart(2, '0')
    return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
  }
  // 完整时间(含年份)，供请求记录 hover 查看
  const fmtTimeFull = ts => {
    if (!ts) return '—'
    const d = new Date(ts * 1000), p = n => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
  }
  // 状态展示文案：0=网络失败，否则状态码
  const statusText = s => s === 0 ? '网络失败' : s === 499 ? '客户端取消' : String(s)
  // 端点路径简化展示：去掉 /v1/ 前缀，空则 —
  const fmtEndpoint = p => {
    if (!p) return '—'
    const gemini = p.match(/^\/v1(?:alpha|beta)?\/models\/([^:]+):(streamGenerateContent|generateContent)$/)
    if (gemini) {
      let model = gemini[1]
      try { model = decodeURIComponent(model) } catch {}
      return `gemini/${model}${gemini[2] === 'streamGenerateContent' ? ' · stream' : ''}`
    }
    return p.replace(/^\/v1\//, '')
  }
  return {
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
    billedInputTokens, tokenInflation, tokenInflationText, tokenInflationClass,
  }
}
