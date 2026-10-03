//go:build unix

package sessionwatch_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
)

// holdLock takes the session's writer lock the way DSH does, and returns a
// release function. flock is per open file description, so a second open in the
// same process still conflicts — which is what makes this a faithful stand-in
// for the desktop holding a session.
func (f *fixture) holdLock(t *testing.T) func() {
	t.Helper()
	path := filepath.Join(filepath.Dir(f.path), "session.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		t.Fatalf("flock: %v", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}
}

// TestRunningNeedsBothAnOpenTurnAndALiveWriter is the pair of facts the session
// list needs to show a desk session as busy: the log ends mid-turn, and the
// process that wrote it is still there. Either one alone is wrong — a finished
// turn is not busy, and a turn whose process died is not coming back.
func TestRunningNeedsBothAnOpenTurnAndALiveWriter(t *testing.T) {
	f := newFixture(t, "session-locked")
	f.flush(map[string]any{"type": "turn/start", "data": map[string]any{"turn": 1}})

	watcher, _, sub := f.watcher(t, nil)

	// No process holds the session: the turn is a leftover, not a live one.
	watcher.Sweep(context.Background())
	if watcher.Running("session-locked") {
		t.Error("a turn with no live writer was reported as running")
	}
	if got := drain(sub); len(got) != 0 {
		t.Errorf("first sight published %v, want nothing", typesOf(got))
	}

	// The desk opens it and works: now it is running, and a phone that is
	// already connected has to hear about a turn the gateway did not start.
	release := f.holdLock(t)
	defer release()

	watcher.Sweep(context.Background())
	if !watcher.Running("session-locked") {
		t.Error("a turn with a live writer was not reported as running")
	}
	var busy, running bool
	for _, e := range drain(sub) {
		switch e.Type {
		case events.TypeSessionState:
			state, ok := e.Data.(events.SessionBusy)
			if !ok {
				t.Fatalf("session.state payload = %T, want events.SessionBusy", e.Data)
			}
			busy = state.Busy
		case events.TypeTurnState:
			state, ok := e.Data.(events.TurnState)
			if !ok {
				t.Fatalf("turn.state payload = %T, want events.TurnState", e.Data)
			}
			running = state.State == "running"
		default:
			// The bus carries eleven frame types; a test asserting on two of them
			// is not an omission.
		}
	}
	if !busy || !running {
		t.Error("a turn already in flight was not announced when it was first seen")
	}

	// The turn ends.
	f.flush(map[string]any{"type": "turn/end", "data": map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}})
	watcher.Sweep(context.Background())
	if watcher.Running("session-locked") {
		t.Error("a finished turn was still reported as running")
	}
}

// TestTurnBoundariesBecomeState closes the loop for the session list: a desk
// session that is working right now should say so.
func TestTurnBoundariesBecomeState(t *testing.T) {
	f := newFixture(t, "session-seven")

	watcher, _, sub := f.watcher(t, nil)
	// A turn counts as running only while the session has a live writer, so the
	// test holds the lock the way DSH does for as long as it is open.
	release := f.holdLock(t)
	defer release()
	watcher.Sweep(context.Background())

	f.flush(map[string]any{"type": "turn/start", "data": map[string]any{"turn": 1}})
	watcher.Sweep(context.Background())
	got := drain(sub)

	var busy bool
	var running bool
	for _, e := range got {
		switch e.Type {
		case events.TypeSessionState:
			state, ok := e.Data.(events.SessionBusy)
			if !ok {
				t.Fatalf("session.state payload = %T, want events.SessionBusy", e.Data)
			}
			busy = state.Busy
		case events.TypeTurnState:
			if state, ok := e.Data.(events.TurnState); ok && state.State == "running" {
				running = true
			}
		default:
			// The bus carries eleven frame types; a test asserting on two of them
			// is not an omission.
		}
	}
	if !busy || !running {
		t.Errorf("turn/start published %v, want busy and a running turn", typesOf(got))
	}

	f.flush(map[string]any{"type": "turn/end", "data": map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}})
	watcher.Sweep(context.Background())

	var completed bool
	for _, e := range drain(sub) {
		if state, ok := e.Data.(events.TurnState); e.Type == events.TypeTurnState && ok && state.State == "completed" {
			completed = true
		}
	}
	if !completed {
		t.Error("turn/end did not report the turn as finished")
	}
}

// TestSessionLockDoesNotOutliveTheProcess pins the reason the lock is consulted
// at all: a crashed turn leaves `turn/start` behind, and only the lock says the
// difference.
func TestSessionLockDoesNotOutliveTheProcess(t *testing.T) {
	f := newFixture(t, "session-crashed")
	f.flush(map[string]any{"type": "turn/start", "data": map[string]any{"turn": 1}})

	watcher, _, _ := f.watcher(t, nil)
	release := f.holdLock(t)
	watcher.Sweep(context.Background())
	if !watcher.Running("session-crashed") {
		t.Fatal("setup: the session should look running while its lock is held")
	}

	release() // the process died
	watcher.Sweep(context.Background())
	if watcher.Running("session-crashed") {
		t.Error("the writer is gone but the session still claims to be running")
	}
}

// TestSettledTurnIsNotReparsedEverySweep pins the cost of a crashed turn.
//
// A log that ends mid-turn and whose writer is gone never changes again, so a
// reader that re-reads it because the log itself says "running" pays for the
// whole history on every tick, for as long as the gateway lives. Five of those
// in one sessions root held a core at 100% around the clock; the lock, not the
// log, is what says the turn is over.
func TestSettledTurnIsNotReparsedEverySweep(t *testing.T) {
	stale := newFixture(t, "session-a-stale")
	stale.flush(map[string]any{"type": "turn/start", "data": map[string]any{"turn": 1}})
	// Neighbours enough to push the stale projection out of the store's cache,
	// which is the state a real root is in: a cached projection would hide the
	// re-read this test is about.
	for i := 0; i < 17; i++ {
		stale.sibling(t, fmt.Sprintf("session-b-%02d", i))
	}

	watcher, store, _, sub := stale.watcherWithStore(t, nil)
	watcher.Sweep(context.Background())
	drain(sub)
	seeded := store.Decodes()
	if seeded != 18 {
		t.Fatalf("seeding read %d logs, want one per session", seeded)
	}

	// No writer, no change: further sweeps have nothing to do.
	for i := 0; i < 5; i++ {
		watcher.Sweep(context.Background())
	}
	if got := store.Decodes(); got != seeded {
		t.Errorf("idle sweeps re-read %d settled logs, want none", got-seeded)
	}
	if got := drain(sub); len(got) != 0 {
		t.Errorf("idle sweeps published %v, want nothing", typesOf(got))
	}

	// The desk opens it again. That is news, and the lock is how it arrives.
	release := stale.holdLock(t)
	defer release()
	watcher.Sweep(context.Background())
	if !watcher.Running("session-a-stale") {
		t.Error("a session whose writer came back was not reported as running")
	}
}

// TestRunningIsReportedPerSession keeps a busy session from painting its
// neighbours busy: the list asks per id.
func TestRunningIsReportedPerSession(t *testing.T) {
	f := newFixture(t, "session-busy")
	f.flush(map[string]any{"type": "turn/start", "data": map[string]any{"turn": 1}})

	watcher, _, _ := f.watcher(t, nil)
	release := f.holdLock(t)
	defer release()
	watcher.Sweep(context.Background())

	if !watcher.Running("session-busy") {
		t.Error("the busy session was not reported as running")
	}
	if watcher.Running("session-somewhere-else") {
		t.Error("an unrelated session id was reported as running")
	}
}
