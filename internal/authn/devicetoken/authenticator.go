package devicetoken

import (
	"context"
	"strings"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/authn"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/idgen"
)

// lastSeenResolution throttles LastSeen writes. Persisting on every request
// would rewrite the store file thousands of times an hour for no benefit.
const lastSeenResolution = 5 * time.Minute

// Authenticator verifies device tokens against a store.
type Authenticator struct {
	store authn.DeviceStore
	ttl   time.Duration
	now   func() time.Time
}

// New builds an Authenticator. ttl is how long a newly enrolled device stays
// valid; now is injectable so tests need no clock control.
func New(store authn.DeviceStore, ttl time.Duration, now func() time.Time) *Authenticator {
	if now == nil {
		now = time.Now
	}
	return &Authenticator{store: store, ttl: ttl, now: now}
}

// Enroll creates a device and returns it alongside its token. The token is
// returned exactly once, here; it is not recoverable afterwards.
func (a *Authenticator) Enroll(ctx context.Context, name, userAgent string) (authn.Device, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Unnamed device"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	if len(userAgent) > 256 {
		userAgent = userAgent[:256]
	}

	token, hash := newToken()
	now := a.now()
	device := authn.Device{
		ID:        idgen.New("dev"),
		Name:      name,
		TokenHash: hash,
		CreatedAt: now,
		LastSeen:  now,
		ExpiresAt: now.Add(a.ttl),
		UserAgent: userAgent,
	}
	if err := a.store.Insert(ctx, device); err != nil {
		return authn.Device{}, "", err
	}
	return device, token, nil
}

// Authenticate implements authn.Authenticator.
func (a *Authenticator) Authenticate(ctx context.Context, cred authn.Credential) (authn.Principal, error) {
	token := strings.TrimSpace(cred.Value)
	if token == "" {
		return authn.Principal{}, authn.ErrNoCredentials
	}

	// The hash lookup is the only path: a credential that does not hash to a
	// known device is indistinguishable from a random string.
	device, err := a.store.FindByTokenHash(ctx, HashToken(token))
	if err != nil {
		return authn.Principal{}, err
	}

	now := a.now()
	// Every rejection returns the same error so that a caller cannot probe
	// device state — which devices exist, which expired, which were revoked.
	if device.Revoked || device.Expired(now) {
		return authn.Principal{}, errx.New(errx.KindUnauthenticated, "unknown_credential", "the credential is not valid")
	}

	if now.Sub(device.LastSeen) >= lastSeenResolution {
		// A failure here must not fail the request: an unwritable state
		// directory should degrade bookkeeping, not lock the operator out.
		_ = a.store.Touch(ctx, device.ID, now)
	}

	return authn.Principal{
		DeviceID:   device.ID,
		DeviceName: device.Name,
		PairedAt:   device.CreatedAt,
		ExpiresAt:  device.ExpiresAt,
	}, nil
}

// List exposes enrolled devices for the device-management screen.
func (a *Authenticator) List(ctx context.Context) ([]authn.Device, error) {
	return a.store.List(ctx)
}

// Revoke disables a device. It is idempotent.
func (a *Authenticator) Revoke(ctx context.Context, id string) error {
	return a.store.Revoke(ctx, id)
}
