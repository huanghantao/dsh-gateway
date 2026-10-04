package approvals_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/approvals"
	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

/* --------------------------------------------------------------- harness */

// clock is a hand-wound clock, so a grant's expiry is a thing a test asserts
// rather than waits for.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// bench is a broker plus the bus it publishes on, wired the way production does.
type bench struct {
	broker *approvals.Broker
	bus    *events.Bus
	clock  *clock
	sub    *events.Subscription
}

func newBench(t *testing.T, grantTTL time.Duration) *bench {
	t.Helper()
	bus := events.New(events.Config{Replay: 128, Queue: 128})
	clk := newClock()
	broker := approvals.New(approvals.Options{
		Timeout:  5 * time.Minute,
		GrantTTL: grantTTL,
		Bus:      bus,
		Logger:   logx.Discard(),
		Now:      clk.now,
	})
	t.Cleanup(broker.Close)
	sub := bus.Subscribe(events.Cursor{Generation: bus.Generation()}, nil).Subscription
	t.Cleanup(sub.Close)
	return &bench{broker: broker, bus: bus, clock: clk, sub: sub}
}

// drain returns every event published so far.
func (b *bench) drain() []events.Event {
	out := []events.Event{}
	for {
		select {
		case e := <-b.sub.Events():
			out = append(out, e)
		default:
			return out
		}
	}
}

// eventsDuring collects everything published in a short window.
//
// It returns the set rather than answering one question, because the assertions
// that matter here are about what did and did not happen together, and a helper
// that consumed the stream looking for one type would eat the evidence for the
// other.
func (b *bench) eventsDuring(within time.Duration) map[events.Type]int {
	seen := map[events.Type]int{}
	deadline := time.After(within)
	for {
		select {
		case e := <-b.sub.Events():
			seen[e.Type]++
		case <-deadline:
			return seen
		}
	}
}

// requestID is the id a request is announced under, derived so a test can name
// it without threading a value through.
func requestID(sessionID, tool, input string) string {
	return "apr_" + sessionID + "_" + tool + "_" + input
}

// request is one in-flight approval the test can observe.
type request struct {
	decision chan harness.PermissionDecision
	failure  chan error
}

// ask starts a permission request, as the harness adapter does.
func (b *bench) ask(sessionID, tool, input string, opts ...harness.PermissionOption) *request {
	if len(opts) == 0 {
		opts = []harness.PermissionOption{
			{ID: harness.OptionAllowOnce, Name: "Allow once", Kind: "allow_once"},
			{ID: harness.OptionRejectOnce, Name: "Reject", Kind: "reject_once"},
		}
	}
	r := &request{
		decision: make(chan harness.PermissionDecision, 1),
		failure:  make(chan error, 1),
	}
	go func() {
		d, err := b.broker.RequestPermission(context.Background(), harness.PermissionRequest{
			ID:          requestID(sessionID, tool, input),
			SessionID:   sessionID,
			ToolCallID:  "call_" + tool,
			Tool:        tool,
			Input:       input,
			RequestedAt: b.clock.now(),
			ExpiresAt:   b.clock.now().Add(5 * time.Minute),
			Options:     opts,
		})
		if err != nil {
			r.failure <- err
			return
		}
		r.decision <- d
	}()
	return r
}

// wait blocks for the request to settle.
func (r *request) wait(t *testing.T) (harness.PermissionDecision, error) {
	t.Helper()
	select {
	case d := <-r.decision:
		return d, nil
	case err := <-r.failure:
		return harness.PermissionDecision{}, err
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the approval to settle")
		return harness.PermissionDecision{}, nil
	}
}

// pending waits until the request has been announced and returns its view.
func (b *bench) pending(t *testing.T, id string) approvals.View {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if view, ok := b.broker.Get(id); ok {
			return view
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("approval %q was never announced", id)
	return approvals.View{}
}

/* ----------------------------------------------------------------- tests */

// TestAllowOnceStillWorks is the baseline every other behaviour is measured
// against: a person looks at a prompt and says yes.
func TestAllowOnceStillWorks(t *testing.T) {
	b := newBench(t, time.Minute)
	b.ask("session-1", "bash", `{"command":"ls"}`)

	view := b.pending(t, requestID("session-1", "bash", `{"command":"ls"}`))
	if view.Tool != "bash" {
		t.Errorf("tool = %q, want bash", view.Tool)
	}
	if err := b.broker.Decide(context.Background(), view.ID, harness.OptionAllowOnce, "dev_test"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
}

// TestUnknownOptionIsRefusedAndKeepsThePromptOpen: a client inventing a choice
// must not be able to resolve a request, and must not be able to close it either
// by mistake.
func TestUnknownOptionIsRefusedAndKeepsThePromptOpen(t *testing.T) {
	b := newBench(t, time.Minute)
	r := b.ask("session-1", "bash", "{}")
	view := b.pending(t, requestID("session-1", "bash", "{}"))

	err := b.broker.Decide(context.Background(), view.ID, "allow-everything", "dev_test")
	if code := errx.CodeOf(err); code != "unknown_option" {
		t.Fatalf("code = %q, want unknown_option", code)
	}
	if _, ok := b.broker.Get(view.ID); !ok {
		t.Error("a rejected decision closed the request; the operator can no longer answer it")
	}
	if err := b.broker.Decide(context.Background(), view.ID, harness.OptionRejectOnce, "dev_test"); err != nil {
		t.Fatalf("Decide after a rejected one: %v", err)
	}
	<-r.decision
}

// TestDecideOnAClosedApprovalIsAConflict covers the double tap.
func TestDecideOnAClosedApprovalIsAConflict(t *testing.T) {
	b := newBench(t, time.Minute)
	b.ask("session-1", "bash", "{}")
	view := b.pending(t, requestID("session-1", "bash", "{}"))

	if err := b.broker.Decide(context.Background(), view.ID, harness.OptionAllowOnce, "dev"); err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	err := b.broker.Decide(context.Background(), view.ID, harness.OptionAllowOnce, "dev")
	if code := errx.CodeOf(err); code != "approval_closed" {
		t.Fatalf("code = %q, want approval_closed", code)
	}
}

// TestExpiryRefusesAndSaysSo is the fail-closed property.
func TestExpiryRefusesAndSaysSo(t *testing.T) {
	bus := events.New(events.Config{Replay: 64, Queue: 64})
	broker := approvals.New(approvals.Options{
		Timeout: 20 * time.Millisecond, Bus: bus, Logger: logx.Discard(),
	})
	defer broker.Close()
	sub := bus.Subscribe(events.Cursor{Generation: bus.Generation()}, nil).Subscription
	defer sub.Close()

	// The wait is bounded by the caller's context, which is how the ACP adapter
	// applies session.approvalTimeout in-process. The broker's own deadline is
	// the second, independent bound, and it is what covers the relayed path; see
	// the test below.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := broker.RequestPermission(ctx, harness.PermissionRequest{
			ID: "apr_x", SessionID: "session-1", Tool: "bash", Input: "{}",
		})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) && errx.CodeOf(err) != "approval_timeout" {
			t.Fatalf("err = %v, want a timeout refusal", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the request never expired")
	}

	// The refusal is announced, so every client dismisses the sheet and the
	// operator learns the tool did not run.
	var resolved *events.ApprovalDecision
	for len(sub.Events()) > 0 {
		e := <-sub.Events()
		if e.Type != events.TypeApprovalResolved {
			continue
		}
		if decision, ok := e.Data.(events.ApprovalDecision); ok {
			resolved = &decision
		}
	}
	if resolved == nil {
		t.Fatal("an expired approval published no resolution")
	}
	if resolved.DecidedBy != "timeout" || resolved.OptionID != harness.OptionRejectOnce {
		t.Errorf("resolution = %+v, want a timeout refusal", resolved)
	}
	if resolved.Tool != "bash" {
		t.Errorf("tool = %q, want the tool that was refused; a client cannot look it up afterwards", resolved.Tool)
	}
}

// TestAnApprovalExpiresOnItsOwnDeadlineWhenTheContextOutlivesIt is the relayed
// path, and the reason the broker cannot lean on its caller.
//
// A permission request that arrives from the agent host is handled under that
// connection's context, which is meant to live for months. Waiting only on the
// context therefore means a prompt nobody answers is never resolved: the card
// stays in every snapshot, no "approval expired" notification is ever sent, and
// the handler goroutine stays parked until the socket drops.
func TestAnApprovalExpiresOnItsOwnDeadlineWhenTheContextOutlivesIt(t *testing.T) {
	bus := events.New(events.Config{Replay: 64, Queue: 64})
	broker := approvals.New(approvals.Options{
		Timeout: 30 * time.Millisecond, Bus: bus, Logger: logx.Discard(),
	})
	defer broker.Close()
	sub := bus.Subscribe(events.Cursor{Generation: bus.Generation()}, nil).Subscription
	defer sub.Close()

	// Deliberately a context that never ends: this stands in for the agent-host
	// connection, and it is the whole point of the test.
	done := make(chan error, 1)
	go func() {
		_, err := broker.RequestPermission(context.Background(), harness.PermissionRequest{
			ID: "apr_relayed", SessionID: "session-1", Tool: "bash", Input: "{}",
		})
		done <- err
	}()

	select {
	case err := <-done:
		if errx.CodeOf(err) != "approval_timeout" {
			t.Fatalf("err = %v (code %q), want an approval_timeout refusal", err, errx.CodeOf(err))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the approval never expired: the broker waited on a context that does not end")
	}

	// The entry must be gone. Leaving it is what puts a dead card in every
	// snapshot for the life of the connection, and what makes tapping it answer
	// 409 from a host that gave up long ago.
	if view, ok := broker.Get("apr_relayed"); ok {
		t.Errorf("the expired approval is still pending: %+v", view)
	}

	var resolved *events.ApprovalDecision
	for len(sub.Events()) > 0 {
		e := <-sub.Events()
		if e.Type != events.TypeApprovalResolved {
			continue
		}
		if decision, ok := e.Data.(events.ApprovalDecision); ok {
			resolved = &decision
		}
	}
	if resolved == nil {
		t.Fatal("an approval that expired on its own deadline published no resolution")
	}
	if resolved.DecidedBy != "timeout" || resolved.OptionID != harness.OptionRejectOnce {
		t.Errorf("resolution = %+v, want a timeout refusal", resolved)
	}
}

// TestScopedOptionsAreOfferedOnlyWhenGrantsAreOn keeps the escape hatch honest.
func TestScopedOptionsAreOfferedOnlyWhenGrantsAreOn(t *testing.T) {
	withGrants := newBench(t, 30*time.Minute)
	withGrants.ask("session-1", "bash", "{}")
	view := withGrants.pending(t, requestID("session-1", "bash", "{}"))
	if !hasGrantOption(view.Options) {
		t.Errorf("options = %+v, want the scoped choices offered", view.Options)
	}

	without := newBench(t, 0)
	without.ask("session-1", "bash", "{}")
	plain := without.pending(t, requestID("session-1", "bash", "{}"))
	if hasGrantOption(plain.Options) {
		t.Errorf("options = %+v, want only the harness's own choices when grants are off", plain.Options)
	}
}

// TestScopedOptionsAreNotOfferedWithoutAnAffirmativeChoice: the gateway must not
// offer an operator something it cannot deliver.
func TestScopedOptionsAreNotOfferedWithoutAnAffirmativeChoice(t *testing.T) {
	b := newBench(t, 30*time.Minute)
	b.ask("session-1", "bash", "{}", harness.PermissionOption{
		ID: harness.OptionRejectOnce, Name: "Reject", Kind: "reject_once",
	})
	view := b.pending(t, requestID("session-1", "bash", "{}"))
	if hasGrantOption(view.Options) {
		t.Errorf("options = %+v, want no scoped choice on a request that cannot be allowed", view.Options)
	}
}

// hasGrantOption reports whether the synthesised choices are present.
func hasGrantOption(options []approvals.Option) bool {
	for _, o := range options {
		if o.Grant {
			return true
		}
	}
	return false
}

// TestToolGrantAnswersLaterRequests is the feature: answer once, and stop being
// asked the same question.
func TestToolGrantAnswersLaterRequests(t *testing.T) {
	b := newBench(t, 30*time.Minute)
	b.ask("session-1", "bash", `{"command":"go test ./..."}`)
	view := b.pending(t, requestID("session-1", "bash", `{"command":"go test ./..."}`))

	if err := b.broker.Decide(context.Background(), view.ID, approvals.OptionAllowSessionTool, "dev"); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	grants := b.broker.Grants()
	if len(grants) != 1 {
		t.Fatalf("grants = %+v, want one recorded", grants)
	}
	if grants[0].Scope != approvals.ScopeTool || grants[0].Tool != "bash" {
		t.Errorf("grant = %+v, want a tool-scoped grant for bash", grants[0])
	}

	// Everything published so far is drained, so what follows is only what the
	// second request produces.
	b.drain()

	// A different invocation of the same tool, in the same session, does not ask.
	r := b.ask("session-1", "bash", `{"command":"rm -rf build"}`)
	decision, err := r.wait(t)
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	if decision.OptionID != harness.OptionAllowOnce || decision.DecidedBy != "grant" {
		t.Errorf("decision = %+v, want an allow answered by the grant", decision)
	}
	seen := b.eventsDuring(200 * time.Millisecond)
	if seen[events.TypeApprovalRequested] != 0 {
		t.Error("a request was announced for something already authorised")
	}
	if seen[events.TypeApprovalGranted] == 0 {
		t.Error("no approval.granted event: a client cannot tell an automated yes from a person's")
	}
}

// TestToolGrantIsScopedToItsSessionAndTool pins the two axes that make a grant a
// scope rather than a switch.
func TestToolGrantIsScopedToItsSessionAndTool(t *testing.T) {
	b := newBench(t, 30*time.Minute)
	b.ask("session-1", "bash", "{}")
	view := b.pending(t, requestID("session-1", "bash", "{}"))
	if err := b.broker.Decide(context.Background(), view.ID, approvals.OptionAllowSessionTool, "dev"); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// Another tool in the same session still asks.
	b.ask("session-1", "write", "{}")
	other := b.pending(t, requestID("session-1", "write", "{}"))
	if err := b.broker.Decide(context.Background(), other.ID, harness.OptionRejectOnce, "dev"); err != nil {
		t.Fatalf("Decide other tool: %v", err)
	}

	// The same tool in another session still asks. This is the axis that matters
	// most: a grant made while looking at one conversation must not answer for
	// another.
	b.ask("session-2", "bash", "{}")
	elsewhere := b.pending(t, requestID("session-2", "bash", "{}"))
	if err := b.broker.Decide(context.Background(), elsewhere.ID, harness.OptionRejectOnce, "dev"); err != nil {
		t.Fatalf("Decide in another session: %v", err)
	}
}

// TestExactGrantMatchesOnlyTheInvocationItWasGiven is the narrow scope, and the
// one an operator reaches for on a dangerous command.
func TestExactGrantMatchesOnlyTheInvocationItWasGiven(t *testing.T) {
	b := newBench(t, 30*time.Minute)
	b.ask("session-1", "bash", `{"command":"rm -rf build"}`)
	view := b.pending(t, requestID("session-1", "bash", `{"command":"rm -rf build"}`))

	if err := b.broker.Decide(context.Background(), view.ID, approvals.OptionAllowExact, "dev"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	grants := b.broker.Grants()
	if len(grants) != 1 || grants[0].Scope != approvals.ScopeExact {
		t.Fatalf("grants = %+v, want one exact grant", grants)
	}
	if grants[0].Summary == "" {
		t.Error("an exact grant carries no summary; a list cannot say what was agreed to")
	}

	// The same command again is covered.
	same := b.ask("session-1", "bash", `{"command":"rm -rf build"}`)
	if _, err := same.wait(t); err != nil {
		t.Fatalf("the same command was not covered: %v", err)
	}

	// A different command is not — and this is the whole reason to prefer the
	// exact scope on a destructive one.
	b.ask("session-1", "bash", `{"command":"rm -rf /"}`)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := b.broker.Get(requestID("session-1", "bash", `{"command":"rm -rf /"}`)); ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("a different command was auto-approved by an exact grant")
}

// TestGrantExpiresWithItsTTL is the bound that keeps a decision from becoming a
// setting.
func TestGrantExpiresWithItsTTL(t *testing.T) {
	b := newBench(t, 30*time.Minute)
	b.ask("session-1", "bash", "{}")
	view := b.pending(t, requestID("session-1", "bash", "{}"))
	if err := b.broker.Decide(context.Background(), view.ID, approvals.OptionAllowSessionTool, "dev"); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	b.clock.advance(31 * time.Minute)

	if grants := b.broker.Grants(); len(grants) != 0 {
		t.Errorf("grants = %+v, want none after the TTL", grants)
	}
	// And a new request asks again rather than being answered from a dead rule.
	b.ask("session-1", "bash", "{}")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := b.broker.Get(requestID("session-1", "bash", "{}")); ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("an expired grant still answered a request")
}

// TestRevokedGrantAsksAgain is what makes revocation a control rather than a
// tidy-up.
func TestRevokedGrantAsksAgain(t *testing.T) {
	b := newBench(t, 30*time.Minute)
	b.ask("session-1", "bash", "{}")
	view := b.pending(t, requestID("session-1", "bash", "{}"))
	if err := b.broker.Decide(context.Background(), view.ID, approvals.OptionAllowSessionTool, "dev"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	grantID := b.broker.Grants()[0].ID

	if !b.broker.RevokeGrant(grantID) {
		t.Fatal("RevokeGrant reported nothing to revoke")
	}
	if b.broker.RevokeGrant(grantID) {
		t.Error("RevokeGrant reported revoking an already-revoked grant")
	}

	b.ask("session-1", "bash", "{}")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := b.broker.Get(requestID("session-1", "bash", "{}")); ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("a revoked grant still answered a request")
}

// TestGrantsDieWithTheirSession: an authorisation for work that is over must not
// apply to whatever runs in that session next.
func TestGrantsDieWithTheirSession(t *testing.T) {
	b := newBench(t, 30*time.Minute)
	b.ask("session-1", "bash", "{}")
	first := b.pending(t, requestID("session-1", "bash", "{}"))
	if err := b.broker.Decide(context.Background(), first.ID, approvals.OptionAllowSessionTool, "dev"); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if removed := b.broker.RevokeSessionGrants("session-1"); removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if grants := b.broker.Grants(); len(grants) != 0 {
		t.Errorf("grants = %+v, want none", grants)
	}
}

// TestCloseRefusesWhatIsPending is the shutdown path: no tool is left waiting on
// a gateway that is going away.
func TestCloseRefusesWhatIsPending(t *testing.T) {
	b := newBench(t, time.Minute)
	r := b.ask("session-1", "bash", "{}")
	b.pending(t, requestID("session-1", "bash", "{}"))

	b.broker.Close()

	decision, err := r.wait(t)
	if err != nil {
		t.Fatalf("a closed broker returned an error rather than a refusal: %v", err)
	}
	if decision.OptionID != harness.OptionRejectOnce {
		t.Errorf("decision = %+v, want a refusal", decision)
	}
}
