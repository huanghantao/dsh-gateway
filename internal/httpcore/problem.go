// Package httpcore holds the transport-level primitives shared by every HTTP
// handler: the middleware chain, RFC 9457 problem documents, client-address
// resolution, and the security header policy.
//
// Nothing here knows about agents, sessions, or DSH. Handlers depend on this
// package; this package depends on nothing but the standard library and errx.
package httpcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// ProblemNamespace is the URN prefix for problem type identifiers. A URN is used
// rather than an https URL because the project does not own a domain that is
// guaranteed to serve documentation; the identifier stays stable regardless.
const ProblemNamespace = "urn:dsh-gateway:problem:"

// Problem is an RFC 9457 "problem detail" document.
type Problem struct {
	// Type is a stable identifier for the problem class.
	Type string `json:"type"`
	// Title is a short, human-readable summary that does not change per occurrence.
	Title string `json:"title"`
	// Status is the HTTP status code, duplicated for consumers that lose it.
	Status int `json:"status"`
	// Detail explains this specific occurrence.
	Detail string `json:"detail,omitempty"`
	// Instance identifies this occurrence; it equals the request id.
	Instance string `json:"instance,omitempty"`
	// Code is the errx code, convenient for clients that branch on it.
	Code string `json:"code,omitempty"`
	// Retryable tells a client whether retrying unchanged could succeed.
	Retryable bool `json:"retryable,omitempty"`
}

// statusFor maps an error kind onto an HTTP status code. This is the only place
// in the gateway where that mapping exists.
func statusFor(kind errx.Kind) int {
	switch kind {
	case errx.KindInvalid:
		return http.StatusBadRequest
	case errx.KindNotFound:
		return http.StatusNotFound
	case errx.KindConflict:
		return http.StatusConflict
	case errx.KindUnauthenticated:
		return http.StatusUnauthorized
	case errx.KindForbidden:
		return http.StatusForbidden
	case errx.KindRateLimited:
		return http.StatusTooManyRequests
	case errx.KindUnavailable:
		return http.StatusServiceUnavailable
	case errx.KindTimeout:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

func titleFor(status int) string {
	if text := http.StatusText(status); text != "" {
		return text
	}
	return "Error"
}

// WriteError renders err as a problem document. The response never contains
// wrapped causes or internal messages; the caller-supplied logger receives the
// full error for server-side diagnosis.
//
// instance, when non-empty, is echoed as the problem instance.
func WriteError(w http.ResponseWriter, r *http.Request, logger *logx.Logger, instance string, err error) {
	kind := errx.KindOf(err)
	status := statusFor(kind)

	// A 401 must advertise how to authenticate, or browsers and native clients
	// have no way to discover the scheme.
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="dsh-gateway", charset="UTF-8"`)
	}

	problem := Problem{
		Type:      ProblemNamespace + errx.CodeOf(err),
		Title:     titleFor(status),
		Status:    status,
		Detail:    errx.PublicMessage(err),
		Instance:  instance,
		Code:      errx.CodeOf(err),
		Retryable: kind.Retryable(),
	}

	if status == http.StatusInternalServerError {
		logger.ErrorContext(r.Context(), "request failed",
			"code", problem.Code,
			"instance", instance,
			"method", r.Method,
			"path", r.URL.Path,
			"error", err.Error(),
		)
	} else {
		logger.DebugContext(r.Context(), "request rejected",
			"code", problem.Code,
			"status", status,
			"instance", instance,
			"error", err.Error(),
		)
	}

	WriteProblem(w, problem)
}

// WriteProblem serialises problem as application/problem+json.
func WriteProblem(w http.ResponseWriter, problem Problem) {
	body, err := json.Marshal(problem)
	if err != nil {
		// Marshalling a flat struct of strings, ints, and bools cannot fail;
		// fall back to a hand-written document rather than panicking mid-response.
		body = []byte(`{"type":"urn:dsh-gateway:problem:internal_error","title":"Internal Server Error","status":500}`)
	}
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(problem.Status)
	_, _ = w.Write(body)
}

// RespondJSON writes v as a JSON response with a no-store cache policy. Most
// gateway responses are per-device and must never be cached by a shared proxy.
func RespondJSON(w http.ResponseWriter, status int, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("httpcore: marshal response: %w", err)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, err = w.Write(body)
	return err
}

// DecodeJSON reads a JSON request body into v, rejecting unknown fields so that a
// client typo fails loudly instead of being silently ignored, and rejecting
// trailing content so that two documents cannot be smuggled in one body.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		return errx.Wrap(err, errx.KindInvalid, "invalid_body", "the request body is not valid JSON for this endpoint")
	}
	// A second Decode must report io.EOF; anything else means trailing data.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return errx.New(errx.KindInvalid, "invalid_body", "the request body must contain exactly one JSON document")
	}
	return nil
}
