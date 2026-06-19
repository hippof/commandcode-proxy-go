// Package auth extracts the caller's Command Code API key from a request.
//
// The proxy is keyless by design: every request must carry the caller's own key,
// which is relayed verbatim to Command Code. The proxy never reads a key from the
// environment or disk, so one instance can serve callers on different accounts.
//
// Two header styles are accepted: OpenAI's "Authorization: Bearer <key>" and
// Anthropic's "x-api-key: <key>" (used by the /v1/messages surface).
package auth

import "strings"

// placeholders are values some hosts forward verbatim instead of resolving.
var placeholders = map[string]bool{
	"$COMMANDCODE_API_KEY": true,
	"COMMANDCODE_API_KEY":  true,
	"null":                 true,
	"none":                 true,
}

// ResolveAPIKey returns the caller's Command Code key, or "".
// x-api-key (Anthropic style) wins when present; otherwise the bearer token from
// Authorization is used.
func ResolveAPIKey(authorization, xAPIKey string) string {
	for _, tok := range []string{strings.TrimSpace(xAPIKey), bearer(authorization)} {
		if tok != "" && !placeholders[tok] {
			return tok
		}
	}
	return ""
}

func bearer(header string) string {
	v := strings.TrimSpace(header)
	if strings.EqualFold(v, "bearer") { // scheme keyword with no token
		return ""
	}
	if len(v) >= 7 && strings.EqualFold(v[:7], "bearer ") {
		v = strings.TrimSpace(v[7:])
	}
	return v
}
