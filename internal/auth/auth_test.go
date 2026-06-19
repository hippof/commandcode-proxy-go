package auth

import "testing"

func TestResolveBearer(t *testing.T) {
	if got := ResolveAPIKey("Bearer user_abc", ""); got != "user_abc" {
		t.Fatalf("bearer: %q", got)
	}
}

func TestResolveXAPIKeyWins(t *testing.T) {
	if got := ResolveAPIKey("Bearer user_auth", "user_xkey"); got != "user_xkey" {
		t.Fatalf("x-api-key should win over Authorization: %q", got)
	}
}

func TestResolveFallsBackToBearer(t *testing.T) {
	if got := ResolveAPIKey("Bearer user_auth", ""); got != "user_auth" {
		t.Fatalf("fallback: %q", got)
	}
}

func TestResolveRawXAPIKey(t *testing.T) {
	if got := ResolveAPIKey("", "user_raw"); got != "user_raw" {
		t.Fatalf("raw x-api-key (no scheme): %q", got)
	}
}

func TestResolveBareBearerIsEmpty(t *testing.T) {
	if got := ResolveAPIKey("Bearer", ""); got != "" {
		t.Fatalf("bare Bearer should resolve to empty: %q", got)
	}
}

func TestResolvePlaceholdersRejected(t *testing.T) {
	for _, h := range []string{
		"Bearer $COMMANDCODE_API_KEY",
		"Bearer COMMANDCODE_API_KEY",
		"Bearer null",
		"Bearer none",
	} {
		if got := ResolveAPIKey(h, ""); got != "" {
			t.Errorf("placeholder %q should be rejected, got %q", h, got)
		}
	}
}

func TestResolveMissing(t *testing.T) {
	if got := ResolveAPIKey("", ""); got != "" {
		t.Fatalf("missing key should be empty: %q", got)
	}
}
