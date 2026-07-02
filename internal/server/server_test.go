package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/liwei/commandcode-proxy-go/internal/config"
)

// ccStream returns a handler that emits the given newline-delimited event lines
// under the given HTTP status, standing in for Command Code's /alpha/generate.
func ccStream(status int, lines ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		for _, l := range lines {
			io.WriteString(w, l+"\n")
		}
	}
}

func testServer(genURL string, aliases map[string]string) *Server {
	if aliases == nil {
		aliases = map[string]string{}
	}
	return New(&config.Config{
		GenerateURL:        genURL,
		ModelsURL:          genURL + "/models",
		DefaultTemperature: 0.3,
		DefaultMaxTokens:   32000,
		MaxTokensCap:       64000,
		MaxRetries:         2,
		ModelAliases:       aliases,
		LogLevel:           "warn", // keep route-test output quiet
	})
}

func do(srv *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return m
}

const authHdr = "Authorization"

func TestHealth(t *testing.T) {
	srv := testServer("http://unused", nil)
	rec := do(srv, "GET", "/health", "", nil)
	body := decode(t, rec)
	if rec.Code != 200 || body["status"] != "ok" {
		t.Fatalf("health: %d %s", rec.Code, rec.Body)
	}
	if body["version"] != "dev" {
		t.Fatalf("version=%v", body["version"])
	}
}

func TestChatMissingKey(t *testing.T) {
	srv := testServer("http://unused", nil)
	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m","messages":[]}`, nil)
	if rec.Code != 401 {
		t.Fatalf("status %d", rec.Code)
	}
	if decode(t, rec)["error"].(map[string]any)["type"] != "authentication_error" {
		t.Fatalf("body %s", rec.Body)
	}
}

func TestChatModelRequired(t *testing.T) {
	srv := testServer("http://unused", nil)
	rec := do(srv, "POST", "/v1/chat/completions", `{"messages":[]}`, map[string]string{authHdr: "Bearer user_x"})
	if rec.Code != 400 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestChatNonStreamingSuccess(t *testing.T) {
	cc := httptest.NewServer(ccStream(200,
		`{"type":"text-delta","text":"Hi"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":1}}`,
	))
	defer cc.Close()
	srv := testServer(cc.URL, nil)
	rec := do(srv, "POST", "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{authHdr: "Bearer user_x"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	choice := body["choices"].([]any)[0].(map[string]any)
	if msg := choice["message"].(map[string]any); msg["content"] != "Hi" {
		t.Fatalf("content=%v", msg["content"])
	}
	if usage := body["usage"].(map[string]any); usage["total_tokens"].(float64) != 2 {
		t.Fatalf("usage=%v", body["usage"])
	}
}

func TestChatToolCall(t *testing.T) {
	cc := httptest.NewServer(ccStream(200,
		`{"type":"tool-call","toolCallId":"call_1","toolName":"get_weather","input":{"city":"SF"}}`,
		`{"type":"finish","finishReason":"tool-calls"}`,
	))
	defer cc.Close()
	srv := testServer(cc.URL, nil)
	rec := do(srv, "POST", "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"weather?"}]}`,
		map[string]string{authHdr: "Bearer user_x"})
	body := decode(t, rec)
	choice := body["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish=%v", choice["finish_reason"])
	}
	tc := choice["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if tc["function"].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("tool_calls=%v", choice["message"])
	}
}

func TestChatModelAliasResolves(t *testing.T) {
	cc := httptest.NewServer(ccStream(200,
		`{"type":"text-delta","text":"Hi"}`,
		`{"type":"finish","finishReason":"stop"}`,
	))
	defer cc.Close()
	srv := testServer(cc.URL, map[string]string{"sonnet": "anthropic/claude-x"})
	rec := do(srv, "POST", "/v1/chat/completions",
		`{"model":"sonnet","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{authHdr: "Bearer user_x"})
	if decode(t, rec)["model"] != "anthropic/claude-x" {
		t.Fatalf("model=%v", decode(t, rec)["model"])
	}
}

func TestChatUpstreamErrorClean(t *testing.T) {
	cc := httptest.NewServer(ccStream(403,
		`{"success":false,"error":{"code":"FORBIDDEN","status":403,"message":"MODEL_NOT_IN_PLAN: nope"}}`))
	defer cc.Close()
	srv := testServer(cc.URL, nil)
	rec := do(srv, "POST", "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{authHdr: "Bearer user_x"})
	if rec.Code != 403 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	e := decode(t, rec)["error"].(map[string]any)
	if e["code"] != "FORBIDDEN" || e["type"] != "upstream_error" || !strings.Contains(e["message"].(string), "MODEL_NOT_IN_PLAN") {
		t.Fatalf("error=%v", e)
	}
}

func TestChatStreamingDone(t *testing.T) {
	cc := httptest.NewServer(ccStream(200,
		`{"type":"text-delta","text":"yo"}`,
		`{"type":"finish","finishReason":"stop"}`,
	))
	defer cc.Close()
	srv := testServer(cc.URL, nil)
	rec := do(srv, "POST", "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		map[string]string{authHdr: "Bearer user_x"})
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("no DONE: %s", rec.Body)
	}
}

func TestChatBodyTooLarge(t *testing.T) {
	srv := testServer("http://unused", nil)
	body := `{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("x", maxBodyBytes) + `"}]}`
	rec := do(srv, "POST", "/v1/chat/completions", body, map[string]string{authHdr: "Bearer user_x"})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", rec.Code)
	}
}

func TestChatImagePartForwarded(t *testing.T) {
	var upstreamContent any
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &body)
		msgs := body["params"].(map[string]any)["messages"].([]any)
		upstreamContent = msgs[0].(map[string]any)["content"]
		w.WriteHeader(200)
		io.WriteString(w, `{"type":"text-delta","text":"Red"}`+"\n")
		io.WriteString(w, `{"type":"finish","finishReason":"stop"}`+"\n")
	}))
	defer cc.Close()
	srv := testServer(cc.URL, nil)
	rec := do(srv, "POST", "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":[
			{"type":"text","text":"what color?"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`,
		map[string]string{authHdr: "Bearer user_x"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	parts, ok := upstreamContent.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("upstream content=%v, want 2 typed parts", upstreamContent)
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image" || img["image"] != "data:image/png;base64,AAA" {
		t.Fatalf("image part=%v", img)
	}
}

// ── Anthropic /v1/messages ───────────────────────────────────────────────────

func TestAnthropicMissingKey(t *testing.T) {
	srv := testServer("http://unused", nil)
	rec := do(srv, "POST", "/v1/messages", `{"model":"m","max_tokens":16,"messages":[]}`, nil)
	if rec.Code != 401 {
		t.Fatalf("status %d", rec.Code)
	}
	b := decode(t, rec)
	if b["type"] != "error" || b["error"].(map[string]any)["type"] != "authentication_error" {
		t.Fatalf("body %s", rec.Body)
	}
}

func TestAnthropicMaxTokensRequired(t *testing.T) {
	srv := testServer("http://unused", nil)
	rec := do(srv, "POST", "/v1/messages", `{"model":"m","messages":[]}`, map[string]string{"x-api-key": "user_x"})
	if rec.Code != 400 || decode(t, rec)["error"].(map[string]any)["type"] != "invalid_request_error" {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestAnthropicNonStreamingSuccess(t *testing.T) {
	cc := httptest.NewServer(ccStream(200,
		`{"type":"text-delta","text":"Hi"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":1}}`,
	))
	defer cc.Close()
	srv := testServer(cc.URL, nil)
	rec := do(srv, "POST", "/v1/messages",
		`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": "user_x"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	b := decode(t, rec)
	if b["type"] != "message" || b["role"] != "assistant" {
		t.Fatalf("body %s", rec.Body)
	}
	block := b["content"].([]any)[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "Hi" {
		t.Fatalf("content=%v", b["content"])
	}
	u := b["usage"].(map[string]any)
	if u["input_tokens"].(float64) != 1 || u["output_tokens"].(float64) != 1 {
		t.Fatalf("usage=%v", u)
	}
}

func TestAnthropicModelAlias(t *testing.T) {
	var upstreamModel string
	cc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &body)
		if params, ok := body["params"].(map[string]any); ok {
			upstreamModel, _ = params["model"].(string)
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"type":"text-delta","text":"hi"}`+"\n")
		io.WriteString(w, `{"type":"finish","finishReason":"stop"}`+"\n")
	}))
	defer cc.Close()
	srv := testServer(cc.URL, map[string]string{"claude-opus-4-8": "deepseek/deepseek-v4-pro"})
	rec := do(srv, "POST", "/v1/messages",
		`{"model":"claude-opus-4-8","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": "user_x"})
	// the alias is applied to the UPSTREAM request
	if upstreamModel != "deepseek/deepseek-v4-pro" {
		t.Fatalf("upstream model=%q, want deepseek/deepseek-v4-pro", upstreamModel)
	}
	// but the response echoes the REQUESTED model, so Claude Code can restore the
	// session (it would reject an unrecognized upstream id like deepseek/...)
	if decode(t, rec)["model"] != "claude-opus-4-8" {
		t.Fatalf("response model=%v, want claude-opus-4-8", decode(t, rec)["model"])
	}
}

func TestCountTokens(t *testing.T) {
	srv := testServer("http://unused", nil) // no upstream call is made
	rec := do(srv, "POST", "/v1/messages/count_tokens",
		`{"model":"m","messages":[{"role":"user","content":"hello world"}]}`,
		map[string]string{"x-api-key": "user_x"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if n := decode(t, rec)["input_tokens"].(float64); n <= 0 {
		t.Fatalf("input_tokens=%v", n)
	}
}

func TestCountTokensMissingKey(t *testing.T) {
	srv := testServer("http://unused", nil)
	rec := do(srv, "POST", "/v1/messages/count_tokens", `{"model":"m","messages":[]}`, nil)
	if rec.Code != 401 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestAnthropicStreamingEvents(t *testing.T) {
	cc := httptest.NewServer(ccStream(200,
		`{"type":"text-delta","text":"yo"}`, `{"type":"finish","finishReason":"stop"}`))
	defer cc.Close()
	srv := testServer(cc.URL, nil)
	rec := do(srv, "POST", "/v1/messages",
		`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		map[string]string{"x-api-key": "user_x"})
	text := rec.Body.String()
	if !strings.Contains(text, "event: message_start") || !strings.Contains(text, "event: message_stop") {
		t.Fatalf("missing events: %s", text)
	}
}

func TestAnthropicUpstreamError(t *testing.T) {
	cc := httptest.NewServer(ccStream(403,
		`{"success":false,"error":{"code":"FORBIDDEN","status":403,"message":"MODEL_NOT_IN_PLAN: nope"}}`))
	defer cc.Close()
	srv := testServer(cc.URL, nil)
	rec := do(srv, "POST", "/v1/messages",
		`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": "user_x"})
	if rec.Code != 403 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	b := decode(t, rec)
	if b["type"] != "error" || b["error"].(map[string]any)["type"] != "permission_error" {
		t.Fatalf("body %s", rec.Body)
	}
	if !strings.Contains(b["error"].(map[string]any)["message"].(string), "MODEL_NOT_IN_PLAN") {
		t.Fatalf("message=%v", b["error"])
	}
}
