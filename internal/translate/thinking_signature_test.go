package translate

import (
	"encoding/base64"
	"strings"
	"testing"
)

// assertThinkingSig checks a signature satisfies Claude Code's shallow check:
// base64 starting with 'E', payload first byte 0x12.
func assertThinkingSig(t *testing.T, sig string) {
	t.Helper()
	if !strings.HasPrefix(sig, "E") {
		t.Fatalf("signature %q should start with E", sig)
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || len(raw) == 0 || raw[0] != 0x12 {
		t.Fatalf("signature payload should begin with 0x12: %q (err %v)", sig, err)
	}
}

func TestAnthropicThinkingHasSignature(t *testing.T) {
	completion := map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "reasoning_content": "ponder", "content": "hi"},
			"finish_reason": "stop",
		}},
	}
	blocks := MessageFromCompletion(completion, "m", "msg_1")["content"].([]any)
	think := blocks[0].(map[string]any)
	if think["type"] != "thinking" {
		t.Fatalf("first block not thinking: %v", think)
	}
	sig, _ := think["signature"].(string)
	assertThinkingSig(t, sig)
}

func collectStream(t *testing.T, events []Event) []string {
	t.Helper()
	i := 0
	next := func() (Event, bool) {
		i++
		if i < len(events) {
			return events[i], true
		}
		return nil, false
	}
	var frames []string
	if err := StreamMessage(events[0], next, "m", "msg_1", func(s string) error {
		frames = append(frames, s)
		return nil
	}); err != nil {
		t.Fatalf("StreamMessage: %v", err)
	}
	return frames
}

func firstFrameIndex(frames []string, sub string) int {
	for i, f := range frames {
		if strings.Contains(f, sub) {
			return i
		}
	}
	return -1
}

func TestAnthropicStreamThinkingSignature(t *testing.T) {
	frames := collectStream(t, []Event{
		{"type": "reasoning-delta", "text": "let me think"},
		{"type": "text-delta", "text": "answer"},
		{"type": "finish", "finishReason": "stop"},
	})
	sig := firstFrameIndex(frames, `"signature_delta"`)
	txt := firstFrameIndex(frames, `"text_delta"`)
	if sig < 0 {
		t.Fatal("no signature_delta frame emitted for the thinking block")
	}
	if txt < 0 || sig > txt {
		t.Fatalf("signature_delta (%d) must precede the text block's text_delta (%d)", sig, txt)
	}
	// The accumulated thinking text seeds the signature.
	if want := fakeThinkingSignature("let me think"); !strings.Contains(frames[sig], want) {
		t.Fatalf("signature_delta frame missing expected signature %q: %s", want, frames[sig])
	}
}
