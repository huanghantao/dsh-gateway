// Package turns schedules prompt turns, one at a time per session.
//
// Why this is a service rather than part of the prompt handler: "one turn at a
// time" and "what happens to a prompt that arrives while a turn is running" are
// policy, and policy that lives in a driving adapter is policy a second adapter
// would have to reimplement. See docs/adr/0004.
//
// Two properties are the point of the design.
//
// **Admission is atomic.** The scheduler decides between "start now", "queue" and
// "refuse" inside a single critical section. The handler it replaced checked
// whether a turn was running and then marked one running in two separate
// acquisitions, so two requests arriving together — a double tap on a phone, or
// a retry after a flaky radio — could both be admitted, and the loser surfaced
// as a failed turn the operator could not explain.
//
// **A follow-up typed mid-turn is not lost.** It is queued and runs when the
// current turn settles, which is what someone typing "also run the tests" while
// watching the agent work actually means. The queue is bounded, and the bound is
// named in the refusal rather than applied silently.
package turns

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/idgen"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// Turn states, as they appear on the wire. They are the strings in
// events.TurnState.State and the ones a client switches on.
const (
	// StateQueued means the prompt is accepted and waiting behind a turn.
	StateQueued = "queued"
	// StateRunning means the harness is executing it.
	StateRunning = "running"
	// StateCompleted means the turn finished on its own terms.
	StateCompleted = "completed"
	// StateCancelled means a human stopped it.
	StateCancelled = "cancelled"
	// StateFailed means it ended in an error.
	StateFailed = "failed"
)

// DefaultQueueDepth is how many prompts may wait behind a running turn when the
// configuration does not say.
const DefaultQueueDepth = 4

// Ticket is one admitted prompt, as reported to a client.
type Ticket struct {
	// ID identifies this turn within the gateway's lifetime. It is `turnId` on
	// the wire, matching the event payload and the transcript's vocabulary.
	ID string `json:"turnId"`
	// State is one of the State* constants.
	State string `json:"state"`
	// Position is the 1-based place in the queue while queued, and 0 while
	// running. A queued client watches this number.
	Position int `json:"position,omitempty"`
	// QueuedAt is when the prompt was accepted.
	QueuedAt time.Time `json:"queuedAt"`
	// StartedAt is when the turn began, and nil while it is still queued.
	StartedAt *time.Time `json:"startedAt,omitempty"`
}

// Queue is one session's turn state: what is running, and what waits behind it.
type Queue struct {
	Running *Ticket  `json:"turn,omitempty"`
	Queued  []Ticket `json:"queue,omitempty"`
}

// Busy reports whether a turn is running.
func (q Queue) Busy() bool { return q.Running != nil }

// CancelResult says what a cancel actually did, so a handler can report honestly
// rather than assuming something was stopped because the call returned.
type CancelResult struct {
	// Cancelled is true when a running turn was asked to stop.
	Cancelled bool `json:"cancelled"`
	// Dropped is how many queued prompts were discarded.
	Dropped int `json:"dropped"`
}

// Options configures a Scheduler.
type Options struct {
	// Harness runs the turns.
	Harness harness.Harness
	// Bus receives turn.state events.
	Bus *events.Bus
	// Logger may be nil.
	Logger *logx.Logger
	// Timeout bounds one turn. A turn that exceeds it is failed and the lease
	// stops being pinned by it, so a wedged harness cannot hold a session
	// forever.
	Timeout time.Duration
	// QueueDepth is how many prompts may wait behind a running one in a single
	// session. Zero refuses a mid-turn prompt instead of queueing it.
	QueueDepth int
	// MaxConcurrent caps how many turns run across all sessions at once. Zero
	// means the only bound is the per-session queue.
	MaxConcurrent int
	// Now is injectable for tests.
	Now func() time.Time
}

// Scheduler admits and runs prompts. It is safe for concurrent use.
type Scheduler struct {
	harness harness.Harness
	bus     *events.Bus
	logger  *logx.Logger
	timeout time.Duration
	depth   int
	max     int
	now     func() time.Time

	mu       sync.Mutex
	sessions map[string]*session
	running  int
}

// session is one session's scheduling state. It exists only while the session
// has a running turn or a non-empty queue.
type session struct {
	running *turn
	queue   []*turn
}

// turn is one admitted prompt.
type turn struct {
	id        string
	sessionID string
	blocks    []harness.PromptBlock
	// base is the caller's context with its cancellation and deadline removed.
	// A turn outlives the request that admitted it — the phone locks its screen,
	// the radio drops — so what the turn inherits is the request's *values*, not
	// its lifetime.
	base context.Context

	state     string
	position  int
	queuedAt  time.Time
	startedAt time.Time
	// cancelled records that a human asked this turn to stop. It is what makes
	// an error from the harness report as "cancelled" rather than "failed".
	cancelled bool
}

// New builds a Scheduler.
func New(opts Options) *Scheduler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Minute
	}
	if opts.QueueDepth < 0 {
		opts.QueueDepth = 0
	}
	if opts.Logger == nil {
		opts.Logger = logx.Discard()
	}
	return &Scheduler{
		harness:  opts.Harness,
		bus:      opts.Bus,
		logger:   opts.Logger,
		timeout:  opts.Timeout,
		depth:    opts.QueueDepth,
		max:      opts.MaxConcurrent,
		now:      opts.Now,
		sessions: map[string]*session{},
	}
}

// Submit admits a prompt.
//
// The returned ticket says whether the turn started or is queued, and where.
// The scheduler takes ownership of blocks: they are held until the turn runs,
// which for a queued prompt can be minutes.
//
// ctx is the caller's, and only its values survive: the turn is detached from
// the request's lifetime on purpose, so that a phone that walks away does not
// cancel the agent mid-edit.
func (s *Scheduler) Submit(ctx context.Context, sessionID string, blocks []harness.PromptBlock) (Ticket, error) {
	now := s.now().UTC()
	base := context.WithoutCancel(ctx)

	s.mu.Lock()
	if s.max > 0 && s.running >= s.max {
		s.mu.Unlock()
		return Ticket{}, errx.New(errx.KindConflict, "too_many_turns",
			"this gateway is already running as many turns as it allows at once")
	}
	st := s.sessions[sessionID]
	if st == nil {
		st = &session{}
		s.sessions[sessionID] = st
	}

	if st.running == nil {
		t := &turn{
			id:        idgen.New("turn"),
			sessionID: sessionID,
			blocks:    blocks,
			base:      base,
			state:     StateRunning,
			queuedAt:  now,
			startedAt: now,
		}
		st.running = t
		s.running++
		s.mu.Unlock()

		s.logger.Info("turn started", "session", sessionID, "turn", t.id)
		s.publishEcho(sessionID, t.blocks)
		// The session is busy now, and it was not before: this is the one place
		// that transition happens, so it is the one place that announces it.
		s.bus.Publish(events.TypeSessionState, sessionID, events.SessionBusy{Busy: true})
		s.publish(t, 0)
		// The context travels on the turn, detached from the request that
		// admitted it — see turn.base. Detachment is the point of the design, so
		// the linter's advice to thread the caller's context here is the one
		// thing this must not do.
		go s.run(t) //nolint:contextcheck // the turn's context is deliberately not the caller's
		return ticketOf(t), nil
	}

	if s.depth == 0 {
		s.mu.Unlock()
		return Ticket{}, errx.New(errx.KindConflict, "prompt_in_flight",
			"a turn is already running for this session; wait for it or cancel it")
	}
	if len(st.queue) >= s.depth {
		waiting := len(st.queue)
		s.mu.Unlock()
		return Ticket{}, errx.New(errx.KindConflict, "queue_full",
			"this session already has "+itoa(waiting)+" prompt(s) waiting; "+
				"wait for one to run or cancel the turn")
	}

	t := &turn{
		id:        idgen.New("turn"),
		sessionID: sessionID,
		blocks:    blocks,
		base:      base,
		state:     StateQueued,
		position:  len(st.queue) + 1,
		queuedAt:  now,
	}
	st.queue = append(st.queue, t)
	behind := len(st.queue) - t.position
	s.mu.Unlock()

	s.logger.Info("prompt queued behind a running turn",
		"session", sessionID, "turn", t.id, "position", t.position)
	// A queued prompt is announced like any other: the phone that typed it shows
	// its own copy, and every other client has to be able to see what is waiting
	// — the queue strip deliberately carries no text, so this frame is the only
	// place the words exist until the turn runs.
	s.publishEcho(sessionID, t.blocks)
	s.publish(t, behind)
	return ticketOf(t), nil
}

// Cancel stops the running turn and discards everything queued behind it.
//
// The interrupt is the harness's own: it is asked to stop, and the turn settles
// when it says so. Tearing the call down locally instead would abandon a turn
// DSH is still running and — worse — start the next queued prompt while the
// previous one was still writing to the session.
//
// Dropping the queue is deliberate. "Stop" on a phone means stop, not stop this
// and immediately begin the next thing.
func (s *Scheduler) Cancel(ctx context.Context, sessionID string) (CancelResult, error) {
	s.mu.Lock()
	st := s.sessions[sessionID]
	if st == nil {
		s.mu.Unlock()
		return CancelResult{}, nil
	}
	result := CancelResult{Dropped: len(st.queue)}
	st.queue = nil
	running := st.running
	if running == nil {
		// Nothing was running, so the session has no remaining state at all.
		delete(s.sessions, sessionID)
		s.mu.Unlock()
		if result.Dropped > 0 {
			s.logger.Info("dropped queued prompts", "session", sessionID, "count", result.Dropped)
		}
		return result, nil
	}
	running.cancelled = true
	result.Cancelled = true
	s.mu.Unlock()

	s.logger.Info("turn cancel requested",
		"session", sessionID, "turn", running.id, "dropped", result.Dropped)
	if err := s.harness.Cancel(ctx, sessionID); err != nil {
		// The flag stands, so the turn still reports as cancelled if it settles;
		// but the agent may not have heard, and the caller should say so.
		return result, err
	}
	return result, nil
}

// Drop removes one queued prompt. It reports whether the ticket was there to
// remove, so a caller can tell "cancelled it" from "it had already started".
func (s *Scheduler) Drop(sessionID, turnID string) (bool, error) {
	s.mu.Lock()
	st := s.sessions[sessionID]
	if st == nil {
		s.mu.Unlock()
		return false, nil
	}
	index := -1
	for i, t := range st.queue {
		if t.id == turnID {
			index = i
			break
		}
	}
	if index < 0 {
		s.mu.Unlock()
		return false, nil
	}
	st.queue = append(st.queue[:index], st.queue[index+1:]...)
	moved := s.reindex(st)
	behind := len(st.queue)
	if st.running == nil && behind == 0 {
		delete(s.sessions, sessionID)
	}
	s.mu.Unlock()

	for _, t := range moved {
		s.publish(t, behind-t.position)
	}
	s.logger.Info("queued prompt dropped", "session", sessionID, "turn", turnID)
	return true, nil
}

// Busy reports whether a turn is running for the session. This is the single
// answer the lease manager consults, so that "mid-turn" has one definition.
func (s *Scheduler) Busy(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.sessions[sessionID]
	return st != nil && st.running != nil
}

// Queue returns the session's running and queued turns, oldest first.
func (s *Scheduler) Queue(sessionID string) Queue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return queueOf(s.sessions[sessionID])
}

// BusyAny reports whether any session has a turn running.
//
// It is what a drain waits on. A queued prompt deliberately does not count: it
// has not started, so a deploy that is waiting to be polite is not obliged to
// wait for work the agent has not begun — and the queue is re-established from
// the client, which is still holding the prompt it typed.
func (s *Scheduler) BusyAny() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running > 0
}

// run executes one turn and hands the session to whatever is queued behind it.
func (s *Scheduler) run(t *turn) {
	// The context is deliberately not tied to any request: a turn outlives the
	// phone that started it, and inheriting a request's deadline would cancel
	// the agent mid-edit the moment the app went to the background. What it
	// inherits is the caller's values, and its own bounding timeout.
	ctx, cancel := context.WithTimeout(t.base, s.timeout)
	stopReason, err := s.harness.Prompt(ctx, t.sessionID, t.blocks)
	cancel()

	state, detail := settle(t, stopReason, err)
	next, behind, moved := s.advance(t)
	s.publishSettled(t, state, detail, behind)
	// A promotion moves everything behind it, and a client has no way to learn
	// that a queue shifted until something tells it.
	for _, queued := range moved {
		s.publish(queued, behind-queued.position)
	}

	if state == StateFailed {
		s.logger.Warn("turn failed",
			"session", t.sessionID, "turn", t.id, "error", detail)
	} else {
		s.logger.Info("turn settled",
			"session", t.sessionID, "turn", t.id, "state", state, "stopReason", stopReason)
	}

	if next != nil {
		s.publish(next, behind)
		go s.run(next)
		return
	}
	// Nothing is left to run, so the session is no longer busy. Publishing the
	// *change* rather than the state is what keeps this honest: while a queued
	// prompt is promoted the session was busy and still is, and a frame saying so
	// would only be noise.
	s.bus.Publish(events.TypeSessionState, t.sessionID, events.SessionBusy{Busy: false})
}

// publishEcho announces a prompt this gateway has admitted.
//
// The client that typed it draws its own copy the moment the request succeeds.
// Every other client — and that one, after a reload — would otherwise know
// nothing until the session log committed the prompt, which is why a follow-up
// typed elsewhere arrived late or not at all. The echo carries no id: the log
// owns the durable one, and a client that merges history by id must let the
// committed row replace this.
func (s *Scheduler) publishEcho(sessionID string, blocks []harness.PromptBlock) {
	text := promptText(blocks)
	attachments := promptAttachments(blocks)
	if strings.TrimSpace(text) == "" && attachments == 0 {
		return
	}
	s.bus.Publish(events.TypeSessionMessage, sessionID, events.MessageData{
		Role:        "user",
		Text:        text,
		Attachments: attachments,
	})
}

// promptText is the prompt as a reader typed it: the text blocks concatenated,
// in order, exactly the way the transcript projects the same message. The two
// have to agree — a client folds history over live frames, and a prompt that
// read differently in each would look like two prompts.
func promptText(blocks []harness.PromptBlock) string {
	var body strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			body.WriteString(block.Text)
		}
	}
	return body.String()
}

// promptAttachments counts the blocks that are not text: a screenshot, a pasted
// image. A prompt that was only pictures still has to render as something.
func promptAttachments(blocks []harness.PromptBlock) int {
	count := 0
	for _, block := range blocks {
		if block.Type != "text" {
			count++
		}
	}
	return count
}

// advance retires t and promotes the next queued prompt, if any. It returns the
// promoted turn, how many remain behind it, and the queued tickets whose
// position the promotion changed.
func (s *Scheduler) advance(t *turn) (next *turn, behind int, moved []*turn) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.sessions[t.sessionID]
	if st == nil || st.running != t {
		// Cancelled and cleaned up, or superseded. Nothing to promote.
		return nil, 0, nil
	}
	st.running = nil
	s.running--

	if len(st.queue) > 0 {
		next = st.queue[0]
		st.queue = st.queue[1:]
		next.state = StateRunning
		next.position = 0
		next.startedAt = s.now().UTC()
		st.running = next
		s.running++
	}
	moved = s.reindex(st)
	behind = len(st.queue)
	if st.running == nil && behind == 0 {
		delete(s.sessions, t.sessionID)
	}
	return next, behind, moved
}

// reindex rewrites the queue positions after a removal and returns the tickets
// whose position changed, so their clients can be told. It must be called with
// the lock held.
func (s *Scheduler) reindex(st *session) []*turn {
	moved := make([]*turn, 0, len(st.queue))
	for i, t := range st.queue {
		if t.position != i+1 {
			t.position = i + 1
			moved = append(moved, t)
		}
	}
	return moved
}

// settle decides how a finished turn is reported.
func settle(t *turn, stopReason string, err error) (state, detail string) {
	switch {
	case err != nil && t.cancelled:
		// A cancel often surfaces as an error from the harness. Reporting it as
		// a failure would tell the operator their agent broke when they were the
		// one who stopped it.
		return StateCancelled, ""
	case err != nil:
		return StateFailed, err.Error()
	case strings.EqualFold(stopReason, "cancelled"):
		return StateCancelled, ""
	default:
		return StateCompleted, ""
	}
}

// publish emits the current state of a turn.
func (s *Scheduler) publish(t *turn, behind int) {
	s.bus.Publish(events.TypeTurnState, t.sessionID, turnStateOf(t, behind))
}

// publishSettled emits a settled turn, with the queue depth it leaves behind.
func (s *Scheduler) publishSettled(t *turn, state, detail string, behind int) {
	s.bus.Publish(events.TypeTurnState, t.sessionID, events.TurnState{
		TurnID:     t.id,
		State:      state,
		Detail:     detail,
		StartedAt:  utc(t.startedAt),
		QueuedAt:   utc(t.queuedAt),
		QueueDepth: behind,
	})
}

// turnStateOf renders a live turn for the wire.
func turnStateOf(t *turn, behind int) events.TurnState {
	return events.TurnState{
		TurnID:     t.id,
		State:      t.state,
		Position:   t.position,
		QueueDepth: behind,
		QueuedAt:   utc(t.queuedAt),
		StartedAt:  utc(t.startedAt),
	}
}

// ticketOf renders a turn as the caller sees it.
func ticketOf(t *turn) Ticket {
	ticket := Ticket{
		ID:       t.id,
		State:    t.state,
		Position: t.position,
		QueuedAt: t.queuedAt,
	}
	if t.state == StateRunning && !t.startedAt.IsZero() {
		started := t.startedAt
		ticket.StartedAt = &started
	}
	return ticket
}

// queueOf renders a session's scheduling state.
func queueOf(st *session) Queue {
	if st == nil {
		return Queue{}
	}
	out := Queue{}
	if st.running != nil {
		running := ticketOf(st.running)
		out.Running = &running
	}
	if len(st.queue) > 0 {
		out.Queued = make([]Ticket, 0, len(st.queue))
		for _, t := range st.queue {
			out.Queued = append(out.Queued, ticketOf(t))
		}
	}
	return out
}

// utc renders a zero time as nil, so an omitted field means "unknown" rather
// than the year one.
func utc(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	at := t.UTC()
	return &at
}

// itoa is a tiny local conversion, so this package does not pull in strconv for
// one error message.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
