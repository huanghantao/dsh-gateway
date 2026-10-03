// Package hostlink owns the socket the gateway and the agent host meet on.
//
// It is small on purpose, and it exists because the alternative — each side
// dialling and listening with its own copy of the rules — is how a deployment
// ends up with a socket one process creates world-writable and the other
// expects to be private. The rules are worth being explicit about, because this
// is the one place in the system where a local process could reach an agent that
// runs commands as the operator:
//
//   - A unix socket, never TCP. There is no port to scan, no interface to bind
//     wrongly, and the filesystem's own permissions are the access control.
//   - The socket lives inside the state directory, which the installer creates
//     with mode 0700, so only the owning user can traverse to it at all.
//   - The listener is removed before it is created, because a stale socket file
//     from a crashed host would otherwise make `bind` fail with "address already
//     in use" for a host that is not running.
//   - A live listener is detected by *connecting*, not by the file existing, for
//     the same reason.
package hostlink

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// maxPath is the length a unix socket path may have.
//
// macOS allows 104 bytes including the terminator and Linux 108, so the smaller
// of the two is the portable bound. It is checked explicitly because exceeding
// it fails with "bind: invalid argument", which names neither the limit nor the
// path length — an operator who set a long state directory would have nothing
// to go on.
const maxPath = 103

// Listen creates the socket and returns a listener on it.
//
// It refuses to start when something is already answering, so a second host
// cannot silently take over from a first: the second would have no sessions and
// the first would keep running turns nobody could reach.
func Listen(path string) (net.Listener, error) {
	if len(path) > maxPath {
		return nil, fmt.Errorf(
			"hostlink: the socket path is %d characters and the limit is %d, so it cannot be created\n"+
				"       path: %s\n"+
				"       shorten stateDir in the config, or pass -socket with a shorter path",
			len(path), maxPath, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("hostlink: create %s: %w", filepath.Dir(path), err)
	}
	if err := removeStale(path); err != nil {
		return nil, err
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("hostlink: listen on %s: %w", path, err)
	}
	// The directory is already private, and the socket is tightened too: a
	// deployment that put the state directory somewhere shared would otherwise
	// expose the agent to every account on the machine.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("hostlink: restrict %s: %w", path, err)
	}
	return listener, nil
}

// Dial connects to a host, waiting for one if waitFor is positive.
//
// The wait is what makes start-up ordering irrelevant: a supervisor may start
// the gateway before the host, and a gateway that refused to come up for that
// reason would be a deployment that only works when two launchd jobs happen to
// start in the right order.
func Dial(ctx context.Context, path string, waitFor time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(waitFor)
	for {
		conn, err := net.DialTimeout("unix", path, 2*time.Second)
		if err == nil {
			return conn, nil
		}
		if waitFor <= 0 || time.Now().After(deadline) {
			return nil, fmt.Errorf("hostlink: connect to %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Alive reports whether something is listening and accepting on path.
func Alive(ctx context.Context, path string) bool {
	conn, err := Dial(ctx, path, 0)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// removeStale deletes a socket file that nothing is listening on.
//
// It refuses when something *is* listening, which is the case that matters: a
// host that is already running must not be displaced by a second one, because
// the second would come up with no sessions while the first kept running turns
// that nothing could reach.
func removeStale(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("hostlink: stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("hostlink: %s exists and is not a socket; refusing to replace it", path)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if Alive(ctx, path) {
		return fmt.Errorf("hostlink: %s is already served by a running agent host", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("hostlink: remove stale socket %s: %w", path, err)
	}
	return nil
}

// Remove deletes the socket on shutdown, so a restart does not have to.
func Remove(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Nothing useful to do: the next start removes a stale socket anyway.
		_ = err
	}
}
