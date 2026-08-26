// 延迟工具（deferred tool loading）净化。
//
// 新版 Claude Code 在 ToolSearch / deferred 模式下会把大部分工具定义从请求的
// tools[] 中省略，只发送一个名为 DeferredToolPlaceholder 的占位工具（带
// defer_loading: true），被省略工具的名字仅出现在 system prompt 里。Anthropic
// 官方端点会在服务端按需注入 schema，而普通第三方上游拿不到这些定义：模型一旦
// 按名单调用被省略的工具，上游直接 400（"cannot call tools omitted from tools[]"）。
//
// 网关无法凭空恢复精确 schema（报文里根本没有），这里做两层兜底：
//  1. 净化：删除占位工具、剥除 defer_loading 字段；
//  2. 注入：消息历史里被 tool_use 引用却未在上游 tools[] 中定义的工具，
//     补一个宽松 schema 的 stub 定义。CC 客户端会按真实 schema 校验入参并把
//     错误作为 tool_result 回给模型自纠，功能可用性优先。
package translate

import (
	"encoding/json"
)

// deferredToolPlaceholder 是 Claude Code 在 deferred 模式下发送的保留占位工具名。
const deferredToolPlaceholder = "DeferredToolPlaceholder"

// stubToolDescription 说明该定义由网关注入，参数由客户端校验兜底。
const stubToolDescription = "Tool loaded without a local schema; the client validates arguments and reports errors back."

// sanitizeDeferredTools 在 NewExchange 组装完 UpstreamRequest 后调用，
// 仅处理 Claude 入口；其他入口协议没有该机制，请求保持原样。
func sanitizeDeferredTools(e *Exchange) {
	if e == nil || e.Source != Claude || len(e.UpstreamRequest) == 0 {
		return
	}

	var body map[string]json.RawMessage
	if json.Unmarshal(e.UpstreamRequest, &body) != nil {
		return
	}
	rawTools, hasTools := body["tools"]
	referenced := claudeReferencedToolNames(e.OriginalRequest)
	if !hasTools && len(referenced) == 0 {
		return // 普通请求，快速路径
	}

	var tools []map[string]any
	if hasTools {
		if json.Unmarshal(rawTools, &tools) != nil {
			return
		}
	}

	defined := make(map[string]bool, len(tools))
	kept := make([]map[string]any, 0, len(tools)+len(referenced))
	stripped := false
	for _, tool := range tools {
		name := toolName(tool, e.Target)
		if name == deferredToolPlaceholder {
			continue // 占位工具对上游毫无意义，直接丢弃
		}
		if strippedDeferredLoading(tool) {
			stripped = true
		}
		if name != "" {
			defined[name] = true
		}
		kept = append(kept, tool)
	}

	injected := false
	for name := range referenced {
		if defined[name] {
			continue
		}
		kept = append(kept, stubTool(name, e.Target))
		defined[name] = true
		injected = true
	}

	if !stripped && !injected && len(kept) == len(tools) {
		return
	}
	next, err := json.Marshal(kept)
	if err != nil || !json.Valid(next) {
		return
	}
	body["tools"] = next
	out, err := json.Marshal(body)
	if err != nil || !json.Valid(out) {
		return
	}
	e.UpstreamRequest = out
}

// toolName 按目标形状取工具名：翻译后的 openai 形状在 function.name，
// 透传的 anthropic / Responses 形状在顶层 name。
func toolName(tool map[string]any, target Format) string {
	if target == OpenAI || target == OpenAIResponses || target == Codex {
		if fn, ok := tool["function"].(map[string]any); ok {
			name, _ := fn["name"].(string)
			return name
		}
	}
	name, _ := tool["name"].(string)
	return name
}

// strippedDeferredLoading 剥除工具定义上的 defer_loading 字段（新旧位置都查）。
func strippedDeferredLoading(tool map[string]any) bool {
	stripped := false
	if _, has := tool["defer_loading"]; has {
		delete(tool, "defer_loading")
		stripped = true
	}
	if fn, ok := tool["function"].(map[string]any); ok {
		if _, has := fn["defer_loading"]; has {
			delete(fn, "defer_loading")
			stripped = true
		}
	}
	return stripped
}

// stubTool 按目标形状生成宽松 schema 的兜底定义。参数不合规时由 CC 客户端
// 按真实 schema 校验并把错误作为 tool_result 回给模型自纠。
func stubTool(name string, target Format) map[string]any {
	switch target {
	case OpenAI:
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": stubToolDescription,
				"parameters":  map[string]any{"type": "object"},
			},
		}
	default: // passthrough claude 及其他目标：anthropic 工具形状
		return map[string]any{
			"name":         name,
			"description":  stubToolDescription,
			"input_schema": map[string]any{"type": "object"},
		}
	}
}

// claudeReferencedToolNames 收集原始 Claude 请求中所有 tool_use 引用的工具名。
func claudeReferencedToolNames(original []byte) map[string]bool {
	names := make(map[string]bool)
	if len(original) == 0 {
		return names
	}
	var body struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(original, &body) != nil {
		return names
	}
	for _, msg := range body.Messages {
		if len(msg.Content) == 0 || msg.Content[0] != '[' {
			continue // 字符串内容没有工具调用
		}
		var blocks []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type == "tool_use" && block.Name != "" {
				names[block.Name] = true
			}
		}
	}
	return names
}
