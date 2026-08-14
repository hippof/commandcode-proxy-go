package translate

import (
	"regexp"
	"strings"
)

// Secret-shaped substrings scrubbed from upstream-derived error text before it
// reaches a client, in case Command Code ever echoes a credential back (D7).
// Ported from the pi extension's redactCommandCodeErrorText.
var (
	bearerRe           = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	credentialRe       = regexp.MustCompile(`(?i)\b(?:api[-_ ]?key|apikey|access[-_ ]?token|refresh[-_ ]?token|token|secret|password|authorization)\s*[=:]\s*[^\s,;)]+`)
	userTokenRe        = regexp.MustCompile(`\b(?:user|cc)_[A-Za-z0-9_-]{8,}\b`)
	querySecretRe      = regexp.MustCompile(`(?i)([?&](?:api[-_ ]?key|apikey|access_token|refresh_token|token|secret|password)=)[^&#\s]+`)
	standaloneSecretRe = regexp.MustCompile(`\b(?:sk|rk|ghp|github_pat|xox[baprs])[-_A-Za-z0-9]{16,}\b|\beyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
)

// RedactSecrets replaces credential-shaped substrings with "[redacted]".
func RedactSecrets(s string) string {
	s = bearerRe.ReplaceAllString(s, "Bearer [redacted]")
	s = credentialRe.ReplaceAllStringFunc(s, func(m string) string {
		if i := strings.IndexAny(m, "=:"); i >= 0 {
			return m[:i+1] + "[redacted]"
		}
		return "[redacted]"
	})
	s = userTokenRe.ReplaceAllString(s, "[redacted]")
	s = querySecretRe.ReplaceAllString(s, "${1}[redacted]")
	s = standaloneSecretRe.ReplaceAllString(s, "[redacted]")
	return s
}
