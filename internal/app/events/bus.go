// Package events is the gateway's event bus.
//
// It sits between the harness adapter, which publishes agent activity, and the
// WebSocket subscribers, which consume it. Its single most important property is
// that publishing never blocks. The call happens on the adapter's stdout reader
// goroutine; if that goroutine waits for a slow phone, the child's pipe fills,
// and DeepSeek Harness itself wedges mid-turn. Every policy here exists to make
// that impossible.
//
// The mechanism is a bounded per-subscriber queue plus a replay ring:
//
//   - Subscribers that keep up see every event in order.
//   - A subscriber that falls behind has its oldest events discarded and is told
//     to resynchronise, rather than being allowed to apply backpressure.
//   - Reconnecting clients resume from a cursor, so a phone that loses its radio
//     in a tunnel does not lose the agent's output.
//
// A cursor is a *generation* and a sequence number, never a bare sequence number.
// The sequence space belongs to one run of the process: `seq` starts at zero
// every start. Without the generation, a client that saw `seq: 900` before a
// redeploy reconnects with `since=900` against a bus whose watermark is now 0,
// and the arithmetic that guards the replay ring — "is the cursor older than what
// I still hold?" — answers "no, it is newer, so there is nothing to send". The
// client then receives frames numbered 1, 2, 3 and discards every one of them as
// stale, on a socket that is open and healthy. That failure is silent, which is
// why the generation is part of the contract rather than an internal detail:
// every cursor that cannot be resumed now resolves to exactly one answer, a
// resync, and there is no fourth case in which a stale resume appears to work.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"
)

// Type names an event class. The strings are part of the wire contract; see
// docs/api.md.
type Type string

const (
	// TypeHello is the first frame on a new connection.
	TypeHello Type = "hello"
	// TypeSessionState reports session metadata changes.
	TypeSessionState Type = "session.state"
	// TypeSessionMessage reports a committed assistant message.
	TypeSessionMessage Type = "session.message"
	// TypeSessionThought reports a committed reasoning block. It is separate
	// from session.message because clients render it collapsed by default.
	TypeSessionThought Type = "session.thought"
	// TypeSessionTool reports tool lifecycle progress.
	TypeSessionTool Type = "session.tool"
	// TypeUsage reports context occupancy.
	TypeUsage Type = "usage.update"
	// TypeApprovalRequested reports a pending human decision.
	TypeApprovalRequested Type = "approval.requested"
	// TypeApprovalResolved reports a decision.
	TypeApprovalResolved Type = "approval.resolved"
	// TypeApprovalGranted reports that a standing grant, rather than a person,
	// answered a request. It is separate from TypeApprovalResolved because the
	// two say different things to a reader: one is "you decided", the other is
	// "a decision you made earlier applied here", and a client that folded them
	// together would show an automated yes as a fresh human judgement.
	TypeApprovalGranted Type = "approval.granted"
	// TypeTurnState reports turn lifecycle.
	TypeTurnState Type = "turn.state"
	// TypeHarnessState reports the child process lifecycle.
	TypeHarnessState Type = "harness.state"
	// TypeResync tells a subscriber its view is incomplete.
	TypeResync Type = "resync"
	// TypeSnapshot carries the present state of what a client renders, and
	// always follows a TypeResync. See payloads.go for why the two are separate
	// frames rather than one.
	TypeSnapshot Type = "snapshot"
	// TypeDraining reports that the gateway is about to go away on purpose.
	//
	// It is a distinct frame from TypeHarnessState because it says something
	// different: the harness is fine, and it is *this process* that is leaving.
	// A client that folded the two together would show a child-process failure
	// for a redeploy.
	TypeDraining Type = "gateway.draining"
)

// ResyncReason explains why a subscriber's cursor could not be honoured. It is
// part of the wire contract: a client does not branch on it, but an operator
// reading a log, and an engineer reading a bug report, need to know which of the
// four ways the resume failed. The zero value never reaches the wire.
type ResyncReason string

const (
	// ResyncNoCursor means the client asked to start from now. Nothing was lost;
	// the client is simply owed a snapshot of the present.
	ResyncNoCursor ResyncReason = "no cursor"
	// ResyncWindowExceeded means the cursor is older than the retained events.
	ResyncWindowExceeded ResyncReason = "replay window exceeded"
	// ResyncGenerationChanged means the cursor belongs to an earlier run of the
	// process — a redeploy, or a crash the supervisor restarted.
	ResyncGenerationChanged ResyncReason = "the gateway restarted"
	// ResyncAheadOfStream means the cursor claims events this process has not
	// published. It is the restart case seen from the other side, and it is what
	// a client with a stale high watermark sends.
	ResyncAheadOfStream ResyncReason = "cursor is ahead of this gateway"
	// ResyncFellBehind means the subscriber could not keep up while connected
	// and had events discarded for it.
	ResyncFellBehind ResyncReason = "events were dropped for a slow client"
)

// ResyncReasonOf renders a reason for the wire, so an empty value cannot be
// mistaken for a real one.
func (r ResyncReason) String() string {
	if r == "" {
		return string(ResyncWindowExceeded)
	}
	return string(r)
}

// Event is one message on the bus and one frame on the wire.
type Event struct {
	// Seq is globally monotonic across the gateway's lifetime. A client resumes
	// with the last Seq it processed.
	Seq uint64 `json:"seq"`
	// Time is when the gateway observed the event.
	Time time.Time `json:"time"`
	// Type discriminates Data.
	Type Type `json:"type"`
	// SessionID is empty for gateway-wide events such as harness.state.
	SessionID string `json:"sessionId,omitempty"`
	// Data is the type-specific payload, already shaped for JSON.
	Data any `json:"data,omitempty"`
}

// Config parameterises the bus.
type Config struct {
	// Replay is how many recent events are retained for reconnect replay.
	Replay int
	// Queue is the per-subscriber buffer depth before it is considered slow.
	Queue int
	// Generation names this sequence space. It is minted once per process start,
	// and every cursor a client holds is scoped to it. Empty means "make one",
	// which is what a test wants and what production gets.
	Generation string
}

// Bus is the event bus. It is safe for concurrent use.
type Bus struct {
	cfg Config

	// generation names this run's sequence space; see the package comment.
	generation string

	seq atomic.Uint64

	mu   sync.RWMutex
	ring []Event
	subs map[*Subscription]struct{}

	// dropped counts events discarded because a subscriber could not keep up.
	// It is exposed so an operator can see backpressure happening rather than
	// having to infer it from client reports.
	dropped atomic.Uint64
}

// New builds a bus.
func New(cfg Config) *Bus {
	if cfg.Replay < 1 {
		cfg.Replay = 1024
	}
	if cfg.Queue < 1 {
		cfg.Queue = 256
	}
	if cfg.Generation == "" {
		cfg.Generation = newGeneration()
	}
	return &Bus{
		cfg:        cfg,
		generation: cfg.Generation,
		subs:       map[*Subscription]struct{}{},
	}
}

// Generation names the sequence space this bus publishes in. A client echoes it
// on reconnect, which is what lets the bus tell "you are up to date" from "you
// are holding a cursor from a process that no longer exists".
func (b *Bus) Generation() string { return b.generation }

// Watermark is the highest sequence number published so far, and zero before
// anything is published.
func (b *Bus) Watermark() uint64 { return b.seq.Load() }

// newGeneration mints an opaque, short, non-secret name for one run. It is not a
// secret and not a capability: it is a label whose only job is to differ between
// runs, so 64 bits of randomness is more than enough and keeps the frame small.
func newGeneration() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on a supported platform, and a bus that
		// cannot name its own sequence space would silently reintroduce exactly
		// the bug this field exists to prevent. Failing loudly is the only safe
		// answer; see internal/idgen for the same reasoning.
		panic("events: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Publish appends an event and fans it out. It never blocks.
func (b *Bus) Publish(t Type, sessionID string, data any) Event {
	e := Event{
		Seq:       b.seq.Add(1),
		Time:      time.Now().UTC(),
		Type:      t,
		SessionID: sessionID,
		Data:      data,
	}

	b.mu.Lock()
	b.ring = append(b.ring, e)
	if len(b.ring) > b.cfg.Replay {
		// Re-slice rather than copy: the backing array is reused, and the
		// retained window keeps its own head index.
		overflow := len(b.ring) - b.cfg.Replay
		b.ring = append(b.ring[:0], b.ring[overflow:]...)
	}
	subs := make([]*Subscription, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	b.mu.Unlock()

	// Fan out outside the lock. A subscriber's queue is buffered and its send is
	// non-blocking, so this loop cannot be stalled by a slow client — but there
	// is no reason to hold the bus lock while touching N channels either.
	for _, s := range subs {
		if !s.offer(e) {
			b.dropped.Add(1)
		}
	}
	return e
}

// Dropped reports how many event deliveries were discarded for slow subscribers.
func (b *Bus) Dropped() uint64 { return b.dropped.Load() }

// Cursor is a client's position in the event stream.
//
// It is deliberately a struct rather than a bare sequence number, because a
// position is meaningless without the sequence space it is a position *in*. The
// zero Cursor means "no position": start from now.
type Cursor struct {
	// Generation is the sequence space the client was reading. Empty means the
	// client did not say — an older client, or a first connection — and is
	// treated as unusable rather than assumed, because assuming is precisely the
	// silent failure this type exists to prevent.
	Generation string
	// Seq is the last sequence number the client processed.
	Seq uint64
}

// Resume is what a Subscribe call produced: the subscription, and whether the
// client's view of the past could be completed.
type Resume struct {
	// Subscription receives every event after the resolved position.
	Subscription *Subscription
	// Reason explains why the client's view is incomplete, and is empty when the
	// resume was exact. It is the single answer to "does this client need
	// repairing": there is no separate boolean, because two fields that must
	// agree are two fields that can disagree.
	Reason ResyncReason
	// From is the sequence number this subscription resumes at: every delivered
	// event has a strictly greater Seq. It is what the server reports to the
	// client as the position it actually got, as opposed to the one it asked for.
	From uint64
	// Backlog is how many retained events were replayed. It exists for logs: an
	// operator asking "was that reconnect cheap or enormous" has no other way to
	// find out.
	Backlog int
}

// Resumable reports whether the client's cursor was honoured in full. A false
// answer means the caller owes the client a snapshot, and that the client's view
// of everything it was showing is suspect.
func (r Resume) Resumable() bool { return r.Reason == "" }

// Subscribe registers a new subscriber. The caller must Close the result.
//
// The contract is deliberately total: every cursor either resumes the client
// exactly, or resolves to a resync with a reason. There is no third outcome in
// which the client believes it is up to date and is not.
func (b *Bus) Subscribe(cursor Cursor, filter func(Event) bool) Resume {
	sub := &Subscription{
		bus:    b,
		ch:     make(chan Event, b.cfg.Queue),
		filter: filter,
		done:   make(chan struct{}),
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.subs[sub] = struct{}{}

	watermark := b.seq.Load()

	// The three ways a cursor is not a position in this stream at all. Each is
	// decided before the ring, because none of them is a question about
	// retention — and answering them as though they were is exactly the silent
	// failure this type exists to prevent.
	switch {
	case cursor.Seq == 0:
		// No position: start from now. Nothing was lost, but the client is owed
		// the present state, which is what a snapshot is for.
		return Resume{Subscription: sub, Reason: ResyncNoCursor, From: watermark}
	case cursor.Generation != "" && cursor.Generation != b.generation:
		// The cursor belongs to a run of the process that is gone.
		return Resume{Subscription: sub, Reason: ResyncGenerationChanged, From: watermark}
	case cursor.Seq > watermark:
		// The cursor claims a position this run never published: a client from a
		// previous run that did not send a generation, or a hand-edited URL.
		// Either way the stream must not be presented as continuous.
		return Resume{Subscription: sub, Reason: ResyncAheadOfStream, From: watermark}
	}

	// Everything the client is owed, bounded by what its queue can hold.
	//
	// The bound is not an optimisation. The replay ring is normally larger than a
	// subscriber's queue, so a client reconnecting after a long gap can owe more
	// events than the channel holds — and these sends happen while the bus lock
	// is held. Unbounded, a single reconnecting phone would block here, and every
	// Publish in the process would block behind it: one client would freeze event
	// delivery for all of them. A bounded batch plus an honest "replay was
	// incomplete" is the correct trade.
	var pending []Event
	for _, e := range b.ring {
		if e.Seq > cursor.Seq && sub.matches(e) {
			pending = append(pending, e)
		}
	}

	if len(b.ring) > 0 && b.ring[0].Seq > cursor.Seq+1 {
		// Events between the cursor and the head of the ring were discarded, so
		// no replay can fill the hole.
		return Resume{Subscription: sub, Reason: ResyncWindowExceeded, From: b.ring[0].Seq - 1}
	}

	from := cursor.Seq
	reason := ResyncReason("")
	if len(pending) > cap(sub.ch) {
		// Keep the newest: a client cares far more about the present state of a
		// turn than about the oldest events it missed.
		pending = pending[len(pending)-cap(sub.ch):]
		reason = ResyncWindowExceeded
		from = pending[0].Seq - 1
	}
	for _, e := range pending {
		sub.ch <- e
	}
	return Resume{Subscription: sub, Reason: reason, From: from, Backlog: len(pending)}
}

// Subscription is one consumer's view of the bus.
type Subscription struct {
	bus    *Bus
	ch     chan Event
	filter func(Event) bool

	// desynced is set when an event was dropped for this subscriber. The reader
	// observes it, through TakeResync, and emits a resync.
	desynced atomic.Bool

	closeOnce sync.Once
	done      chan struct{}
}

// Events is the receive channel. It is closed when the subscription closes.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Done is closed when the subscription has been closed.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// TakeResync reports whether this subscriber lost events, clearing the flag. A
// non-empty reason means the caller must emit a resync frame, because the
// subscriber's view of session state is no longer trustworthy.
func (s *Subscription) TakeResync() ResyncReason {
	if !s.desynced.Swap(false) {
		return ""
	}
	return ResyncFellBehind
}

// Close unregisters the subscriber and closes its channel.
func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		s.bus.mu.Lock()
		delete(s.bus.subs, s)
		s.bus.mu.Unlock()

		close(s.done)
		// The channel is not closed: a concurrent Publish may already be about to
		// send on it, and closing would panic. The reader selects on Done.
	})
}

// matches applies the subscription filter.
func (s *Subscription) matches(e Event) bool {
	if s.filter == nil {
		return true
	}
	return s.filter(e)
}

// offer queues an event without blocking, reporting false when the event had to
// be dropped.
func (s *Subscription) offer(e Event) bool {
	if !s.matches(e) {
		return true // filtered out is not dropped
	}

	select {
	case <-s.done:
		return true // the subscriber left; not a backpressure problem
	default:
	}

	select {
	case s.ch <- e:
		return true
	default:
	}

	// The queue is full. Discarding the oldest event makes room for the newest,
	// which is the right bias: a phone wants the present state of a turn far more
	// than it wants a message from two minutes ago, and it will resynchronise
	// from the transcript anyway.
	select {
	case <-s.ch:
	default:
	}
	select {
	case s.ch <- e:
	default:
	}
	s.desynced.Store(true)
	return false
}
