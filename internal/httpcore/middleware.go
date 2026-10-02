package httpcore

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/idgen"
	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/reqctx"
)

// Middleware wraps a handler with one cross-cutting concern.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware so that the first argument is the outermost layer.
// Reading the call site top to bottom therefore reads as the request's path
// through the stack, which is how the security policy is meant to be reviewed.
func Chain(mw ...Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		for i := len(mw) - 1; i >= 0; i-- {
			next = mw[i](next)
		}
		return next
	}
}

type ctxKeyClientIP struct{}
type ctxKeyNonce struct{}

// RequestIDFrom returns the request identifier, or "" outside the middleware.
func RequestIDFrom(ctx context.Context) string { return reqctx.ID(ctx) }

// ClientIPFrom returns the resolved client address, or nil outside the middleware.
func ClientIPFrom(ctx context.Context) net.IP {
	ip, _ := ctx.Value(ctxKeyClientIP{}).(net.IP)
	return ip
}

// NonceFrom returns the per-response CSP nonce, or "" outside the middleware.
func NonceFrom(ctx context.Context) string {
	s, _ := ctx.Value(ctxKeyNonce{}).(string)
	return s
}

// RequestID assigns every request a fresh identifier and echoes it back so a
// user-reported failure can be traced to one log line.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Never trust a client-supplied id; a forged one would let an
			// attacker collide with another session's log entries.
			id := idgen.New("req")
			w.Header().Set("X-Request-Id", id)
			next.ServeHTTP(w, r.WithContext(reqctx.WithID(r.Context(), id)))
		})
	}
}

// ClientAddress resolves and attaches the client IP, and exposes the resolver to
// handlers so they can honour forwarding headers consistently.
func ClientAddress(resolver *ClientIPResolver) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), ctxKeyClientIP{}, resolver.ClientIP(r))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Recover turns a panic into a 500 problem document and keeps the process alive.
// A panic in one request must never take down the tunnel for every device.
func Recover(logger *logx.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { //nolint:contextcheck // the panic path uses r.Context() via WriteError
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is the documented way for a handler to
				// abandon a response; it is not a bug and must not be logged as
				// one. Compared with errors.Is so that a wrapped sentinel is
				// still recognised.
				if err, isErr := rec.(error); isErr && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				logger.ErrorContext(r.Context(), "panic recovered",
					"panic", fmt.Sprint(rec),
					"method", r.Method,
					"path", r.URL.Path,
					"request_id", RequestIDFrom(r.Context()),
					"stack", string(debug.Stack()),
				)
				WriteError(w, r, logger, RequestIDFrom(r.Context()),
					errx.New(errx.KindInternal, "internal_error", ""))
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// statusRecorder captures the status code and byte count for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, which is
// required for flushing streaming responses and for WebSocket upgrades.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// AccessLog records one line per request.
//
// Only the path is logged, never the query string: the pairing link carries a
// one-time code in its query and that must not reach the log.
func AccessLog(logger *logx.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			status := rec.status
			if status == 0 {
				status = http.StatusOK // handler returned without writing anything
			}

			// Server faults are errors, client faults are warnings, the rest is
			// routine traffic. This keeps a noisy scanner from drowning real
			// failures at error level.
			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"bytes", rec.written,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", RequestIDFrom(r.Context()),
				"client_ip", ipString(ClientIPFrom(r.Context())),
			}
			switch {
			case status >= 500:
				logger.ErrorContext(r.Context(), "http", attrs...)
			case status >= 400:
				logger.WarnContext(r.Context(), "http", attrs...)
			default:
				logger.InfoContext(r.Context(), "http", attrs...)
			}
		})
	}
}

// BodyLimit caps the request body. Handlers still bound their own semantic limits
// (for example maxPromptBytes); this is the blunt transport-level guard.
func BodyLimit(maxBytes int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds handler execution.
//
// Upgrade requests are exempt. A WebSocket's lifetime is the connection's, not a
// request's, and cancelling its context after a fixed interval would tear down
// the event stream on a timer — the client would reconnect, get torn down again,
// and the symptom would look like a flaky network rather than a middleware bug.
// The check is explicit here so that the exception is visible at the layer that
// creates it.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isUpgrade(r) {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// isUpgrade reports whether the request asks to switch protocols.
func isUpgrade(r *http.Request) bool {
	if !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		return false
	}
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// SecurityHeaders applies the response header policy. Because every asset is
// embedded and served same-origin, the policy needs no inline-script escape
// hatch: the CSP nonce covers the one inline bootstrap script the shell needs.
func SecurityHeaders(secure bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			nonce, err := newNonce()
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}

			h := w.Header()
			h.Set("Content-Security-Policy", contentSecurityPolicy(nonce))
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Permissions-Policy", "accelerometer=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), usb=()")
			if secure {
				// Two years, subdomains included. Only meaningful over HTTPS,
				// which is exactly when secure is true.
				h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			}

			ctx := context.WithValue(r.Context(), ctxKeyNonce{}, nonce)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// contentSecurityPolicy is intentionally strict. connect-src covers the
// WebSocket event stream; img-src allows data: and blob: for locally rendered
// previews; frame-ancestors and X-Frame-Options together defeat clickjacking.
func contentSecurityPolicy(nonce string) string {
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'nonce-" + nonce + "'",
		"style-src 'self' 'nonce-" + nonce + "'",
		"img-src 'self' data: blob:",
		"font-src 'self'",
		"connect-src 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"object-src 'none'",
	}, "; ")
}

func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("httpcore: generate csp nonce: %w", err)
	}
	return base64.RawStdEncoding.EncodeToString(b[:]), nil
}

func ipString(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}
