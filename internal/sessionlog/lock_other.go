//go:build !unix

package sessionlog

// lockHeld assumes the lock is held where flock is not available.
//
// The alternative — reporting "not held" — would silently stop a running turn
// from ever being shown on the phone, which is worse than the occasional stale
// turn in a deployment this build does not target.
func lockHeld(string) bool { return true }

// supportsFlock reports whether this platform can answer the question at all.
func supportsFlock() bool { return false }
