//go:build unix

package sessionlog

import (
	"os"
	"syscall"
)

// supportsFlock reports whether this platform can answer the question at all.
// Tests use it to skip rather than to assert something the platform cannot do.
func supportsFlock() bool { return true }

// lockHeld reports whether another process holds an exclusive flock on a file.
//
// The lock is taken with LOCK_NB and released immediately: the question is
// "would acquiring it block", and the answer is the only thing wanted. A file
// that cannot even be opened is reported as not held, because a lock nobody can
// look at is not evidence that someone is writing.
func lockHeld(path string) bool {
	file, err := os.Open(path) //nolint:gosec // a path derived from a validated session id
	if err != nil {
		return false
	}
	// Nothing is written, so a Close error carries no information.
	defer func() { _ = file.Close() }()

	fd := int(file.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// EWOULDBLOCK is the answer we are looking for; anything else (a
		// filesystem without flock, say) is reported as not held so that a
		// missing answer cannot invent a running turn.
		return err == syscall.EWOULDBLOCK
	}
	_ = syscall.Flock(fd, syscall.LOCK_UN)
	return false
}
