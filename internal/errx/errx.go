// Package errx defines the gateway's transport-neutral error taxonomy.
//
// Domain and application layers return *errx.Error values; the HTTP layer is the
// only place that knows how a kind maps onto a status code. This keeps use-cases
// free of HTTP vocabulary while still producing a stable, machine-readable
// public contract (RFC 9457 problem documents).
package errx

import (
	"errors"
	"fmt"
)

// Kind classifies a failure independently of any transport.
type Kind int

const (
	// KindInternal is an unexpected fault. Its detail is never exposed to clients.
	KindInternal Kind = iota
	// KindInvalid means the caller's input was rejected.
	KindInvalid
	// KindNotFound means the addressed resource does not exist.
	KindNotFound
	// KindConflict means the request conflicts with current state.
	KindConflict
	// KindUnauthenticated means no valid credential was presented.
	KindUnauthenticated
	// KindForbidden means a valid credential lacks permission.
	KindForbidden
	// KindRateLimited means the caller exceeded a quota and may retry later.
	KindRateLimited
	// KindUnavailable means a dependency is not currently able to serve.
	KindUnavailable
	// KindTimeout means a dependency did not answer within its deadline.
	KindTimeout
)

func (k Kind) String() string {
	switch k {
	case KindInternal:
		return "internal"
	case KindInvalid:
		return "invalid"
	case KindNotFound:
		return "not_found"
	case KindConflict:
		return "conflict"
	case KindUnauthenticated:
		return "unauthenticated"
	case KindForbidden:
		return "forbidden"
	case KindRateLimited:
		return "rate_limited"
	case KindUnavailable:
		return "unavailable"
	case KindTimeout:
		return "timeout"
	default:
		return "unknown"
	}
}

// Retryable reports whether the same request could plausibly succeed later
// without the caller changing anything. The HTTP layer surfaces it as the
// "retryable" problem extension member.
func (k Kind) Retryable() bool {
	switch k {
	case KindRateLimited, KindUnavailable, KindTimeout:
		return true
	default:
		return false
	}
}

// Error is a classified failure. Code is a stable, lower_snake_case identifier
// that clients may branch on; Msg is a human-readable summary safe to expose.
type Error struct {
	Kind Kind
	Code string
	Msg  string
	Err  error
}

// Error implements error.
func (e *Error) Error() string {
	switch {
	case e.Msg != "" && e.Err != nil:
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Msg, e.Err)
	case e.Msg != "":
		return fmt.Sprintf("%s: %s", e.Code, e.Msg)
	case e.Err != nil:
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	default:
		return e.Code
	}
}

// Unwrap exposes the wrapped cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Err }

// New builds a classified error with no wrapped cause.
func New(kind Kind, code, msg string) *Error {
	return &Error{Kind: kind, Code: code, Msg: msg}
}

// Wrap classifies cause. A nil cause returns nil so callers can write
// `return errx.Wrap(err, ...)` without a nil check.
func Wrap(cause error, kind Kind, code, msg string) *Error {
	if cause == nil {
		return nil
	}
	return &Error{Kind: kind, Code: code, Msg: msg, Err: cause}
}

// As reports whether err is or wraps an *Error, and if so returns it.
func As(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// KindOf reports the kind of err, defaulting to KindInternal for unclassified
// errors. A nil error yields KindInternal; callers must not pass nil.
func KindOf(err error) Kind {
	if e, ok := As(err); ok {
		return e.Kind
	}
	return KindInternal
}

// CodeOf reports the stable code of err, defaulting to "internal_error".
func CodeOf(err error) string {
	if e, ok := As(err); ok && e.Code != "" {
		return e.Code
	}
	return "internal_error"
}

// PublicMessage returns the message safe to show a client. Unclassified and
// internal errors deliberately collapse to a generic string so that wrapped
// causes (file paths, upstream payloads, credentials) never leak.
func PublicMessage(err error) string {
	e, ok := As(err)
	if !ok {
		return "the server encountered an unexpected condition"
	}
	if e.Kind == KindInternal && e.Msg == "" {
		return "the server encountered an unexpected condition"
	}
	if e.Msg != "" {
		return e.Msg
	}
	return e.Kind.String()
}
