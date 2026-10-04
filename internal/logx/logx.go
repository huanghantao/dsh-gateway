// Package logx configures the process-wide structured logger.
//
// The gateway logs exclusively through log/slog. Handlers are chosen once in the
// composition root and injected; packages never reach for a global.
package logx

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Logger is the concrete logger type the gateway passes around. It aliases
// slog.Logger so that helpers can accept *logx.Logger without callers importing
// log/slog themselves.
type Logger = slog.Logger

// Format selects a slog handler implementation.
type Format string

const (
	// FormatText is human-readable output for a terminal.
	FormatText Format = "text"
	// FormatJSON is one JSON object per line, for journald or log shipping.
	FormatJSON Format = "json"
)

// Config describes logger construction.
type Config struct {
	// Level is one of debug, info, warn, error. Unknown values fall back to info.
	Level string
	// Format selects the handler. Unknown values fall back to FormatText.
	Format Format
	// AddSource records the calling file and line.
	AddSource bool
	// File is where the log is written. Empty means stderr — what a daemon under
	// launchd or systemd should do, and what New always does. A path is opened
	// with rotation by Open, and is how a deployment whose supervisor does not
	// rotate anything stops growing a log forever.
	File string
	// MaxBytes bounds the file before it is rotated to a single ".1" generation.
	// Zero takes DefaultMaxBytes. It is ignored when File is empty.
	MaxBytes int64
}

// New builds a logger writing to w. File and MaxBytes are ignored: a caller that
// wants them wants Open.
func New(w io.Writer, cfg Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(cfg.Level), AddSource: cfg.AddSource}

	var h slog.Handler
	if cfg.Format == FormatJSON {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}

// Open builds a logger that writes where cfg says, and returns the function that
// closes whatever it opened.
//
// A log file that cannot be opened is not fatal, and deliberately so: refusing to
// serve the operator's phone because a log could not be written would trade the
// product for its diagnostics. The error is returned alongside a logger on
// stderr, so the caller can say what happened and carry on.
func Open(cfg Config) (*slog.Logger, func() error, error) {
	if cfg.File == "" {
		return New(os.Stderr, cfg), func() error { return nil }, nil
	}
	file, err := OpenRotatingFile(cfg.File, cfg.MaxBytes)
	if err != nil {
		return New(os.Stderr, cfg), func() error { return nil }, err
	}
	return New(file, cfg), file.Close, nil
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

// Discard returns a logger that drops everything. Tests use it to keep output
// readable; production code must not.
func Discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
