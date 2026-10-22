// Package logging configures the process-wide structured logger.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Setup builds the logger from the LOG_LEVEL and LOG_FORMAT settings, installs it
// as the slog default, and returns it. Because slog.SetDefault also redirects the
// standard library's log package, any log.Printf call that has not been migrated
// yet still comes out in the same format.
//
// level is debug, info, warn or error (default info). format is json or text
// (default text when stderr is a terminal, json otherwise).
func Setup(level, format string) *slog.Logger {
	if format == "" && !isTerminal(os.Stderr) {
		format = "json"
	}
	l := newLogger(os.Stderr, level, format)
	install(l)
	return l
}

func install(l *slog.Logger) {
	slog.SetDefault(l)
}

func newLogger(w io.Writer, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
