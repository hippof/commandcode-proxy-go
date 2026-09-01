package probe

import "testing"

func TestClassifyUpstreamError(t *testing.T) {
	cases := []struct {
		code, msg       string
		denied, blocked bool
	}{
		{"BAD_REQUEST", "You have insufficient credits to make this request. Please purchase more credits to continue using the service.", true, true},
		{"MODEL_NOT_IN_PLAN", "model not in plan", true, false},
		{"INVALID_REQUEST", "messages field required", false, false},
		{"upgrade_required", "Your Command Code CLI is out of date", true, false},
	}
	for _, c := range cases {
		d, b, reason := classifyUpstreamError(c.code, c.msg)
		if d != c.denied || b != c.blocked {
			t.Fatalf("classify(%q,%q) = (%v,%v,%q), want (%v,%v)", c.code, c.msg, d, b, reason, c.denied, c.blocked)
		}
	}
}

func TestParseErrBody(t *testing.T) {
	code, msg := parseErrBody([]byte(`{"error":{"code":"BAD_REQUEST","message":"insufficient credits","type":"upstream_error"}}`))
	if code != "BAD_REQUEST" || msg != "insufficient credits" {
		t.Fatalf("got %q %q", code, msg)
	}
	if code, msg := parseErrBody([]byte("plain text")); code != "" || msg != "plain text" {
		t.Fatalf("fallback: %q %q", code, msg)
	}
}
