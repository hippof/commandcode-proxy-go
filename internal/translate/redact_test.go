package translate

import "testing"

func TestRedactSecrets(t *testing.T) {
	cases := []struct{ in, want string }{
		{"401 for Bearer user_abc123DEF456", "401 for Bearer [redacted]"},
		{"invalid key user_abc123DEF456", "invalid key [redacted]"},
		{"api_key=user_secret1234 rejected", "api_key=[redacted] rejected"},
		{"see https://x.test/cb?access_token=abc123 and retry", "see https://x.test/cb?access_token=[redacted] and retry"},
		{"leaked sk-proj-abcdefghijklmnop123", "leaked [redacted]"},
		{"plain error message", "plain error message"},
	}
	for _, c := range cases {
		if got := RedactSecrets(c.in); got != c.want {
			t.Errorf("RedactSecrets(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseStreamEventLineRedactsErrorMessage(t *testing.T) {
	ev := ParseStreamEventLine(`{"type":"error","error":{"message":"denied for Bearer user_abc123DEF456"}}`)
	msg := ev["error"].(map[string]any)["message"]
	if msg != "denied for Bearer [redacted]" {
		t.Fatalf("message=%v", msg)
	}
}
