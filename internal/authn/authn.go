// Package authn defines the gateway's authentication port.
//
// The port is transport-neutral on purpose: it deals in opaque credentials and
// principals, not HTTP headers or cookies. That keeps the door open for stronger
// authenticators later (TOTP, WebAuthn, client certificates) as additional
// adapters, without any handler changing.
package authn

import (
	"context"
	"errors"
	"time"
)

// CredentialKind distinguishes how a credential reached the gateway. The kind
// matters for policy — a browser cookie and a native app's bearer token may
// warrant different lifetimes — but not for verification.
type CredentialKind string

const (
	// KindCookie is a credential carried in the session cookie.
	KindCookie CredentialKind = "cookie"
	// KindBearer is a credential carried in an Authorization header. Native
	// apps use this; browsers use KindCookie.
	KindBearer CredentialKind = "bearer"
)

// Credential is an opaque secret presented by a client.
type Credential struct {
	Kind  CredentialKind
	Value string
}

// Principal is the authenticated identity behind a request.
//
// The gateway is single-operator by design — one human owns the machine — so a
// Principal identifies a *device*, not a user. Every device has the operator's
// full authority; devices exist to be revocable independently.
type Principal struct {
	// DeviceID is stable for the lifetime of the pairing.
	DeviceID string
	// DeviceName is the human label chosen at pairing time.
	DeviceName string
	// PairedAt is when the device was enrolled.
	PairedAt time.Time
	// ExpiresAt bounds the device's validity.
	ExpiresAt time.Time
}

// Authenticator resolves a credential to a principal.
//
// Implementations must return an error of kind errx.KindUnauthenticated when the
// credential is missing, malformed, expired, or revoked, and must not
// distinguish those cases in the returned message: telling an attacker that a
// token was "expired" rather than "unknown" leaks validity information.
type Authenticator interface {
	Authenticate(ctx context.Context, cred Credential) (Principal, error)
}

// DeviceStore persists enrolled devices. It is the narrow persistence port the
// devicetoken adapter depends on, so a future SQLite or keychain-backed store is
// a drop-in replacement.
type DeviceStore interface {
	// List returns every device, including revoked ones, for the devices screen.
	List(ctx context.Context) ([]Device, error)
	// FindByTokenHash returns the device whose token hashes to hash.
	FindByTokenHash(ctx context.Context, hash string) (Device, error)
	// Insert adds a device. It fails with errx.KindConflict if the id exists.
	Insert(ctx context.Context, device Device) error
	// Touch records successful use, for the "last seen" column.
	Touch(ctx context.Context, id string, at time.Time) error
	// Revoke marks a device unusable. Revoking an unknown id returns
	// errx.KindNotFound; revoking an already-revoked device is a no-op.
	Revoke(ctx context.Context, id string) error
	// Prune forgets every device whose credential expired before cutoff, and
	// reports how many records went.
	//
	// Expiry stops a credential working; it does not remove the record. Without
	// this the file is a log of every phone ever paired — re-pairing after the
	// session TTL enrols a *new* id and leaves the old row behind, so the devices
	// screen grows a dead entry per month and nothing ever takes one away. The
	// cutoff is the caller's rather than a constant here because how long a dead
	// record is worth showing is a retention decision, not a property of the
	// file format.
	//
	// A revoked device is pruned on the same rule, not immediately: its record is
	// the only evidence that the revocation happened, and the devices screen is
	// where an operator goes to check.
	Prune(ctx context.Context, cutoff time.Time) (int, error)
}

// Device is one enrolled client.
type Device struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// TokenHash is the hex-encoded SHA-256 of the device token. The token itself
	// is never stored: a stolen state file must not yield usable credentials.
	//
	// A plain hash (no salt, no KDF) is correct here because the token is 256
	// bits of CSPRNG output, so there is no dictionary to attack.
	TokenHash string    `json:"tokenHash"`
	CreatedAt time.Time `json:"createdAt"`
	LastSeen  time.Time `json:"lastSeen"`
	ExpiresAt time.Time `json:"expiresAt"`
	UserAgent string    `json:"userAgent,omitempty"`
	Revoked   bool      `json:"revoked,omitempty"`
}

// Expired reports whether the device is past its expiry at time now.
func (d Device) Expired(now time.Time) bool { return !now.Before(d.ExpiresAt) }

// ErrNoCredentials is returned when a request carries no credential at all. The
// HTTP layer treats it as a plain 401 rather than an error worth logging.
var ErrNoCredentials = errors.New("authn: no credentials presented")
