package server

// Live end-to-end smoke test against the real Command Code API.
//
// Opt-in: requires RUN_LIVE=1 and COMMANDCODE_API_KEY (relayed to the proxy as
// the Authorization header). It runs the real handler in a local httptest server
// pointed at the live upstream (config.Load() defaults). Model defaults to one
// that works on the Go plan; override with COMMANDCODE_SMOKE_MODEL. Plan errors
// (402 / MODEL_NOT_IN_PLAN) count as a pass — they still prove the request was
// translated and reached Command Code.

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/liwei/commandcode-proxy-go/internal/config"
)

func liveServer(t *testing.T) (url, key string, client *http.Client) {
	t.Helper()
	if os.Getenv("RUN_LIVE") != "1" {
		t.Skip("set RUN_LIVE=1 to run live tests")
	}
	key = os.Getenv("COMMANDCODE_API_KEY")
	if key == "" {
		t.Skip("set COMMANDCODE_API_KEY to run live tests")
	}
	ts := httptest.NewServer(New(config.Load()).Handler())
	t.Cleanup(ts.Close)
	return ts.URL, key, &http.Client{Timeout: 120 * time.Second}
}

func smokeModel() string {
	if m := os.Getenv("COMMANDCODE_SMOKE_MODEL"); m != "" {
		return m
	}
	return "Qwen/Qwen3.7-Plus"
}

func liveChatBody(stream bool) string {
	s := "false"
	if stream {
		s = "true"
	}
	return `{"model":"` + smokeModel() + `","messages":[{"role":"user","content":"Reply with exactly: PONG"}],"max_tokens":50,"stream":` + s + `}`
}

func TestLiveModels(t *testing.T) {
	url, key, client := liveServer(t)
	req, _ := http.NewRequest("GET", url+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	if data["object"] != "list" {
		t.Fatalf("object=%v", data["object"])
	}
	if arr, _ := data["data"].([]any); len(arr) == 0 {
		t.Fatal("no models returned")
	}
}

func TestLiveNonStreamingCompletion(t *testing.T) {
	url, key, client := liveServer(t)
	req, _ := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(liveChatBody(false)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	// Plan-gated models legitimately 402 — that still proves the pipeline works.
	if resp.StatusCode != 200 && resp.StatusCode != 402 {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if resp.StatusCode == 200 {
		var data map[string]any
		json.Unmarshal(b, &data)
		choice := data["choices"].([]any)[0].(map[string]any)
		if content, _ := choice["message"].(map[string]any)["content"].(string); strings.TrimSpace(content) == "" {
			t.Fatalf("empty content: %s", b)
		}
	}
}

// redPNG32 is a 32x32 solid-red PNG (upstream rejects images ≤ 10px per side).
const redPNG32 = "iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAIAAAD8GO2jAAAAKElEQVR4nO3NsQ0AAAzCMP5/un0CNkuZ41wybXsHAAAAAAAAAAAAxR4yw/wuPL6QkAAAAABJRU5ErkJggg=="

// postMessages POSTs an Anthropic /v1/messages body and decodes the response.
func postMessages(t *testing.T, url, key, body string) (int, map[string]any) {
	t.Helper()
	client := &http.Client{Timeout: 120 * time.Second}
	req, _ := http.NewRequest("POST", url+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return resp.StatusCode, data
}

// messageText joins the text blocks of an Anthropic message.
func messageText(data map[string]any) string {
	var parts []string
	blocks, _ := data["content"].([]any)
	for _, bi := range blocks {
		if b, ok := bi.(map[string]any); ok && b["type"] == "text" {
			if s, _ := b["text"].(string); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func TestLiveAnthropicMessages(t *testing.T) {
	url, key, _ := liveServer(t)
	status, data := postMessages(t, url, key,
		`{"model":"`+smokeModel()+`","max_tokens":50,"messages":[{"role":"user","content":"Reply with exactly: PONG"}]}`)
	if status == 402 {
		return // plan-gated, acceptable
	}
	if status != 200 {
		t.Fatalf("status %d: %v", status, data)
	}
	if data["type"] != "message" || data["role"] != "assistant" {
		t.Fatalf("body %v", data)
	}
	if strings.TrimSpace(messageText(data)) == "" {
		t.Fatalf("empty text: %v", data["content"])
	}
}

// TestLiveAnthropicVision pins the vision-capable model verified in
// docs/ROADMAP.md (the smoke model may be overridden to a text-only one).
func TestLiveAnthropicVision(t *testing.T) {
	url, key, _ := liveServer(t)
	status, data := postMessages(t, url, key,
		`{"model":"Qwen/Qwen3.7-Plus","max_tokens":50,"messages":[{"role":"user","content":[
			{"type":"text","text":"What is the dominant color of the attached image? One word."},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+redPNG32+`"}}]}]}`)
	if status == 402 {
		return // plan-gated, acceptable
	}
	if status != 200 {
		t.Fatalf("status %d: %v", status, data)
	}
	if text := messageText(data); !strings.Contains(strings.ToLower(text), "red") {
		t.Fatalf("model did not see the red image: %q", text)
	}
}

// TestLiveReasoningEffort pins a model whose effort support is known from the
// Command Code CLI catalog (deepseek v4 accepts high/max), proving upstream
// accepts the forwarded params.reasoning_effort field.
func TestLiveReasoningEffort(t *testing.T) {
	url, key, client := liveServer(t)
	body := `{"model":"deepseek/deepseek-v4-flash","reasoning_effort":"high","max_tokens":400,"messages":[{"role":"user","content":"Reply with exactly: PONG"}]}`
	req, _ := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 402 {
		return // plan-gated, acceptable
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var data map[string]any
	json.Unmarshal(b, &data)
	msg := data["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	content, _ := msg["content"].(string)
	reasoning, _ := msg["reasoning_content"].(string)
	if strings.TrimSpace(content) == "" && strings.TrimSpace(reasoning) == "" {
		t.Fatalf("no content or reasoning: %s", b)
	}
}

// TestLiveToolResultImage proves a tool-result image actually reaches the model:
// Command Code drops media inside tool-result outputs (docs/ROADMAP.md probe),
// so a correct color answer can only come from the proxy re-emitting the image
// as a follow-up user turn.
func TestLiveToolResultImage(t *testing.T) {
	url, key, _ := liveServer(t)
	status, data := postMessages(t, url, key,
		`{"model":"Qwen/Qwen3.7-Plus","max_tokens":50,
		"tools":[{"name":"screenshot","description":"Take a screenshot","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":"Take a screenshot and tell me its dominant color. Answer with one word."},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_live_1","name":"screenshot","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_live_1","content":[
				{"type":"text","text":"screenshot captured"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+redPNG32+`"}}]}]}]}`)
	if status == 402 {
		return // plan-gated, acceptable
	}
	if status != 200 {
		t.Fatalf("status %d: %v", status, data)
	}
	if text := messageText(data); !strings.Contains(strings.ToLower(text), "red") {
		t.Fatalf("model did not see the tool-result image: %q", text)
	}
}

// TestLiveThinkingSignature proves a thinking block that reaches Claude Code is
// signed. It streams /v1/messages with thinking enabled on a reasoning-capable
// model; if the model surfaces thinking at all, that block must be signed
// (signature_delta) or Claude Code would drop it. No thinking surfaced → nothing
// to verify (skip), since the model, not the proxy, decides to think.
func TestLiveThinkingSignature(t *testing.T) {
	url, key, _ := liveServer(t)
	client := &http.Client{Timeout: 120 * time.Second}
	body := `{"model":"deepseek/deepseek-v4-flash","max_tokens":512,"stream":true,
		"thinking":{"type":"enabled","budget_tokens":20000},
		"messages":[{"role":"user","content":"Think step by step, then answer: what is 17 * 23?"}]}`
	req, _ := http.NewRequest("POST", url+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 402 {
		return // plan-gated, acceptable
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	s := string(raw)
	if !strings.Contains(s, `"thinking_delta"`) {
		t.Skip("model surfaced no thinking; nothing to sign")
	}
	if !strings.Contains(s, `"signature_delta"`) {
		t.Fatalf("thinking present but unsigned (no signature_delta): %s", s)
	}
}

func TestLiveStreamingCompletion(t *testing.T) {
	url, key, client := liveServer(t)
	req, _ := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(liveChatBody(true)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if resp.StatusCode != 402 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status %d: %s", resp.StatusCode, b)
		}
		return // plan-gated, acceptable
	}
	sawDone := false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "data: [DONE]" {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("stream ended without a [DONE] sentinel")
	}
}
