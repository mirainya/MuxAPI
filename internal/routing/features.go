package routing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// FeatureOptions supplies protocol/session hints when the request body does
// not contain them. Headers are case-insensitive; callers may pass a nil map.
type FeatureOptions struct {
	Protocol string
	Headers  http.Header
}

// ExtractRequestFeatures recognizes OpenAI Chat/Responses, Anthropic
// Messages, and Gemini generateContent-shaped JSON. It intentionally does not
// mutate the body or add provider-specific cache controls. Unknown JSON is
// still measured as generic text so an unsupported protocol can use the
// ordinary-cost fallback.
func ExtractRequestFeatures(body []byte, options FeatureOptions) (RequestFeatures, error) {
	var root any
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &root); err != nil {
			return RequestFeatures{}, err
		}
	}
	object, _ := root.(map[string]any)
	protocol := NormalizeProtocol(options.Protocol)
	if protocol == "openai" && object != nil {
		if _, ok := object["contents"]; ok {
			protocol = "gemini"
		}
		if _, ok := object["input"]; ok {
			protocol = "responses"
		}
	}
	if protocol == "" {
		protocol = "openai"
	}
	features := RequestFeatures{Protocol: protocol}
	features.Model = firstString(object, "model", "model_name")
	features.SessionID = headerValue(options.Headers,
		"x-claude-code-session-id", "x-codex-window-id", "x-codex-session-id",
		"x-session-id", "session-id", "conversation-id", "conversation_id", "thread-id", "thread_id")
	features.CacheKey = headerValue(options.Headers, "x-prompt-cache-key", "prompt-cache-key", "prompt_cache_key")
	if features.CacheKey == "" {
		features.CacheKey = firstString(object, "prompt_cache_key", "promptCacheKey", "cache_key")
	}
	features.ReasoningEffort = firstString(object, "reasoning_effort", "reasoningEffort")
	features.MaxOutputTokens = firstInt(object, "max_tokens", "max_completion_tokens", "max_output_tokens", "maxOutputTokens")
	if reasoning, ok := object["reasoning"].(map[string]any); ok && features.ReasoningEffort == "" {
		features.ReasoningEffort = firstString(reasoning, "effort")
	}
	if generation, ok := object["generationConfig"].(map[string]any); ok && features.MaxOutputTokens == 0 {
		features.MaxOutputTokens = firstInt(generation, "maxOutputTokens", "max_output_tokens")
	}
	features.Stream = firstBool(object, "stream")

	var parts []textPart
	switch protocol {
	case "responses":
		parts = append(parts, extractResponses(object)...)
	case "claude":
		parts = append(parts, extractMessages(object["system"], "system")...)
		parts = append(parts, extractMessages(object["messages"], "")...)
	case "gemini":
		parts = append(parts, extractGemini(object["systemInstruction"], "system")...)
		parts = append(parts, extractGemini(object["contents"], "")...)
	default:
		parts = append(parts, extractMessages(object["messages"], "")...)
		// Some OpenAI-compatible clients send a single prompt string.
		if len(parts) == 0 {
			if prompt := firstString(object, "prompt", "input"); prompt != "" {
				parts = append(parts, textPart{Text: prompt})
			}
		}
	}
	for _, part := range parts {
		features.InputTokens += estimateTokens(part.Text)
		features.MessageCount++
		if part.Tool {
			features.ToolCallCount++
		}
		if part.Code {
			features.CodeRatio += float64(estimateTokens(part.Text))
		}
	}
	// The latest content part is normally the newly appended user turn and is
	// not reusable. A client-supplied session/cache key identifies the scope;
	// it does not make the current turn cacheable.
	for i, part := range parts {
		if i == len(parts)-1 {
			continue
		}
		features.ReusableInputTokens += estimateTokens(part.Text)
	}
	if protocol == "responses" {
		if total, reusable, ok := codexInputTokenCounts(body, features.Model); ok {
			features.InputTokens = total
			features.ReusableInputTokens = reusable
		}
	}
	if features.InputTokens > 0 {
		features.CodeRatio /= float64(features.InputTokens)
	}
	features.ComplexityScore = EstimateComplexity(features)
	if features.CacheKey == "" {
		features.CacheKey = hashPrefix(parts, features.Model, protocol)
	}
	if features.SessionID == "" {
		features.SessionID = features.CacheKey
	}
	if features.EstimatedOutputTokens == 0 {
		features.EstimatedOutputTokens = estimateOutputTokens(object, features)
	}
	return features.Normalize(), nil
}

type textPart struct {
	Text string
	Role string
	Tool bool
	Code bool
}

func extractResponses(object map[string]any) []textPart {
	if object == nil {
		return nil
	}
	var out []textPart
	// Responses serializes instructions and tool definitions before the input
	// items. Keep that order so the final input item remains the non-reusable
	// suffix used by the cache forecast.
	out = append(out, extractResponseInstructions(object["instructions"])...)
	out = append(out, extractResponseTools(object["tools"])...)
	if text, ok := object["text"].(map[string]any); ok {
		if format, ok := text["format"]; ok {
			out = append(out, extractResponseSchema(format)...)
		}
	}
	out = append(out, extractResponseInput(object["input"])...)
	return out
}

func extractResponseInstructions(value any) []textPart {
	if value == nil {
		return nil
	}
	if text, ok := value.(string); ok {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []textPart{classifyText(text, "instructions")}
	}
	return extractResponseInput(value)
}

func extractResponseInput(value any) []textPart {
	var out []textPart
	switch item := value.(type) {
	case string:
		if strings.TrimSpace(item) != "" {
			out = append(out, classifyText(item, "user"))
		}
	case []any:
		for _, raw := range item {
			if text, ok := raw.(string); ok {
				if strings.TrimSpace(text) != "" {
					out = append(out, classifyText(text, "user"))
				}
				continue
			}
			out = append(out, extractResponseItem(raw)...)
		}
	case map[string]any:
		out = append(out, extractResponseItem(item)...)
	}
	return out
}

func extractResponseItem(value any) []textPart {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	typ := strings.ToLower(strings.TrimSpace(firstString(object, "type")))
	role := firstString(object, "role")
	if role == "" {
		role = typ
	}
	if role == "" {
		role = "user"
	}
	switch typ {
	case "message":
		if content, ok := object["content"]; ok {
			return extractMessagesWithRole(content, role)
		}
		return extractResponseTextFields(object, role, false)
	case "function_call":
		return extractResponseTextFields(object, role, true, "name", "arguments")
	case "function_call_output":
		return extractResponseTextFields(object, role, true, "output")
	case "custom_tool_call":
		return extractResponseTextFields(object, role, true, "name", "input")
	case "custom_tool_call_output":
		return extractResponseTextFields(object, role, true, "output")
	case "reasoning":
		// encrypted_content is an opaque provider token, not user text. Counting
		// its Base64 representation would turn a transport envelope into a
		// several-times larger client estimate.
		return extractResponseTextFields(object, role, false, "summary", "content")
	case "additional_tools":
		return extractResponseTools(object["tools"])
	default:
		return extractResponseTextFields(object, role, strings.Contains(typ, "tool"),
			"text", "input_text", "output_text", "content", "output", "input", "arguments", "name")
	}
}

func extractResponseTextFields(object map[string]any, role string, tool bool, keys ...string) []textPart {
	var out []textPart
	for _, key := range keys {
		value, ok := object[key]
		if !ok {
			continue
		}
		out = append(out, extractResponseValue(value, role, tool)...)
	}
	return out
}

func extractResponseValue(value any, role string, tool bool) []textPart {
	var out []textPart
	switch item := value.(type) {
	case string:
		if strings.TrimSpace(item) != "" {
			part := classifyText(item, role)
			part.Tool = part.Tool || tool
			out = append(out, part)
		}
	case []any:
		for _, raw := range item {
			out = append(out, extractResponseValue(raw, role, tool)...)
		}
	case map[string]any:
		// summary entries and content blocks carry text under one of these
		// fields. Do not recurse through arbitrary metadata or encrypted blobs.
		found := false
		for _, key := range []string{"text", "input_text", "output_text", "content", "output", "input", "arguments"} {
			if nested, ok := item[key]; ok {
				found = true
				out = append(out, extractResponseValue(nested, role, tool)...)
			}
		}
		if !found {
			if encoded, err := json.Marshal(item); err == nil {
				out = append(out, classifyText(string(encoded), role))
				if tool && len(out) > 0 {
					out[len(out)-1].Tool = true
				}
			}
		}
	default:
		if encoded, err := json.Marshal(item); err == nil {
			out = append(out, classifyText(string(encoded), role))
		}
	}
	return out
}

func extractResponseTools(value any) []textPart {
	var out []textPart
	switch item := value.(type) {
	case []any:
		for _, raw := range item {
			if encoded, err := json.Marshal(raw); err == nil {
				out = append(out, textPart{Text: string(encoded), Role: "tool", Tool: true, Code: true})
			}
		}
	case map[string]any:
		if encoded, err := json.Marshal(item); err == nil {
			out = append(out, textPart{Text: string(encoded), Role: "tool", Tool: true, Code: true})
		}
	}
	return out
}

func extractResponseSchema(value any) []textPart {
	encoded, err := json.Marshal(value)
	if err != nil || strings.TrimSpace(string(encoded)) == "null" {
		return nil
	}
	return []textPart{{Text: string(encoded), Role: "schema", Code: true}}
}

func extractMessages(value any, defaultRole string) []textPart {
	var out []textPart
	switch item := value.(type) {
	case string:
		if strings.TrimSpace(item) != "" {
			out = append(out, classifyText(item, defaultRole))
		}
	case []any:
		for _, raw := range item {
			obj, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role := firstString(obj, "role", "type")
			if role == "" {
				role = defaultRole
			}
			start := len(out)
			if content, ok := obj["content"]; ok {
				out = append(out, extractMessagesWithRole(content, role)...)
			} else if parts, ok := obj["parts"]; ok {
				out = append(out, extractMessagesWithRole(parts, role)...)
			} else if text := firstString(obj, "text", "input_text", "output_text"); text != "" {
				out = append(out, classifyText(text, role))
			}
			if strings.Contains(strings.ToLower(role), "tool") || strings.Contains(strings.ToLower(firstString(obj, "name")), "tool") {
				for i := start; i < len(out); i++ {
					if out[i].Role == role && out[i].Text != "" {
						out[i].Tool = true
					}
				}
			}
		}
	case map[string]any:
		if text := firstString(item, "text", "input_text", "output_text"); text != "" {
			out = append(out, classifyText(text, defaultRole))
		} else if content, ok := item["content"]; ok {
			out = append(out, extractMessagesWithRole(content, defaultRole)...)
		} else if parts, ok := item["parts"]; ok {
			out = append(out, extractMessagesWithRole(parts, defaultRole)...)
		}
	}
	return out
}

func extractMessagesWithRole(value any, role string) []textPart {
	var out []textPart
	switch item := value.(type) {
	case string:
		if strings.TrimSpace(item) != "" {
			out = append(out, classifyText(item, role))
		}
	case []any:
		for _, raw := range item {
			switch part := raw.(type) {
			case string:
				out = append(out, classifyText(part, role))
			case map[string]any:
				kind := firstString(part, "type")
				text := firstString(part, "text", "input_text", "output_text", "content")
				if text != "" {
					entry := classifyText(text, role)
					entry.Tool = strings.Contains(strings.ToLower(kind), "tool")
					out = append(out, entry)
				}
			}
		}
	case map[string]any:
		text := firstString(item, "text", "input_text", "output_text", "content")
		if text != "" {
			out = append(out, classifyText(text, role))
		}
	}
	return out
}

func extractGemini(value any, role string) []textPart {
	return extractMessages(value, role)
}

func classifyText(text, role string) textPart {
	lower := strings.ToLower(text)
	return textPart{Text: text, Role: role,
		Tool: strings.Contains(strings.ToLower(role), "tool") || strings.Contains(lower, "tool_call"),
		Code: strings.Contains(lower, "```") || strings.Contains(lower, "import ") || strings.Contains(lower, "func ") || strings.Contains(lower, "def ")}
}

func estimateTokens(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	// Four bytes per token is a conservative protocol-independent estimate for
	// English/code; rune count prevents tiny Unicode strings becoming zero.
	bytes := len([]byte(value))
	runes := utf8.RuneCountInString(value)
	byBytes := (bytes + 3) / 4
	byRunes := (runes + 1) / 2
	if byRunes > byBytes {
		return int64(byRunes)
	}
	return int64(byBytes)
}

func hashPrefix(parts []textPart, model, protocol string) string {
	var builder strings.Builder
	builder.WriteString(protocol)
	builder.WriteByte('\x00')
	builder.WriteString(model)
	for i, part := range parts {
		// A one-message request has no reusable prefix, but still needs a
		// request-specific fallback key to avoid aliasing every new session.
		if len(parts) > 1 && i == len(parts)-1 {
			break
		}
		builder.WriteByte('\x00')
		builder.WriteString(part.Role)
		builder.WriteByte('\x00')
		builder.WriteString(strings.TrimSpace(part.Text))
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return "mux_" + hex.EncodeToString(sum[:])
}

func estimateOutputTokens(object map[string]any, features RequestFeatures) int64 {
	if value := features.MaxOutputTokens; value > 0 {
		// max_tokens is an upper bound, not a likely output. A 25% prior is
		// less expensive to overestimate than selecting a cache on fiction.
		return maxInt64(1, value/4)
	}
	if features.MessageCount > 0 {
		base := maxInt64(64, features.InputTokens/4)
		// Conversation depth, tools, code, and reasoning effort are a useful
		// cold-start prior for output volume. Once durable usage is available,
		// the scheduler replaces this estimate with the observed average.
		factor := 0.75 + 0.75*features.ComplexityScore
		return maxInt64(64, int64(math.Ceil(float64(base)*factor)))
	}
	return 256
}

// EstimateComplexity produces a bounded protocol-independent prior from
// request size, conversation depth, tools, code, and explicit reasoning mode.
// It is intended for cold-start output/latency prediction; measured history
// should replace it as soon as enough samples exist.
func EstimateComplexity(features RequestFeatures) float64 {
	features = features.Normalize()
	length := math.Log1p(float64(features.InputTokens)) / math.Log1p(128_000)
	if length > 1 {
		length = 1
	}
	messages := math.Min(float64(features.MessageCount)/20, 1)
	tools := math.Min(float64(features.ToolCallCount)/5, 1)
	reasoning := float64(0)
	switch strings.ToLower(strings.TrimSpace(features.ReasoningEffort)) {
	case "minimal", "low":
		reasoning = 0.25
	case "medium":
		reasoning = 0.6
	case "high", "xhigh", "max":
		reasoning = 1
	}
	score := 0.3*length + 0.2*messages + 0.2*tools + 0.1*features.CodeRatio + 0.2*reasoning
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

func firstString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := object[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstInt(object map[string]any, keys ...string) int64 {
	for _, key := range keys {
		switch value := object[key].(type) {
		case float64:
			if value > 0 {
				return int64(value)
			}
		case json.Number:
			if parsed, err := strconv.ParseInt(string(value), 10, 64); err == nil && parsed > 0 {
				return parsed
			}
		}
	}
	return 0
}

func firstBool(object map[string]any, key string) bool {
	value, _ := object[key].(bool)
	return value
}

func headerValue(headers http.Header, keys ...string) string {
	if headers == nil {
		return ""
	}
	for _, key := range keys {
		if value := strings.TrimSpace(headers.Get(key)); value != "" {
			return value
		}
	}
	// A plain map may not have canonicalized keys.
	values := make(map[string]string, len(headers))
	for key, items := range headers {
		if len(items) > 0 {
			values[strings.ToLower(key)] = items[0]
		}
	}
	for _, key := range keys {
		if value := strings.TrimSpace(values[strings.ToLower(key)]); value != "" {
			return value
		}
	}
	return ""
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// StableHeaderKeys returns the known client/cache/session headers. It is
// useful to copy only routing-relevant metadata into an audit record.
func StableHeaderKeys() []string {
	keys := []string{"x-claude-code-session-id", "x-codex-window-id", "x-codex-session-id", "x-session-id", "session-id", "conversation-id", "conversation_id", "thread-id", "thread_id", "x-prompt-cache-key", "prompt-cache-key", "prompt_cache_key"}
	sort.Strings(keys)
	return keys
}
