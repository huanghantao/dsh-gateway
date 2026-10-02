package pairing_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/authn"
	"github.com/huanghantao/dsh-gateway/internal/authn/ratelimit"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/pairing"
)

// testTTL is the shipped pairing window. Tests start their clocks exactly on a
// window boundary so that "the previous window" is an exact instant rather than
// an off-by-one guess.
const testTTL = 10 * time.Minute

// pairingAlphabet is the published code alphabet, duplicated from the package on
// purpose: it is a product contract (these are the characters an operator may
// have to read off a screen and type), so changing it should require a
// deliberate edit here as well.
const pairingAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"

// startOfWindow returns the first instant of window n for ttl.
func startOfWindow(n int64, ttl time.Duration) time.Time {
	return time.Unix(n*int64(ttl.Seconds()), 0)
}

// clock is the injected time source, so no test sleeps.
type clock struct{ at time.Time }

func (c *clock) now() time.Time  { return c.at }
func (c *clock) set(t time.Time) { c.at = t }

// fakeEnroller records enrollments instead of touching a device store, so a test
// can assert exactly how many devices one code produced and with what metadata.
type fakeEnroller struct {
	mu       sync.Mutex
	calls    int
	lastName string
	lastUA   string
	err      error
}

func (e *fakeEnroller) Enroll(_ context.Context, name, userAgent string) (authn.Device, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.calls++
	e.lastName, e.lastUA = name, userAgent
	if e.err != nil {
		return authn.Device{}, "", e.err
	}
	return authn.Device{ID: "dev_test", Name: name}, "tok_test", nil
}

func (e *fakeEnroller) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *fakeEnroller) metadata() (string, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastName, e.lastUA
}

// attemptLimiter is deliberately tiny: a test reaches the lockout in three
// rejected codes instead of a hundred.
func attemptLimiter(c *clock) *ratelimit.Limiter {
	return ratelimit.New(ratelimit.Config{
		PerSecond:    1,
		Burst:        1,
		MaxFailures:  2,
		LockoutBase:  time.Minute,
		MaxLockout:   time.Hour,
		IdleEviction: time.Hour,
	}, c.now)
}

func openService(t *testing.T, dir string, c *clock, enr pairing.Enroller, lim *ratelimit.Limiter) *pairing.Service {
	t.Helper()

	svc, err := pairing.Open(dir, testTTL, enr, lim, c.now)
	if err != nil {
		t.Fatalf("pairing.Open(%s): %v", dir, err)
	}
	return svc
}

// harness is one running gateway: a fresh secret, a fresh limiter, and a
// recording enroller.
type harness struct {
	dir   string
	clock *clock
	lim   *ratelimit.Limiter
	enr   *fakeEnroller
	svc   *pairing.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	dir := t.TempDir()
	c := &clock{at: startOfWindow(1_000_000, testTTL)}
	lim := attemptLimiter(c)
	enr := &fakeEnroller{}
	return &harness{dir: dir, clock: c, lim: lim, enr: enr, svc: openService(t, dir, c, enr, lim)}
}

func TestCurrentIsStableInsideAWindowAndRotatesAtTheBoundary(t *testing.T) {
	h := newHarness(t)

	first, expires := h.svc.Current()
	if want := h.clock.at.Add(testTTL); !expires.Equal(want) {
		t.Errorf("expiresAt = %v, want %v (the end of the window)", expires, want)
	}

	h.clock.set(h.clock.at.Add(testTTL - time.Nanosecond))
	if again, _ := h.svc.Current(); again != first {
		t.Errorf("the code changed one nanosecond before the boundary: %q then %q", first, again)
	}

	h.clock.set(h.clock.at.Add(time.Nanosecond))
	next, nextExpires := h.svc.Current()
	if next == first {
		t.Error("the code did not rotate at the window boundary")
	}
	if want := h.clock.at.Add(testTTL); !nextExpires.Equal(want) {
		t.Errorf("expiresAt after rotation = %v, want %v", nextExpires, want)
	}
}

func TestTheSameSecretAndWindowProduceTheSameCodeInSeparateProcesses(t *testing.T) {
	// This is the whole reason the code is derived rather than stored: the
	// `dsh-gateway pair` helper is a separate process with its own Service and
	// no enroller, and it must print the code the running gateway will accept.
	dir := t.TempDir()
	const window = 1_000_000

	gatewayClock := &clock{at: startOfWindow(window, testTTL).Add(time.Minute)}
	helperClock := &clock{at: startOfWindow(window, testTTL).Add(9 * time.Minute)}

	gateway := openService(t, dir, gatewayClock, &fakeEnroller{}, attemptLimiter(gatewayClock))
	raw, err := os.ReadFile(filepath.Join(dir, "pairing.key"))
	if err != nil {
		t.Fatalf("read secret: %v", err)
	}
	helper := openService(t, dir, helperClock, nil, attemptLimiter(helperClock))

	gatewayCode, _ := gateway.Current()
	helperCode, _ := helper.Current()
	if gatewayCode != helperCode {
		t.Errorf("the gateway derived %q but the pair helper derived %q from the same secret", gatewayCode, helperCode)
	}

	after, err := os.ReadFile(filepath.Join(dir, "pairing.key"))
	if err != nil {
		t.Fatalf("reread secret: %v", err)
	}
	if !bytes.Equal(raw, after) {
		t.Error("the second Open rewrote the secret file")
	}
}

func TestRedeemAcceptsTheCurrentAndPreviousWindows(t *testing.T) {
	tests := []struct {
		name   string
		offset time.Duration
		wantOK bool
	}{
		{"the current window", 0, true},
		{"the previous window, so a code printed as it rotated still works", -testTTL, true},
		{"two windows back, long expired", -2 * testTTL, false},
		{"the next window, which has not begun", testTTL, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gatewayClock := &clock{at: startOfWindow(1_000_000, testTTL)}
			helperClock := &clock{at: startOfWindow(1_000_000, testTTL).Add(tc.offset)}

			enr := &fakeEnroller{}
			gateway := openService(t, dir, gatewayClock, enr, attemptLimiter(gatewayClock))
			// The helper shares the secret file, so this is the code an operator
			// would have been shown at that instant.
			helper := openService(t, dir, helperClock, nil, attemptLimiter(helperClock))
			code, _ := helper.Current()

			_, _, err := gateway.Redeem(context.Background(), "client", code, "phone", "test-agent")
			switch {
			case tc.wantOK && err != nil:
				t.Fatalf("Redeem(%q) = %v, want success", code, err)
			case !tc.wantOK && err == nil:
				t.Fatalf("Redeem(%q) succeeded, want rejection", code)
			}
			want := 0
			if tc.wantOK {
				want = 1
			}
			if got := enr.count(); got != want {
				t.Errorf("Enroll called %d times, want %d", got, want)
			}
		})
	}
}

func TestRedeemNormalisesCaseAndSurroundingWhitespace(t *testing.T) {
	h := newHarness(t)
	code, _ := h.svc.Current()

	// A phone keyboard autocapitalises unpredictably and a code read off a
	// terminal is easy to copy with stray whitespace; neither should cost the
	// operator a lockout.
	variants := []struct {
		name string
		code string
	}{
		{"lower case", strings.ToLower(code)},
		{"surrounded by spaces", "  " + code + "  "},
		{"surrounded by tabs and a newline", "\t" + code + "\n"},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			if _, _, err := h.svc.Redeem(context.Background(), "client-"+v.name, v.code, "phone", ""); err != nil {
				t.Errorf("Redeem(%q) = %v, want success", v.code, err)
			}
		})
	}
	if got := h.enr.count(); got != len(variants) {
		t.Errorf("Enroll called %d times, want %d", got, len(variants))
	}
}

func TestRedeemRejectsMalformedAndWrongCodes(t *testing.T) {
	h := newHarness(t)
	code, _ := h.svc.Current()

	// Replace the last character with a different alphabet symbol: the same
	// length, the same alphabet, one character wrong.
	last := "A"
	if code[len(code)-1:] == last {
		last = "B"
	}

	tests := []struct {
		name string
		code string
	}{
		{"empty", ""},
		{"one character short", code[:len(code)-1]},
		{"one character long", code + "A"},
		{"one character wrong", code[:len(code)-1] + last},
		{"letters the alphabet deliberately omits", "IIIIIIII"},
		{"digits the alphabet deliberately omits", "00000000"},
		{"a space in the middle", code[:4] + " " + code[5:]},
		{"punctuation only", "!!!!!!!!"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// One key per case: the limiter locks a key after three failures,
			// and a lockout would mask the code-validation rejection this test
			// is about.
			_, _, err := h.svc.Redeem(context.Background(), "client:"+tc.name, tc.code, "phone", "")
			if err == nil {
				t.Fatalf("Redeem(%q) succeeded, want rejection", tc.code)
			}
			if got := errx.KindOf(err); got != errx.KindUnauthenticated {
				t.Errorf("kind = %v, want unauthenticated", got)
			}
			if got := errx.CodeOf(err); got != "invalid_pairing_code" {
				t.Errorf("code = %q, want invalid_pairing_code", got)
			}
			if got := h.enr.count(); got != 0 {
				t.Errorf("a rejected code reached the enroller %d times", got)
			}
		})
	}
}

func TestRedeemRejectsACodeFromAnotherSecret(t *testing.T) {
	mine := newHarness(t)
	other := newHarness(t)

	otherCode, _ := other.svc.Current()
	if _, _, err := mine.svc.Redeem(context.Background(), "client", otherCode, "phone", ""); err == nil {
		t.Fatal("a code derived from a different secret was accepted")
	}
	// Positive control: the code is genuine, just not ours.
	if _, _, err := other.svc.Redeem(context.Background(), "client", otherCode, "phone", ""); err != nil {
		t.Fatalf("the owning service rejected its own code: %v", err)
	}
}

func TestRedeemWhileLockedOutIsRateLimitedWithoutEnrolling(t *testing.T) {
	h := newHarness(t)
	correct, _ := h.svc.Current()

	for i := 0; i < 3; i++ {
		if _, _, err := h.svc.Redeem(context.Background(), "client", "!!!!!!!!", "phone", ""); err == nil {
			t.Fatal("a wrong code was accepted")
		}
	}
	if h.lim.LockedFor("client") == 0 {
		t.Fatal("three rejected codes did not lock the client out")
	}

	// Even the correct code must be refused while the lockout is in force;
	// otherwise the lockout is advisory only.
	_, _, err := h.svc.Redeem(context.Background(), "client", correct, "phone", "")
	if err == nil {
		t.Fatal("a locked-out client redeemed the correct code")
	}
	if got := errx.KindOf(err); got != errx.KindRateLimited {
		t.Errorf("kind = %v, want rate limited", got)
	}
	if got := errx.CodeOf(err); got != "pairing_locked" {
		t.Errorf("code = %q, want pairing_locked", got)
	}
	if got := h.enr.count(); got != 0 {
		t.Errorf("a locked-out request reached the enroller %d times", got)
	}
}

func TestASuccessfulRedeemEnrollsOnceAndClearsTheFailureCount(t *testing.T) {
	h := newHarness(t)
	code, _ := h.svc.Current()

	h.lim.Fail("client") // a typo, tolerated
	h.lim.Fail("client") // another, still tolerated (the limit is two)

	device, token, err := h.svc.Redeem(context.Background(), "client", code, "Pixel 9", "Mozilla/5.0")
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if got := h.enr.count(); got != 1 {
		t.Errorf("Enroll called %d times, want exactly 1", got)
	}
	if device.ID != "dev_test" || token != "tok_test" {
		t.Errorf("Redeem returned (%+v, %q), want the enroller's device and token", device, token)
	}
	if name, ua := h.enr.metadata(); name != "Pixel 9" || ua != "Mozilla/5.0" {
		t.Errorf("Enroll saw (%q, %q), want (%q, %q)", name, ua, "Pixel 9", "Mozilla/5.0")
	}

	// Success must wipe the history: two more tolerated failures are allowed,
	// and only the third locks. Without the reset the very next failure would
	// lock, punishing the operator for the typos they just recovered from.
	h.lim.Fail("client")
	if got := h.lim.LockedFor("client"); got != 0 {
		t.Errorf("LockedFor = %v right after a successful pairing: the failure count survived", got)
	}
	h.lim.Fail("client")
	if got := h.lim.LockedFor("client"); got != 0 {
		t.Errorf("LockedFor = %v after two failures past the reset, want 0", got)
	}
	h.lim.Fail("client")
	if got := h.lim.LockedFor("client"); got == 0 {
		t.Error("the client never locks, so the counting above proved nothing")
	}
}

func TestRedeemWithoutAnEnrollerFailsClosed(t *testing.T) {
	c := &clock{at: startOfWindow(1_000_000, testTTL)}
	svc := openService(t, t.TempDir(), c, nil, attemptLimiter(c))
	code, _ := svc.Current()

	_, _, err := svc.Redeem(context.Background(), "client", code, "phone", "")
	if err == nil {
		t.Fatal("a valid code was redeemed by a process that cannot enroll devices")
	}
	if got := errx.KindOf(err); got != errx.KindInternal {
		t.Errorf("kind = %v, want internal", got)
	}
	if got := errx.CodeOf(err); got != "pairing_not_configured" {
		t.Errorf("code = %q, want pairing_not_configured", got)
	}
}

func TestOpenCreatesTheSecretWithOwnerOnlyPermissionsAndReusesIt(t *testing.T) {
	dir := t.TempDir()
	c := &clock{at: startOfWindow(1_000_000, testTTL)}
	first := openService(t, dir, c, &fakeEnroller{}, attemptLimiter(c))
	code, _ := first.Current()

	path := filepath.Join(dir, "pairing.key")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat secret: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("pairing.key mode = %04o, want 0600: every code is derived from this file", perm)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("pairing.key is readable by other users (mode %04o)", perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read secret: %v", err)
	}
	decoded, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("the secret file is not hex: %v", err)
	}
	if len(decoded) != 32 {
		t.Errorf("the secret holds %d bytes, want 32", len(decoded))
	}

	c.set(c.at.Add(time.Minute)) // still inside the same window
	second := openService(t, dir, c, &fakeEnroller{}, attemptLimiter(c))
	again, _ := second.Current()
	if again != code {
		t.Errorf("a second Open changed the code from %q to %q: the secret was replaced", code, again)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reread secret: %v", err)
	}
	if !bytes.Equal(raw, after) {
		t.Error("a second Open rewrote the secret file")
	}
}

// TestAnEnrollerFailureIsPropagatedAndNotChargedToTheClient covers the case the
// enroller itself is broken — a full disk, an unwritable state directory. The
// operator must see that error, and it must not count against their lockout
// budget, because it is not something they did.
func TestAnEnrollerFailureIsPropagatedAndNotChargedToTheClient(t *testing.T) {
	dir := t.TempDir()
	c := &clock{at: startOfWindow(1_000_000, testTTL)}
	lim := attemptLimiter(c)
	enr := &fakeEnroller{err: errors.New("device store is not writable")}
	svc := openService(t, dir, c, enr, lim)
	code, _ := svc.Current()

	_, _, err := svc.Redeem(context.Background(), "client", code, "phone", "")
	if err == nil {
		t.Fatal("a failing enroller produced a successful redemption")
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Errorf("error = %v, want the enroller's cause to be reported", err)
	}
	if got := lim.LockedFor("client"); got != 0 {
		t.Errorf("LockedFor = %v after an enroller failure, want 0: the client was blamed for a broken "+
			"device store", got)
	}

	// The code itself is still good, so a retry once the store recovers works.
	enr.err = nil
	if _, _, err := svc.Redeem(context.Background(), "client", code, "phone", ""); err != nil {
		t.Errorf("Redeem after the enroller recovered: %v", err)
	}
}

// TestAClockBeforeNineteenSeventyStillDerivesACode guards the window clamp: a
// machine with no RTC (or a badly wrong one) must not panic on the conversion of
// a negative window to uint64.
func TestAClockBeforeNineteenSeventyStillDerivesACode(t *testing.T) {
	dir := t.TempDir()
	c := &clock{at: time.Unix(-1_000_000, 0)}
	svc := openService(t, dir, c, nil, attemptLimiter(c))

	code, expires := svc.Current()
	if len(code) != pairing.CodeLength {
		t.Fatalf("code %q has %d characters, want %d", code, len(code), pairing.CodeLength)
	}
	for _, r := range code {
		if !strings.ContainsRune(pairingAlphabet, r) {
			t.Fatalf("code %q contains %q, which is not in the published alphabet", code, r)
		}
	}
	if !expires.After(c.now()) {
		t.Errorf("expiresAt = %v, want a time after the clock (%v)", expires, c.now())
	}
}

func TestOpenWithANilClockUsesTheWallClock(t *testing.T) {
	dir := t.TempDir()
	c := &clock{at: startOfWindow(1_000_000, testTTL)}

	svc, err := pairing.Open(dir, testTTL, nil, attemptLimiter(c), nil)
	if err != nil {
		t.Fatalf("pairing.Open with a nil clock: %v", err)
	}
	code, expires := svc.Current()
	if len(code) != pairing.CodeLength {
		t.Errorf("code %q has %d characters, want %d", code, len(code), pairing.CodeLength)
	}
	if !expires.After(time.Now()) {
		t.Errorf("expiresAt = %v, want a future instant", expires)
	}
}

// TestOpenFailsWhenTheSecretPathCannotBeRead covers a read error that is not
// "missing": a directory left where the secret belongs must be reported, not
// silently replaced by a fresh secret.
func TestOpenFailsWhenTheSecretPathCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "pairing.key"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	c := &clock{at: startOfWindow(1_000_000, testTTL)}
	if _, err := pairing.Open(dir, testTTL, nil, attemptLimiter(c), c.now); err == nil {
		t.Fatal("Open accepted a secret path it cannot read")
	}
}

func TestOpenRefusesToSilentlyReplaceACorruptSecret(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"not hex at all", "this is not a secret\n", true},
		{"too few bytes", hex.EncodeToString([]byte("too short")), true},
		{"one byte too many", hex.EncodeToString(bytes.Repeat([]byte{0x01}, 33)), true},
		{"empty file", "", true},
		{"valid hex with a trailing newline", hex.EncodeToString(bytes.Repeat([]byte{0x7f}, 32)) + "\n", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "pairing.key")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("seed secret: %v", err)
			}

			c := &clock{at: startOfWindow(1_000_000, testTTL)}
			_, err := pairing.Open(dir, testTTL, nil, attemptLimiter(c), c.now)
			if tc.wantErr {
				if err == nil {
					t.Fatal("Open accepted a corrupt secret; regenerating it would silently invalidate every code already printed")
				}
				after, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatalf("reread secret: %v", readErr)
				}
				if string(after) != tc.content {
					t.Error("Open rewrote the corrupt secret instead of refusing to start")
				}
				return
			}
			if err != nil {
				t.Fatalf("Open with a valid secret = %v, want success", err)
			}
		})
	}
}

func TestCodesUseOnlyThePublishedAlphabet(t *testing.T) {
	h := newHarness(t)

	seen := map[rune]bool{}
	for i := 0; i < 5000; i++ {
		h.clock.set(startOfWindow(1_000_000+int64(i), testTTL))
		code, _ := h.svc.Current()

		if len(code) != pairing.CodeLength {
			t.Fatalf("code %q has %d characters, want %d", code, len(code), pairing.CodeLength)
		}
		for _, r := range code {
			if !strings.ContainsRune(pairingAlphabet, r) {
				t.Fatalf("code %q contains %q, which is not in the published alphabet", code, r)
			}
			seen[r] = true
		}
	}

	// If only a handful of symbols ever appeared, the membership check above
	// would be nearly vacuous.
	if len(seen) != len(pairingAlphabet) {
		t.Errorf("only %d of the alphabet's %d symbols appeared across 5000 codes", len(seen), len(pairingAlphabet))
	}
}

// TestCodeCharactersAreUniformlyDistributed is currently expected to fail: see
// the arithmetic in the failure message. It pins the property codeFor's own
// comment claims ("rejection sampling so that every character is uniformly
// distributed").
func TestCodeCharactersAreUniformlyDistributed(t *testing.T) {
	const windows = 20_000

	// A fixed secret makes the derivation deterministic, so this is not a
	// flaky statistical test: the same 20,000 codes are produced every run.
	dir := t.TempDir()
	secret := bytes.Repeat([]byte{0x5a}, 32)
	if err := os.WriteFile(filepath.Join(dir, "pairing.key"), []byte(hex.EncodeToString(secret)), 0o600); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	c := &clock{at: startOfWindow(1_000_000, testTTL)}
	svc := openService(t, dir, c, nil, attemptLimiter(c))

	counts := make([]int, len(pairingAlphabet))
	total := 0
	for i := 0; i < windows; i++ {
		c.set(startOfWindow(1_000_000+int64(i), testTTL))
		code, _ := svc.Current()
		for j := 0; j < len(code); j++ {
			counts[strings.IndexByte(pairingAlphabet, code[j])]++
			total++
		}
	}

	expected := float64(total) / float64(len(pairingAlphabet))
	chiSquare := 0.0
	for _, n := range counts {
		d := float64(n) - expected
		chiSquare += d * d / expected
	}

	// 29 degrees of freedom: a uniform mapping lands near 29 (sd about 7.6).
	const maxChiSquare = 120
	if chiSquare > maxChiSquare {
		// 248 is 8*31: the largest multiple of 31 below 256, and not a multiple
		// of this 30-symbol alphabet. Digest bytes 240..247 therefore map onto
		// symbols 0..7 for a ninth time, making those symbols 9/248 likely and
		// the other 22 symbols 8/248.
		t.Errorf("chi-square = %.1f over %d code characters (threshold %d): the code alphabet is "+
			"not uniform. codeFor rejects bytes >= 248, but 248 = 8*31 while the alphabet has %d "+
			"symbols; the largest multiple of %d below 256 is %d.",
			chiSquare, total, maxChiSquare, len(pairingAlphabet), len(pairingAlphabet), 256/len(pairingAlphabet)*len(pairingAlphabet))
	}
}

func TestAUsedCodeCanStillEnrollASecondDevice(t *testing.T) {
	h := newHarness(t)
	code, _ := h.svc.Current()
	ctx := context.Background()

	if _, _, err := h.svc.Redeem(ctx, "phone-one", code, "first phone", ""); err != nil {
		t.Fatalf("first redeem: %v", err)
	}

	// Redeem keeps no record of use, so a code observed by someone else stays
	// good for the rest of its window even after the operator has paired. The
	// CodeLength comment calls codes "single-use"; this test pins the real
	// behaviour so that enforcing single use later is a deliberate, visible
	// change rather than a silent one.
	if _, _, err := h.svc.Redeem(ctx, "phone-two", code, "second phone", ""); err != nil {
		t.Fatalf("the same code was refused a second time: %v", err)
	}
	if got := h.enr.count(); got != 2 {
		t.Errorf("Enroll called %d times, want 2", got)
	}
}

func TestTTLReportsTheConfiguredWindow(t *testing.T) {
	h := newHarness(t)
	if got := h.svc.TTL(); got != testTTL {
		t.Errorf("TTL = %v, want %v", got, testTTL)
	}
}

// TestRedeemWithoutCollaboratorsReportsRatherThanPanics covers the `dsh-gateway
// pair` helper's configuration.
//
// That command opens the service with neither an enroller nor a limiter,
// because it only derives a code and never redeems one. Reaching Redeem in that
// process would be a programming error, and an error naming the problem is a far
// better outcome than a nil dereference — especially in a CLI, where a panic
// prints a stack trace at an operator who cannot act on it.
func TestRedeemWithoutCollaboratorsReportsRatherThanPanics(t *testing.T) {
	dir := t.TempDir()

	service, err := pairing.Open(dir, time.Minute, nil, nil, time.Now)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	code, _ := service.Current()

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("Redeem panicked instead of returning an error: %v", rec)
		}
	}()

	device, token, err := service.Redeem(context.Background(), "127.0.0.1", code, "phone", "test")
	if err == nil {
		t.Fatal("Redeem succeeded without an enroller, which would mean a device was never stored")
	}
	if device.ID != "" || token != "" {
		t.Errorf("a failed Redeem returned a device (%q) or token (%q)", device.ID, token)
	}
	if kind := errx.KindOf(err); kind != errx.KindInternal {
		t.Errorf("kind = %v, want %v: a misconfigured process is not the caller's fault", kind, errx.KindInternal)
	}
	if !strings.Contains(err.Error(), "cannot redeem") {
		t.Errorf("error does not explain the situation: %v", err)
	}
}
