// Package logging builds the process-wide slog logger from config.
package logging

import (
	"io"
	"log/slog"

	"github.com/monarr-media/monarr/internal/infra/config"
)

// New returns a slog.Logger writing to w per the config's level and format.
func New(w io.Writer, cfg config.Config) *slog.Logger {
	level, err := cfg.SlogLevel()
	if err != nil {
		// Config was validated at load; fall back defensively anyway.
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	if cfg.LogFormat == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}
