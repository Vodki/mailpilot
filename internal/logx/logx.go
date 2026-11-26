// Package logx builds the JSON slog logger. Callers must log identifiers,
// UIDs, hashes and technical metadata only: never message content.
package logx

import (
	"io"
	"log/slog"
)

// New returns a JSON logger writing to w at the given level name.
func New(w io.Writer, level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv}))
}
