package push_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/huanghantao/dsh-gateway/internal/push"
)

// TestVAPIDDerivesThePublishedPublicKey pins the derivation against a vector
// somebody else published.
//
// The public key is what every phone's subscription was created against, so a
// change in how it is derived or encoded breaks every existing subscription
// silently — the push service simply starts refusing the signature. RFC 8291
// Appendix A publishes a P-256 private scalar and the uncompressed point it must
// produce, which turns "the encoding looks right" into a byte-for-byte answer.
//
// This matters more than usual here because the derivation was moved off
// elliptic.ScalarBaseMult and elliptic.Marshal, both deprecated. The bytes are
// meant to be identical; this is the test that says so.
func TestVAPIDDerivesThePublishedPublicKey(t *testing.T) {
	// Both values are copied from RFC 8291 Appendix A.
	const (
		scalar     = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
		wantPublic = "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIg" +
			"Dll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"
	)

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "vapid.key")
	if err := os.WriteFile(keyPath, []byte(scalar), 0o600); err != nil {
		t.Fatalf("write the key: %v", err)
	}

	vapid, err := push.OpenVAPID(dir, "mailto:ops@example.test")
	if err != nil {
		t.Fatalf("OpenVAPID: %v", err)
	}
	if got := vapid.PublicKey(); got != wantPublic {
		t.Errorf("public key = %s\nwant the RFC's %s", got, wantPublic)
	}

	// The encoding itself, because a subscription sends these bytes to
	// `pushManager.subscribe` and Chrome rejects anything that is not an
	// uncompressed point.
	raw, err := base64.RawURLEncoding.DecodeString(vapid.PublicKey())
	if err != nil {
		t.Fatalf("the public key is not base64url: %v", err)
	}
	if len(raw) != 65 || raw[0] != 4 {
		t.Errorf("public key is %d bytes starting with 0x%02x, want 65 starting with 0x04",
			len(raw), raw[0])
	}
}

// TestVAPIDRoundTripsAndKeepsItsFormat covers the other half: a key this gateway
// generates must survive being written and read back unchanged, and the file
// must stay the single 32-byte scalar it has always been.
//
// The format is load-bearing for upgrades. Losing or re-encoding the key costs
// every paired phone a re-subscribe, so a tidier on-disk representation is not
// worth the migration — and this test is what stops one being introduced by
// accident.
func TestVAPIDRoundTripsAndKeepsItsFormat(t *testing.T) {
	dir := t.TempDir()

	first, err := push.OpenVAPID(dir, "mailto:ops@example.test")
	if err != nil {
		t.Fatalf("OpenVAPID (create): %v", err)
	}

	stored, err := os.ReadFile(filepath.Join(dir, "vapid.key"))
	if err != nil {
		t.Fatalf("read the key file: %v", err)
	}
	scalar, err := base64.RawURLEncoding.DecodeString(string(stored))
	if err != nil {
		t.Fatalf("the stored key is not base64url: %v", err)
	}
	if len(scalar) != 32 {
		t.Errorf("the stored key is %d bytes, want a bare 32-byte scalar", len(scalar))
	}

	info, err := os.Stat(filepath.Join(dir, "vapid.key"))
	if err != nil {
		t.Fatalf("stat the key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %04o, want 0600: it signs notifications to every paired phone", perm)
	}

	second, err := push.OpenVAPID(dir, "mailto:ops@example.test")
	if err != nil {
		t.Fatalf("OpenVAPID (reload): %v", err)
	}
	if first.PublicKey() != second.PublicKey() {
		t.Errorf("reloading the key changed the public key:\n  %s\n  %s",
			first.PublicKey(), second.PublicKey())
	}
}

// TestVAPIDRejectsAKeyThatIsNotAScalar: a corrupt or truncated file must be an
// error rather than a key that signs nothing anyone accepts.
func TestVAPIDRejectsAKeyThatIsNotAScalar(t *testing.T) {
	for name, contents := range map[string]string{
		"empty":         "",
		"not base64url": "!!! not base64 !!!",
		"too short":     base64.RawURLEncoding.EncodeToString([]byte("short")),
		"all zero":      base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "vapid.key"), []byte(contents), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := push.OpenVAPID(dir, "mailto:ops@example.test"); err == nil {
				t.Error("a malformed vapid.key was accepted")
			}
		})
	}
}
