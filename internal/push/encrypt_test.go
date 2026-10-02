package push

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// mustDecode reads the base64url values the RFC prints, whitespace and all.
func mustDecode(t *testing.T, value string) []byte {
	t.Helper()
	compact := strings.Join(strings.Fields(value), "")
	raw, err := base64.RawURLEncoding.DecodeString(compact)
	if err != nil {
		t.Fatalf("decode %q: %v", compact, err)
	}
	return raw
}

// TestEncryptMatchesRFC8291AppendixA is the whole justification for hand-rolling
// this: RFC 8291 publishes every input and the exact bytes that must come out, so
// "our crypto is probably fine" is replaced by a byte-for-byte answer.
func TestEncryptMatchesRFC8291AppendixA(t *testing.T) {
	// All values below are copied from RFC 8291 Appendix A.
	plaintext := mustDecode(t, "V2hlbiBJIGdyb3cgdXAsIEkgd2FudCB0byBiZSBhIHdhdGVybWVsb24")
	asPrivate := mustDecode(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw")
	asPublic := mustDecode(t, "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIg"+
		"Dll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	salt := mustDecode(t, "DGv6ra1nlYgDCS1FRnbzlw")

	subscription := Subscription{
		Endpoint: "https://push.example.net/push/JzLQ3raZJfFBR0aqvOMsLrt54w4rJUsV",
		P256DH:   "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
		Auth:     "BTBZMqHH6r4Tts7J_aSIgg",
	}

	body, gotPublic, err := Encrypt(plaintext, subscription, salt, asPrivate)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !bytes.Equal(gotPublic, asPublic) {
		t.Errorf("application server public key = %s, want the RFC's %s",
			EncodeKey(gotPublic), EncodeKey(asPublic))
	}

	// The header the RFC prints, then the ciphertext it prints, concatenated.
	header := mustDecode(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml"+
		"mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	ciphertext := mustDecode(t, "8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ")
	want := append(append([]byte{}, header...), ciphertext...)

	if !bytes.Equal(body, want) {
		t.Errorf("encrypted body does not match the RFC\ngot  %s\nwant %s", EncodeKey(body), EncodeKey(want))
	}
}

// TestSealIsFreshPerMessage: reusing a salt or a key pair across messages would
// be the classic nonce-reuse mistake, so two seals of the same payload must not
// look alike.
func TestSealIsFreshPerMessage(t *testing.T) {
	subscription := Subscription{
		Endpoint: "https://push.example.net/x",
		P256DH:   "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
		Auth:     "BTBZMqHH6r4Tts7J_aSIgg",
	}
	first, err := Seal([]byte(`{"title":"hi"}`), subscription)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, err := Seal([]byte(`{"title":"hi"}`), subscription)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Error("two seals of the same payload produced identical bytes")
	}
}

// TestSealRejectsBadSubscriptions keeps a malformed subscription from becoming a
// mysterious failure at the push service.
func TestSealRejectsBadSubscriptions(t *testing.T) {
	const goodKey = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	tests := []struct {
		name string
		sub  Subscription
	}{
		{"no endpoint", Subscription{P256DH: goodKey, Auth: "BTBZMqHH6r4Tts7J_aSIgg"}},
		{"short key", Subscription{Endpoint: "https://x", P256DH: "AAAA", Auth: "BTBZMqHH6r4Tts7J_aSIgg"}},
		{"short auth", Subscription{Endpoint: "https://x", P256DH: goodKey, Auth: "AAAA"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.sub.Validate(); err == nil {
				t.Error("Validate accepted a subscription that cannot be used")
			}
		})
	}

	// Encryption needs the keys, not the endpoint: where the message goes is the
	// sender's business, and conflating the two would make a bad endpoint look
	// like a crypto failure.
	if _, err := Seal([]byte("hi"), Subscription{P256DH: "AAAA", Auth: "AAAA"}); err == nil {
		t.Error("Seal accepted keys it cannot encrypt to")
	}
	if _, err := Seal([]byte("hi"), tests[0].sub); err != nil {
		t.Errorf("Seal refused keys that are fine: %v", err)
	}
}

// TestEncryptRefusesAnOversizedPayload: a notification is a sentence, and a
// silent failure at the push service is worse than a refusal here.
func TestEncryptRefusesAnOversizedPayload(t *testing.T) {
	subscription := Subscription{
		Endpoint: "https://push.example.net/x",
		P256DH:   "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4",
		Auth:     "BTBZMqHH6r4Tts7J_aSIgg",
	}
	if _, err := Seal(bytes.Repeat([]byte("x"), payloadLimit+1), subscription); err == nil {
		t.Error("an oversized payload was encrypted anyway")
	}
}
