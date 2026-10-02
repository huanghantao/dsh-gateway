// Package logx configures the process-wide structured logger.
//
// The gateway logs exclusively through log/slog. Handlers are chosen once in the
// composition root and injected; packages never reach for a global. Secrets are
// never logged: see Redact below.
package logx

import (
	"context"
	"io"
	"log/slog"
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
}

// New builds a logger writing to w.
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

// Redact masks a secret so that a correlation-friendly prefix survives without
// exposing the value. Values shorter than 8 bytes are fully masked.
func Redact(secret string) string {
	if len(secret) < 8 {
		return "***"
	}
	return secret[:4] + "…(" + itoa(len(secret)) + "B)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

type ctxKey struct{}

// WithLogger returns a context carrying logger.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, logger)
}

// FromContext returns the context's logger, or defaultLogger when absent.
func FromContext(ctx context.Context, defaultLogger *slog.Logger) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return defaultLogger
}
