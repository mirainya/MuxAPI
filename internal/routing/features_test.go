package routing

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestExtractClaudeFeaturesPreservesSessionScope(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-5",
		"system":"You are a coding assistant.",
		"messages":[
			{"role":"user","content":"Inspect this repository and explain the scheduler."},
			{"role":"assistant","content":"I found a priority scheduler."},
			{"role":"user","content":"Now optimize it."}
		],
		"max_tokens":800,
		"stream":true
	}`)
	features, err := ExtractRequestFeatures(body, FeatureOptions{
		Protocol: "anthropic",
		Headers:  http.Header{"X-Claude-Code-Session-Id": []string{"session-42"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if features.Protocol != "claude" || features.Model != "claude-sonnet-4-5" {
		t.Fatalf("unexpected protocol/model: %+v", features)
	}
	if features.SessionID != "session-42" {
		t.Fatalf("session ID = %q", features.SessionID)
	}
	if features.InputTokens <= 0 || features.ReusableInputTokens <= 0 || features.ReusableInputTokens >= features.InputTokens {
		t.Fatalf("bad token estimates: %+v", features)
	}
	if features.EstimatedOutputTokens != 200 || !features.Stream {
		t.Fatalf("bad output/stream inference: %+v", features)
	}
}

func TestExtractGeminiFeatures(t *testing.T) {
	body := []byte(`{
		"contents":[
			{"role":"user","parts":[{"text":"Write a Go function."}]},
			{"role":"model","parts":[{"text":"Here is the function."}]},
			{"role":"user","parts":[{"text":"Add tests."}]}
		],
		"generationConfig":{"maxOutputTokens":400}
	}`)
	features, err := ExtractRequestFeatures(body, FeatureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if features.Protocol != "gemini" || features.InputTokens <= 0 || features.ReusableInputTokens <= 0 {
		t.Fatalf("Gemini request was not extracted: %+v", features)
	}
	if features.MaxOutputTokens != 400 || features.EstimatedOutputTokens != 100 {
		t.Fatalf("Gemini generation config was not extracted: %+v", features)
	}
	if !strings.HasPrefix(features.CacheKey, "mux_") || len(features.CacheKey) != 68 {
		t.Fatalf("invalid derived cache key %q", features.CacheKey)
	}
}

func TestDerivedCacheKeyTracksStablePrefix(t *testing.T) {
	first := []byte(`{"model":"gpt-5","messages":[{"role":"system","content":"same rules"},{"role":"user","content":"first turn"}]}`)
	second := []byte(`{"model":"gpt-5","messages":[{"role":"system","content":"same rules"},{"role":"user","content":"different turn"}]}`)
	a, err := ExtractRequestFeatures(first, FeatureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := ExtractRequestFeatures(second, FeatureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.CacheKey != b.CacheKey {
		t.Fatalf("same reusable prefix should share key: %s != %s", a.CacheKey, b.CacheKey)
	}
	if a.ReusableInputTokens <= 0 || b.ReusableInputTokens <= 0 {
		t.Fatalf("stable prefix not detected: %+v / %+v", a, b)
	}
}

func TestSingleTurnDerivedKeysDoNotAlias(t *testing.T) {
	a, _ := ExtractRequestFeatures([]byte(`{"messages":[{"role":"user","content":"alpha"}]}`), FeatureOptions{})
	b, _ := ExtractRequestFeatures([]byte(`{"messages":[{"role":"user","content":"beta"}]}`), FeatureOptions{})
	if a.CacheKey == b.CacheKey {
		t.Fatalf("unrelated one-shot requests share cache key %q", a.CacheKey)
	}
	if a.ReusableInputTokens != 0 || b.ReusableInputTokens != 0 {
		t.Fatalf("one-shot requests should not predict a reusable prefix: %+v / %+v", a, b)
	}
}

func TestExtractResponsesCountsInstructionsToolsAndToolItems(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5-codex",
		"instructions":"You are a repository coding assistant.",
		"tools":[{"type":"function","name":"read_file","description":"Read a file from the workspace.","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}],
		"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"summary":{"type":"string"}}}}},
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Inspect the project."}]},
			{"type":"function_call","name":"read_file","arguments":"{\"path\":\"README.md\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"# MuxAPI"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"Reviewing the repository."}],"encrypted_content":"opaque-provider-payload"}
		],
		"max_output_tokens":400
	}`)
	features, err := ExtractRequestFeatures(body, FeatureOptions{Protocol: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if features.Protocol != "responses" || features.InputTokens < 50 {
		t.Fatalf("Responses fields were not counted: %+v", features)
	}
	if features.ToolCallCount < 3 || features.MessageCount < 7 {
		t.Fatalf("tool/schema items were not represented: %+v", features)
	}
	if features.EstimatedOutputTokens != 100 {
		t.Fatalf("max output estimate = %d, want 100", features.EstimatedOutputTokens)
	}
}

func TestExtractResponsesDoesNotCountEncryptedContentAsBase64Text(t *testing.T) {
	base := `{"model":"gpt-5","input":[{"type":"reasoning","summary":[{"text":"short"}],"encrypted_content":"%s"}]}`
	small, err := ExtractRequestFeatures([]byte(fmt.Sprintf(base, "a")), FeatureOptions{Protocol: "responses"})
	if err != nil {
		t.Fatal(err)
	}
	large, err := ExtractRequestFeatures([]byte(fmt.Sprintf(base, strings.Repeat("a", 100000))), FeatureOptions{Protocol: "responses"})
	if err != nil {
		t.Fatal(err)
	}
	if large.InputTokens != small.InputTokens {
		t.Fatalf("encrypted content changed estimate: small=%d large=%d", small.InputTokens, large.InputTokens)
	}
}

func TestExtractResponsesUsesCodexTokenizerAndPrefixCount(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"instructions":"You are a coding assistant.",
		"tools":[{"type":"function","name":"read_file","description":"Read a file.","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}],
		"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"summary":{"type":"string"}}}}},
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Inspect the repository."}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Now explain the result."}]}
		]
	}`)
	features, err := ExtractRequestFeatures(body, FeatureOptions{Protocol: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	total, reusable, ok := codexInputTokenCounts(body, features.Model)
	if !ok {
		t.Fatal("Codex tokenizer count unavailable")
	}
	if features.InputTokens != total || features.ReusableInputTokens != reusable {
		t.Fatalf("tokenizer counts not published: features=%+v total=%d reusable=%d", features, total, reusable)
	}

	// The Responses JSON envelope and uncounted metadata must not affect the
	// provider-aligned count.
	withMetadata := []byte(strings.Replace(string(body),
		`"tools":`, `"metadata":{"large":"`+strings.Repeat("x", 10000)+`"},"tools":`, 1))
	metadataFeatures, err := ExtractRequestFeatures(withMetadata, FeatureOptions{Protocol: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if metadataFeatures.InputTokens != total || metadataFeatures.ReusableInputTokens != reusable {
		t.Fatalf("uncounted Responses metadata changed count unexpectedly: got=%d/%d base=%d/%d", metadataFeatures.InputTokens, metadataFeatures.ReusableInputTokens, total, reusable)
	}
}

func TestCodexTokenizerCountsNativeMessageShapes(t *testing.T) {
	base := `{"model":"gpt-5","input":[{"role":"user","content":%s},{"type":"function_call_output","output":%s}]}`
	body := []byte(fmt.Sprintf(base,
		`"`+strings.Repeat("visible message ", 100)+`"`,
		`{"type":"input_text","text":"`+strings.Repeat("tool result ", 100)+`"}`))
	features, err := ExtractRequestFeatures(body, FeatureOptions{Protocol: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if features.InputTokens < 400 {
		t.Fatalf("native content/output shapes were under-counted: %+v", features)
	}
}

func TestCodexTokenizerCountsInlineAdditionalTools(t *testing.T) {
	body := []byte(`{"model":"gpt-5","input":[{"type":"additional_tools","tools":[{"type":"function","name":"search","description":"Search the workspace.","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}]},{"role":"user","content":"run search"}]}`)
	features, err := ExtractRequestFeatures(body, FeatureOptions{Protocol: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if features.InputTokens < 20 {
		t.Fatalf("inline additional_tools were not counted: %+v", features)
	}
}
