// Package idgen produces the opaque identifiers the gateway uses on the wire.
//
// Identifiers are 128 bits of crypto/rand rendered as lowercase hex, optionally
// carrying a short type prefix so that logs and audit records are readable.
package idgen

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// New returns a prefixed 128-bit random identifier, for example "req_9f2c…".
// An empty prefix yields a bare hex string.
func New(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read is documented never to fail on supported platforms.
		// If the kernel CSPRNG is genuinely unavailable there is no safe way to
		// continue issuing identifiers, so failing loudly beats emitting
		// predictable ones.
		panic(fmt.Sprintf("idgen: crypto/rand unavailable: %v", err))
	}
	if prefix == "" {
		return hex.EncodeToString(b[:])
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// Bytes returns n cryptographically random bytes. It panics for n < 0 or on
// CSPRNG failure, for the same reason as New.
func Bytes(n int) []byte {
	if n < 0 {
		panic("idgen: negative length")
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("idgen: crypto/rand unavailable: %v", err))
	}
	return b
}

// Token returns n random bytes encoded as lowercase hex. Hex keeps every secret
// URL-safe, case-insensitive-safe, and free of padding, which matters for
// pairing codes a human may have to read aloud or retype.
func Token(n int) string {
	return hex.EncodeToString(Bytes(n))
}
