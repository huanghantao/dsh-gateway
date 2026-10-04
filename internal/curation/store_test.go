package curation_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/huanghantao/dsh-gateway/internal/curation"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// desk writes a DSH workspace store, the way the desktop leaves it.
func desk(t *testing.T, home string, archived, pinned []string) string {
	t.Helper()
	path := filepath.Join(home, "storages", "workspace.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := map[string]any{
		"global": map[string]any{
			"archivedSessionIds": archived,
			"pinnedSessionIds":   pinned,
		},
		"tables": map[string]any{"workspaces": map[string]any{}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func open(t *testing.T, stateDir, dshHome string) *curation.Store {
	t.Helper()
	s, err := curation.Open(curation.Options{StateDir: stateDir, DSHHome: dshHome, Logger: logx.Discard()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// TestTheDeskIsTheFirstOpinion: the phone must not show what the desktop has
// put away, and must not pretend it did the archiving itself.
func TestTheDeskIsTheFirstOpinion(t *testing.T) {
	home := t.TempDir()
	desk(t, home, []string{"session-archived"}, []string{"session-pinned"})
	s := open(t, t.TempDir(), home)

	hidden := s.Decision("session-archived")
	if !hidden.Archived || !hidden.ArchivedOnDesk {
		t.Errorf("desk-archived session = %+v, want archived and attributed to the desk", hidden)
	}
	if !s.Hide("session-archived") {
		t.Error("an archived session would still be listed")
	}
	pinned := s.Decision("session-pinned")
	if !pinned.Pinned || !pinned.PinnedOnDesk {
		t.Errorf("desk-pinned session = %+v, want pinned and attributed to the desk", pinned)
	}
	if s.Hide("session-pinned") {
		t.Error("pinning a session hid it")
	}
	if d := s.Decision("session-untouched"); d.Archived || d.Pinned {
		t.Errorf("an untouched session = %+v, want no opinion", d)
	}
}

// TestPhoneDecisionsSurviveAndUndoTheDesk covers the reason the overlay has four
// lists: the phone has to be able to archive something the desk does not know
// about, and to take back something the desk decided, without editing the desk's
// file.
func TestPhoneDecisionsSurviveAndUndoTheDesk(t *testing.T) {
	home := t.TempDir()
	desk(t, home, []string{"session-desk-archived"}, nil)
	stateDir := t.TempDir()
	s := open(t, stateDir, home)

	changed, err := s.Archive([]string{"session-mine", "session-mine", ""}, true)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if changed != 1 {
		t.Errorf("archiving one id twice reported %d change(s), want 1", changed)
	}
	if !s.Hide("session-mine") {
		t.Error("a session archived on the phone is still listed")
	}

	// A second store over the same directory sees the decision: this is state,
	// not a cache.
	reopened := open(t, stateDir, home)
	if !reopened.Hide("session-mine") {
		t.Error("the decision did not survive a restart")
	}

	// Undoing a desk archive from the phone wins, and is attributed locally.
	if _, err := reopened.Archive([]string{"session-desk-archived"}, false); err != nil {
		t.Fatalf("Archive(false): %v", err)
	}
	if d := reopened.Decision("session-desk-archived"); d.Archived || d.ArchivedOnDesk {
		t.Errorf("after an undo on the phone: %+v, want visible and no longer the desk's doing", d)
	}

	// And re-archiving clears the undo rather than leaving contradictory state.
	if _, err := reopened.Archive([]string{"session-desk-archived"}, true); err != nil {
		t.Fatalf("Archive(true): %v", err)
	}
	if !reopened.Hide("session-desk-archived") {
		t.Error("archiving again after an undo did not stick")
	}
}

// TestPinIsNotArchive keeps the two lists from bleeding into each other.
func TestPinIsNotArchive(t *testing.T) {
	s := open(t, t.TempDir(), "")
	if _, err := s.Pin([]string{"session-one"}, true); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	decision := s.Decision("session-one")
	if !decision.Pinned || decision.Archived {
		t.Errorf("pinned session = %+v, want pinned and not archived", decision)
	}
}

// TestOverlaySurvivesCorruption: a damaged overlay costs the operator their
// curation, which is visible and redoable. Refusing to start is not.
func TestOverlaySurvivesCorruption(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "curation.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := open(t, stateDir, "")
	if s.Hide("session-whatever") {
		t.Error("a corrupt overlay hid a session")
	}
	if _, err := s.Archive([]string{"session-whatever"}, true); err != nil {
		t.Fatalf("Archive after corruption: %v", err)
	}
	if !s.Hide("session-whatever") {
		t.Error("the rewritten overlay did not take effect")
	}
}

// TestDeskIsRereadWhenItChanges pins the cache's other half: the desktop can
// archive while the gateway is running.
func TestDeskIsRereadWhenItChanges(t *testing.T) {
	home := t.TempDir()
	path := desk(t, home, nil, nil)
	s := open(t, t.TempDir(), home)

	if s.Hide("session-later") {
		t.Fatal("setup: nothing is archived yet")
	}
	desk(t, home, []string{"session-later"}, nil)
	// The file must look different to the cache; a same-size rewrite would be
	// indistinguishable, which is why the desk is read on a change of size or
	// mtime rather than on a timer.
	if info, err := os.Stat(path); err == nil && info.Size() == 0 {
		t.Fatal("the fixture wrote nothing")
	}
	if !s.Hide("session-later") {
		t.Error("an archive made at the desk after the gateway started was not picked up")
	}
}

// TestForgetDropsEveryDecisionAboutASession is the only way an id leaves the
// overlay.
//
// Archive and Pin move an id between the two lists of a pair; neither removes
// one. So a session deleted for good — which is what the trash purge does —
// would otherwise be named by this file for as long as the state directory
// exists, and nothing would ever read those ids again.
func TestForgetDropsEveryDecisionAboutASession(t *testing.T) {
	stateDir := t.TempDir()
	s := open(t, stateDir, "")

	// One id in each of the four lists, because they are four different fields
	// and a Forget that missed one would leave the id behind in the file.
	ids := []string{"session-archived", "session-unarchived", "session-pinned", "session-unpinned"}
	if _, err := s.Archive(ids[:1], true); err != nil {
		t.Fatalf("Archive(true): %v", err)
	}
	if _, err := s.Archive(ids[1:2], false); err != nil {
		t.Fatalf("Archive(false): %v", err)
	}
	if _, err := s.Pin(ids[2:3], true); err != nil {
		t.Fatalf("Pin(true): %v", err)
	}
	if _, err := s.Pin(ids[3:], false); err != nil {
		t.Fatalf("Pin(false): %v", err)
	}

	removed, err := s.Forget(ids)
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if removed != len(ids) {
		t.Errorf("Forget reported %d removal(s), want %d", removed, len(ids))
	}
	for _, id := range ids {
		if d := s.Decision(id); d.Archived || d.Pinned {
			t.Errorf("%s still carries a decision after being forgotten: %+v", id, d)
		}
	}

	// State, not a cache: the file itself must no longer name them.
	raw, err := os.ReadFile(filepath.Join(stateDir, "curation.json"))
	if err != nil {
		t.Fatalf("read overlay: %v", err)
	}
	for _, id := range ids {
		if bytes.Contains(raw, []byte(id)) {
			t.Errorf("the overlay still names %s after it was forgotten", id)
		}
	}

	// Forgetting something that was never recorded is a no-op, not an error and
	// not a rewrite.
	again, err := open(t, stateDir, "").Forget([]string{"session-never-seen"})
	if err != nil {
		t.Fatalf("Forget(unknown): %v", err)
	}
	if again != 0 {
		t.Errorf("Forget(unknown) reported %d removal(s), want 0", again)
	}
}
