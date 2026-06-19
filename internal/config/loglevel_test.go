package config

import "testing"

func TestLogEnabled(t *testing.T) {
	cases := []struct {
		configured, level string
		want              bool
	}{
		{"info", "debug", false},
		{"info", "info", true},
		{"info", "error", true},
		{"debug", "debug", true},
		{"warn", "info", false},
		{"warn", "warn", true},
		{"", "info", true},        // unset -> info threshold
		{"", "debug", false},      //
		{"bogus", "debug", false}, // unrecognized -> info threshold
	}
	for _, c := range cases {
		cfg := &Config{LogLevel: c.configured}
		if got := cfg.LogEnabled(c.level); got != c.want {
			t.Errorf("LogLevel=%q LogEnabled(%q)=%v want %v", c.configured, c.level, got, c.want)
		}
	}
}
