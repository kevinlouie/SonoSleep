// Package logging centralizes the service's structured logging via the standard
// library's log/slog. It exposes a single Setup that installs a process-wide
// default logger at a configurable level (HWN_LOG_LEVEL), so every package can
// just call slog.Info/Warn/Error with key/value attributes and get consistent,
// readable output. No third-party logging dependency.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// ParseLevel maps a case-insensitive level name to an slog.Level. Empty or
// unknown input falls back to Info, and ok reports whether the input was a
// recognized non-empty level (so callers can warn on a typo).
func ParseLevel(s string) (level slog.Level, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info", "":
		return slog.LevelInfo, s == "" || strings.EqualFold(strings.TrimSpace(s), "info")
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// Setup installs a process-wide slog default logger writing text to stderr at
// the level parsed from levelStr (HWN_LOG_LEVEL; default Info). It returns the
// logger and whether levelStr was recognized — main logs a warning on an
// unrecognized value rather than failing startup.
func Setup(levelStr string) (*slog.Logger, bool) {
	level, ok := ParseLevel(levelStr)
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	logger := slog.New(h)
	slog.SetDefault(logger)
	return logger, ok
}
