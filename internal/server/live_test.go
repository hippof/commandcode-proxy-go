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
