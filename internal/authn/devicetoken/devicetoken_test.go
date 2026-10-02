// Package devicetoken is tested from inside the package, unlike the rest of this
// suite: the LastSeen throttle is defined by the unexported lastSeenResolution
// constant, and the on-disk schema test names storeVersion. Everything else
// below uses only the exported API.
package devicetoken

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/authn"
	"github.com/huanghantao/dsh-gateway/internal/errx"
)

// clock is the injected time source, so expiry and the LastSeen throttle are
// asserted at exact instants instead of being slept through.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock { return &clock{at: time.Unix(1_700_000_000, 0)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "devices.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, path
}

func bearer(token string) authn.Credential {
	return authn.Credential{Kind: authn.KindBearer, Value: token}
}

func readStoreFile(t *testing.T, path string) []byte {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	return raw
}

func TestEnrollReturnsATokenThatIsNeverWrittenInPlaintext(t *testing.T) {
	s, path := newTestStore(t)
	a := New(s, 24*time.Hour, newClock().now)

	device, token, err := a.Enroll(context.Background(), "Pixel 9", "Mozilla/5.0")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if len(token) != 2*TokenBytes {
		t.Errorf("token length = %d, want %d hex characters", len(token), 2*TokenBytes)
	}
	if device.TokenHash != HashToken(token) {
		t.Errorf("stored hash = %q, want HashToken(token)", device.TokenHash)
	}

	raw := readStoreFile(t, path)
	if bytes.Contains(raw, []byte(token)) {
		t.Fatal("the device token appears verbatim in the store file: a leaked state file would hand over a usable credential")
	}
	// Positive control. Without it the check above would also pass if the store
	// had written nothing at all.
	if !bytes.Contains(raw, []byte(HashToken(token))) {
		t.Fatal("the token hash is absent from the store file, so the plaintext check proved nothing")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat store: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("store mode = %04o, want 0600", perm)
	}
}

func TestAuthenticateAcceptsTheIssuedToken(t *testing.T) {
	s, _ := newTestStore(t)
	c := newClock()
	a := New(s, 24*time.Hour, c.now)

	device, token, err := a.Enroll(context.Background(), "Pixel 9", "")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	principal, err := a.Authenticate(context.Background(), bearer(token))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal.DeviceID != device.ID {
		t.Errorf("DeviceID = %q, want %q", principal.DeviceID, device.ID)
	}
	if principal.DeviceName != "Pixel 9" {
		t.Errorf("DeviceName = %q, want %q", principal.DeviceName, "Pixel 9")
	}
	if !principal.PairedAt.Equal(device.CreatedAt) {
		t.Errorf("PairedAt = %v, want %v", principal.PairedAt, device.CreatedAt)
	}
	if !principal.ExpiresAt.Equal(device.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", principal.ExpiresAt, device.ExpiresAt)
	}

	// A different token of the same shape must not authenticate.
	if _, err := a.Authenticate(context.Background(), bearer(strings.Repeat("ab", TokenBytes))); err == nil {
		t.Error("an unrelated token authenticated")
	}
}

// TestEveryRejectionIsIndistinguishable pins the promise in the authn port: an
// attacker must not be able to probe which devices exist, which expired, or
// which were revoked.
func TestEveryRejectionIsIndistinguishable(t *testing.T) {
	s, _ := newTestStore(t)
	c := newClock()
	ctx := context.Background()

	longLived := New(s, 24*time.Hour, c.now)
	_, liveToken, err := longLived.Enroll(ctx, "live", "")
	if err != nil {
		t.Fatalf("Enroll(live): %v", err)
	}
	revoked, revokedToken, err := longLived.Enroll(ctx, "revoked", "")
	if err != nil {
		t.Fatalf("Enroll(revoked): %v", err)
	}
	if err := longLived.Revoke(ctx, revoked.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// The expired device is inserted by hand so that its token is known: a
	// device enrolled with a one-minute TTL would otherwise keep its token
	// inside the store, which is exactly what the design forbids.
	expiredToken := strings.Repeat("ef", TokenBytes)
	if err := s.Insert(ctx, authn.Device{
		ID:        "dev_expired",
		Name:      "expired",
		TokenHash: HashToken(expiredToken),
		CreatedAt: c.now(),
		LastSeen:  c.now(),
		ExpiresAt: c.now().Add(time.Minute),
	}); err != nil {
		t.Fatalf("Insert(expired): %v", err)
	}
	c.advance(2 * time.Minute)

	rejections := map[string]string{
		"never issued": strings.Repeat("cd", TokenBytes),
		"revoked":      revokedToken,
		"expired":      expiredToken,
		"not even hex": "not-a-token",
	}
	want := ""
	for name, token := range rejections {
		_, err := longLived.Authenticate(ctx, bearer(token))
		if err == nil {
			t.Fatalf("the %s credential authenticated", name)
		}
		if got := errx.KindOf(err); got != errx.KindUnauthenticated {
			t.Errorf("%s: kind = %v, want unauthenticated", name, got)
		}
		if got := errx.CodeOf(err); got != "unknown_credential" {
			t.Errorf("%s: code = %q, want unknown_credential", name, got)
		}
		if want == "" {
			want = err.Error()
			continue
		}
		if err.Error() != want {
			t.Errorf("%s was rejected with %q but the other cases with %q: the difference "+
				"tells a caller which devices exist, expired, or were revoked", name, err, want)
		}
	}

	// Positive control: the store and the clock are live, so the four
	// rejections above are rejections and not a broken fixture.
	if _, err := longLived.Authenticate(ctx, bearer(liveToken)); err != nil {
		t.Fatalf("the live device was rejected: %v", err)
	}
}

func TestMissingCredentialsAreReportedAsSuch(t *testing.T) {
	s, _ := newTestStore(t)
	a := New(s, time.Hour, newClock().now)
	ctx := context.Background()

	for _, cred := range []authn.Credential{{}, {Kind: authn.KindCookie, Value: "   "}} {
		_, err := a.Authenticate(ctx, cred)
		if !errors.Is(err, authn.ErrNoCredentials) {
			t.Errorf("Authenticate(%+v) = %v, want authn.ErrNoCredentials", cred, err)
		}
	}
}

func TestATokenExpiresExactlyAtItsExpiryInstant(t *testing.T) {
	s, _ := newTestStore(t)
	c := newClock()
	const ttl = time.Hour
	a := New(s, ttl, c.now)
	ctx := context.Background()

	_, token, err := a.Enroll(ctx, "phone", "")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	c.advance(ttl - time.Nanosecond)
	if _, err := a.Authenticate(ctx, bearer(token)); err != nil {
		t.Errorf("the token was rejected a nanosecond before expiry: %v", err)
	}

	c.advance(time.Nanosecond)
	_, err = a.Authenticate(ctx, bearer(token))
	if err == nil {
		t.Fatal("the token was still accepted at its expiry instant")
	}
	if got := errx.KindOf(err); got != errx.KindUnauthenticated {
		t.Errorf("kind = %v, want unauthenticated", got)
	}
}

func TestLastSeenWritesAreThrottled(t *testing.T) {
	s, path := newTestStore(t)
	c := newClock()
	a := New(s, 24*time.Hour, c.now)
	ctx := context.Background()

	device, token, err := a.Enroll(ctx, "phone", "")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	before := readStoreFile(t, path)

	// One nanosecond short of the resolution: repeated use must not touch the
	// disk, or every request would rewrite the whole document.
	c.advance(lastSeenResolution - time.Nanosecond)
	if _, err := a.Authenticate(ctx, bearer(token)); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got := readStoreFile(t, path); !bytes.Equal(got, before) {
		t.Error("Authenticate rewrote the store before the resolution elapsed")
	}

	// Exactly at the resolution the bookkeeping write must happen.
	c.advance(time.Nanosecond)
	if _, err := a.Authenticate(ctx, bearer(token)); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	after := readStoreFile(t, path)
	if bytes.Equal(after, before) {
		t.Fatal("Authenticate did not persist LastSeen after the resolution elapsed")
	}

	devices, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(devices) != 1 || devices[0].ID != device.ID {
		t.Fatalf("List = %+v, want the one enrolled device", devices)
	}
	if !devices[0].LastSeen.Equal(c.now()) {
		t.Errorf("LastSeen = %v, want the instant of that call (%v)", devices[0].LastSeen, c.now())
	}

	// And the next request, immediately afterwards, must not write again.
	if _, err := a.Authenticate(ctx, bearer(token)); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got := readStoreFile(t, path); !bytes.Equal(got, after) {
		t.Error("Authenticate rewrote the store twice inside one resolution window")
	}
}

func TestStoreRoundTripsThroughOpen(t *testing.T) {
	s, path := newTestStore(t)
	c := newClock()
	a := New(s, 24*time.Hour, c.now)
	ctx := context.Background()

	device, token, err := a.Enroll(ctx, "tablet", "Safari")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open after Enroll: %v", err)
	}
	restarted := New(reopened, 24*time.Hour, c.now)

	principal, err := restarted.Authenticate(ctx, bearer(token))
	if err != nil {
		t.Fatalf("the token issued before the restart no longer authenticates: %v", err)
	}
	if principal.DeviceID != device.ID || principal.DeviceName != "tablet" {
		t.Errorf("principal = %+v, want device %q named %q", principal, device.ID, "tablet")
	}
	if !principal.ExpiresAt.Equal(device.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v (the expiry must survive a restart)", principal.ExpiresAt, device.ExpiresAt)
	}

	list, err := restarted.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].TokenHash != device.TokenHash {
		t.Errorf("List after reopen = %+v, want the enrolled device", list)
	}
}

func TestOpenRefusesAnUnknownSchemaVersion(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		wantErr bool
		wantMsg string
	}{
		{"the current schema", `{"version":1,"devices":[]}`, false, ""},
		{"a future schema", `{"version":2,"devices":[]}`, true, "version"},
		{"a document with no version field", `{"devices":[]}`, true, "version"},
		{"an explicit zero version", `{"version":0,"devices":[]}`, true, "version"},
		{"not JSON at all", `{"version":`, true, "parse"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "devices.json")
			if err := os.WriteFile(path, []byte(tc.doc), 0o600); err != nil {
				t.Fatalf("seed store: %v", err)
			}

			_, err := Open(path)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Open = %v, want success", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Open accepted a document this build does not understand; misreading it could resurrect a revoked device")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tc.wantMsg)
			}
		})
	}
}

func TestInsertOfADuplicateIDIsAConflict(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	device := authn.Device{ID: "dev_1", Name: "one", TokenHash: HashToken("first"), CreatedAt: time.Unix(0, 0)}
	if err := s.Insert(ctx, device); err != nil {
		t.Fatalf("first Insert: %v", err)
	}

	err := s.Insert(ctx, device)
	if err == nil {
		t.Fatal("a duplicate device id was accepted, overwriting the existing device")
	}
	if got := errx.KindOf(err); got != errx.KindConflict {
		t.Errorf("kind = %v, want conflict", got)
	}
	if got := errx.CodeOf(err); got != "device_exists" {
		t.Errorf("code = %q, want device_exists", got)
	}

	// The original must be untouched.
	got, err := s.FindByTokenHash(ctx, device.TokenHash)
	if err != nil {
		t.Fatalf("FindByTokenHash after the rejected Insert: %v", err)
	}
	if got.ID != device.ID || got.Name != "one" {
		t.Errorf("stored device = %+v, want the original", got)
	}
}

func TestFindByTokenHashRejectsUnknownHashes(t *testing.T) {
	s, _ := newTestStore(t)

	// A wrong length must be rejected outright rather than compared byte by
	// byte, and a same-length mismatch must fall through to the same error.
	for _, hash := range []string{strings.Repeat("00", 32), "", "00", strings.Repeat("00", 31)} {
		_, err := s.FindByTokenHash(context.Background(), hash)
		if err == nil {
			t.Fatalf("hash %q was found in an empty store", hash)
		}
		if got := errx.KindOf(err); got != errx.KindUnauthenticated {
			t.Errorf("hash %q: kind = %v, want unauthenticated", hash, got)
		}
		if got := errx.CodeOf(err); got != "unknown_credential" {
			t.Errorf("hash %q: code = %q, want unknown_credential", hash, got)
		}
	}
}

func TestEnrollNormalisesDeviceNamesAndUserAgents(t *testing.T) {
	// Both fields are written straight into the JSON document and shown on the
	// devices screen, so they are bounded here rather than trusted from a
	// client that controls the header.
	s, _ := newTestStore(t)
	a := New(s, time.Hour, nil) // nil clock: the wall clock is fine for this
	ctx := context.Background()

	tests := []struct {
		name      string
		givenName string
		givenUA   string
		wantName  string
		wantUA    string
	}{
		{"a trimmed name", "  Pixel 9  ", "Mozilla/5.0", "Pixel 9", "Mozilla/5.0"},
		{"an empty name becomes a usable label", "   ", "", "Unnamed device", ""},
		{"an over-long name is truncated", strings.Repeat("n", 100), "", strings.Repeat("n", 64), ""},
		{"an over-long user agent is truncated", "phone", strings.Repeat("u", 1000), "phone", strings.Repeat("u", 256)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			device, _, err := a.Enroll(ctx, tc.givenName, tc.givenUA)
			if err != nil {
				t.Fatalf("Enroll: %v", err)
			}
			if device.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", device.Name, tc.wantName)
			}
			if device.UserAgent != tc.wantUA {
				t.Errorf("UserAgent has length %d, want %d", len(device.UserAgent), len(tc.wantUA))
			}
		})
	}
}

func TestOpenReportsAnUnreadableStorePath(t *testing.T) {
	// A directory where the store belongs is an operator mistake; it must be an
	// error, not an empty store that then refuses every device.
	if _, err := Open(t.TempDir()); err == nil {
		t.Error("Open accepted a directory as the store file")
	}
}

func TestRevokeOfAnUnknownDeviceIsNotFound(t *testing.T) {
	s, _ := newTestStore(t)

	err := s.Revoke(context.Background(), "dev_missing")
	if err == nil {
		t.Fatal("revoking an unknown device succeeded")
	}
	if got := errx.KindOf(err); got != errx.KindNotFound {
		t.Errorf("kind = %v, want not found", got)
	}
	if got := errx.CodeOf(err); got != "device_not_found" {
		t.Errorf("code = %q, want device_not_found", got)
	}
}

func TestRevokingTwiceIsNotAnError(t *testing.T) {
	s, _ := newTestStore(t)
	c := newClock()
	a := New(s, time.Hour, c.now)
	ctx := context.Background()

	device, token, err := a.Enroll(ctx, "stolen phone", "")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	if err := a.Revoke(ctx, device.ID); err != nil {
		t.Fatalf("first Revoke: %v", err)
	}
	// A device screen that retries must not see a scary error the second time.
	if err := a.Revoke(ctx, device.ID); err != nil {
		t.Errorf("second Revoke = %v, want nil", err)
	}

	_, err = a.Authenticate(ctx, bearer(token))
	if err == nil {
		t.Fatal("a revoked token still authenticated")
	}
	if got := errx.KindOf(err); got != errx.KindUnauthenticated {
		t.Errorf("kind = %v, want unauthenticated", got)
	}

	// A revoked device must stay visible on the devices screen, flagged, rather
	// than vanishing from it.
	devices, err := a.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(devices) != 1 || devices[0].ID != device.ID || !devices[0].Revoked {
		t.Errorf("List = %+v, want the revoked device still listed and flagged", devices)
	}
}

func TestRevocationSurvivesARestart(t *testing.T) {
	s, path := newTestStore(t)
	c := newClock()
	a := New(s, time.Hour, c.now)
	ctx := context.Background()

	device, token, err := a.Enroll(ctx, "stolen phone", "")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if err := a.Revoke(ctx, device.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// A revocation that does not reach the disk would be undone by a restart,
	// which is the one moment an operator is most likely to rely on it.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	restarted := New(reopened, time.Hour, c.now)
	if _, err := restarted.Authenticate(ctx, bearer(token)); err == nil {
		t.Fatal("a token revoked before the restart authenticated after it")
	}
}

func TestTouchOfAnUnknownDeviceIsNotFound(t *testing.T) {
	s, _ := newTestStore(t)

	err := s.Touch(context.Background(), "dev_missing", time.Unix(0, 0))
	if err == nil {
		t.Fatal("touching an unknown device succeeded")
	}
	if got := errx.KindOf(err); got != errx.KindNotFound {
		t.Errorf("kind = %v, want not found", got)
	}
}

func TestListReturnsNewestFirst(t *testing.T) {
	s, _ := newTestStore(t)
	c := newClock()
	a := New(s, time.Hour, c.now)
	ctx := context.Background()

	var want []string
	for i := 0; i < 3; i++ {
		device, _, err := a.Enroll(ctx, fmt.Sprintf("device-%d", i), "")
		if err != nil {
			t.Fatalf("Enroll %d: %v", i, err)
		}
		want = append([]string{device.ID}, want...) // newest first
		c.advance(time.Minute)
	}

	list, err := a.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != len(want) {
		t.Fatalf("List returned %d devices, want %d", len(list), len(want))
	}
	for i, id := range want {
		if list[i].ID != id {
			t.Errorf("device %d = %q, want %q (newest first)", i, list[i].ID, id)
		}
	}
}

func TestConcurrentEnrollAndAuthenticate(t *testing.T) {
	s, _ := newTestStore(t)
	a := New(s, time.Hour, newClock().now)
	ctx := context.Background()

	if _, _, err := a.Enroll(ctx, "existing", ""); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	_, existingToken, err := a.Enroll(ctx, "existing-2", "")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	const workers = 8
	tokens := make([]string, workers)
	errs := make([]error, workers)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				if _, err := a.Authenticate(ctx, bearer(existingToken)); err != nil {
					errs[i] = fmt.Errorf("authenticate: %w", err)
					return
				}
			}
			if err := a.Revoke(ctx, "dev_missing"); errx.KindOf(err) != errx.KindNotFound {
				errs[i] = errors.New("revoking an unknown id did not report not-found")
				return
			}
			_, token, err := a.Enroll(ctx, fmt.Sprintf("device-%d", i), "")
			if err != nil {
				errs[i] = fmt.Errorf("enroll: %w", err)
				return
			}
			tokens[i] = token
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
	}
	for i, token := range tokens {
		if token == "" {
			continue
		}
		principal, err := a.Authenticate(ctx, bearer(token))
		if err != nil {
			t.Errorf("the token issued by worker %d does not authenticate: %v", i, err)
			continue
		}
		if principal.DeviceID == "" {
			t.Errorf("worker %d authenticated without a device id", i)
		}
	}

	list, err := a.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != workers+2 {
		t.Errorf("List returned %d devices, want %d", len(list), workers+2)
	}
}
