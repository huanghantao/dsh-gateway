package sessionlog

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/errx"
)

func newTrash(t *testing.T, root string) (*Store, *Trash) {
	t.Helper()
	store := newTestStore(t, root)
	trash, err := OpenTrash(store, filepath.Join(t.TempDir(), "trash"))
	if err != nil {
		t.Fatalf("OpenTrash: %v", err)
	}
	return store, trash
}

// TestTrashTakesASessionOutOfTheStore is the product promise: the session stops
// being listed, and it is still there.
func TestTrashTakesASessionOutOfTheStore(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	store, trash := newTrash(t, root)

	entry, err := trash.Move(testSession)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if entry.SessionID != testSession || entry.DeletedAt.IsZero() {
		t.Errorf("entry = %+v, want the session and when it went", entry)
	}

	// Gone from the store...
	if ids := store.Logs(); len(ids) != 0 {
		t.Errorf("the store still lists %d session(s) after a delete", len(ids))
	}
	if _, err := store.Meta(context.Background(), testSession); err == nil {
		t.Error("a trashed session still answers with metadata")
	}
	// ...and present in the trash, with its log intact.
	if entries := trash.List(); len(entries) != 1 || entries[0].SessionID != testSession {
		t.Fatalf("trash holds %+v, want the session", entries)
	}
	if _, err := os.Stat(filepath.Join(entry.Path, logFileName)); err != nil {
		t.Errorf("the log did not survive the move: %v", err)
	}
}

// TestTrashRestoresByteForByte: "deleted" has to mean "put away", or nobody will
// use the button that tidies their list.
func TestTrashRestoresByteForByte(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	store, trash := newTrash(t, root)

	before, err := os.ReadFile(filepath.Join(root, "--ws--", testSession, logFileName))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if _, err := trash.Move(testSession); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := trash.Restore(testSession); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	after, err := os.ReadFile(filepath.Join(root, "--ws--", testSession, logFileName))
	if err != nil {
		t.Fatalf("read restored log: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the restored log differs from the one that was deleted")
	}
	if entries := trash.List(); len(entries) != 0 {
		t.Errorf("the trash still holds %+v after a restore", entries)
	}
	if ids := store.Logs(); len(ids) != 1 {
		t.Errorf("the store lists %d session(s) after a restore, want 1", len(ids))
	}
}

// TestTrashRefusesALiveSession: moving a directory out from under a running agent
// is how a session ends up half-written in two places.
func TestTrashRefusesALiveSession(t *testing.T) {
	if !supportsFlock() {
		t.Skip("flock is not available on this platform")
	}
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	_, trash := newTrash(t, root)

	lock, err := os.OpenFile(filepath.Join(root, "--ws--", testSession, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	_, err = trash.Move(testSession)
	if err == nil {
		t.Fatal("a session being written to was deleted anyway")
	}
	if code := errx.CodeOf(err); code != "session_running" {
		t.Errorf("error code = %q, want session_running so the app can explain it", code)
	}
	if ids := newTestStorePath(t, root); len(ids) != 1 {
		t.Error("the refused delete still moved the session")
	}
}

// newTestStorePath lists session ids from a fresh store, to check that a failed
// operation left the disk alone.
func newTestStorePath(t *testing.T, root string) []string {
	t.Helper()
	store := newTestStore(t, root)
	logs := store.Logs()
	ids := make([]string, 0, len(logs))
	for _, log := range logs {
		ids = append(ids, log.ID)
	}
	return ids
}

// TestTrashPurgesOnlyWhatIsOld: the grace period is the whole reason deletion is
// safe to offer from a phone.
func TestTrashPurgesOnlyWhatIsOld(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	_, trash := newTrash(t, root)

	entry, err := trash.Move(testSession)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}

	// A day later nothing is purged...
	if purged, err := trash.Purge(30*24*time.Hour, entry.DeletedAt.Add(24*time.Hour)); err != nil || purged != 0 {
		t.Fatalf("purge after a day removed %d entry(ies) (err %v), want none", purged, err)
	}
	if len(trash.List()) != 1 {
		t.Fatal("the entry vanished before its time")
	}
	// ...and after the grace period it is gone for good.
	if purged, err := trash.Purge(30*24*time.Hour, entry.DeletedAt.Add(31*24*time.Hour)); err != nil || purged != 1 {
		t.Fatalf("purge after the grace period removed %d (err %v), want 1", purged, err)
	}
	if len(trash.List()) != 0 {
		t.Error("a purged entry is still listed")
	}
	if _, err := os.Stat(entry.Path); !os.IsNotExist(err) {
		t.Error("a purged entry is still on disk")
	}
}

// TestTrashRefusesToRestoreOverALiveSession: the desktop may have resumed it in
// the meantime, and an older copy must not win.
func TestTrashRefusesToRestoreOverALiveSession(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	_, trash := newTrash(t, root)

	if _, err := trash.Move(testSession); err != nil {
		t.Fatalf("Move: %v", err)
	}
	// The same session reappears in the store — resumed on the desktop.
	writeLog(t, root, "--ws--", testSession, sampleEvents())

	if _, err := trash.Restore(testSession); err == nil {
		t.Fatal("a restore overwrote a session that exists again")
	} else if code := errx.CodeOf(err); code != "session_exists" {
		t.Errorf("error code = %q, want session_exists", code)
	}
}
