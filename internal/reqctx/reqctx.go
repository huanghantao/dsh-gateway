// Package reqctx carries the request-scoped correlation identifier.
//
// It exists as its own package so that low-level writers — the audit log, the
// harness supervisor — can attach and read the request id without importing the
// HTTP layer, and so that exactly one context key type exists for it. Two
// packages each defining their own key would compile happily and silently never
// find each other's value.
package reqctx

import "context"

type key struct{}

// WithID returns ctx carrying id.
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, key{}, id)
}

// ID returns the request id, or "" when the context carries none.
func ID(ctx context.Context) string {
	id, _ := ctx.Value(key{}).(string)
	return id
}
