package forward

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const maxAuditPayload = 2 << 20

// tokenUsage 保存跨协议统一后的 Token 用量。
type tokenUsage struct {
	input         int64
	output        int64
	cached        int64
	cacheCreation int64
	normalized    bool
}

func (u *tokenUsage) merge(other tokenUsage) {
	// 流式协议通常重复发送累计值，取最大值可避免重复计数。
	if other.input > u.input {
		u.input = other.input
	}
	if other.output > u.output {
		u.output = other.output
	}
	if other.cached > u.cached {
		u.cached = other.cached
	}
	if other.cacheCreation > u.cacheCreation {
		u.cacheCreation = other.cacheCreation
	}
	u.normalized = u.normalized || other.normalized
}

// responseAudit 在不影响响应转发的前提下提取完成状态和用量。
// 非流式正文超过上限后放弃解析，防止审计逻辑放大内存占用。
type responseAudit struct {
	stream          bool
	protocol        string
	body            []byte
	overflow        bool
	sseBuffer       []byte
	usage           tokenUsage
	lastEvent       string
	streamCompleted bool
	// streamErrored 表示上游在 2xx 流里投递过明确的错误事件。
	streamErrored bool
}

// auditReadCloser 装饰上游正文，使所有转发分支共享同一套审计解析。
type auditReadCloser struct {
	io.ReadCloser
	audit *responseAudit
}

func newAuditReadCloser(body io.ReadCloser, stream bool, protocol ...string) *auditReadCloser {
	hint := ""
	if len(protocol) > 0 {
		hint = strings.TrimSpace(protocol[0])
	}
	return &auditReadCloser{ReadCloser: body, audit: &responseAudit{stream: stream, protocol: hint}}
}

func (r *auditReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.audit.feed(p[:n])
	}
	if err == io.EOF {
		r.audit.finish()
	}
	return n, err
}

func (a *responseAudit) feed(chunk []byte) {
	if a.stream {
		// TCP 读取边界与 SSE 事件边界无关，残缺事件留到下一批数据继续解析。
		a.sseBuffer = append(a.sseBuffer, chunk...)
		for {
			end, skip := nextSSEBlock(a.sseBuffer)
			if end < 0 {
				break
			}
			a.consumeSSEBlock(a.sseBuffer[:end])
			a.sseBuffer = a.sseBuffer[end+skip:]
		}
		if len(a.sseBuffer) > maxAuditPayload {
			a.sseBuffer = append([]byte(nil), a.sseBuffer[len(a.sseBuffer)-maxAuditPayload:]...)
		}
		return
	}
	if a.overflow {
		return
	}
	if len(a.body)+len(chunk) > maxAuditPayload {
		a.body = nil
		a.overflow = true
		return
	}
	a.body = append(a.body, chunk...)
}

func (a *responseAudit) finish() {
	if a.stream {
		if len(bytes.TrimSpace(a.sseBuffer)) > 0 {
			a.consumeSSEBlock(a.sseBuffer)
		}
		a.sseBuffer = nil
		return
	}
	if !a.overflow {
		a.usage.merge(usageFromJSON(a.body, a.protocol))
	}
}

// nextSSEBlock 同时支持 LF 和 CRLF 两种合法的 SSE 事件分隔符。
func nextSSEBlock(data []byte) (end, separatorLength int) {
	lf := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	switch {
	case lf < 0 && crlf < 0:
		return -1, 0
	case lf >= 0 && (crlf < 0 || lf < crlf):
		return lf, 2
	default:
		return crlf, 4
	}
}

// consumeSSEBlock 兼容 event 字段、JSON type 字段和 [DONE] 三类完成标记。
func (a *responseAudit) consumeSSEBlock(block []byte) {
	var eventName string
	var dataLines []string
	for _, line := range strings.Split(strings.ReplaceAll(string(block), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if eventName != "" {
		a.lastEvent = clipLabel(eventName)
		if isCompletionEvent(eventName) {
			a.streamCompleted = true
		}
		if isStreamErrorEvent(eventName) {
			a.streamErrored = true
		}
	}
	data := strings.Join(dataLines, "\n")
	if data == "" {
		return
	}
	if data == "[DONE]" {
		a.lastEvent = "[DONE]"
		a.streamCompleted = true
		return
	}
	var payload map[string]any
	if json.Unmarshal([]byte(data), &payload) != nil {
		return
	}
	if typ, _ := payload["type"].(string); typ != "" {
		a.lastEvent = clipLabel(typ)
		if isCompletionEvent(typ) {
			a.streamCompleted = true
		}
		if isStreamErrorEvent(typ) {
			a.streamErrored = true
		}
	}
	if geminiResponseComplete(payload) {
		a.lastEvent = "generateContent.completed"
		a.streamCompleted = true
	}
	a.usage.merge(usageFromValue(payload, a.protocol))
}

func geminiResponseComplete(payload map[string]any) bool {
	candidates, ok := payload["candidates"].([]any)
	if !ok {
		return false
	}
	for _, candidate := range candidates {
		object, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		if reason, _ := object["finishReason"].(string); strings.TrimSpace(reason) != "" {
			return true
		}
	}
	return false
}

// isStreamErrorEvent 识别各协议在 2xx 流中投递失败的终止事件：
// Claude 的 error、Responses 的 response.failed。
func isStreamErrorEvent(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "error", "response.failed":
		return true
	default:
		return false
	}
}

func isCompletionEvent(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "response.completed", "message_stop", "message.completed", "done", "[done]":
		return true
	default:
		return false
	}
}

// clipLabel 截断 SSE 事件名。同 clipErr，结果会入库，必须是合法 UTF-8。
func clipLabel(value string) string {
	return clipUTF8(strings.TrimSpace(value), 80)
}

func usageFromJSON(data []byte, protocol ...string) tokenUsage {
	var value any
	if len(bytes.TrimSpace(data)) == 0 || json.Unmarshal(data, &value) != nil {
		return tokenUsage{}
	}
	return usageFromValue(value, protocol...)
}

// usageFromValue 兼容顶层 usage 以及 Responses/Claude 的嵌套 usage。
func usageFromValue(value any, protocol ...string) tokenUsage {
	root, ok := value.(map[string]any)
	if !ok {
		return tokenUsage{}
	}
	hint := usageProtocolHint(protocol...)
	if hint == "" {
		hint = inferUsageProtocol(root)
	}
	var out tokenUsage
	mergeUsageObject := func(candidate any) {
		if object, ok := candidate.(map[string]any); ok {
			out.merge(parseUsageObject(object, hint))
		}
	}
	mergeUsageObject(root["usage"])
	mergeUsageObject(root["usageMetadata"])
	for _, key := range []string{"response", "message"} {
		if nested, ok := root[key].(map[string]any); ok {
			nestedHint := hint
			if nestedHint == "" && key == "response" {
				nestedHint = "responses"
			}
			if object, ok := nested["usage"].(map[string]any); ok {
				out.merge(parseUsageObject(object, nestedHint))
			}
		}
	}
	return out
}

// parseUsageObject 将 OpenAI / Anthropic / Gemini 的 usage 归一为统一计数。
//
// 语义 (Anthropic 约定):
//
//	input          = uncached prompt tokens (排他,不含 cached / cacheCreation)
//	cached         = cache_read tokens (命中缓存部分)
//	cacheCreation  = cache_write tokens (首次写入部分)
//	output         = completion / response tokens
//
// 协议差异:
//
//	Anthropic:  usage.input_tokens 本身就是 uncached; cache_read_input_tokens /
//	            cache_creation_input_tokens 独立字段。
//	OpenAI Chat/Responses/Codex: prompt_tokens 或 input_tokens 是 INCLUSIVE 的 ——
//	            已经包含 cached。cached 出现在 cached_tokens、
//	            input_tokens_details.cached_tokens 或 prompt_tokens_details.cached_tokens。
//	Gemini:     usage.promptTokenCount 是 INCLUSIVE 的 —— 已经包含 cachedContentTokenCount。
//
// 因此对 OpenAI/Gemini/Responses/Codex 需要在这里减去 cached,让存入 routing_observations 的
// input 列在所有协议下保持"uncached"这个不变量。下游缓存用量统计依赖这个
// 不变量；不做归一会把 cached 重复计入 input。
func parseUsageObject(usage map[string]any, protocol ...string) tokenUsage {
	inputTokens := maxInt64(number(usage["input_tokens"]), number(usage["prompt_tokens"]))
	inputTokens = maxInt64(inputTokens, number(usage["promptTokenCount"]))
	output := maxInt64(maxInt64(maxInt64(number(usage["output_tokens"]), number(usage["completion_tokens"])), number(usage["candidatesTokenCount"])), number(usage["thoughtsTokenCount"]))
	cached := maxInt64(maxInt64(number(usage["cached_tokens"]), number(usage["cache_read_input_tokens"])), number(usage["cachedContentTokenCount"]))
	cacheCreation := maxInt64(number(usage["cache_creation_tokens"]), number(usage["cache_creation_input_tokens"]))
	for _, key := range []string{"input_tokens_details", "prompt_tokens_details"} {
		if details, ok := usage[key].(map[string]any); ok {
			cached = maxInt64(cached, number(details["cached_tokens"]))
		}
	}
	input := inputTokens
	hint := usageProtocolHint(protocol...)
	if hint == "" {
		hint = inferUsageProtocol(usage)
	}
	if usageInputInclusive(hint, usage) {
		input -= cached
		if input < 0 {
			input = 0
		}
	}
	return tokenUsage{input: input, output: output, cached: cached, cacheCreation: cacheCreation, normalized: hint != ""}
}

func usageProtocolHint(protocol ...string) string {
	if len(protocol) == 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(protocol[0]))
}

func inferUsageProtocol(object map[string]any) string {
	if number(object["promptTokenCount"]) > 0 || number(object["cachedContentTokenCount"]) > 0 {
		return "gemini"
	}
	if number(object["prompt_tokens"]) > 0 || object["prompt_tokens_details"] != nil || object["input_tokens_details"] != nil {
		return "responses"
	}
	if object["cache_read_input_tokens"] != nil || object["cache_creation_input_tokens"] != nil {
		return "claude"
	}
	return ""
}

func usageInputInclusive(protocol string, usage map[string]any) bool {
	switch protocol {
	case "claude", "anthropic", "messages", "anthropic-messages":
		return false
	case "gemini", "google", "generativelanguage", "generatecontent",
		"responses", "openai-response", "openai_responses", "codex":
		return true
	case "openai", "chat", "chat_completions", "chat-completions":
		return number(usage["prompt_tokens"]) > 0 || usage["prompt_tokens_details"] != nil || usage["input_tokens_details"] != nil || number(usage["cached_tokens"]) > 0
	default:
		return number(usage["prompt_tokens"]) > 0 || number(usage["promptTokenCount"]) > 0 || usage["input_tokens_details"] != nil || usage["prompt_tokens_details"] != nil
	}
}

func number(value any) int64 {
	switch n := value.(type) {
	case float64:
		return int64(n)
	case json.Number:
		v, _ := n.Int64()
		return v
	default:
		return 0
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// upstreamRequestID 按常见网关头的优先顺序提取链路标识。
func upstreamRequestID(header http.Header) string {
	for _, key := range []string{"X-Request-ID", "Request-ID", "OpenAI-Request-ID", "CF-Ray"} {
		if value := strings.TrimSpace(header.Get(key)); value != "" {
			return clipLabel(value)
		}
	}
	return ""
}
