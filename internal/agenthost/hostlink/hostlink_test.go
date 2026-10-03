package hostlink

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// socketPath returns a socket path short enough to bind.
//
// t.TempDir() is not: its name embeds the test's own name, and on macOS a unix
// socket path is limited to 104 bytes, so a descriptively named test under Go's
// temporary directory overflows it. That is the same failure an operator hits
// with a long state directory, which is why the code checks the length and says
// so rather than letting `bind` answer "invalid argument".
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hl")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "h.sock")
	if len(path) > maxPath {
		t.Fatalf("test socket path is %d characters, over the %d limit: %s", len(path), maxPath, path)
	}
	return path
}

// TestListenAndDialRoundTrip covers the ordinary path over a real unix socket.
//
// A real socket rather than an in-memory pipe, because the properties that
// matter here are filesystem ones — permissions, staleness, who may connect —
// and a pipe has none of them.
func TestListenAndDialRoundTrip(t *testing.T) {
	path := socketPath(t)

	listener, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := Dial(ctx, path, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	select {
	case server := <-accepted:
		if server == nil {
			t.Fatal("Accept failed")
		}
		_ = server.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the listener never accepted")
	}
}

// TestSocketIsNotWorldAccessible is the security property of the seam.
//
// This socket reaches an agent that runs commands as the operator. It is
// protected by the state directory's mode and by the socket's own, and both are
// asserted here: a deployment that put the state directory somewhere shared
// would otherwise hand the agent to every account on the machine, silently.
func TestSocketIsNotWorldAccessible(t *testing.T) {
	path := socketPath(t)

	listener, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %04o, want 0600: any local account could drive the agent", perm)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Errorf("socket path is not a socket: %v", info.Mode())
	}
}

// TestListenRefusesWhenAHostIsAlreadyServing is what stops two hosts from
// splitting the work.
//
// A second host would come up holding no sessions while the first kept running
// turns that nothing could reach, and the symptom would be "the phone shows a
// turn that never finishes" on a deployment that looked healthy.
func TestListenRefusesWhenAHostIsAlreadyServing(t *testing.T) {
	path := socketPath(t)

	first, err := Listen(path)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	defer func() { _ = first.Close() }()

	// A listener with nothing accepting still binds, so keep accepting in the
	// background: `Alive` dials, and a dial that nothing answers is exactly the
	// ambiguity this rule has to resolve correctly.
	go func() {
		for {
			conn, err := first.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	if _, err := Listen(path); err == nil {
		t.Fatal("a second host bound the same socket while the first was serving; " +
			"it would hold no sessions and the first would keep turns nothing could reach")
	}
}

// TestListenRemovesAStaleSocket covers the crash: a host that was killed left a
// socket file behind, and the replacement must be able to start.
//
// This is the difference between a service that recovers from a crash and one
// that needs an operator to delete a file — and `bind` reports both as
// "address already in use", so without the liveness check there is no way to
// tell them apart.
func TestListenRemovesAStaleSocket(t *testing.T) {
	path := socketPath(t)

	first, err := Listen(path)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	// Closed without removing the file, which is what a SIGKILL leaves.
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("the platform removed the socket on close, so there is no stale case: %v", err)
	}

	second, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	defer func() { _ = second.Close() }()
}

// TestListenRefusesToReplaceSomethingElse is the safety rail: a misconfigured
// path must not turn into a deleted file.
func TestListenRefusesToReplaceSomethingElse(t *testing.T) {
	path := socketPath(t)
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := Listen(path); err == nil {
		t.Fatal("Listen replaced a regular file; a wrong path must fail, not delete")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the file was removed anyway: %v", err)
	}
}

// TestDialWaitsForAHostThatIsNotUpYet is what makes start-up ordering
// irrelevant: a supervisor may start the gateway before the host, and a gateway
// that refused to come up for that reason is a deployment that only works when
// two jobs happen to start in the right order.
func TestDialWaitsForAHostThatIsNotUpYet(t *testing.T) {
	path := socketPath(t)

	go func() {
		time.Sleep(300 * time.Millisecond)
		listener, err := Listen(path)
		if err != nil {
			return
		}
		defer func() { _ = listener.Close() }()
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	started := time.Now()
	conn, err := Dial(ctx, path, 3*time.Second)
	if err != nil {
		t.Fatalf("Dial did not wait for the host to appear: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
		t.Errorf("Dial returned after %s, before the host could have been up", elapsed)
	}
}
