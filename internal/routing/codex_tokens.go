package routing

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tiktoken-go/tokenizer"
)

// codexInputTokenCounts mirrors the Codex executor's CountTokens path. The
// provider counts selected semantic fields with the GPT-family tokenizer, not
// the JSON envelope or every field in every Responses item.
//
// The second result counts the reusable prefix: request instructions, prior
// input items, tools, and the response schema. The final input item is treated
// as the new suffix and is therefore excluded from the reusable prefix.
func codexInputTokenCounts(body []byte, model string) (total, reusable int64, ok bool) {
	if len(body) == 0 {
		return 0, 0, true
	}
	enc, err := codexTokenizer(model)
	if err != nil {
		return 0, 0, false
	}
	root := gjson.ParseBytes(body)
	var all, prefix []string
	appendSegment := func(value string, reusableSegment bool) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		all = append(all, value)
		if reusableSegment {
			prefix = append(prefix, value)
		}
	}

	inputItems := root.Get("input")
	hasInput := inputItems.IsArray() && len(inputItems.Array()) > 0
	appendSegment(root.Get("instructions").String(), hasInput)
	if inputItems.IsArray() {
		items := inputItems.Array()
		for i, item := range items {
			segments := codexInputItemSegments(item)
			for _, segment := range segments {
				appendSegment(segment, i < len(items)-1)
			}
		}
	} else if inputItems.Type == gjson.String {
		// Responses also permits the compact input string form. The forwarding
		// layer expands it before translation, so count it here as one message.
		appendSegment(inputItems.String(), false)
	}

	// Keep this order in sync with CLIProxyAPI's Codex CountTokens path:
	// instructions, input items, tools, then response schema.
	tools := root.Get("tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			var segments []string
			appendInlineTools(tool, &segments)
			for _, segment := range segments {
				appendSegment(segment, true)
			}
		}
	}
	textFormat := root.Get("text.format")
	if textFormat.Exists() {
		appendSegment(textFormat.Get("name").String(), true)
		appendRawSegment(textFormat.Get("schema"), &all, &prefix)
	}

	allText := strings.Join(all, "\n")
	if allText == "" {
		return 0, 0, true
	}
	totalCount, err := enc.Count(allText)
	if err != nil {
		return 0, 0, false
	}
	prefixText := strings.Join(prefix, "\n")
	prefixCount := 0
	if prefixText != "" {
		prefixCount, err = enc.Count(prefixText)
		if err != nil {
			return 0, 0, false
		}
	}
	return int64(totalCount), int64(prefixCount), true
}

func codexInputItemSegments(item gjson.Result) []string {
	var out []string
	appendValue := func(value gjson.Result) {
		if text := strings.TrimSpace(value.String()); text != "" {
			out = append(out, text)
		}
	}
	appendStructured := func(value gjson.Result) {
		if !value.Exists() || value.Type == gjson.Null {
			return
		}
		if value.Type == gjson.String {
			appendValue(value)
			return
		}
		if text := strings.TrimSpace(value.Raw); text != "" && text != "null" {
			out = append(out, text)
		}
	}
	appendContent := func(value gjson.Result) {
		if value.Type == gjson.String {
			appendValue(value)
			return
		}
		if !value.IsArray() {
			return
		}
		for _, part := range value.Array() {
			if part.Type == gjson.String {
				appendValue(part)
				continue
			}
			for _, key := range []string{"text", "input_text", "output_text"} {
				if text := part.Get(key); text.Exists() {
					appendValue(text)
					break
				}
			}
		}
	}
	switch item.Get("type").String() {
	case "message":
		appendContent(item.Get("content"))
	case "function_call":
		appendValue(item.Get("name"))
		appendStructured(item.Get("arguments"))
	case "function_call_output":
		appendStructured(item.Get("output"))
	case "custom_tool_call":
		appendValue(item.Get("name"))
		appendStructured(item.Get("input"))
	case "custom_tool_call_output":
		appendStructured(item.Get("output"))
	case "additional_tools":
		appendInlineTools(item.Get("tools"), &out)
	case "reasoning", "compaction", "compaction_trigger":
		// Encrypted reasoning/compaction is opaque provider state, not text.
	default:
		// Native Codex clients often omit type for ordinary role/content items.
		if item.Get("content").Exists() {
			appendContent(item.Get("content"))
		} else if item.Get("text").Exists() {
			appendValue(item.Get("text"))
		} else if item.Get("output").Exists() {
			appendStructured(item.Get("output"))
		} else if item.Get("input").Exists() {
			appendStructured(item.Get("input"))
		}
	}
	return out
}

func appendInlineTools(value gjson.Result, out *[]string) {
	if !value.Exists() {
		return
	}
	items := value.Array()
	if !value.IsArray() {
		items = []gjson.Result{value}
	}
	for _, tool := range items {
		for _, key := range []string{"name", "description"} {
			if text := strings.TrimSpace(tool.Get(key).String()); text != "" {
				*out = append(*out, text)
			}
		}
		for _, key := range []string{"parameters", "input_schema", "schema"} {
			if raw := tool.Get(key); raw.Exists() {
				text := strings.TrimSpace(raw.Raw)
				if raw.Type == gjson.String {
					text = strings.TrimSpace(raw.String())
				}
				if text != "" && text != "null" {
					*out = append(*out, text)
				}
			}
		}
		if nested := tool.Get("tools"); nested.Exists() {
			appendInlineTools(nested, out)
		}
	}
}

func appendRawSegment(value gjson.Result, all, prefix *[]string) {
	if !value.Exists() {
		return
	}
	text := value.Raw
	if value.Type == gjson.String {
		text = value.String()
	}
	text = strings.TrimSpace(text)
	if text == "" || text == "null" {
		return
	}
	*all = append(*all, text)
	*prefix = append(*prefix, text)
}

func codexTokenizer(model string) (tokenizer.Codec, error) {
	sanitized := strings.ToLower(strings.TrimSpace(model))
	switch {
	case sanitized == "":
		return tokenizer.Get(tokenizer.Cl100kBase)
	case strings.HasPrefix(sanitized, "gpt-5"):
		return tokenizer.ForModel(tokenizer.GPT5)
	case strings.HasPrefix(sanitized, "gpt-4.1"):
		return tokenizer.ForModel(tokenizer.GPT41)
	case strings.HasPrefix(sanitized, "gpt-4o"):
		return tokenizer.ForModel(tokenizer.GPT4o)
	case strings.HasPrefix(sanitized, "gpt-4"):
		return tokenizer.ForModel(tokenizer.GPT4)
	case strings.HasPrefix(sanitized, "gpt-3.5"), strings.HasPrefix(sanitized, "gpt-3"):
		return tokenizer.ForModel(tokenizer.GPT35Turbo)
	default:
		return tokenizer.Get(tokenizer.Cl100kBase)
	}
}
