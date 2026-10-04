package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenWritesToTheConfiguredFile: a deployment whose supervisor never rotates
// anything gets a file the gateway rotates itself, directory included.
func TestOpenWritesToTheConfiguredFile(t *testing.T) {
	// The directory deliberately does not exist: the installer creates it, but a
	// hand-run binary pointing somewhere new should not need that first.
	path := filepath.Join(t.TempDir(), "logs", "gateway.log")

	logger, closeLog, err := Open(Config{Level: "info", File: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	logger.Info("serving", "listen", "127.0.0.1:8787")
	if err := closeLog(); err != nil {
		t.Fatalf("close: %v", err)
	}

	raw := readFile(t, path)
	if !strings.Contains(raw, "serving") || !strings.Contains(raw, "127.0.0.1:8787") {
		t.Errorf("log file = %q, want the record that was logged", raw)
	}
}

// TestOpenFallsBackToStderrRatherThanFailing pins the trade: a log that cannot be
// written must not stop the gateway from driving the operator's phone.
func TestOpenFallsBackToStderrRatherThanFailing(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	logger, closeLog, err := Open(Config{Level: "info", File: filepath.Join(blocker, "gateway.log")})
	if err == nil {
		t.Fatal("Open accepted a log path under a regular file")
	}
	if logger == nil {
		t.Fatal("Open returned no logger, so a caller has nothing to report the failure with")
	}
	logger.Warn("this goes to stderr")
	if err := closeLog(); err != nil {
		t.Errorf("the stderr fallback still returned a closer that fails: %v", err)
	}
}

// TestOpenWithNoFileIsStderr keeps the hand-run case honest: an unset log file is
// not a file called "", it is the process's own stderr.
func TestOpenWithNoFileIsStderr(t *testing.T) {
	logger, closeLog, err := Open(Config{Level: "info"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if logger == nil {
		t.Fatal("Open returned no logger")
	}
	if err := closeLog(); err != nil {
		t.Errorf("closing a logger that opened nothing failed: %v", err)
	}
}
