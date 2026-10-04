package logx

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	return string(raw)
}

// TestTheLogRotatesAtItsBoundAndKeepsOneGeneration is the promise the rotating
// writer makes: two files at most, and no record lost across the seam.
func TestTheLogRotatesAtItsBoundAndKeepsOneGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.log")
	r, err := OpenRotatingFile(path, 64)
	if err != nil {
		t.Fatalf("OpenRotatingFile: %v", err)
	}
	defer func() { _ = r.Close() }()

	if _, err := r.Write([]byte(strings.Repeat("a", 40) + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatal("the log rotated before it reached its bound")
	}

	// The second record does not fit, so it goes to a fresh file and the first
	// record stays readable in the previous one.
	if _, err := r.Write([]byte(strings.Repeat("b", 40) + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readFile(t, path+".1"); !strings.HasPrefix(got, "aaaa") {
		t.Errorf("previous generation = %q, want the first record", got)
	}
	if got := readFile(t, path); !strings.HasPrefix(got, "bbbb") {
		t.Errorf("current log = %q, want the second record", got)
	}

	// The bound is two files, not a growing pile: a third rotation overwrites the
	// older generation rather than adding one.
	for i := 0; i < 3; i++ {
		if _, err := r.Write([]byte(strings.Repeat("c", 40) + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "gateway.log") {
			t.Errorf("unexpected file %q: rotation invented a file nothing will read", entry.Name())
		}
	}
	if len(entries) != 2 {
		t.Errorf("the log directory holds %d files, want 2 (the log and one generation)", len(entries))
	}
}

// TestTheBoundSurvivesARestart: the size is read from the file rather than
// counted from zero, so a process that restarts does not let the log grow to
// twice the bound before it notices.
func TestTheBoundSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.log")
	first, err := OpenRotatingFile(path, 64)
	if err != nil {
		t.Fatalf("OpenRotatingFile: %v", err)
	}
	if _, err := first.Write([]byte(strings.Repeat("a", 60) + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := OpenRotatingFile(path, 64)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = second.Close() }()
	if _, err := second.Write([]byte("this does not fit\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("a reopened log wrote past its bound instead of rotating: %v", err)
	}
}

// TestConcurrentWritersDoNotInterleaveRotations is why Write holds a lock: slog
// handlers are called from every goroutine in the process, and two of them
// rotating at once is how a log ends up with a record in the wrong generation —
// or no file at all.
func TestConcurrentWritersDoNotInterleaveRotations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.log")
	r, err := OpenRotatingFile(path, 128)
	if err != nil {
		t.Fatalf("OpenRotatingFile: %v", err)
	}
	defer func() { _ = r.Close() }()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			line := []byte(strings.Repeat("x", 63) + "\n")
			for j := 0; j < 50; j++ {
				if _, err := r.Write(line); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Every record is a whole line, in one file or the other: rotation happens
	// between writes, never inside one.
	for _, name := range []string{path, path + ".1"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Base(name), err)
		}
		if len(raw) == 0 {
			continue
		}
		if raw[len(raw)-1] != '\n' {
			t.Errorf("%s ends mid-record: a write was split by a rotation", filepath.Base(name))
		}
		for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
			if len(line) != 63 {
				t.Errorf("%s has a %d-byte line, want 63: two writers interleaved", filepath.Base(name), len(line))
			}
		}
	}
}
