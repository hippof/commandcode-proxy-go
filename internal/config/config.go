// Package config holds runtime settings, all overridable via environment
// variables. Defaults target Command Code's public API and mirror the values
// the upstream pi extension (github.com/patlux/pi-commandcode-provider) sends so
// Command Code recognizes the client.
package config

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the resolved runtime configuration.
type Config struct {
	// GenerateURL is Command Code's streaming coding-agent endpoint that the
	// subscription plans expose; ModelsURL is the provider model catalog.
	GenerateURL string
	ModelsURL   string

	// CLIVersion identifies the client to Command Code; TasteLearning controls
	// whether traffic folds into the account's taste profile (off by default).
	CLIVersion    string
	TasteLearning string

	// Sampling defaults applied only when the request omits them.
	DefaultTemperature float64
	DefaultMaxTokens   int
	MaxTokensCap       int

	// MaxRetries bounds the retry of transient upstream failures before any
	// content; RequestTimeout caps a single upstream request.
	MaxRetries     int
	RequestTimeout time.Duration

	// ModelAliases are exact model-id overrides from COMMANDCODE_MODEL_ALIASES.
	ModelAliases map[string]string

	// LogLevel sets log verbosity: debug, info (default), warn, or error.
	LogLevel string
}

// familyDefaults map a Claude model family (matched as a case-insensitive
// substring) to a Command Code id, so Claude Code's ids work with no config. An
// exact ModelAliases entry, or a ModelAliases key equal to the family name,
// overrides these. Ordered for deterministic matching.
var familyDefaults = []struct{ family, target string }{
	{"opus", "deepseek/deepseek-v4-pro"},
	{"sonnet", "deepseek/deepseek-v4-flash"},
	{"haiku", "deepseek/deepseek-v4-flash"},
}

// Load reads the configuration from the environment.
func Load() *Config {
	base := strings.TrimRight(env("COMMANDCODE_API_BASE", "https://api.commandcode.ai"), "/")
	return &Config{
		GenerateURL:        base + "/alpha/generate",
		ModelsURL:          env("COMMANDCODE_MODELS_URL", base+"/provider/v1/models"),
		CLIVersion:         env("COMMANDCODE_CLI_VERSION", "0.29.0"),
		TasteLearning:      env("COMMANDCODE_TASTE_LEARNING", "false"),
		DefaultTemperature: envFloat("COMMANDCODE_DEFAULT_TEMPERATURE", 0.3),
		DefaultMaxTokens:   envInt("COMMANDCODE_DEFAULT_MAX_TOKENS", 32000),
		MaxTokensCap:       envInt("COMMANDCODE_MAX_TOKENS_CAP", 64000),
		MaxRetries:         envInt("COMMANDCODE_MAX_RETRIES", 2),
		RequestTimeout:     time.Duration(envFloat("COMMANDCODE_TIMEOUT", 300) * float64(time.Second)),
		ModelAliases:       parseAliases(os.Getenv("COMMANDCODE_MODEL_ALIASES")),
		LogLevel:           strings.ToLower(env("COMMANDCODE_PROXY_LOG_LEVEL", "info")),
	}
}

var logLevelRank = map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3}

// LogEnabled reports whether a message at the given level should be emitted
// under the configured LogLevel (an unrecognized LogLevel is treated as info).
func (c *Config) LogEnabled(level string) bool {
	threshold, ok := logLevelRank[c.LogLevel]
	if !ok {
		threshold = logLevelRank["info"]
	}
	return logLevelRank[level] >= threshold
}

// ResolveModel maps a client-supplied model id to a Command Code id.
// Precedence: an exact ModelAliases entry, then a family match — where a
// ModelAliases key equal to the family name (e.g. "opus") overrides the target,
// otherwise the built-in default applies — then the id unchanged.
func (c *Config) ResolveModel(name string) string {
	if v, ok := c.ModelAliases[name]; ok {
		return v
	}
	lowered := strings.ToLower(name)
	for _, fd := range familyDefaults {
		if strings.Contains(lowered, fd.family) {
			if override, ok := c.ModelAliases[fd.family]; ok {
				return override
			}
			return fd.target
		}
	}
	return name
}

// parseAliases parses COMMANDCODE_MODEL_ALIASES (a JSON object) into {alias: id}.
// Malformed or non-object values yield an empty map — aliasing is a convenience,
// never a hard failure.
func parseAliases(raw string) map[string]string {
	out := map[string]string{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	var data map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &data) != nil {
		return out
	}
	for k, rv := range data {
		var s string
		if json.Unmarshal(rv, &s) == nil {
			if s = strings.TrimSpace(s); s != "" {
				out[k] = s
			}
		}
	}
	return out
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
