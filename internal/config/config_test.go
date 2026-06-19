package config

import "testing"

func TestResolveModelFamilyDefaults(t *testing.T) {
	c := &Config{ModelAliases: map[string]string{}}
	cases := map[string]string{
		"claude-opus-4-8":          "deepseek/deepseek-v4-pro",
		"claude-opus-4-1-20250805": "deepseek/deepseek-v4-pro", // dated variant
		"claude-sonnet-4-6":        "deepseek/deepseek-v4-flash",
		"claude-haiku-4-5":         "deepseek/deepseek-v4-flash",
		"CLAUDE-OPUS-4":            "deepseek/deepseek-v4-pro", // case-insensitive
	}
	for in, want := range cases {
		if got := c.ResolveModel(in); got != want {
			t.Errorf("ResolveModel(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestResolveModelPassthrough(t *testing.T) {
	c := &Config{ModelAliases: map[string]string{}}
	if got := c.ResolveModel("Qwen/Qwen3.7-Plus"); got != "Qwen/Qwen3.7-Plus" {
		t.Fatalf("passthrough: got %q", got)
	}
}

func TestResolveModelAliasOverridesFamily(t *testing.T) {
	c := &Config{ModelAliases: map[string]string{"claude-opus-4-8": "override/model"}}
	if got := c.ResolveModel("claude-opus-4-8"); got != "override/model" {
		t.Fatalf("exact alias should override family default: got %q", got)
	}
}

func TestParseAliasesViaEnv(t *testing.T) {
	t.Setenv("COMMANDCODE_MODEL_ALIASES", `{"sonnet":"anthropic/claude-x","bad":5,"blank":"  "}`)
	c := Load()
	if c.ModelAliases["sonnet"] != "anthropic/claude-x" {
		t.Errorf("sonnet alias missing: %v", c.ModelAliases)
	}
	if _, ok := c.ModelAliases["bad"]; ok {
		t.Errorf("non-string value should be dropped")
	}
	if _, ok := c.ModelAliases["blank"]; ok {
		t.Errorf("blank value should be dropped")
	}
}

func TestParseAliasesMalformed(t *testing.T) {
	t.Setenv("COMMANDCODE_MODEL_ALIASES", "not json")
	if c := Load(); len(c.ModelAliases) != 0 {
		t.Errorf("malformed should yield empty, got %v", c.ModelAliases)
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("COMMANDCODE_API_BASE", "")
	t.Setenv("COMMANDCODE_MODEL_ALIASES", "")
	c := Load()
	if c.GenerateURL != "https://api.commandcode.ai/alpha/generate" {
		t.Errorf("GenerateURL=%q", c.GenerateURL)
	}
	if c.MaxRetries != 2 || c.DefaultMaxTokens != 32000 || c.MaxTokensCap != 64000 {
		t.Errorf("unexpected defaults: %+v", c)
	}
}
