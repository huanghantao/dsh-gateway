package turns_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/app/turns"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

/* ------------------------------------------------------------------ fake */

// call is one Prompt invocation the test can hold open and release by hand.
type call struct {
	sessionID string
	release   chan struct{}
	settled   chan struct{}
}

// fakeHarness runs prompts only when the test says so, which is what makes the
// queue observable: a real harness would settle too fast to see a second prompt
// waiting behind the first.
type fakeHarness struct {
	mu      sync.Mutex
	calls   []*call
	started chan *call
	stop    string
	fail    error

	cancelMu sync.Mutex
	cancels  []string
	cancelEr error
}

func newFakeHarness() *fakeHarness {
	return &fakeHarness{started: make(chan *call, 64)}
}

func (h *fakeHarness) Prompt(ctx context.Context, sessionID string, _ []harness.PromptBlock) (string, error) {
	c := &call{sessionID: sessionID, release: make(chan struct{}), settled: make(chan struct{})}
	h.mu.Lock()
	h.calls = append(h.calls, c)
	stop, fail := h.stop, h.fail
	h.mu.Unlock()

	h.started <- c
	select {
	case <-c.release:
		close(c.settled)
		return stop, fail
	case <-ctx.Done():
		close(c.settled)
		return "", ctx.Err()
	}
}

func (h *fakeHarness) Cancel(_ context.Context, sessionID string) error {
	h.cancelMu.Lock()
	h.cancels = append(h.cancels, sessionID)
	err := h.cancelEr
	h.cancelMu.Unlock()
	return err
}

func (h *fakeHarness) cancelled() []string {
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	return append([]string(nil), h.cancels...)
}

// nextStart waits for the next prompt to reach the harness.
func (h *fakeHarness) nextStart(t *testing.T) *call {
	t.Helper()
	select {
	case c := <-h.started:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a prompt to reach the harness")
		return nil
	}
}

// finish lets the held prompt return.
func (c *call) finish() { close(c.release) }

// waitSettled waits for the held prompt to return.
func (c *call) waitSettled(t *testing.T) {
	t.Helper()
	select {
	case <-c.settled:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a turn to settle")
	}
}

// The remaining methods are unused by these tests.

func (h *fakeHarness) Start(context.Context) error        { return nil }
func (h *fakeHarness) Capabilities() harness.Capabilities { return harness.Capabilities{} }
func (h *fakeHarness) Close(context.Context) error        { return nil }
func (h *fakeHarness) State() harness.State               { return harness.StateReady }
func (h *fakeHarness) ListSessions(context.Context, string, string) (harness.SessionPage, error) {
	return harness.SessionPage{}, nil
}
func (h *fakeHarness) NewSession(context.Context, string) (harness.Session, error) {
	return harness.Session{}, nil
}
func (h *fakeHarness) ResumeSession(context.Context, string, string) (harness.Session, error) {
	return harness.Session{}, nil
}
func (h *fakeHarness) CloseSession(context.Context, string) error { return nil }
func (h *fakeHarness) SetConfigOption(context.Context, string, string, string) ([]harness.ConfigOption, error) {
	return nil, nil
}

/* --------------------------------------------------------------- helpers */

func newScheduler(t *testing.T, h *fakeHarness, opts turns.Options) (*turns.Scheduler, *events.Bus) {
	t.Helper()
	bus := events.New(events.Config{Replay: 256, Queue: 256})
	opts.Harness = h
	opts.Bus = bus
	opts.Logger = logx.Discard()
	if opts.Timeout == 0 {
		opts.Timeout = time.Minute
	}
	return turns.New(opts), bus
}

// collect drains every turn.state event published so far.
func collect(t *testing.T, bus *events.Bus, sub *events.Subscription, want int) []events.TurnState {
	t.Helper()
	out := make([]events.TurnState, 0, want)
	deadline := time.After(3 * time.Second)
	for len(out) < want {
		select {
		case e := <-sub.Events():
			if e.Type != events.TypeTurnState {
				continue
			}
			state, ok := e.Data.(events.TurnState)
			if !ok {
				t.Fatalf("turn.state payload = %T, want events.TurnState", e.Data)
			}
			out = append(out, state)
		case <-deadline:
			t.Fatalf("timed out with %d of %d turn.state events: %+v", len(out), want, out)
		}
	}
	return out
}

/* ----------------------------------------------------------------- tests */

// TestFirstPromptRunsImmediately is the baseline the queue is a departure from.
func TestFirstPromptRunsImmediately(t *testing.T) {
	h := newFakeHarness()
	s, _ := newScheduler(t, h, turns.Options{QueueDepth: 4})

	ticket, err := s.Submit(context.Background(), "session-1", nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if ticket.State != turns.StateRunning {
		t.Errorf("state = %q, want %q", ticket.State, turns.StateRunning)
	}
	if ticket.Position != 0 {
		t.Errorf("position = %d, want 0 for a running turn", ticket.Position)
	}
	if ticket.StartedAt == nil {
		t.Error("startedAt is nil; a client needs it to tick an elapsed timer")
	}
	if !s.Busy("session-1") {
		t.Error("Busy = false while a turn is running")
	}
	h.nextStart(t).finish()
}

// TestSecondPromptQueuesBehindTheFirst is the behaviour a phone needs: the
// follow-up typed while the agent works is kept, in order, with a position the
// client can show.
func TestSecondPromptQueuesBehindTheFirst(t *testing.T) {
	h := newFakeHarness()
	s, _ := newScheduler(t, h, turns.Options{QueueDepth: 4})

	if _, err := s.Submit(context.Background(), "session-1", nil); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	first := h.nextStart(t)

	second, err := s.Submit(context.Background(), "session-1", nil)
	if err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if second.State != turns.StateQueued || second.Position != 1 {
		t.Errorf("second = %+v, want queued at position 1", second)
	}
	if second.StartedAt != nil {
		t.Error("a queued turn reports startedAt; it has not started")
	}

	third, err := s.Submit(context.Background(), "session-1", nil)
	if err != nil {
		t.Fatalf("third Submit: %v", err)
	}
	if third.Position != 2 {
		t.Errorf("third position = %d, want 2", third.Position)
	}

	queue := s.Queue("session-1")
	if queue.Running == nil || queue.Running.ID != first.sessionID && queue.Running.ID == "" {
		t.Errorf("queue.Running = %+v, want the first turn", queue.Running)
	}
	if len(queue.Queued) != 2 {
		t.Fatalf("queued = %d, want 2", len(queue.Queued))
	}
	if queue.Queued[0].ID != second.ID || queue.Queued[1].ID != third.ID {
		t.Errorf("queue order = %q,%q want %q,%q",
			queue.Queued[0].ID, queue.Queued[1].ID, second.ID, third.ID)
	}

	first.finish()
}

// TestQueuedPromptRunsWhenTheTurnSettles is the whole point: the follow-up is
// not merely stored, it runs.
func TestQueuedPromptRunsWhenTheTurnSettles(t *testing.T) {
	h := newFakeHarness()
	s, _ := newScheduler(t, h, turns.Options{QueueDepth: 4})

	if _, err := s.Submit(context.Background(), "session-1", nil); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	first := h.nextStart(t)
	if _, err := s.Submit(context.Background(), "session-1", nil); err != nil {
		t.Fatalf("second Submit: %v", err)
	}

	first.finish()
	second := h.nextStart(t)

	// The promoted turn is running, not queued, and the queue behind it is empty.
	queue := s.Queue("session-1")
	if queue.Running == nil {
		t.Fatal("nothing is running after the first turn settled")
	}
	if len(queue.Queued) != 0 {
		t.Errorf("queued = %d, want 0", len(queue.Queued))
	}
	if !s.Busy("session-1") {
		t.Error("Busy = false while the promoted turn runs")
	}
	second.finish()
}

// TestConcurrentSubmitsAdmitExactlyOneTurn is the regression test for the race
// the handler had: a double tap must not start two turns.
func TestConcurrentSubmitsAdmitExactlyOneTurn(t *testing.T) {
	h := newFakeHarness()
	s, _ := newScheduler(t, h, turns.Options{QueueDepth: 64})

	const submitters = 16
	var wg sync.WaitGroup
	results := make([]turns.Ticket, submitters)
	errs := make([]error, submitters)
	for i := range submitters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.Submit(context.Background(), "session-1", nil)
		}()
	}
	wg.Wait()

	running := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		if results[i].State == turns.StateRunning {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("running turns = %d, want exactly 1", running)
	}

	// Exactly one prompt reached the harness.
	h.nextStart(t)
	select {
	case extra := <-h.started:
		t.Fatalf("a second prompt reached the harness: %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestQueueIsBoundedAndSaysSo checks the refusal names the limit instead of
// silently dropping the prompt.
func TestQueueIsBoundedAndSaysSo(t *testing.T) {
	h := newFakeHarness()
	s, _ := newScheduler(t, h, turns.Options{QueueDepth: 1})

	if _, err := s.Submit(context.Background(), "session-1", nil); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	first := h.nextStart(t)
	if _, err := s.Submit(context.Background(), "session-1", nil); err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	_, err := s.Submit(context.Background(), "session-1", nil)
	if err == nil {
		t.Fatal("third Submit succeeded with a full queue")
	}
	if code := codeOf(t, err); code != "queue_full" {
		t.Errorf("code = %q, want queue_full", code)
	}
	first.finish()
}

// TestZeroQueueDepthRestoresStrictBehaviour is the escape hatch for a deployment
// that wants one prompt at a time.
func TestZeroQueueDepthRestoresStrictBehaviour(t *testing.T) {
	h := newFakeHarness()
	s, _ := newScheduler(t, h, turns.Options{QueueDepth: 0})

	if _, err := s.Submit(context.Background(), "session-1", nil); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	first := h.nextStart(t)
	if _, err := s.Submit(context.Background(), "session-1", nil); err == nil {
		t.Fatal("second Submit succeeded with queueing disabled")
	} else if code := codeOf(t, err); code != "prompt_in_flight" {
		t.Errorf("code = %q, want prompt_in_flight", code)
	}
	first.finish()
}

// TestCancelStopsTheTurnAndDropsTheQueue pins the semantics of the Stop button.
func TestCancelStopsTheTurnAndDropsTheQueue(t *testing.T) {
	h := newFakeHarness()
	s, _ := newScheduler(t, h, turns.Options{QueueDepth: 4})

	if _, err := s.Submit(context.Background(), "session-1", nil); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	first := h.nextStart(t)
	ticket, err := s.Submit(context.Background(), "session-1", nil)
	if err != nil {
		t.Fatalf("second Submit: %v", err)
	}

	result, err := s.Cancel(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !result.Cancelled || result.Dropped != 1 {
		t.Errorf("Cancel = %+v, want cancelled with 1 dropped", result)
	}
	if got := h.cancelled(); len(got) != 1 || got[0] != "session-1" {
		t.Errorf("harness cancels = %v, want one for session-1", got)
	}
	// The dropped prompt never reaches the harness.
	first.finish()
	first.waitSettled(t)
	select {
	case extra := <-h.started:
		t.Fatalf("a dropped prompt still reached the harness: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
	if s.Queue("session-1").Queued != nil {
		t.Error("the queue survived a cancel")
	}
	_ = ticket
}

// TestCancelOnIdleSessionIsNotAnError makes "stop" idempotent, which is what a
// double-tap on a phone needs it to be.
func TestCancelOnIdleSessionIsNotAnError(t *testing.T) {
	h := newFakeHarness()
	s, _ := newScheduler(t, h, turns.Options{QueueDepth: 4})

	result, err := s.Cancel(context.Background(), "session-nobody-touched")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if result.Cancelled || result.Dropped != 0 {
		t.Errorf("Cancel = %+v, want a no-op", result)
	}
	if got := h.cancelled(); len(got) != 0 {
		t.Errorf("the harness was asked to cancel %v; nothing was running", got)
	}
}

// TestCancelledTurnSettlesAsCancelledNotFailed checks the state a client shows.
func TestCancelledTurnSettlesAsCancelledNotFailed(t *testing.T) {
	h := newFakeHarness()
	h.fail = errors.New("the turn was interrupted")
	s, bus := newScheduler(t, h, turns.Options{QueueDepth: 4})
	sub, _ := bus.Subscribe(0, nil)
	defer sub.Close()

	if _, err := s.Submit(context.Background(), "session-1", nil); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	first := h.nextStart(t)
	if _, err := s.Cancel(context.Background(), "session-1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	first.finish()

	states := collect(t, bus, sub, 2)
	if states[0].State != turns.StateRunning {
		t.Errorf("first event = %q, want running", states[0].State)
	}
	if states[1].State != turns.StateCancelled {
		t.Errorf("settled state = %q, want cancelled: the operator stopped it, "+
			"the agent did not break", states[1].State)
	}
	if states[1].StartedAt == nil {
		t.Error("the settled event omits startedAt; a client cannot show how long it ran")
	}
}

// TestDropRemovesOneQueuedPromptAndRepublishesPositions covers cancelling a
// single follow-up without stopping the turn that is running.
func TestDropRemovesOneQueuedPromptAndRepublishesPositions(t *testing.T) {
	h := newFakeHarness()
	s, bus := newScheduler(t, h, turns.Options{QueueDepth: 4})
	sub, _ := bus.Subscribe(0, nil)
	defer sub.Close()

	if _, err := s.Submit(context.Background(), "session-1", nil); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	first := h.nextStart(t)
	second, _ := s.Submit(context.Background(), "session-1", nil)
	third, _ := s.Submit(context.Background(), "session-1", nil)

	ok, err := s.Drop("session-1", second.ID)
	if err != nil || !ok {
		t.Fatalf("Drop = %v, %v; want true, nil", ok, err)
	}
	queue := s.Queue("session-1")
	if len(queue.Queued) != 1 || queue.Queued[0].ID != third.ID {
		t.Fatalf("queue = %+v, want only the third ticket", queue.Queued)
	}
	if queue.Queued[0].Position != 1 {
		t.Errorf("position = %d, want 1 after the removal", queue.Queued[0].Position)
	}

	// The client that holds the third ticket is told its position moved.
	// running, two queued, then the republish the removal caused.
	states := collect(t, bus, sub, 4)
	last := states[len(states)-1]
	if last.TurnID != third.ID || last.Position != 1 {
		t.Errorf("last event = %+v, want the third ticket republished at position 1", last)
	}

	// Dropping a ticket that already started is not a removal.
	if ok, _ := s.Drop("session-1", first.sessionID); ok {
		t.Error("Drop claimed to remove a running turn")
	}
	first.finish()
}

// TestGlobalCapRefusesRatherThanOverloading gives limits.maxConcurrentTurns the
// meaning its name has always claimed.
func TestGlobalCapRefusesRatherThanOverloading(t *testing.T) {
	h := newFakeHarness()
	s, _ := newScheduler(t, h, turns.Options{QueueDepth: 4, MaxConcurrent: 1})

	if _, err := s.Submit(context.Background(), "session-1", nil); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	first := h.nextStart(t)

	if _, err := s.Submit(context.Background(), "session-2", nil); err == nil {
		t.Fatal("a second session's turn was admitted past the cap")
	} else if code := codeOf(t, err); code != "too_many_turns" {
		t.Errorf("code = %q, want too_many_turns", code)
	}
	first.finish()
}

// TestSettledTurnReleasesTheSession checks the bookkeeping a later Acquire
// depends on: once nothing runs and nothing waits, the session is forgotten.
func TestSettledTurnReleasesTheSession(t *testing.T) {
	h := newFakeHarness()
	s, _ := newScheduler(t, h, turns.Options{QueueDepth: 4})

	if _, err := s.Submit(context.Background(), "session-1", nil); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	first := h.nextStart(t)
	first.finish()
	first.waitSettled(t)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !s.Busy("session-1") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the session stayed busy after its only turn settled")
}

// TestTurnTimeoutFailsTheTurn proves a wedged harness cannot pin a session
// forever.
func TestTurnTimeoutFailsTheTurn(t *testing.T) {
	h := newFakeHarness()
	s, bus := newScheduler(t, h, turns.Options{QueueDepth: 4, Timeout: 20 * time.Millisecond})
	sub, _ := bus.Subscribe(0, nil)
	defer sub.Close()

	if _, err := s.Submit(context.Background(), "session-1", nil); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	states := collect(t, bus, sub, 2)
	if states[1].State != turns.StateFailed {
		t.Errorf("state = %q, want failed after the timeout", states[1].State)
	}
	if states[1].Detail == "" {
		t.Error("a failed turn carries no detail; the operator cannot tell why")
	}
}

// codeOf extracts the stable error code a client branches on.
func codeOf(t *testing.T, err error) string {
	t.Helper()
	code := errx.CodeOf(err)
	if code == "" {
		t.Fatalf("error %v carries no code", err)
	}
	return code
}

// TestAdmittedPromptIsEchoedAndBusyIsAnnounced covers the two session-level
// frames a client cannot derive on its own.
//
// The echo: a prompt this gateway admits is announced to every client, because
// the one that typed it draws its own copy and nobody else would know the words
// until the session log committed them. The busy frame: a client keeps its own
// answer to "is a turn running" — the composer reads it to decide whether Stop
// belongs on screen — and a turn that settles without saying so leaves Stop on
// screen with nothing behind it.
func TestAdmittedPromptIsEchoedAndBusyIsAnnounced(t *testing.T) {
	h := newFakeHarness()
	s, bus := newScheduler(t, h, turns.Options{QueueDepth: 4})
	sub, _ := bus.Subscribe(0, nil)
	defer sub.Close()

	blocks := []harness.PromptBlock{
		{Type: "text", Text: "look at this"},
		{Type: "image", MIMEType: "image/jpeg", Data: []byte{1, 2, 3}},
	}
	if _, err := s.Submit(context.Background(), "session-1", blocks); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	first := h.nextStart(t)

	// The opening frames: the prompt, then the session saying it is busy.
	var echoed *events.MessageData
	busy := -1
	deadline := time.After(3 * time.Second)
	for echoed == nil || busy == -1 {
		select {
		case e := <-sub.Events():
			switch data := e.Data.(type) {
			case events.MessageData:
				copied := data
				echoed = &copied
			case events.SessionBusy:
				if data.Busy {
					busy = 1
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the echo and the busy frame (echo=%v busy=%d)", echoed, busy)
		}
	}
	if echoed.Role != "user" {
		t.Errorf("echo role = %q, want user", echoed.Role)
	}
	if echoed.Text != "look at this" {
		t.Errorf("echo text = %q, want the prompt's text", echoed.Text)
	}
	if echoed.ID != "" {
		t.Errorf("echo id = %q, want empty: the log owns the durable id", echoed.ID)
	}
	if echoed.Attachments != 1 {
		t.Errorf("echo attachments = %d, want 1 for the image block", echoed.Attachments)
	}

	// Settling the last turn has to say the session is idle again.
	first.finish()
	idle := false
	deadline = time.After(3 * time.Second)
	for !idle {
		select {
		case e := <-sub.Events():
			if data, ok := e.Data.(events.SessionBusy); ok && !data.Busy {
				idle = true
			}
		case <-deadline:
			t.Fatal("no busy=false frame after the last turn settled")
		}
	}
}

// TestQueuedPromptIsEchoedToo: a prompt waiting behind a running turn is still a
// message the reader wrote, and the queue strip deliberately carries no text.
func TestQueuedPromptIsEchoedToo(t *testing.T) {
	h := newFakeHarness()
	s, bus := newScheduler(t, h, turns.Options{QueueDepth: 4})
	sub, _ := bus.Subscribe(0, nil)
	defer sub.Close()

	if _, err := s.Submit(context.Background(), "session-1", []harness.PromptBlock{{Type: "text", Text: "first"}}); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	first := h.nextStart(t)
	if _, err := s.Submit(context.Background(), "session-1", []harness.PromptBlock{{Type: "text", Text: "queued behind it"}}); err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	first.finish()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-sub.Events():
			data, ok := e.Data.(events.MessageData)
			if !ok || data.Role != "user" {
				continue
			}
			if data.Text == "queued behind it" {
				return
			}
		case <-deadline:
			t.Fatal("the queued prompt was never echoed")
		}
	}
}
