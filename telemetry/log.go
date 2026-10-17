package telemetry

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Logger returns a logger that writes JSON lines to standard error at the
// named level: debug, info, warn, or error. Any other name means info.
func Logger(level string) *slog.Logger {
	return newLogger(os.Stderr, level)
}

func newLogger(w io.Writer, level string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: ParseLevel(level)}))
}

// ParseLevel returns the slog level of a name: debug, info, warn (or
// warning), or error, in any case. Any other name means info.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}
