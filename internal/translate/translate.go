// Package translate 封装不同 AI API 协议之间的请求、响应和 SSE 事件转换。
package translate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/muxbuiltin"
)

// Format is a request/response schema understood by the CPA translator.
type Format string

const (
	Passthrough     Format = "passthrough"
	OpenAI          Format = "openai"
	OpenAIResponses Format = "openai-response"
	Claude          Format = "claude"
	Codex           Format = "codex"
)

// ErrUnsupported 表示当前源协议与目标协议之间没有完整的双向转换链。
var ErrUnsupported = errors.New("protocol translation is not supported")

// NormalizeFormat 清理数据库中的协议值，并验证其是否受支持。
func NormalizeFormat(value string) (Format, bool) {
	format := Format(strings.ToLower(strings.TrimSpace(value)))
	if format == "" {
		format = Passthrough
	}
	switch format {
	case Passthrough, OpenAI, OpenAIResponses, Claude, Codex:
		return format, true
	default:
		return "", false
	}
}

// SourceFromPath 根据客户端入口路径识别请求协议。
func SourceFromPath(path string) (Format, bool) {
	switch strings.TrimSuffix(strings.TrimSpace(path), "/") {
	case "/v1/chat/completions":
		return OpenAI, true
	case "/v1/responses":
		return OpenAIResponses, true
	case "/v1/messages":
		return Claude, true
	default:
		return "", false
	}
}

// SourceFromRequest distinguishes native Codex clients from generic Responses
// clients. Both use /v1/responses, but native Codex payloads must not be
// normalized a second time before reaching a Codex upstream.
func SourceFromRequest(path string, header http.Header) (Format, bool) {
	format, ok := SourceFromPath(path)
	if !ok || format != OpenAIResponses {
		return format, ok
	}
	for _, key := range []string{
		"X-Codex-Turn-Metadata",
		"X-Codex-Beta-Features",
		"X-OpenAI-Internal-Codex-Responses-Lite",
	} {
		if strings.TrimSpace(header.Get(key)) != "" {
			return Codex, true
		}
	}
	if strings.Contains(strings.ToLower(header.Get("Originator")), "codex") {
		return Codex, true
	}
	return format, true
}

// TargetPath 返回目标协议的标准端点；透传模式保留原路径。
func TargetPath(format Format, originalPath string) (string, error) {
	switch format {
	case Passthrough:
		return originalPath, nil
	case OpenAI:
		return "/v1/chat/completions", nil
	case OpenAIResponses, Codex:
		return "/v1/responses", nil
	case Claude:
		return "/v1/messages", nil
	default:
		return "", fmt.Errorf("%w: target format %q", ErrUnsupported, format)
	}
}

// ConfigureRequestHeaders removes source-protocol headers after translation
// and adds headers required by the target protocol.
func ConfigureRequestHeaders(header http.Header, target Format, translated bool) {
	if target == Passthrough {
		return
	}
	if translated {
		header.Del("Content-Encoding")
		header.Del("Content-Length")
		header.Del("Accept-Encoding")
		header.Set("Content-Type", "application/json")
	}
	if target == Claude {
		if header.Get("anthropic-version") == "" {
			header.Set("anthropic-version", "2023-06-01")
		}
		return
	}
	header.Del("anthropic-version")
	header.Del("anthropic-beta")
}

// ErrorResponse converts an upstream error envelope to the client's protocol.
func ErrorResponse(source Format, status int, body []byte) []byte {
	if source == Passthrough {
		return append([]byte(nil), body...)
	}
	details := parseErrorDetails(body)
	if details.Message == "" {
		details.Message = http.StatusText(status)
	}
	if details.Type == "" {
		details.Type = errorTypeForStatus(status)
	}

	var response any
	if source == Claude {
		response = map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    details.Type,
				"message": details.Message,
			},
		}
	} else {
		response = map[string]any{
			"error": map[string]any{
				"message": details.Message,
				"type":    details.Type,
				"param":   nil,
				"code":    details.Code,
			},
		}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return append([]byte(nil), body...)
	}
	return encoded
}

type errorDetails struct {
	Message string
	Type    string
	Code    any
}

func parseErrorDetails(body []byte) errorDetails {
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    any             `json:"code"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return errorDetails{Message: strings.TrimSpace(string(body))}
	}
	details := errorDetails{Message: envelope.Message, Type: envelope.Type, Code: envelope.Code}
	if len(envelope.Error) == 0 || string(envelope.Error) == "null" {
		return details
	}
	var message string
	if json.Unmarshal(envelope.Error, &message) == nil {
		details.Message = message
		return details
	}
	var nested struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	}
	if json.Unmarshal(envelope.Error, &nested) == nil {
		if nested.Message != "" {
			details.Message = nested.Message
		}
		if nested.Type != "" {
			details.Type = nested.Type
		}
		if nested.Code != nil {
			details.Code = nested.Code
		}
	}
	return details
}

func errorTypeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

// Exchange owns the translated request and the state used to translate one response stream.
type Exchange struct {
	Source          Format
	Target          Format
	Model           string
	Stream          bool
	UpstreamStream  bool
	OriginalRequest []byte
	UpstreamRequest []byte

	registry *sdktranslator.Registry
	state    any
	// sdkSource 是查 SDK 转换表时使用的来源协议，可能与 Source 不同（见 resolveSDKSource）。
	// Source 始终保留真实客户端协议，透传判定和错误回写都依赖它。
	sdkSource Format
}

// NewExchange 校验双向转换能力，并从原始客户端正文生成上游请求。
func NewExchange(source, target Format, model string, stream bool, original []byte) (exchange *Exchange, err error) {
	// 第三方转换器的旧实现可能 panic，协议边界统一转换成普通错误。
	defer func() {
		if recovered := recover(); recovered != nil {
			exchange = nil
			err = fmt.Errorf("translate request %s -> %s: %v", source, target, recovered)
		}
	}()
	if source == "" {
		return nil, fmt.Errorf("%w: source format is empty", ErrUnsupported)
	}
	if target == "" {
		target = Passthrough
	}
	exchange = &Exchange{
		Source:          source,
		Target:          target,
		Model:           model,
		Stream:          stream,
		OriginalRequest: append([]byte(nil), original...),
		registry:        muxbuiltin.Registry(),
	}
	// 部分目标转换器只提供流式输出，非流式客户端也先请求流，再由转发层聚合。
	exchange.UpstreamStream = stream || (exchange.Translated() && (target == Claude || target == Codex))
	if !exchange.Translated() {
		exchange.UpstreamRequest = append([]byte(nil), original...)
		sanitizeDeferredTools(exchange)
		return exchange, nil
	}
	exchange.sdkSource = resolveSDKSource(exchange.registry, source, target)
	from, to := exchange.sdkFormats()
	if !exchange.registry.HasRequestTransformer(from, to) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrUnsupported, source, target)
	}
	if stream {
		if !exchange.registry.HasStreamResponseTransformer(from, to) {
			return nil, fmt.Errorf("%w: streaming response %s <- %s", ErrUnsupported, source, target)
		}
	} else if !exchange.registry.HasNonStreamResponseTransformer(from, to) {
		return nil, fmt.Errorf("%w: response %s <- %s", ErrUnsupported, source, target)
	}
	// 归一化顺序：先展开 input 字符串简写，再把 additional_tools 搬到顶层 tools。
	// 后者依赖 input 已是数组形式。
	upstreamBody := normalizeResponsesInput(exchange.sdkSource, original)
	upstreamBody = hoistAdditionalTools(exchange.sdkSource, upstreamBody)
	// Response translators also inspect the source request to restore tool kinds
	// and namespaces. Keep the normalized source shape so definitions delivered
	// through Codex Desktop's additional_tools remain visible on the way back.
	exchange.OriginalRequest = append([]byte(nil), upstreamBody...)
	translated := exchange.registry.TranslateRequest(from, to, model, upstreamBody, exchange.UpstreamStream)
	if !json.Valid(translated) {
		return nil, fmt.Errorf("translate request %s -> %s: invalid JSON", source, target)
	}
	exchange.UpstreamRequest = translated
	sanitizeDeferredTools(exchange)
	return exchange, nil
}

// resolveSDKSource 选出查 SDK 转换表时该用的来源协议。
// SDK 只把 codex 注册为目标协议，从未注册为来源，所以 Codex CLI 的请求发往
// Claude/OpenAI 渠道时查表必然落空。但 Codex 的请求体本身就是合法的 Responses
// 形状（codex 专有字段只是额外键），而目标端翻译器都是从零构造上游请求、只读取
// 自己认识的字段，多出来的键会被自然忽略，因此降级成 openai-response 是安全的。
// 仅在「codex 直连不可用而降级后可用」时才降级，其余情况原样返回，
// 避免把 codex -> codex 这类透传误变成翻译。
func resolveSDKSource(registry *sdktranslator.Registry, source, target Format) Format {
	if source != Codex {
		return source
	}
	to := sdktranslator.FromString(string(target))
	if registry.HasRequestTransformer(sdktranslator.FromString(string(Codex)), to) {
		return source
	}
	if registry.HasRequestTransformer(sdktranslator.FromString(string(OpenAIResponses)), to) {
		return OpenAIResponses
	}
	return source
}

// hoistAdditionalTools 把 Codex Desktop 的 additional_tools input 项搬到顶层 tools。
// Codex Desktop（Responses Lite）用 input 里的 additional_tools 项携带工具定义，
// 而 Claude 方向的翻译器只读顶层 tools，不搬运工具就会被静默丢弃 —— 模型收到一个
// 没有工具的请求，只能自述意图后放弃。openai 方向自己合并了两个来源，搬运后结果
// 等价（同一个转换函数、同一份扁平列表），所以这里对两个目标统一处理。
// 顶层已有的工具排在前面，两个来源合并而非覆盖；搬空后的 additional_tools 项
// 必须从 input 移除，否则会被当成一条普通消息发给模型。
func hoistAdditionalTools(source Format, original []byte) []byte {
	if source != OpenAIResponses && source != Codex {
		return original
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(original, &body) != nil {
		return original
	}
	var items []json.RawMessage
	if json.Unmarshal(body["input"], &items) != nil {
		return original
	}
	var hoisted []json.RawMessage
	kept := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		var probe struct {
			Type  string            `json:"type"`
			Tools []json.RawMessage `json:"tools"`
		}
		if json.Unmarshal(item, &probe) != nil || strings.TrimSpace(probe.Type) != "additional_tools" {
			kept = append(kept, item)
			continue
		}
		hoisted = append(hoisted, probe.Tools...)
	}
	if len(hoisted) == 0 {
		return original
	}
	merged := make([]json.RawMessage, 0, len(hoisted))
	if raw, ok := body["tools"]; ok {
		var existing []json.RawMessage
		if json.Unmarshal(raw, &existing) == nil {
			merged = append(merged, existing...)
		}
	}
	merged = append(merged, hoisted...)
	rewritten, err := setJSONField(original, "tools", merged)
	if err != nil {
		return original
	}
	rewritten, err = setJSONField(rewritten, "input", kept)
	if err != nil {
		return original
	}
	return rewritten
}

// setJSONField 用 value 替换 body 顶层的 field，其余字段保持原始字节不变。
func setJSONField(body []byte, field string, value any) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	object[field] = encoded
	return json.Marshal(object)
}

// normalizeResponsesInput 把 Responses 协议的 input 字符串简写展开成标准数组形式。
// 官方 Responses API 两种都合法，但 SDK 的翻译器只解析数组，收到字符串会静默产出
// 空 messages —— 上游（如 New API）随即报「field messages is required」。
// 只在源协议是 Responses 且 input 确为字符串时改写，其余情况原样返回。
func normalizeResponsesInput(source Format, original []byte) []byte {
	if source != OpenAIResponses {
		return original
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(original, &body) != nil {
		return original
	}
	raw, ok := body["input"]
	if !ok {
		return original
	}
	var text string
	if json.Unmarshal(raw, &text) != nil { // 已是数组或其他结构，交给翻译器
		return original
	}
	expanded, err := json.Marshal([]any{map[string]any{
		"type":    "message",
		"role":    "user",
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}})
	if err != nil {
		return original
	}
	body["input"] = expanded
	rewritten, err := json.Marshal(body)
	if err != nil {
		return original
	}
	return rewritten
}

// Translated 判断本次交换是否需要跨协议转换。
func (e *Exchange) Translated() bool {
	return e != nil && e.Target != Passthrough && e.Source != e.Target
}

// TranslateNonStream 将完整上游响应还原为客户端协议。
func (e *Exchange) TranslateNonStream(ctx context.Context, body []byte) (out []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			out = nil
			err = fmt.Errorf("translate response %s <- %s: %v", e.Source, e.Target, recovered)
		}
	}()
	if e == nil || !e.Translated() {
		return append([]byte(nil), body...), nil
	}
	from, to := e.sdkFormats()
	out = e.registry.TranslateNonStream(ctx, to, from, e.Model, e.OriginalRequest, e.UpstreamRequest, body, &e.state)
	if !json.Valid(out) {
		return nil, fmt.Errorf("translate response %s <- %s: invalid JSON", e.Source, e.Target)
	}
	return out, nil
}

// TranslateStream accepts one upstream SSE line and may emit zero or more client SSE events.
func (e *Exchange) TranslateStream(ctx context.Context, line []byte) (outputs [][]byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			outputs = nil
			err = fmt.Errorf("translate stream %s <- %s: %v", e.Source, e.Target, recovered)
		}
	}()
	if e == nil || !e.Translated() {
		return [][]byte{append([]byte(nil), line...)}, nil
	}
	from, to := e.sdkFormats()
	outputs = e.registry.TranslateStream(ctx, to, from, e.Model, e.OriginalRequest, e.UpstreamRequest, line, &e.state)
	return outputs, nil
}

func (e *Exchange) sdkFormats() (sdktranslator.Format, sdktranslator.Format) {
	source := e.sdkSource
	if source == "" {
		source = e.Source
	}
	return sdktranslator.FromString(string(source)), sdktranslator.FromString(string(e.Target))
}
