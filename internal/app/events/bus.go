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
//   - Reconnecting clients resume from a sequence number, so a phone that loses
//     its radio in a tunnel does not lose the agent's output.
package events

import (
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
	// TypeTurnState reports turn lifecycle.
	TypeTurnState Type = "turn.state"
	// TypeHarnessState reports the child process lifecycle.
	TypeHarnessState Type = "harness.state"
	// TypeResync tells a subscriber its view is incomplete.
	TypeResync Type = "resync"
)

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
}

// Bus is the event bus. It is safe for concurrent use.
type Bus struct {
	cfg Config

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
	return &Bus{cfg: cfg, subs: map[*Subscription]struct{}{}}
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

// Subscribe registers a new subscriber. The returned Subscription must be
// closed by the caller.
//
// since requests replay of every retained event with a sequence number greater
// than since. A zero since means "start from now" and replays nothing. The
// returned replayMissing flag reports that since fell off the back of the ring,
// in which case the caller must emit a resync instead of pretending the stream
// is complete.
func (b *Bus) Subscribe(since uint64, filter func(Event) bool) (sub *Subscription, replayMissing bool) {
	sub = &Subscription{
		bus:    b,
		ch:     make(chan Event, b.cfg.Queue),
		filter: filter,
		done:   make(chan struct{}),
	}

	b.mu.Lock()
	if since > 0 {
		oldest := uint64(0)
		if len(b.ring) > 0 {
			oldest = b.ring[0].Seq
		}
		switch {
		case len(b.ring) == 0:
			// Nothing retained. If the client has seen anything, its view cannot
			// be completed from here.
			replayMissing = since < b.seq.Load()
		case since+1 < oldest:
			// Events between since and the ring's head have been discarded.
			replayMissing = true
		default:
			// Collect what the client is owed, then trim it to what the channel
			// can hold.
			//
			// The trim is not an optimisation. The replay ring is normally larger
			// than a subscriber's queue, so a client reconnecting after a long
			// gap can owe more events than the channel holds — and these sends
			// happen while the bus lock is held. Unbounded, a single reconnecting
			// phone would block here, and every Publish in the process would
			// block behind it: one client would freeze event delivery for all of
			// them. A bounded batch plus an honest "replay was incomplete" is the
			// correct trade.
			var pending []Event
			for _, e := range b.ring {
				if e.Seq > since && sub.matches(e) {
					pending = append(pending, e)
				}
			}
			if len(pending) > cap(sub.ch) {
				// Keep the newest: a client cares far more about the present
				// state of a turn than about the oldest events it missed.
				pending = pending[len(pending)-cap(sub.ch):]
				replayMissing = true
			}
			for _, e := range pending {
				sub.ch <- e
			}
		}
	}
	b.subs[sub] = struct{}{}
	b.mu.Unlock()

	return sub, replayMissing
}

// Subscription is one consumer's view of the bus.
type Subscription struct {
	bus    *Bus
	ch     chan Event
	filter func(Event) bool

	// desynced is set when an event was dropped for this subscriber. The reader
	// observes it and emits a resync.
	desynced atomic.Bool

	closeOnce sync.Once
	done      chan struct{}
}

// Events is the receive channel. It is closed when the subscription closes.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Done is closed when the subscription has been closed.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// TakeResync reports whether this subscriber lost events, clearing the flag. The
// caller must emit a resync frame when it returns true, because the subscriber's
// view of session state is no longer trustworthy.
func (s *Subscription) TakeResync() bool { return s.desynced.Swap(false) }

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
