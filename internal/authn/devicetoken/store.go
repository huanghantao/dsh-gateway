// Package devicetoken implements the gateway's baseline authenticator: a
// browser or app presents a high-entropy device token obtained once through a
// short-lived pairing code.
//
// Design notes:
//
//   - Tokens are 256 bits from crypto/rand. Only their SHA-256 is stored, so a
//     leaked state file yields no usable credential. A plain hash suffices
//     because the token has full entropy — there is no dictionary to attack, so
//     a password KDF would add cost without adding security.
//   - The store is a single JSON document written atomically. The gateway is
//     single-operator with at most a handful of devices, so a database would be
//     unjustified weight.
//   - Verification never reveals *why* a credential failed.
package devicetoken

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/atomicfile"
	"github.com/huanghantao/dsh-gateway/internal/authn"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/idgen"
)

// storeVersion is the on-disk schema version. It is bumped whenever the document
// shape changes so that an older gateway refuses rather than misreads it.
const storeVersion = 1

// TokenBytes is the device token length. 32 bytes is the size of a SHA-256
// digest and far beyond brute-force reach.
const TokenBytes = 32

type document struct {
	Version int            `json:"version"`
	Devices []authn.Device `json:"devices"`
}

// Store is a file-backed authn.DeviceStore. All methods are safe for concurrent
// use.
type Store struct {
	path string

	mu      sync.RWMutex
	devices map[string]authn.Device // keyed by device id
}

// Open loads the device store at path, creating an empty one if absent.
func Open(path string) (*Store, error) {
	s := &Store{path: path, devices: map[string]authn.Device{}}

	// path is the state directory the operator configured.
	raw, err := os.ReadFile(path) //nolint:gosec // operator-configured state path
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("devicetoken: read %s: %w", path, err)
	}

	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("devicetoken: parse %s: %w", path, err)
	}
	if doc.Version != storeVersion {
		// Refusing beats guessing: a future schema could encode revocation
		// differently, and silently ignoring it would be a security bug.
		return nil, fmt.Errorf("devicetoken: %s has schema version %d, this build understands %d",
			path, doc.Version, storeVersion)
	}
	for _, d := range doc.Devices {
		s.devices[d.ID] = d
	}
	return s, nil
}

// List implements authn.DeviceStore, newest first.
func (s *Store) List(context.Context) ([]authn.Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]authn.Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// FindByTokenHash implements authn.DeviceStore.
func (s *Store) FindByTokenHash(_ context.Context, hash string) (authn.Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, d := range s.devices {
		// Compare in constant time so a timing signal cannot be used to
		// discover a valid hash byte by byte.
		if subtleCompare(d.TokenHash, hash) {
			return d, nil
		}
	}
	return authn.Device{}, errx.New(errx.KindUnauthenticated, "unknown_credential", "the credential is not valid")
}

// Insert implements authn.DeviceStore.
func (s *Store) Insert(_ context.Context, device authn.Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.devices[device.ID]; exists {
		return errx.New(errx.KindConflict, "device_exists", "a device with that id already exists")
	}
	s.devices[device.ID] = device
	return s.flushLocked()
}

// Touch implements authn.DeviceStore.
func (s *Store) Touch(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.devices[id]
	if !ok {
		return errx.New(errx.KindNotFound, "device_not_found", "no such device")
	}
	d.LastSeen = at
	s.devices[id] = d
	return s.flushLocked()
}

// Revoke implements authn.DeviceStore.
func (s *Store) Revoke(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.devices[id]
	if !ok {
		return errx.New(errx.KindNotFound, "device_not_found", "no such device")
	}
	if d.Revoked {
		return nil
	}
	d.Revoked = true
	s.devices[id] = d
	return s.flushLocked()
}

// flushLocked persists the document. The caller must hold s.mu for writing.
func (s *Store) flushLocked() error {
	doc := document{Version: storeVersion, Devices: make([]authn.Device, 0, len(s.devices))}
	for _, d := range s.devices {
		doc.Devices = append(doc.Devices, d)
	}
	sort.Slice(doc.Devices, func(i, j int) bool { return doc.Devices[i].CreatedAt.Before(doc.Devices[j].CreatedAt) })

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("devicetoken: encode store: %w", err)
	}
	if err := atomicfile.WriteFileSecret(s.path, raw); err != nil {
		return fmt.Errorf("devicetoken: persist store: %w", err)
	}
	return nil
}

// subtleCompare compares two hex strings without an early exit on the first
// differing byte.
func subtleCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// HashToken returns the hex SHA-256 of a device token, the form the store keeps.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newToken returns a fresh device token and its stored hash.
func newToken() (token, hash string) {
	token = idgen.Token(TokenBytes)
	return token, HashToken(token)
}
