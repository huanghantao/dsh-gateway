// Package sessionwatch follows session logs that another DSH process writes.
//
// Why this exists: the gateway drives its own `dsh --profile acp` child, and
// everything that child does arrives over ACP. A session started at the desk —
// the desktop GUI, `dsh headless`, a terminal — is a different process, and the
// gateway never hears about it. The phone could read such a session's history,
// but only by asking again: new messages appeared when the reader pulled to
// refresh, and never before.
//
// The log is the only channel those sessions publish to, and DSH appends to it
// as events commit, so following it is what makes a desk session live on the
// phone. The unit of following is the *projection* the transcript already
// builds (sessionlog.Item), not the raw log line:
//
//   - One diffable value per conversation row, with the same ids the history
//     endpoint hands out, so a row that arrives live and the same row in a
//     refetched history collapse instead of doubling on the phone.
//   - Tool calls and their results are already folded into one item, so a
//     follower sees "started" and "finished" as two states of one row rather
//     than two events it has to correlate itself.
//   - A partially flushed zstd frame is simply an unreadable file this tick; the
//     next tick reads it whole. Nothing parses half an event.
//
// Duplication is the other half of the problem. When the gateway itself holds a
// session, its activity already arrives over ACP, and publishing the same
// message twice would double every row. The caller therefore supplies `Owned`,
// and a session it claims is advanced in silence: the follower keeps its
// baseline current so that a later handover does not replay a whole turn, but
// publishes nothing.
package sessionwatch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/config"
	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/sessionlog"
)

// DefaultInterval is how often the logs are re-read. A turn appends a handful of
// events per second at most, so this is the resolution of "live" on the phone;
// one stat per session per tick is the cost of asking.
const DefaultInterval = time.Second

// Options configures a Watcher.
type Options struct {
	// Store reads and folds the logs. It is the same reader the transcript
	// endpoint uses, including its cache.
	Store *sessionlog.Store
	// Bus receives the events. It must never block the caller; the bus is
	// non-blocking by contract.
	Bus *events.Bus
	// Owned reports whether the gateway itself is driving a session, in which
	// case its activity already arrives over ACP and must not be repeated here.
	// Nil means "no session is owned".
	Owned func(sessionID string) bool
	// Limits is the deployment's byte budget for a tool frame. The watcher
	// publishes tool lifecycle for sessions the desktop is driving, and a phone
	// cannot tell those frames from the bridge's — so they are bounded by the
	// same numbers, in the same place.
	Limits config.Limits
	// Interval between sweeps. Zero means DefaultInterval.
	Interval time.Duration
	Logger   *logx.Logger
	// Now is injectable for tests.
	Now func() time.Time
}

// Watcher follows every session log under a DSH sessions root.
type Watcher struct {
	opts Options
	// seen is the last projection per session id. The first read of a session
	// only seeds it: a gateway that has just started must not replay every
	// session's history onto the phone as if it were happening now.
	seen map[string]*baseline

	// mu guards running, which HTTP handlers read while the sweep writes it.
	mu sync.Mutex
	// running is the last read's answer to "is a turn in flight here", for
	// sessions that are being written by a process that still holds them.
	running map[string]bool
}

// Running reports whether a session's log ends mid-turn and its owner is still
// alive — that is, whether a turn is being written right now.
//
// The session list asks, because the log is the only place a session the
// gateway does not drive says so: without this, opening the app shows a desk
// session that is busy as idle until an event happens to say otherwise.
func (w *Watcher) Running(sessionID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running[sessionID]
}

// maxBaselines bounds how many projections are remembered. A projection is not
// small, and an operator may have hundreds of sessions; the oldest are dropped
// rather than held forever. Dropping one is safe: its fingerprint stays, so an
// unchanged log still costs one stat, and the next read of that session seeds a
// fresh baseline and publishes nothing spurious.
//
// The bound is on projections, not on remembered logs. Forgetting a log
// outright means decoding it again on the next sweep, so a root with more logs
// than this number would decode all of them once a second, forever — which is
// exactly what a sessions root of a few hundred does.
const maxBaselines = 64

// baseline is what the last sweep saw for one session.
//
// It has two halves with different lifetimes. The fingerprint — the size, the
// modification time, the turn state — is a few hundred bytes and is kept for
// every log the sweep has seen, because it is what makes an unchanged log cost
// one stat instead of a decode. The projection — the items and their id index —
// is what a diff is computed against, and is what eviction lets go.
type baseline struct {
	items []sessionlog.Item
	byID  map[string]int
	// dropped is true once eviction has let the projection go. The fingerprint
	// stays, so a log that is not being written is still skipped; a later read
	// re-establishes the projection rather than diffing against nothing.
	dropped bool
	meta    sessionlog.Meta
	// size and modTime are what the log looked like when it was last read, so
	// an unchanged session costs one stat instead of a decode.
	size    int64
	modTime time.Time
	// seeded is false until the first successful read has been recorded.
	seeded bool
	// lastSeen orders eviction, and is only advanced by a real read: a session
	// nobody has written to for an hour is the one to forget first.
	lastSeen time.Time
}

// New builds a Watcher.
func New(opts Options) (*Watcher, error) {
	if opts.Store == nil {
		return nil, errors.New("sessionwatch: a store is required")
	}
	if opts.Bus == nil {
		return nil, errors.New("sessionwatch: a bus is required")
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Watcher{
		opts:    opts,
		seen:    map[string]*baseline{},
		running: map[string]bool{},
	}, nil
}

// Run sweeps until the context is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.opts.Interval)
	defer ticker.Stop()

	w.Sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Sweep(ctx)
		}
	}
}

// stillHeld reports whether a session the last sweep saw mid-turn is still held
// by a live writer.
//
// It exists so the early return in follow can read as "nothing has changed and
// nothing is running", rather than as a chain of &&s with a negated compound at
// the end. The distinction matters because the projection may have been evicted
// since: the baseline is what says the turn was running, and the lock is what
// says it still is.
func (w *Watcher) stillHeld(previous *baseline, sessionID string) bool {
	return previous.meta.TurnRunning && w.opts.Store.Locked(sessionID)
}

// Sweep reads every session log once and publishes what changed.
//
// Exported so a test can drive the watcher deterministically instead of waiting
// on a ticker.
func (w *Watcher) Sweep(ctx context.Context) {
	logs := w.opts.Store.Logs()
	live := make(map[string]struct{}, len(logs))
	for _, log := range logs {
		if err := ctx.Err(); err != nil {
			return
		}
		live[log.ID] = struct{}{}
		w.follow(ctx, log)
	}
	// Forget sessions whose log is gone, then bound what is retained.
	w.mu.Lock()
	for id := range w.seen {
		if _, ok := live[id]; !ok {
			delete(w.seen, id)
			delete(w.running, id)
		}
	}
	w.mu.Unlock()
	w.evict()
}

// evict lets the projections of the least recently seen sessions go once the
// bound is exceeded, keeping their fingerprints.
//
// Keeping the fingerprint is the point: an operator with hundreds of sessions
// must not pay a decode per session per sweep just because most of them are
// idle, and the fingerprint is what an idle log is skipped by. Only the
// projection — what a diff against the next append needs — is worth bounding.
func (w *Watcher) evict() {
	kept := 0
	for _, base := range w.seen {
		if !base.dropped {
			kept++
		}
	}
	for kept > maxBaselines {
		oldestID := ""
		var oldest time.Time
		for id, base := range w.seen {
			if base.dropped {
				continue
			}
			if oldestID == "" || base.lastSeen.Before(oldest) {
				oldestID, oldest = id, base.lastSeen
			}
		}
		if oldestID == "" {
			return
		}
		base := w.seen[oldestID]
		base.items = nil
		base.byID = nil
		base.dropped = true
		kept--
	}
}

// follow reads one session and publishes the difference since the last read.
func (w *Watcher) follow(ctx context.Context, log sessionlog.Log) {
	sessionID := log.ID

	w.mu.Lock()
	runningNow := w.running[sessionID]
	w.mu.Unlock()

	// Nothing written since the last read, and no turn believed to be in flight:
	// there is nothing to see. This is the common case by a wide margin — an
	// operator has hundreds of sessions and one of them being written — and it
	// is what keeps a sweep cheap enough to run every second.
	//
	// A session whose log ends mid-turn is the exception: its `running` answer
	// can change without a byte being written, when the process holding it dies
	// or a new one opens it. So the re-read happens while a writer holds the
	// session, and stops when none does — the lock is what answers the question,
	// at the cost of one flock.
	//
	// Asking the log instead is what makes a crashed turn expensive forever: a
	// process that dies mid-turn leaves its `turn/start` behind as the last
	// boundary for good, and a reader that trusts the log re-decodes the whole
	// history on every tick, for the rest of the gateway's life. Five such logs
	// in one sessions root were enough to hold a core at 100% around the clock.
	// A baseline whose projection eviction let go still answers this question:
	// the skip is decided on the fingerprint, not on the items.
	if previous := w.seen[sessionID]; previous != nil && previous.seeded &&
		previous.size == log.Size && previous.modTime.Equal(log.ModTime) &&
		!runningNow && !w.stillHeld(previous, sessionID) {
		return
	}

	items, meta, err := w.opts.Store.Items(ctx, sessionID)
	if err != nil {
		// A log that cannot be read right now — a partially flushed frame, a
		// format this build does not know — is retried on the next tick, and is
		// not worth a warning per second.
		w.debug("session not readable", sessionID, err)
		return
	}

	previous := w.seen[sessionID]
	current := &baseline{
		items:    items,
		byID:     indexByID(items),
		meta:     meta,
		size:     log.Size,
		modTime:  log.ModTime,
		seeded:   true,
		lastSeen: w.opts.Now(),
	}
	w.seen[sessionID] = current

	// A turn is in flight when the log's last boundary was a start *and* the
	// process that wrote it is still there. Without the second half, a session
	// whose process died mid-turn would look busy forever.
	running := meta.TurnRunning && w.opts.Store.Locked(sessionID)
	w.mu.Lock()
	wasRunning := w.running[sessionID]
	w.running[sessionID] = running
	w.mu.Unlock()

	owned := w.opts.Owned != nil && w.opts.Owned(sessionID)

	if previous == nil || !previous.seeded {
		// First sight seeds the baseline instead of replaying history. A turn
		// that is already running is the exception: that is not history, it is
		// the present, and a phone that is already watching has no other way to
		// learn that the gateway came up in the middle of someone's turn.
		if running && !owned {
			w.publishTurn(sessionID, true, meta.TurnCount)
		}
		return
	}

	// The announcement follows the answer, not the file: a turn can start or
	// finish without the log changing in the same tick as the lock — a process
	// opening the session is one case, a process dying mid-turn is another.
	if running != wasRunning && !owned {
		w.publishTurn(sessionID, running, meta.TurnCount)
	}

	if previous.dropped {
		// Eviction let the projection go, so there is nothing left to diff
		// against: the rows it held are already in the phone's history, and
		// re-sending them as if they were new is what the first-sight rule
		// exists to prevent. This read is what re-establishes the baseline —
		// and it is the only one a long-idle session pays for, because the
		// next append diffs against it.
		w.debug("no projection to diff; re-seeded", sessionID, nil)
		return
	}

	if len(items) < len(previous.items) {
		// The log was rewritten (compaction, or a resume that reorders it). The
		// difference is no longer an append, so there is nothing honest to
		// publish; the next append diffs against this read.
		w.debug("log shrank; re-seeding", sessionID, nil)
		return
	}

	for _, item := range items {
		was, existed := previous.lookup(item)
		switch {
		case !existed:
			if !owned {
				w.publishItem(sessionID, item, false)
			}
		case changed(was, item):
			if !owned {
				w.publishItem(sessionID, item, true)
			}
		}
	}

	if !owned {
		w.publishMeta(sessionID, previous.meta, meta)
	}
}

// publishTurn reports a turn starting or finishing, whoever said so.
func (w *Watcher) publishTurn(sessionID string, running bool, turnCount int) {
	state := "completed"
	if running {
		state = "running"
	}
	w.opts.Bus.Publish(events.TypeSessionState, sessionID, events.SessionBusy{Busy: running})
	w.opts.Bus.Publish(events.TypeTurnState, sessionID, events.TurnState{
		TurnID: fmt.Sprintf("log-%d", turnCount+1),
		State:  state,
	})
}

// publishItem emits the event that carries one conversation row.
func (w *Watcher) publishItem(sessionID string, item sessionlog.Item, update bool) {
	switch item.Role {
	case sessionlog.RoleUser:
		// Not a row the phone typed: it came from the desk, and it should appear
		// there too. `role` is what tells the two apart.
		w.opts.Bus.Publish(events.TypeSessionMessage, sessionID, events.MessageData{
			ID:          item.ID,
			Role:        "user",
			Text:        item.Text,
			Attachments: item.Attachments,
		})

	case sessionlog.RoleAssistant:
		data := events.MessageData{
			ID:   item.ID,
			Role: "assistant",
			Text: item.Text,
		}
		if item.Thinking != "" {
			data.Thinking = item.Thinking
		}
		if item.Model != "" {
			data.Model = item.Model
		}
		if item.Usage != nil {
			data.Usage = &events.MessageUsage{
				InputTokens:  item.Usage.InputTokens,
				OutputTokens: item.Usage.OutputTokens,
				TotalTokens:  item.Usage.TotalTokens,
			}
		}
		w.opts.Bus.Publish(events.TypeSessionMessage, sessionID, data)

	case sessionlog.RoleTool:
		// An update is the result landing on a card the phone already has; a
		// first sighting is the call itself. A call that started and finished
		// between two sweeps is published as both, so the card is complete
		// rather than a result that arrived from nowhere.
		if !update {
			w.publishTool(sessionID, events.ToolFacts{
				Phase:  events.ToolStarted,
				CallID: item.ID,
				Tool:   item.Tool,
				Status: events.ToolInProgress,
				Input:  item.Input,
			})
		}
		if !item.Pending {
			// The status is the log's own answer, not an assumption: a result
			// recorded as an error is a failed call, and publishing "completed"
			// for it would have the two producers disagree about the same fact.
			status := events.ToolCompleted
			if item.IsError {
				status = events.ToolFailed
			}
			w.publishTool(sessionID, events.ToolFacts{
				Phase:   events.ToolEnded,
				CallID:  item.ID,
				Tool:    item.Tool,
				Status:  status,
				Output:  item.Output,
				IsError: item.IsError,
				Facts:   item.Facts,
			})
		}

	case sessionlog.RoleNotice:
		// Most notices are bookkeeping — a turn boundary, a stop marker — and the
		// phone shows them only in history.
		//
		// A *settlement* is not. It is the one notice the operator is waiting for:
		// a delegated child agent finishing, recorded by the harness with a
		// typed source and its own summary. It reaches the stream as the user-role
		// message the harness committed, which is how the client already reads
		// live settlements for a session this gateway drives — one shape on both
		// producers, so the row is drawn the same way whichever end ran the turn.
		if item.Actor != "" {
			w.publishMessage(sessionID, events.MessageData{
				ID:   item.ID,
				Role: "user",
				Text: item.Text,
			})
		}
	}
}

// publishMessage emits one committed message row.
func (w *Watcher) publishMessage(sessionID string, data events.MessageData) {
	w.opts.Bus.Publish(events.TypeSessionMessage, sessionID, data)
}

// publishTool emits one tool lifecycle frame, bounded by the deployment's
// budgets and shaped by the same constructor the ACP bridge uses.
func (w *Watcher) publishTool(sessionID string, facts events.ToolFacts) {
	w.opts.Bus.Publish(events.TypeSessionTool, sessionID, events.NewToolData(facts, w.opts.Limits))
}

// publishMeta emits the session-level changes a list or header cares about. The
// turn itself is not one of them: `publishTurn` owns that frame, because the
// turn's state is the log *and* the lock, and only one place may announce it.
func (w *Watcher) publishMeta(sessionID string, previous, current sessionlog.Meta) {
	patch := map[string]any{}
	if current.Title != previous.Title {
		patch["title"] = current.Title
	}
	if current.Model != previous.Model {
		patch["model"] = current.Model
	}
	if current.TurnCount != previous.TurnCount {
		patch["updatedAt"] = w.opts.Now().UTC()
	}
	if len(patch) > 0 {
		w.opts.Bus.Publish(events.TypeSessionState, sessionID, patch)
	}
}

// lookup finds the previous copy of an item.
func (b *baseline) lookup(item sessionlog.Item) (sessionlog.Item, bool) {
	index, ok := b.byID[item.ID]
	if !ok || index >= len(b.items) {
		return sessionlog.Item{}, false
	}
	return b.items[index], true
}

// indexByID maps item ids to their position. Ids are unique within a session:
// message ids are DSH's own, and a tool call keeps its call id.
func indexByID(items []sessionlog.Item) map[string]int {
	byID := make(map[string]int, len(items))
	for i, item := range items {
		byID[item.ID] = i
	}
	return byID
}

// changed reports whether an item that already existed gained something worth
// publishing. Only the fields a follower turns into events are compared, so an
// incidental difference cannot wake the phone.
func changed(before, after sessionlog.Item) bool {
	return before.Pending != after.Pending ||
		before.Output != after.Output ||
		before.IsError != after.IsError ||
		before.Text != after.Text ||
		before.Thinking != after.Thinking ||
		before.Input != after.Input ||
		before.Tool != after.Tool
}

func (w *Watcher) debug(msg, sessionID string, err error) {
	if w.opts.Logger == nil {
		return
	}
	if err == nil {
		w.opts.Logger.Debug("sessionwatch: "+msg, "session", sessionID)
		return
	}
	w.opts.Logger.Debug("sessionwatch: "+msg, "session", sessionID, "error", err.Error())
}
