package logging

import (
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in    string
		want  slog.Level
		wantR bool // recognized
	}{
		{"debug", slog.LevelDebug, true},
		{"DEBUG", slog.LevelDebug, true},
		{"info", slog.LevelInfo, true},
		{" Info ", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, true},
		{"warning", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"", slog.LevelInfo, true},         // empty → default info, treated as recognized
		{"verbose", slog.LevelInfo, false}, // unknown → default info, not recognized
	}
	for _, c := range cases {
		got, ok := ParseLevel(c.in)
		if got != c.want || ok != c.wantR {
			t.Errorf("ParseLevel(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.wantR)
		}
	}
}

func TestSetup(t *testing.T) {
	logger, ok := Setup("warn")
	if logger == nil {
		t.Fatal("Setup returned nil logger")
	}
	if !ok {
		t.Error("Setup(warn) reported unrecognized level")
	}
	if !slog.Default().Enabled(nil, slog.LevelWarn) {
		t.Error("default logger should be enabled at warn after Setup(warn)")
	}
	if slog.Default().Enabled(nil, slog.LevelInfo) {
		t.Error("default logger should NOT be enabled at info after Setup(warn)")
	}
	// Restore a permissive default for any subsequent tests.
	Setup("debug")
}
