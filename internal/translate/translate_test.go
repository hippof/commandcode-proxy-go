package translate

import (
	"testing"

	"github.com/liwei/commandcode-proxy-go/internal/config"
)

func testCfg() *config.Config {
	return &config.Config{DefaultMaxTokens: 32000, MaxTokensCap: 64000, DefaultTemperature: 0.3}
}

func params(cc map[string]any) map[string]any { return cc["params"].(map[string]any) }

func TestBuildCCRequestSystemLift(t *testing.T) {
	req := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "system", "content": "be nice"},
			map[string]any{"role": "developer", "content": "also concise"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	p := params(BuildCCRequest(req, testCfg()))
	if p["system"] != "be nice\n\nalso concise" {
		t.Fatalf("system=%q", p["system"])
	}
	msgs := p["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Fatalf("messages=%v", msgs)
	}
}

func TestBuildCCRequestMaxTokensCap(t *testing.T) {
	req := map[string]any{"model": "m", "max_tokens": float64(100000), "messages": []any{}}
	if mt := params(BuildCCRequest(req, testCfg()))["max_tokens"].(int); mt != 64000 {
		t.Fatalf("max_tokens=%d want 64000", mt)
	}
}

func TestBuildCCRequestDefaultMaxTokens(t *testing.T) {
	req := map[string]any{"model": "m", "messages": []any{}}
	if mt := params(BuildCCRequest(req, testCfg()))["max_tokens"].(int); mt != 32000 {
		t.Fatalf("max_tokens=%d want default 32000", mt)
	}
}

func TestBuildCCRequestUsesConfigWorkingDir(t *testing.T) {
	cfg := testCfg()
	cfg.WorkingDir = "/srv/app"
	cc := BuildCCRequest(map[string]any{"model": "m", "messages": []any{}}, cfg)
	if wd := cc["config"].(map[string]any)["workingDir"]; wd != "/srv/app" {
		t.Fatalf("workingDir=%v, want /srv/app", wd)
	}
}

func TestBuildCCRequestDanglingToolPruned(t *testing.T) {
	req := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "a", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
				map[string]any{"id": "b", "type": "function", "function": map[string]any{"name": "g", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "a", "content": "ok"},
		},
	}
	msgs := params(BuildCCRequest(req, testCfg()))["messages"].([]any)
	var assistant map[string]any
	toolResults := 0
	for _, mi := range msgs {
		m := mi.(map[string]any)
		switch m["role"] {
		case "assistant":
			assistant = m
		case "tool":
			toolResults++
		}
	}
	calls := 0
	for _, pi := range assistant["content"].([]any) {
		if pi.(map[string]any)["type"] == "tool-call" {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("expected 1 paired tool-call (b is dangling), got %d", calls)
	}
	if toolResults != 1 {
		t.Fatalf("expected 1 tool result, got %d", toolResults)
	}
}

func TestAnthropicToolResultFanOut(t *testing.T) {
	body := map[string]any{
		"model": "m", "max_tokens": float64(16),
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tid", "content": "42"},
				map[string]any{"type": "text", "text": "and more"},
			}},
		},
	}
	msgs := OpenAIRequestFromAnthropic(body)["messages"].([]any)
	var sawTool, sawUser bool
	for _, mi := range msgs {
		m := mi.(map[string]any)
		if m["role"] == "tool" && m["tool_call_id"] == "tid" && m["content"] == "42" {
			sawTool = true
		}
		if m["role"] == "user" && m["content"] == "and more" {
			sawUser = true
		}
	}
	if !sawTool || !sawUser {
		t.Fatalf("tool_result fan-out failed: %v", msgs)
	}
}

func TestUserContentImageMapping(t *testing.T) {
	req := map[string]any{
		"model": "m",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "what color?"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAA"}},
		}}},
	}
	msgs := params(BuildCCRequest(req, testCfg()))["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content=%v", content)
	}
	img := content[1].(map[string]any)
	if img["type"] != "image" || img["image"] != "data:image/png;base64,AAA" {
		t.Fatalf("image part=%v", img)
	}
}

func TestUserContentTextOnlyStaysString(t *testing.T) {
	req := map[string]any{
		"model": "m",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "a"},
			map[string]any{"type": "text", "text": "b"},
		}}},
	}
	msgs := params(BuildCCRequest(req, testCfg()))["messages"].([]any)
	if c := msgs[0].(map[string]any)["content"]; c != "a\nb" {
		t.Fatalf("content=%v, want flattened string", c)
	}
}

func TestAnthropicImageBlockPassthrough(t *testing.T) {
	src := map[string]any{"type": "base64", "media_type": "image/png", "data": "AAA"}
	body := map[string]any{
		"model": "m", "max_tokens": float64(16),
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "what color?"},
			map[string]any{"type": "image", "source": src, "cache_control": map[string]any{"type": "ephemeral"}},
		}}},
	}
	msgs := params(BuildCCRequest(OpenAIRequestFromAnthropic(body), testCfg()))["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	img := content[1].(map[string]any)
	if img["type"] != "image" || img["source"].(map[string]any)["data"] != "AAA" {
		t.Fatalf("image part=%v", img)
	}
	if _, leaked := img["cache_control"]; leaked {
		t.Fatalf("cache_control should not leak upstream: %v", img)
	}
}

func TestCountInputTokens(t *testing.T) {
	body := map[string]any{
		"system": "abcd", // 4 chars
		"messages": []any{
			map[string]any{"role": "user", "content": "12345678"}, // 8 chars
		},
	}
	// ceil(12/4) = 3 text tokens + 3 per-message overhead
	if got := CountInputTokens(body); got != 6 {
		t.Fatalf("CountInputTokens=%d, want 6", got)
	}
	// an image block adds a flat 1500
	body["messages"] = []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "AAA"}},
		}},
	}
	if got := CountInputTokens(body); got != 1+3+1500 {
		t.Fatalf("CountInputTokens=%d, want 1504", got)
	}
}

func TestParseStreamEventLine(t *testing.T) {
	cases := map[string]bool{
		`{"type":"text-delta","text":"x"}`: true,  // raw NDJSON
		`data: {"type":"finish"}`:          true,  // SSE data-prefixed
		`data: [DONE]`:                     false, // sentinel
		`: keep-alive`:                     false, // comment
		``:                                 false, // blank
		`event: message`:                   false, // SSE event line
		`not json`:                         false,
	}
	for line, wantEvent := range cases {
		got := ParseStreamEventLine(line)
		if (got != nil) != wantEvent {
			t.Errorf("ParseStreamEventLine(%q)=%v, want event=%v", line, got, wantEvent)
		}
	}
}
