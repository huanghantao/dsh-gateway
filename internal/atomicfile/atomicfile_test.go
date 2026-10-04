package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSweepTempRemovesOnlyStaleTemporaries is the whole safety argument for the
// sweep, in one test.
//
// Two ways to get it wrong, and both are worse than the litter it cleans: it
// could delete a file someone meant to keep, or it could delete a temp file a
// live writer is still filling — `dsh-gateway pair` writes this same directory
// while the gateway runs.
func TestSweepTempRemovesOnlyStaleTemporaries(t *testing.T) {
	dir := t.TempDir()

	// The shape CreateTemp produces: "." + document + ".tmp-" + random.
	stale := filepath.Join(dir, ".devices.json.tmp-2749183")
	fresh := filepath.Join(dir, ".push.json.tmp-9918237")
	// Neither of these is ours, and neither may go.
	dotfile := filepath.Join(dir, ".gitignore")
	document := filepath.Join(dir, "devices.json")

	for _, path := range []string{stale, fresh, dotfile, document} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", filepath.Base(path), err)
		}
	}
	old := time.Now().Add(-2 * staleTempAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("age the stale temp file: %v", err)
	}

	removed, err := SweepTemp(dir, time.Now())
	if err != nil {
		t.Fatalf("SweepTemp: %v", err)
	}
	if removed != 1 {
		t.Errorf("SweepTemp removed %d file(s), want 1", removed)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a temp file from a write that did not survive is still there: " +
			"nothing else ever looks at it, and it is the size of the document it was writing")
	}
	for _, path := range []string{fresh, dotfile, document} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was swept, and it is not a stale temporary of ours: %v",
				filepath.Base(path), err)
		}
	}
}

// TestSweepTempLeavesAWriteInProgressAlone pins the age rule from the other side:
// a temp file inside the window is what a writer that is still running looks
// like, and the sweep must not be able to race it.
func TestSweepTempLeavesAWriteInProgressAlone(t *testing.T) {
	dir := t.TempDir()
	inProgress := filepath.Join(dir, ".curation.json.tmp-11223344")
	if err := os.WriteFile(inProgress, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// One second short of the window: still nobody else's business.
	justInside := time.Now().Add(-staleTempAge + time.Second)
	if removed, err := SweepTemp(dir, justInside); err != nil || removed != 0 {
		t.Fatalf("SweepTemp inside the window removed %d (err %v), want none", removed, err)
	}
	if _, err := os.Stat(inProgress); err != nil {
		t.Fatalf("the sweep took a file a live writer could still be filling: %v", err)
	}

	// A moment later it is litter by definition.
	if removed, err := SweepTemp(dir, time.Now().Add(staleTempAge+time.Minute)); err != nil || removed != 1 {
		t.Fatalf("SweepTemp past the window removed %d (err %v), want 1", removed, err)
	}
}

// TestWriteFileLeavesNothingBehind is the positive control for the sweep: a write
// that succeeds must not be what the sweep exists for.
func TestWriteFileLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.json")
	if err := WriteFileSecret(path, []byte(`{"devices":[]}`)); err != nil {
		t.Fatalf("WriteFileSecret: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if isTempName(entry.Name()) {
			t.Errorf("a successful write left %s behind", entry.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries after one write, want 1", len(entries))
	}
}
