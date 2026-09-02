// 轻量 admin API 封装：所有请求经 vite proxy /admin → 后端 8080。
// adminToken 存在 localStorage（后端 AdminToken 为空时鉴权跳过，本地调试免填）。
import { unwrap, normalizeTs, validate, ROUTE_DECISION_ENTRY_FIELDS } from './api.generated.js'

const token = () => localStorage.getItem('muxapi_token') || ''
const REQUEST_TIMEOUT_MS = 15000
// 生产库通过 SSH 隧道访问时，管理列表查询可能需要几十秒；页面不应过早中断。
const HEAVY_REQUEST_TIMEOUT_MS = 90000
const TEST_TIMEOUT_MS = 60000

function timedAbortSignal(externalSignal, timeoutMs) {
  const controller = new AbortController()
  let timedOut = false
  const timeout = setTimeout(() => {
    timedOut = true
    controller.abort()
  }, timeoutMs)
  const abort = () => controller.abort()
  if (externalSignal) {
    if (externalSignal.aborted) controller.abort()
    else externalSignal.addEventListener('abort', abort, { once: true })
  }
  return {
    signal: controller.signal,
    didTimeout: () => timedOut,
    dispose: () => {
      clearTimeout(timeout)
      externalSignal?.removeEventListener('abort', abort)
    },
  }
}

async function req(method, path, body, timeoutMs = REQUEST_TIMEOUT_MS, externalSignal) {
  const headers = { 'Content-Type': 'application/json' }
  const t = token()
  if (t) headers.Authorization = 'Bearer ' + t
  const requestControl = timedAbortSignal(externalSignal, timeoutMs)
  try {
    const res = await fetch('/admin' + path, {
      method,
      headers,
      body: body ? JSON.stringify(body) : undefined,
      signal: requestControl.signal,
    })
    // 保留状态码供页面统一处理 401，响应文本作为具体错误信息。
    if (!res.ok) {
      const e = new Error((await res.text()) || String(res.status))
      e.status = res.status
      throw e
    }
    // DELETE/PUT 可能返回空正文，只对 JSON 响应调用解析器。
    const ct = res.headers.get('content-type') || ''
    return ct.includes('json') ? res.json() : null
  } catch (e) {
    if (requestControl.didTimeout()) throw new Error('服务器响应超时，请重新加载。')
    throw e
  } finally {
    requestControl.dispose()
  }
}

function groupTestRequest(protocol, model) {
	if (protocol === 'gemini') {
		return {
			path: `/v1beta/models/${encodeURIComponent(model)}:streamGenerateContent?alt=sse`,
			body: { contents: [{ role: 'user', parts: [{ text: 'hi' }] }], generationConfig: { maxOutputTokens: 32 } },
		}
	}
  if (protocol === 'claude') {
    return { path: '/v1/messages', body: { model, messages: [{ role: 'user', content: 'hi' }], max_tokens: 32, stream: true } }
  }
  if (protocol === 'chat') {
    return { path: '/v1/chat/completions', body: { model, messages: [{ role: 'user', content: 'hi' }], max_tokens: 32, stream: true } }
  }
  return { path: '/v1/responses', body: { model, input: 'hi', max_output_tokens: 32, stream: true } }
}

function groupTestText(protocol, payload) {
	if (protocol === 'gemini') return (payload?.candidates || []).flatMap(item => item?.content?.parts || []).map(item => item?.text || '').join('')
  if (protocol === 'claude') return payload?.delta?.text || ''
  if (protocol === 'chat') return payload?.choices?.[0]?.delta?.content || ''
  return payload?.type === 'response.output_text.delta' ? (payload.delta || '') : ''
}

function groupTestBodyText(protocol, payload) {
	if (protocol === 'gemini') return (payload?.candidates || []).flatMap(item => item?.content?.parts || []).map(item => item?.text || '').join('')
  if (protocol === 'claude') return (payload?.content || []).filter(item => item.type === 'text').map(item => item.text || '').join('')
  if (protocol === 'chat') return payload?.choices?.[0]?.message?.content || ''
  return (payload?.output || []).flatMap(item => item.content || []).filter(item => item.type === 'output_text').map(item => item.text || '').join('')
}

async function testGroupStream({ key, protocol, model }, onText, externalSignal) {
  const request = groupTestRequest(protocol, model)
  const requestControl = timedAbortSignal(externalSignal, TEST_TIMEOUT_MS)
  try {
    const response = await fetch(request.path, {
      method: 'POST',
      headers: { Authorization: 'Bearer ' + key, 'Content-Type': 'application/json', Accept: 'text/event-stream' },
      body: JSON.stringify(request.body),
      signal: requestControl.signal,
    })
    const requestId = response.headers.get('x-request-id') || ''
    if (!response.ok) {
      const error = new Error((await response.text()) || `HTTP ${response.status}`)
      error.status = response.status
      error.requestId = requestId
      throw error
    }
    const contentType = response.headers.get('content-type') || ''
    if (!contentType.includes('text/event-stream')) {
      const payload = await response.json()
      const rawError = payload?.error
      if (rawError) {
        const message = typeof rawError === 'string' ? rawError : rawError.message || JSON.stringify(rawError)
        const error = new Error(message || `HTTP ${response.status}`)
        error.status = response.status
        error.requestId = requestId
        throw error
      }
      const text = groupTestBodyText(protocol, payload)
      if (text) onText(text)
      return { requestId, status: response.status }
    }
    const reader = response.body?.getReader()
    if (!reader) return { requestId, status: response.status }
    const decoder = new TextDecoder()
    let buffer = ''
    let finished = false
    const consume = block => {
      const data = block.split(/\r?\n/).filter(line => line.startsWith('data:')).map(line => line.slice(5).trim()).join('\n')
      if (!data) return
      if (data === '[DONE]') { finished = true; return }
      let payload
      try { payload = JSON.parse(data) } catch { return }
      const rawError = payload?.error
      const nestedError = payload?.response?.error
      const message = typeof rawError === 'string'
        ? rawError
        : rawError?.message || nestedError?.message || payload?.message
          || (['error', 'response.failed', 'response.incomplete'].includes(payload?.type) ? payload.type : '')
      if (message) throw new Error(message)
      const text = groupTestText(protocol, payload)
      if (text) onText(text)
    }
    while (!finished) {
      const { done, value } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })
      const blocks = buffer.split(/\r?\n\r?\n/)
      buffer = blocks.pop()
      for (const block of blocks) {
        consume(block)
        if (finished) break
      }
    }
    if (finished) await reader.cancel().catch(() => {})
    buffer += decoder.decode()
    if (!finished && buffer.trim()) consume(buffer)
    return { requestId, status: response.status }
  } catch (e) {
    if (requestControl.didTimeout()) throw new Error('测试请求超时（60 秒），请检查渠道或稍后重试。')
    throw e
  } finally {
    requestControl.dispose()
  }
}

export const api = {
  getToken: token,
  setToken: t => localStorage.setItem('muxapi_token', t),
  clearToken: () => localStorage.removeItem('muxapi_token'),
  // 上游全局池
  upstreams: () => req('GET', '/upstreams', undefined, HEAVY_REQUEST_TIMEOUT_MS),
  overviewUpstreams: () => req('GET', '/upstreams?view=overview', undefined, HEAVY_REQUEST_TIMEOUT_MS),
  createUpstream: u => req('POST', '/upstreams', u),
  updateUpstream: (id, u) => req('PUT', '/upstreams/' + id, u),
  deleteUpstream: id => req('DELETE', '/upstreams/' + id),
  batchUpdateUpstreams: payload => req('POST', '/upstreams/batch', payload),
  reorderUpstreams: ids => req('POST', '/upstreams/reorder', { ids }),
  testUpstream: (id, signal) => req('GET', `/upstreams/${id}/models`, undefined, TEST_TIMEOUT_MS, signal),
  recoverUpstream: id => req('POST', `/upstreams/${id}/recover`),
  recoverUpstreamModel: (id, model) => req('POST', `/upstreams/${id}/models/recover`, { model }),
  refreshUpstreamBilling: id => req('POST', `/upstreams/${id}/billing/refresh`),
  setUpstreamBillingMultiplier: (id, multiplier) => req('PUT', `/upstreams/${id}/billing/multiplier`, { multiplier }),
  upstreamBillingAudit: (id, window) => req('GET', `/upstreams/${id}/billing/audit?window=${encodeURIComponent(window || '')}`),
  overviewTrends: ({ window = '24h', tag_id = 0 } = {}) => {
    const p = new URLSearchParams({ window, _ts: String(Date.now()) })
    if (Number(tag_id) > 0) p.set('tag_id', String(tag_id))
    return req('GET', `/overview/trends?${p.toString()}`, undefined, HEAVY_REQUEST_TIMEOUT_MS)
  },
  overviewSummary: () => req('GET', `/overview/summary?_ts=${Date.now()}`, undefined, HEAVY_REQUEST_TIMEOUT_MS),
  createMonitorsBatch: (id, payload) => req('POST', `/upstreams/${id}/monitors`, payload),
  // 管理标签
  tags: () => req('GET', '/tags'),
  createTag: tag => req('POST', '/tags', tag),
  updateTag: (id, tag) => req('PUT', '/tags/' + id, tag),
  deleteTag: id => req('DELETE', '/tags/' + id),
  // 真实对话测试：发 hi 请求，SSE 逐块回调 onEvent({type,text,...})。EventSource 不能带鉴权头，故用 fetch 流式解析。
  testUpstreamStream: async (id, model, onEvent, externalSignal) => {
    const headers = {}
    const t = token()
    if (t) headers.Authorization = 'Bearer ' + t
    const requestControl = timedAbortSignal(externalSignal, TEST_TIMEOUT_MS)
    try {
      const res = await fetch(`/admin/upstreams/${id}/test?model=${encodeURIComponent(model)}`, {
        method: 'POST', headers, signal: requestControl.signal,
      })
      if (!res.ok) {
        const error = new Error((await res.text()) || String(res.status))
        error.status = res.status
        throw error
      }
      const reader = res.body?.getReader()
      if (!reader) return
      const dec = new TextDecoder()
      let buf = ''
      let finished = false
      while (!finished) {
        const { done, value } = await reader.read()
        if (done) break
        buf += dec.decode(value, { stream: true })
        const parts = buf.split(/\r?\n\r?\n/)
        buf = parts.pop()
        for (const p of parts) {
          const line = p.trim()
          if (!line.startsWith('data:')) continue
          try {
            const event = JSON.parse(line.slice(5).trim())
            onEvent(event)
            if (event.type === 'test_complete' || event.type === 'error') finished = true
          } catch {}
          if (finished) break
        }
      }
      if (finished) await reader.cancel().catch(() => {})
      if (!finished && buf.trim()) {
        const line = buf.trim()
        if (line.startsWith('data:')) {
          try { onEvent(JSON.parse(line.slice(5).trim())) } catch {}
        }
      }
    } catch (e) {
      if (requestControl.didTimeout()) throw new Error('测试请求超时（60 秒），请检查渠道或稍后重试。')
      throw e
    } finally {
      requestControl.dispose()
    }
  },
  // 分组
  groups: () => req('GET', '/groups'),
  createGroup: g => req('POST', '/groups', g),
  updateGroup: (id, g) => req('PUT', '/groups/' + id, g),
  deleteGroup: id => req('DELETE', '/groups/' + id),
  reorderGroups: ids => req('POST', '/groups/reorder', { ids }),
  groupModels: async (key, externalSignal) => {
    const requestControl = timedAbortSignal(externalSignal, TEST_TIMEOUT_MS)
    try {
      const response = await fetch('/v1/models', { headers: { Authorization: 'Bearer ' + key }, signal: requestControl.signal })
      if (!response.ok) throw new Error((await response.text()) || String(response.status))
      const payload = await response.json()
      return (payload.data || []).map(item => item.id).filter(Boolean)
    } catch (e) {
      if (requestControl.didTimeout()) throw new Error('模型列表请求超时（60 秒），仍可使用默认模型测试。')
      throw e
    } finally {
      requestControl.dispose()
    }
  },
  testGroupStream,
  // 组成员
  members: gid => req('GET', `/groups/${gid}/upstreams`),
  addMember: (gid, m) => req('POST', `/groups/${gid}/upstreams`, m),
  setMemberEnabled: (gid, uid, enabled) => req('PUT', `/groups/${gid}/upstreams/${uid}`, { enabled }),
  removeMember: (gid, uid) => req('DELETE', `/groups/${gid}/upstreams/${uid}`),
  // 组密钥
  keys: gid => req('GET', `/groups/${gid}/keys`),
  createKey: (gid, name) => req('POST', `/groups/${gid}/keys`, { name }),
  setKeyEnabled: (id, enabled) => req('PUT', '/keys/' + id, { enabled }),
  deleteKey: id => req('DELETE', '/keys/' + id),
  // 监控项（渠道+模型，主动探测）
  monitors: () => req('GET', '/monitors', undefined, HEAVY_REQUEST_TIMEOUT_MS),
  createMonitor: m => req('POST', '/monitors', m),
  updateMonitor: (id, m) => req('PUT', '/monitors/' + id, m),
  deleteMonitor: id => req('DELETE', '/monitors/' + id),
  reorderMonitors: ids => req('POST', '/monitors/reorder', { ids }), // 持久化拖拽顺序
  probeMonitor: id => req('POST', `/monitors/${id}/probe`), // 立即探测一次，返回最新快照
  // 运行时设置
  getSettings: () => req('GET', '/settings'),
  saveSettings: s => req('PUT', '/settings', s),

  // 请求记录（游标分页 + 服务端筛选）。
  logs: (opts = {}, signal, timeoutMs = REQUEST_TIMEOUT_MS) => {
    const p = new URLSearchParams()
    for (const key of ['before', 'offset', 'limit', 'model', 'group', 'status', 'key', 'endpoint', 'error_kind',
      'q', 'stream', 'upstream_id', 'since', 'until', 'slow_ms']) {
      if (opts[key] !== undefined && opts[key] !== null && opts[key] !== '') p.set(key, opts[key])
    }
    if (opts.retried) p.set('retried', 'true')
    const qs = p.toString()
    return req('GET', '/logs' + (qs ? '?' + qs : ''), undefined, timeoutMs, signal)
  },
  logStats: (opts = {}) => {
    const p = new URLSearchParams()
    for (const key of ['model', 'group', 'status', 'key', 'endpoint', 'error_kind', 'q', 'stream',
      'upstream_id', 'since', 'until', 'slow_ms']) {
      if (opts[key] !== undefined && opts[key] !== null && opts[key] !== '') p.set(key, opts[key])
    }
    if (opts.retried) p.set('retried', 'true')
    const qs = p.toString()
    return req('GET', '/logs/stats' + (qs ? '?' + qs : ''))
  },
  logCacheStats: (opts = {}) => {
    const p = new URLSearchParams()
    for (const key of ['model', 'group', 'status', 'key', 'endpoint', 'error_kind', 'q', 'stream',
      'upstream_id', 'since', 'until', 'slow_ms']) {
      if (opts[key] !== undefined && opts[key] !== null && opts[key] !== '') p.set(key, opts[key])
    }
    if (opts.retried) p.set('retried', 'true')
    const qs = p.toString()
    return req('GET', '/logs/cache-stats' + (qs ? '?' + qs : ''))
  },
  logDetail: (id, signal, timeoutMs = REQUEST_TIMEOUT_MS) => req('GET', '/logs/' + id, undefined, timeoutMs, signal),
  logOptions: () => req('GET', '/logs/options'),

  // 路由决策
  routeDecisions: async (params) => {
    const p = new URLSearchParams()
    if (params) for (const [k, v] of Object.entries(params)) { if (v != null && v !== '') p.set(k, v) }
    // 列表页可关闭候选渠道，详情页再按需加载，减少首屏响应体。
    if (!p.has('include_candidates')) p.set('include_candidates', 'true')
    const qs = p.toString()
    const raw = await req('GET', '/routing/decisions' + (qs ? '?' + qs : ''))
    const items = unwrap(raw)
    if (Array.isArray(items)) items.forEach(i => validate('RouteDecisionEntry', i, ROUTE_DECISION_ENTRY_FIELDS))
    return items
  },
  routeDecisionDetail: (id) => req('GET', `/routing/decisions/${id}`),

  // 数据备份
  backupConfig: () => req('GET', '/backup/config'),
  saveBackupConfig: cfg => req('PUT', '/backup/config', cfg),
  testBackupConfig: cfg => req('POST', '/backup/config/test', cfg),
  backupSchedule: () => req('GET', '/backup/schedule'),
  saveBackupSchedule: s => req('PUT', '/backup/schedule', s),
  triggerBackup: () => req('POST', '/backup', {}),
  listBackups: () => req('GET', '/backup'),
  deleteBackup: id => req('DELETE', '/backup/records/' + id),
  backupDownloadURL: id => req('GET', '/backup/records/' + id + '/download'),

  // 模型映射
  modelMappings: (upstreamId) => req('GET', '/model-mappings' + (upstreamId ? '?upstream_id=' + upstreamId : '')),
  createModelMapping: payload => req('POST', '/model-mappings', payload),
  deleteModelMapping: id => req('DELETE', '/model-mappings/' + id),
}
