// Package pairing issues and redeems the one-time codes that enroll a device.
//
// The code is derived, not stored: code = f(secret, time window). Deriving it
// has three consequences worth stating, because they are the reason for the
// design.
//
//  1. There is no mutable pairing state to persist or to get out of sync. The
//     gateway and the `dsh-gateway pair` command, running as separate processes,
//     independently compute the same code from the same secret file.
//  2. No administrative HTTP endpoint is needed to re-print a code. Anything
//     reachable over the tunnel is attack surface; a local file read is not.
//  3. Rotation is automatic and needs no timer: advancing the clock advances the
//     window.
//
// A previous-window grace period is accepted so that a code printed a moment
// before a boundary does not fail through no fault of the operator.
package pairing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/atomicfile"
	"github.com/huanghantao/dsh-gateway/internal/authn"
	"github.com/huanghantao/dsh-gateway/internal/authn/ratelimit"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/idgen"
)

// secretBytes is the length of the persisted pairing secret.
const secretBytes = 32

// CodeLength is the number of characters in a pairing code. Eight characters over
// a 30-symbol alphabet is ~39.2 bits, which is only adequate because a code is
// valid for one short window and attempts are rate-limited.
//
// A code is a short-lived shared secret, not a single-use token: Redeem records
// nothing, so a code that is observed can still enroll a second device until its
// window closes. That is a deliberate consequence of deriving codes from a
// secret rather than storing them — the property that lets `dsh-gateway pair`
// compute the same code in a separate process is the same property that makes
// consumption unobservable here. The compensating controls are the TTL (ten
// minutes by default), the rate limit and lockout on the endpoint, and the fact
// that every enrolled device is listed and revocable in one request.
const CodeLength = 8

// codeAlphabet omits 0/O, 1/I/L, and U so a code can be read from a screen and
// typed without ambiguity.
const codeAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"

// Enroller creates a device for a redeemed code.
type Enroller interface {
	Enroll(ctx context.Context, name, userAgent string) (authn.Device, string, error)
}

// Service validates pairing codes and enrolls devices.
type Service struct {
	secret   []byte
	ttl      time.Duration
	enroller Enroller
	attempts *ratelimit.Limiter
	now      func() time.Time
}

// Open loads the pairing secret from dir, creating it on first run.
func Open(dir string, ttl time.Duration, enroller Enroller, attempts *ratelimit.Limiter, now func() time.Time) (*Service, error) {
	if now == nil {
		now = time.Now
	}
	secret, err := loadOrCreateSecret(filepath.Join(dir, "pairing.key"))
	if err != nil {
		return nil, err
	}
	return &Service{secret: secret, ttl: ttl, enroller: enroller, attempts: attempts, now: now}, nil
}

// loadOrCreateSecret reads the secret, generating it with 0600 permissions the
// first time.
func loadOrCreateSecret(path string) ([]byte, error) {
	// path is <stateDir>/pairing.key, built by this package from configuration.
	raw, err := os.ReadFile(path) //nolint:gosec // operator-configured state path
	if err == nil {
		secret, decErr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil {
			return nil, fmt.Errorf("pairing: %s is not valid hex: %w", path, decErr)
		}
		if len(secret) != secretBytes {
			return nil, fmt.Errorf("pairing: %s holds %d bytes, want %d", path, len(secret), secretBytes)
		}
		return secret, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("pairing: read %s: %w", path, err)
	}

	secret := idgen.Bytes(secretBytes)
	// WriteFileSecret creates the file 0600 and replaces it atomically, so a
	// concurrent starter never observes a half-written key.
	if err := atomicfile.WriteFileSecret(path, []byte(hex.EncodeToString(secret))); err != nil {
		return nil, fmt.Errorf("pairing: create %s: %w", path, err)
	}
	return secret, nil
}

// TTL returns the code lifetime.
func (s *Service) TTL() time.Duration { return s.ttl }

// Current returns the code for the current window and when it expires.
func (s *Service) Current() (code string, expiresAt time.Time) {
	now := s.now()
	window := now.Unix() / int64(s.ttl.Seconds())
	return s.codeFor(window), time.Unix((window+1)*int64(s.ttl.Seconds()), 0)
}

// Redeem validates code and, on success, enrolls a device.
//
// key identifies the caller for rate limiting and lockout. A rejected attempt
// counts against key; a successful one clears the history.
func (s *Service) Redeem(ctx context.Context, key, code, deviceName, userAgent string) (authn.Device, string, error) {
	// Both collaborators are optional because the `dsh-gateway pair` helper
	// derives codes without ever redeeming one. Guarding here means a future
	// caller that does redeem gets an error naming the problem instead of a nil
	// dereference.
	if s.attempts == nil || s.enroller == nil {
		return authn.Device{}, "", errx.New(errx.KindInternal, "pairing_not_configured",
			"this process cannot redeem pairing codes")
	}

	if wait := s.attempts.LockedFor(key); wait > 0 {
		return authn.Device{}, "", errx.New(errx.KindRateLimited, "pairing_locked",
			"too many failed pairing attempts; try again later")
	}

	if !s.valid(code) {
		s.attempts.Fail(key)
		return authn.Device{}, "", errx.New(errx.KindUnauthenticated, "invalid_pairing_code",
			"the pairing code is not valid or has expired")
	}

	device, token, err := s.enroller.Enroll(ctx, deviceName, userAgent)
	if err != nil {
		return authn.Device{}, "", err
	}
	s.attempts.Succeed(key)
	return device, token, nil
}

// valid reports whether code matches the current or the previous window.
func (s *Service) valid(code string) bool {
	// Codes are displayed uppercase; accept any case so that a phone keyboard
	// that autocapitalises differently does not cause a failure.
	normalized := strings.ToUpper(strings.TrimSpace(code))
	if len(normalized) != CodeLength {
		return false
	}

	window := s.now().Unix() / int64(s.ttl.Seconds())
	// hmac.Equal is constant time, so the comparison leaks neither the position
	// of the first wrong character nor the code's length beyond CodeLength.
	for _, w := range []int64{window, window - 1} {
		if hmac.Equal([]byte(normalized), []byte(s.codeFor(w))) {
			return true
		}
	}
	return false
}

// codeFor derives the code for a window using HMAC-SHA256 as a PRF, then maps
// the digest onto the unambiguous alphabet with rejection sampling so that every
// character is uniformly distributed.
func (s *Service) codeFor(window int64) string {
	// Windows are derived from a Unix timestamp divided by a positive TTL, so a
	// negative value means the system clock is set before 1970. Clamping keeps
	// the conversion to uint64 well-defined and still yields a stable, secret
	// code — it just makes such a code depend on window 0.
	if window < 0 {
		window = 0
	}

	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(window))

	mac := hmac.New(sha256.New, s.secret)
	mac.Write(counter[:])
	digest := mac.Sum(nil)

	out := make([]byte, 0, CodeLength)

	// Rejection sampling, with the threshold derived from the alphabet rather
	// than hardcoded.
	//
	// limit is the largest multiple of the alphabet length that fits in a byte;
	// bytes at or above it are discarded so every symbol is equally likely.
	// Deriving it is the point: a threshold copied from a different alphabet
	// silently biases the tail, and no test of membership would catch it.
	limit := 256 - (256 % len(codeAlphabet))

	// Each round consumes one HMAC block. One block yields far more accepted
	// symbols than CodeLength in practice, so the loop runs once; repeating with
	// an incremented counter is what keeps the distribution exact in the unlikely
	// event that it does not, rather than padding with a biased byte.
	//
	// The counter is an int written as four bytes rather than a single byte: a
	// byte would wrap, and a wrapped counter would re-derive round 0's digest and
	// spin forever. Reaching even round 2 is already vanishingly improbable, so
	// the wider counter costs nothing and removes the possibility outright.
	for round := 0; len(out) < CodeLength; round++ {
		if round > 0 {
			var extra [4]byte
			binary.BigEndian.PutUint32(extra[:], uint32(round))
			mac.Reset()
			mac.Write(counter[:])
			mac.Write(extra[:])
			digest = mac.Sum(nil)
		}
		for _, b := range digest {
			if int(b) >= limit {
				continue // reject the biased tail
			}
			out = append(out, codeAlphabet[int(b)%len(codeAlphabet)])
			if len(out) == CodeLength {
				break
			}
		}
	}
	return string(out)
}
