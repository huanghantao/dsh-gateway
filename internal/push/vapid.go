package push

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/atomicfile"
)

// vapidFile holds the application server's signing key.
const vapidFile = "vapid.key"

// vapidLifetime is how long a signed token is valid. Push services reject
// anything beyond 24 hours; twelve leaves room for a clock that is a little off
// without ever presenting a token that outlives its usefulness.
const vapidLifetime = 12 * time.Hour

// VAPID is the application server identity a push service authenticates.
//
// It is generated on first use and kept in the state directory: the private key
// is what lets this gateway send to subscriptions it created, so losing it costs
// every phone a re-subscribe, and leaking it would let someone else send as us.
// Hence 0600 and an atomic write, like every other secret here.
type VAPID struct {
	key *ecdsa.PrivateKey
	// publicKey is `pushManager.subscribe`'s applicationServerKey, derived once
	// at open time. It is cached rather than computed per call because deriving
	// it goes through crypto/ecdh, which reports an error for a point it cannot
	// validate — and there is no useful answer to give a caller that asked for
	// the public half of a key that already loaded successfully.
	publicKey string
	subject   string
	now       func() time.Time
}

// OpenVAPID loads the signing key, creating one the first time.
func OpenVAPID(stateDir, subject string) (*VAPID, error) {
	if stateDir == "" {
		return nil, errors.New("push: a state directory is required")
	}
	if subject == "" {
		// Mirrors config.DefaultPushSubject. Duplicated rather than imported
		// because this package is an adapter and does not depend on the
		// configuration layer; the gateway validates the operator's value at
		// startup, so this fallback only serves a caller that built a VAPID
		// directly.
		subject = "mailto:dsh-gateway@localhost"
	}
	path := filepath.Join(stateDir, vapidFile)

	key, err := loadVAPID(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("push: generate vapid key: %w", err)
		}
		if err := saveVAPID(path, key); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	}
	encoded, err := encodeP256Point(key.PublicKey.ECDH())
	if err != nil {
		return nil, fmt.Errorf("push: vapid public key: %w", err)
	}
	return &VAPID{key: key, publicKey: encoded, subject: subject, now: time.Now}, nil
}

// PublicKey is what a browser passes to `pushManager.subscribe`.
func (v *VAPID) PublicKey() string { return v.publicKey }

// encodeP256Point renders a public key the way a browser expects it: the
// uncompressed point, 0x04 followed by X and Y.
//
// The bytes are `ecdh.PublicKey.Bytes`' uncompressed point, identical to what
// `elliptic.Marshal` produced, so a subscription created before the move keeps
// working.
func encodeP256Point(pub *ecdh.PublicKey, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return EncodeKey(pub.Bytes()), nil
}

// Authorization builds the `Authorization` header for one endpoint.
//
// The audience is the push service's origin, not the full endpoint: that is what
// RFC 8292 asks for, and signing the wrong audience is the usual reason a push
// service answers 401 with no explanation.
func (v *VAPID) Authorization(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("push: endpoint: %w", err)
	}
	audience := fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)

	header, err := json.Marshal(map[string]string{"typ": "JWT", "alg": "ES256"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"aud": audience,
		"exp": v.now().Add(vapidLifetime).Unix(),
		"sub": v.subject,
	})
	if err != nil {
		return "", err
	}

	signing := EncodeKey(header) + "." + EncodeKey(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, v.key, digest[:])
	if err != nil {
		return "", fmt.Errorf("push: sign: %w", err)
	}
	// JWS wants r and s fixed-width and concatenated; big.Int.Bytes() drops
	// leading zeros, which would produce a signature that verifies only by luck.
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])

	return fmt.Sprintf("vapid t=%s.%s, k=%s", signing, EncodeKey(signature), v.PublicKey()), nil
}

// loadVAPID reads the stored private scalar.
//
// The format — one base64url-encoded 32-byte scalar — is deliberately unchanged.
// Existing phones subscribed against this key, and losing it costs every one of
// them a re-subscribe, so a tidier encoding is not worth the migration.
//
// The public point is derived through crypto/ecdh rather than
// elliptic.ScalarBaseMult. That function is deprecated as a low-level unsafe API
// and rightly so: it performs no validation, where ecdh.P256().NewPrivateKey
// rejects a scalar that is zero or out of range.
func loadVAPID(path string) (*ecdsa.PrivateKey, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own state directory
	if err != nil {
		return nil, err
	}
	scalar, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("push: vapid key is not readable: %w", err)
	}
	priv, err := ecdh.P256().NewPrivateKey(scalar)
	if err != nil {
		return nil, fmt.Errorf("push: vapid key is not a valid P-256 scalar: %w", err)
	}
	x, y, err := splitP256Point(priv.PublicKey().Bytes())
	if err != nil {
		return nil, fmt.Errorf("push: vapid key: %w", err)
	}
	key := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y},
		D:         new(big.Int).SetBytes(scalar),
	}
	return key, nil
}

// splitP256Point splits an uncompressed point into its coordinates.
//
// Hand-rolled because its two obvious helpers are both unusable here:
// elliptic.Unmarshal is deprecated, and crypto/ecdh deliberately exposes no
// coordinate accessor. There is nothing to validate — the point came from an
// ecdh key that already validated it — so this only has to cut it in half.
func splitP256Point(encoded []byte) (*big.Int, *big.Int, error) {
	const (
		uncompressed = 65
		coordinate   = 32
		prefix       = 4
	)
	if len(encoded) != uncompressed || encoded[0] != prefix {
		return nil, nil, fmt.Errorf("point is %d bytes and does not start with 0x04", len(encoded))
	}
	x := new(big.Int).SetBytes(encoded[1 : 1+coordinate])
	y := new(big.Int).SetBytes(encoded[1+coordinate:])
	return x, y, nil
}

// saveVAPID writes the private scalar, 0600 and atomically.
func saveVAPID(path string, key *ecdsa.PrivateKey) error {
	scalar := make([]byte, 32)
	key.D.FillBytes(scalar)
	if err := atomicfile.WriteFileSecret(path, []byte(base64.RawURLEncoding.EncodeToString(scalar))); err != nil {
		return fmt.Errorf("push: write vapid key: %w", err)
	}
	return nil
}
