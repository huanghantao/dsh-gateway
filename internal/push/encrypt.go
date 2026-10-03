// Package push sends Web Push notifications to the phones this gateway serves.
//
// Why it exists: the agent's most expensive moments are the ones where it is
// waiting for a human. An approval sits unanswered until the timeout rejects it,
// and a long turn finishes while nobody is looking — on a phone that is exactly
// the thing a notification is for, and without one the reader has to keep
// opening the app to find out whether anything happened.
//
// Everything here is the standard library plus RFC 8291 and RFC 8292:
//
//   - the payload is encrypted to the subscription's key with ECDH + HKDF +
//     AES-128-GCM (`aes128gcm`), so a push service relays bytes it cannot read;
//   - the request is authorised with a VAPID JWT, so the service knows which
//     application is sending and can rate-limit or revoke it.
//
// The encryption is tested against the worked example in RFC 8291 Appendix A,
// byte for byte. Hand-rolling crypto is only defensible when a published vector
// says it is right, and this one does.
package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
)

// Subscription is one browser's push endpoint and the keys it published.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	// P256DH is the user agent's public key: an uncompressed P-256 point,
	// base64url without padding.
	P256DH string `json:"p256dh"`
	// Auth is the 16-byte shared secret, base64url without padding.
	Auth string `json:"auth"`
	// DeviceID ties the subscription to a paired device, so revoking a device
	// can take its notifications with it.
	DeviceID string `json:"deviceId,omitempty"`
	// CreatedAt is when the subscription was recorded.
	CreatedAt string `json:"createdAt,omitempty"`
}

// Validate checks that a subscription can actually be used.
func (s Subscription) Validate() error {
	if s.Endpoint == "" {
		return errors.New("push: a subscription needs an endpoint")
	}
	if _, err := decodeKey(s.P256DH, 65); err != nil {
		return fmt.Errorf("push: p256dh: %w", err)
	}
	if _, err := decodeKey(s.Auth, 16); err != nil {
		return fmt.Errorf("push: auth: %w", err)
	}
	return nil
}

// Message is what a notification says.
//
// Title and Body are the sentence a lock screen renders. The fields below them
// are the same facts in structured form, and they exist because the sentence
// alone made every notification look alike: a reader could see that *something*
// had finished, never whether it was the agent or the gateway reporting, and
// never what the work amounted to. The client is not required to read them — a
// notification that only renders Title and Body is still correct — but a client
// that does can group, filter and label what arrives.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// URL is where a tap should land, relative to the app's mount point.
	URL string `json:"url,omitempty"`
	// Tag collapses repeats: a second approval replaces the first notification
	// for the same session rather than stacking behind it.
	Tag string `json:"tag,omitempty"`
	// SessionID lets the client deep-link without parsing the URL.
	SessionID string `json:"sessionId,omitempty"`
	// Actor is who the notification is about: the session's own agent, or the
	// gateway itself.
	Actor *Actor `json:"actor,omitempty"`
	// Summary is what the work amounted to, when there is something to count.
	Summary string `json:"summary,omitempty"`
	// Outcome is the settled word — completed, failed, cancelled, expired —
	// carried separately so a client can colour a row without parsing prose.
	Outcome string `json:"outcome,omitempty"`
}

// maxRecordSize is the plaintext record size advertised in the header. It is the
// largest payload a push service must accept; 4096 is the usual offer.
const maxRecordSize = 4096

// payloadLimit bounds what we will encrypt. A notification is a sentence and a
// title; anything larger is a bug, and refusing it here keeps a malformed
// message from becoming a mysterious 413 from the push service.
const payloadLimit = 3000

// Encrypt seals a payload for one subscription, returning the request body.
//
// The parameters are explicit rather than generated inside so that the RFC's
// test vector can drive them; production callers use Seal, which generates a
// fresh salt and key pair per message.
func Encrypt(plaintext []byte, subscription Subscription, salt, asPrivateKey []byte) (body []byte, asPublic []byte, err error) {
	if len(plaintext) > payloadLimit {
		return nil, nil, fmt.Errorf("push: payload is %d bytes, the limit is %d", len(plaintext), payloadLimit)
	}
	uaPublic, err := decodeKey(subscription.P256DH, 65)
	if err != nil {
		return nil, nil, fmt.Errorf("push: p256dh: %w", err)
	}
	authSecret, err := decodeKey(subscription.Auth, 16)
	if err != nil {
		return nil, nil, fmt.Errorf("push: auth: %w", err)
	}
	if len(salt) != 16 {
		return nil, nil, fmt.Errorf("push: salt must be 16 bytes, got %d", len(salt))
	}

	curve := ecdh.P256()
	asPrivate, err := curve.NewPrivateKey(asPrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("push: application server key: %w", err)
	}
	uaKey, err := curve.NewPublicKey(uaPublic)
	if err != nil {
		return nil, nil, fmt.Errorf("push: subscription key: %w", err)
	}
	shared, err := asPrivate.ECDH(uaKey)
	if err != nil {
		return nil, nil, fmt.Errorf("push: ecdh: %w", err)
	}
	asPublic = asPrivate.PublicKey().Bytes()

	// RFC 8291 §3.3: the input keying material binds the two public keys to the
	// shared secret, so a message cannot be replayed at another subscription.
	authInfo := append([]byte("WebPush: info\x00"), uaPublic...)
	authInfo = append(authInfo, asPublic...)
	ikm, err := hkdf.Key(sha256.New, shared, authSecret, string(authInfo), 32)
	if err != nil {
		return nil, nil, fmt.Errorf("push: ikm: %w", err)
	}

	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, fmt.Errorf("push: prk: %w", err)
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, nil, fmt.Errorf("push: cek: %w", err)
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, nil, fmt.Errorf("push: nonce: %w", err)
	}

	// The last record is marked with 0x02, which is what tells the browser the
	// plaintext ends here rather than continuing into another record.
	record := append(append([]byte{}, plaintext...), 0x02)

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, nil, fmt.Errorf("push: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("push: gcm: %w", err)
	}
	sealed := aead.Seal(nil, nonce, record, nil)

	// The record header carries the key length in a single byte. A P-256
	// uncompressed point is always 65, so this cannot trip today — which is
	// precisely why it is checked: a future key type with a longer point would
	// silently truncate the length and produce a header no push service could
	// parse, and the failure would look like a rejected signature.
	if len(asPublic) > 255 {
		return nil, nil, fmt.Errorf("push: server public key is %d bytes, want at most 255", len(asPublic))
	}

	header := make([]byte, 0, 16+4+1+len(asPublic))
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, maxRecordSize)
	header = append(header, byte(len(asPublic))) //nolint:gosec // length checked immediately above; gosec cannot see the guard
	header = append(header, asPublic...)
	return append(header, sealed...), asPublic, nil
}

// Seal encrypts for a subscription with a fresh salt and key pair.
func Seal(plaintext []byte, subscription Subscription) (body []byte, err error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("push: salt: %w", err)
	}
	asPrivate, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("push: key: %w", err)
	}
	body, _, err = Encrypt(plaintext, subscription, salt, asPrivate.Bytes())
	return body, err
}

// decodeKey reads a base64url key, with or without padding.
func decodeKey(value string, want int) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		// Some user agents pad; accept both rather than refusing a valid key.
		if padded, perr := base64.URLEncoding.DecodeString(value); perr == nil {
			raw = padded
		} else {
			return nil, err
		}
	}
	if len(raw) != want {
		return nil, fmt.Errorf("expected %d bytes, got %d", want, len(raw))
	}
	return raw, nil
}

// EncodeKey renders a key the way a browser publishes and expects it.
func EncodeKey(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }
