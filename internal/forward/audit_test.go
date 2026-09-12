package forward

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResponseAuditParsesSplitResponsesEvent(t *testing.T) {
	audit := &responseAudit{stream: true}
	parts := []string{
		"event: response.comp",
		"leted\r\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{",
		"\"input_tokens\":12,\"output_tokens\":7,\"cache_creation_input_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":3}}}}\r\n\r\n",
	}
	for _, part := range parts {
		audit.feed([]byte(part))
	}
	audit.finish()
	if !audit.streamCompleted || audit.lastEvent != "response.completed" {
		t.Fatalf("completion audit mismatch: completed=%v event=%q", audit.streamCompleted, audit.lastEvent)
	}
	// Responses input_tokens is inclusive of cached_tokens; the audit stores
	// only the uncached portion so cost calculation cannot charge cache twice.
	if audit.usage.input != 9 || audit.usage.output != 7 || audit.usage.cached != 3 || audit.usage.cacheCreation != 5 {
		t.Fatalf("usage audit mismatch: %+v", audit.usage)
	}
}

func TestUsageAuditNormalizesCodexInputTokens(t *testing.T) {
	usage := usageFromJSON([]byte(`{"response":{"usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":80},"output_tokens":4}}}`), "codex")
	if usage.input != 40 || usage.cached != 80 || usage.output != 4 {
		t.Fatalf("codex usage mismatch: %+v", usage)
	}
}

func TestUsageAuditParsesAnthropicCacheTokens(t *testing.T) {
	usage := usageFromJSON([]byte(`{"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":30,"cache_creation_input_tokens":6}}`))
	if usage.input != 10 || usage.output != 4 || usage.cached != 30 || usage.cacheCreation != 6 {
		t.Fatalf("anthropic usage mismatch: %+v", usage)
	}
}

// OpenAI's prompt_tokens is inclusive of cached_tokens; Anthropic's input_tokens
// is exclusive. Downstream SQL (CacheCoverageRatio, TokenInflationFactor)
// assumes a single convention, so we normalize at parse time to Anthropic's
// "uncached input" semantics.
func TestUsageAuditNormalizesOpenAIPromptTokensToUncached(t *testing.T) {
	// prompt_tokens=1000 includes cached=200 → uncached=800.
	usage := usageFromJSON([]byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":200}}}`))
	if usage.input != 800 || usage.cached != 200 || usage.output != 50 {
		t.Fatalf("openai normalization failed: %+v", usage)
	}
}

// Gemini's promptTokenCount is inclusive of cachedContentTokenCount.
func TestUsageAuditNormalizesGeminiPromptTokensToUncached(t *testing.T) {
	usage := usageFromJSON([]byte(`{"usageMetadata":{"promptTokenCount":500,"candidatesTokenCount":40,"cachedContentTokenCount":150}}`))
	if usage.input != 350 || usage.cached != 150 || usage.output != 40 {
		t.Fatalf("gemini normalization failed: %+v", usage)
	}
}

// Guard: if cached > prompt_tokens (garbage upstream), clamp to zero
// instead of going negative.
func TestUsageAuditClampsNegativeUncached(t *testing.T) {
	usage := usageFromJSON([]byte(`{"usage":{"prompt_tokens":10,"cached_tokens":50}}`))
	if usage.input != 0 || usage.cached != 50 {
		t.Fatalf("negative uncached must clamp to zero: %+v", usage)
	}
}

func TestContentBlockStopIsNotWholeStreamCompletion(t *testing.T) {
	audit := &responseAudit{stream: true}
	audit.feed([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\"}\n\n"))
	if audit.streamCompleted {
		t.Fatal("content_block_stop must not mark the whole message complete")
	}
	audit.feed([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	if !audit.streamCompleted || audit.lastEvent != "message_stop" {
		t.Fatalf("message_stop should complete the stream: %+v", audit)
	}
}

func TestRelayResponseCapturesUsageBytesAndRequestID(t *testing.T) {
	body := `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":9,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":2}}}`
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	header.Set("X-Request-ID", "upstream-123")
	resp := &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body))}
	recorder := httptest.NewRecorder()
	result := relayResponse(recorder, resp, time.Now(), nil)
	if result.err != nil || result.bytesSent != int64(len(body)) {
		t.Fatalf("relay result mismatch: %+v", result)
	}
	// input is normalized to Anthropic-style "uncached" semantics: OpenAI
	// prompt_tokens (9) is inclusive of cached (2), so uncached = 9 - 2 = 7.
	if result.usage.input != 7 || result.usage.output != 4 || result.usage.cached != 2 {
		t.Fatalf("usage mismatch: %+v", result.usage)
	}
	if result.upstreamRequestID != "upstream-123" {
		t.Fatalf("request id mismatch: %q", result.upstreamRequestID)
	}
}
