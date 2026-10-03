package agenthost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/agenthost/hostwire"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

/* --------------------------------------------------------------- test double */

// stubHarness is a child that runs turns on command, without a model.
//
// It exists so that the property this whole package is for — a turn survives the
// death of the process that asked for it — can be tested rather than argued
// about. A property that can only be exercised against a real model is a
// property that will not be exercised.
type stubHarness struct {
	mu       sync.Mutex
	state    harness.State
	sessions map[string]harness.Session
	// release, when set, is closed by Prompt to end a turn on demand. It is how
	// a test holds a turn open for as long as it needs to.
	hold   chan struct{}
	onTurn func(sessionID string)

	released []string
	closed   []string
	cancels  int
	// resumes records every attach the child was asked for, so a test can prove
	// the host answered from a handle it already had instead of asking again.
	resumes []string
	// releaseErr, when set, makes ReleaseSession fail the way DSH's adapter does:
	// it cannot detach a session, so the handle stays where it is.
	releaseErr error
}

func newStubHarness() *stubHarness {
	return &stubHarness{state: harness.StateReady, sessions: map[string]harness.Session{}}
}

func (s *stubHarness) Start(context.Context) error { return nil }
func (s *stubHarness) Close(context.Context) error { return nil }
func (s *stubHarness) State() harness.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *stubHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{ProtocolVersion: 1, AgentName: "stub", CanResume: true, CanList: true}
}

func (s *stubHarness) ListSessions(context.Context, string, string) (harness.SessionPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	page := harness.SessionPage{}
	for _, sess := range s.sessions {
		page.Sessions = append(page.Sessions, sess.Info)
	}
	return page, nil
}

func (s *stubHarness) NewSession(_ context.Context, workspace string) (harness.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := harness.Session{Info: harness.SessionInfo{ID: "session-new", Workspace: workspace}}
	s.sessions[sess.Info.ID] = sess
	return sess, nil
}

func (s *stubHarness) ResumeSession(_ context.Context, sessionID, workspace string) (harness.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resumes = append(s.resumes, sessionID)
	sess := harness.Session{Info: harness.SessionInfo{ID: sessionID, Workspace: workspace}}
	s.sessions[sessionID] = sess
	return sess, nil
}

func (s *stubHarness) ReleaseSession(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, sessionID)
	return s.releaseErr
}

func (s *stubHarness) CloseSession(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = append(s.closed, sessionID)
	delete(s.sessions, sessionID)
	return nil
}

func (s *stubHarness) SetConfigOption(context.Context, string, string, string) ([]harness.ConfigOption, error) {
	return nil, nil
}

func (s *stubHarness) Cancel(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels++
	return nil
}

// Prompt runs one turn. It blocks until hold is closed, or until the context
// ends, which is what lets a test keep a turn in flight across a disconnect.
func (s *stubHarness) Prompt(ctx context.Context, sessionID string, blocks []harness.PromptBlock) (string, error) {
	s.mu.Lock()
	hold := s.hold
	notify := s.onTurn
	s.mu.Unlock()

	if notify != nil {
		notify(sessionID)
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return "cancelled", ctx.Err()
		}
	}
	return "end_turn", nil
}

/* ------------------------------------------------------------------- fixture */

// pair is a host and a client wired to each other over an in-memory socket. No
// unix socket is involved, so the tests are about the protocol and the lifetime
// rules rather than about the filesystem.
type pair struct {
	server *Server
	child  *stubHarness
	srvEnd *hostwire.Conn
	client *Client
	// clientEvents collects what the gateway would have published.
	mu       sync.Mutex
	updates  []harness.Update
	states   []harness.State
	approver *stubApprover
}

// stubApprover records permission requests and answers them on command.
//
// It signals when a request is *parked* — past the recording point and blocked
// on the answer channel — because that is the moment an answer stops being able
// to fall into the gap between the two. A test that answers on "recorded" is
// racing, and the race is invisible until it fails once in twenty runs.
type stubApprover struct {
	mu       sync.Mutex
	requests []harness.PermissionRequest
	parked   chan struct{}
	answer   chan harness.PermissionDecision
}

func newStubApprover() *stubApprover {
	return &stubApprover{
		parked: make(chan struct{}, 8),
		answer: make(chan harness.PermissionDecision, 4),
	}
}

func (a *stubApprover) RequestPermission(ctx context.Context, req harness.PermissionRequest) (harness.PermissionDecision, error) {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()
	a.parked <- struct{}{}
	select {
	case d := <-a.answer:
		return d, nil
	case <-ctx.Done():
		return harness.PermissionDecision{}, ctx.Err()
	}
}

// awaitParked waits until one request has been recorded and is waiting.
func (a *stubApprover) awaitParked(t *testing.T) {
	t.Helper()
	select {
	case <-a.parked:
	case <-time.After(5 * time.Second):
		t.Fatal("no permission request reached the gateway")
	}
}

func (a *stubApprover) seen() []harness.PermissionRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]harness.PermissionRequest(nil), a.requests...)
}

type collectingSink struct{ p *pair }

func (c collectingSink) Publish(u harness.Update) {
	c.p.mu.Lock()
	c.p.updates = append(c.p.updates, u)
	c.p.mu.Unlock()
}

func (c collectingSink) PublishState(state harness.State, _ string) {
	c.p.mu.Lock()
	c.p.states = append(c.p.states, state)
	c.p.mu.Unlock()
}

// newPair builds a host and a client, connected, with the client started.
func newPair(t *testing.T) *pair {
	t.Helper()

	serverEnd, clientEnd := net.Pipe()
	child := newStubHarness()
	approver := newStubApprover()

	srv, err := New(Options{
		// A factory rather than a value: the child reports to the server, so it
		// cannot exist before the server does.
		NewHarness: func(harness.UpdateSink, harness.PermissionHandler, harness.StateSink) (harness.Harness, error) {
			return child, nil
		},
		Logger:            logx.Discard(),
		TurnTimeout:       time.Minute,
		PermissionTimeout: 5 * time.Second,
		DrainTimeout:      time.Second,
	})
	if err != nil {
		t.Fatalf("agenthost.New: %v", err)
	}

	p := &pair{server: srv, child: child, approver: approver}
	p.srvEnd = hostwire.NewConn(serverEnd)
	go srv.Serve(context.Background(), p.srvEnd)

	client := NewClient(ClientOptions{
		Dial: func(context.Context) (net.Conn, error) { return clientEnd, nil },
		// One attempt: the fixture is a pipe, and a reconnect test constructs
		// its own pair rather than relying on a redial that cannot succeed.
		ReconnectBackoff:    time.Hour,
		MaxReconnectBackoff: time.Hour,
		Logger:              logx.Discard(),
		Updates:             collectingSink{p},
		StateSink:           collectingSink{p},
		Permissions:         approver,
		InstanceID:          "test-gateway",
	})
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	p.client = client
	t.Cleanup(func() {
		_ = client.Close(context.Background())
		_ = p.srvEnd.Close()
		_ = srv.Close(context.Background())
	})
	return p
}

/* --------------------------------------------------------------------- tests */

// TestATurnSurvivesTheGatewayThatAskedForIt is the test this package exists for.
//
// The gateway submits a prompt and then dies — the socket to the host is torn
// down, which is exactly what a redeploy does to it. The turn must keep running:
// not merely be remembered, but actually continue, because that is the difference
// between a redeploy that costs a conversation and one that costs nothing.
func TestATurnSurvivesTheGatewayThatAskedForIt(t *testing.T) {
	serverEnd, clientEnd := net.Pipe()
	child := newStubHarness()
	hold := make(chan struct{})
	started := make(chan string, 1)
	child.hold = hold
	child.onTurn = func(sessionID string) { started <- sessionID }

	srv := newServerForTest(t, child)
	srvEnd := hostwire.NewConn(serverEnd)
	go srv.Serve(context.Background(), srvEnd)

	// A first gateway: attach, submit, then walk away mid-turn.
	first := newClientForTest(t, clientEnd, newStubApprover())
	if _, err := first.ResumeSession(context.Background(), "session-a", "/w"); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}

	// The caller's own view is not what is under test — a process that dies has
	// no view. What is under test is whether the *work* continues, so the
	// assertion is made against the child and against a fresh connection.
	prompted := make(chan struct{})
	go func() {
		defer close(prompted)
		_, _ = first.Prompt(context.Background(), "session-a",
			[]harness.PromptBlock{{Type: "text", Text: "do the thing"}})
	}()

	select {
	case sessionID := <-started:
		if sessionID != "session-a" {
			t.Fatalf("turn started for %q", sessionID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never started")
	}

	// The redeploy: the gateway process goes away.
	_ = first.Close(context.Background())
	if err := srvEnd.Close(); err != nil {
		t.Fatalf("close host side: %v", err)
	}

	// A new gateway arrives while the turn is still in flight, and finds it
	// running. That is the guarantee, stated in the only way that cannot be
	// faked: a process that did not submit the turn can see that it is alive.
	secondEnd, secondClientEnd := net.Pipe()
	go srv.Serve(context.Background(), hostwire.NewConn(secondEnd))
	second := newClientForTest(t, secondClientEnd, newStubApprover())

	snap, err := second.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot after the first gateway died: %v", err)
	}
	if len(snap.Sessions) != 1 || snap.Sessions[0].Turn == nil {
		t.Fatal("the turn did not survive the death of the gateway that submitted it; " +
			"this is the failure the agent host exists to prevent")
	}

	// And it is a real turn, not a tombstone: it finishes on its own terms. The
	// first gateway's own call returns an error when its socket dies — it has
	// nobody to report to — which is why the assertion here is about the host's
	// state and not about that caller's.
	close(hold)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if after, err := second.Snapshot(context.Background()); err == nil {
			if len(after.Sessions) == 1 && after.Sessions[0].Turn == nil {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the turn never settled after being released")
}

// TestAFreshGatewayLearnsThatATurnIsRunning covers the other half of the
// redeploy: the new process has to be told what it missed, or the phone shows an
// idle session that is not idle.
func TestAFreshGatewayLearnsThatATurnIsRunning(t *testing.T) {
	serverEnd, clientEnd := net.Pipe()
	child := newStubHarness()
	hold := make(chan struct{})
	started := make(chan struct{}, 1)
	child.hold = hold
	child.onTurn = func(string) { started <- struct{}{} }

	srv := newServerForTest(t, child)
	srvEnd := hostwire.NewConn(serverEnd)
	go srv.Serve(context.Background(), srvEnd)

	first := newClientForTest(t, clientEnd, newStubApprover())
	if _, err := first.ResumeSession(context.Background(), "session-a", "/w"); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	go func() {
		_, _ = first.Prompt(context.Background(), "session-a", []harness.PromptBlock{{Type: "text", Text: "hi"}})
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never started")
	}
	_ = first.Close(context.Background())
	_ = srvEnd.Close()

	// A second gateway, on a new connection, asking what is true.
	secondEnd, secondClientEnd := net.Pipe()
	secondSrv := hostwire.NewConn(secondEnd)
	go srv.Serve(context.Background(), secondSrv)

	second := newClientForTest(t, secondClientEnd, newStubApprover())
	snap, err := second.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if len(snap.Sessions) != 1 {
		t.Fatalf("snapshot reports %d sessions, want 1", len(snap.Sessions))
	}
	held := snap.Sessions[0]
	if held.Info.ID != "session-a" {
		t.Errorf("snapshot session = %q, want session-a", held.Info.ID)
	}
	if held.Turn == nil {
		t.Fatal("snapshot says no turn is running, but one is; a fresh gateway would " +
			"show an idle session and let a second prompt be submitted")
	}
	if held.Turn.TurnID == "" {
		t.Error("the running turn has no id, so nothing can follow it to its outcome")
	}
	if held.StartedAt == nil {
		t.Error("the running turn has no start time, so a client cannot render how long it has run")
	}

	close(hold)
}

// TestTheHostRefusesAnOldGatewaysCommand is the fencing rule.
//
// "The newest connection wins" is not enough on its own: a command already
// queued on the connection being replaced can still be delivered after the swap.
// The epoch on every command is what makes that harmless.
func TestTheHostRefusesAnOldGatewaysCommand(t *testing.T) {
	serverEnd, clientEnd := net.Pipe()
	child := newStubHarness()
	srv := newServerForTest(t, child)

	oldConn := hostwire.NewConn(serverEnd)
	go srv.Serve(context.Background(), oldConn)
	old := newClientForTest(t, clientEnd, newStubApprover())
	if _, err := old.ResumeSession(context.Background(), "session-a", "/w"); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}

	// A newer gateway arrives and pre-empts the old connection.
	newEnd, newClientEnd := net.Pipe()
	_ = oldConn // the same server, a second connection
	newConn := hostwire.NewConn(newEnd)
	go srv.Serve(context.Background(), newConn)
	fresh := newClientForTest(t, newClientEnd, newStubApprover())

	// The old connection was closed by the host when it adopted the new one, so
	// its next command must fail rather than act on a host that has moved on.
	err := old.ReleaseSession(context.Background(), "session-a")
	if err == nil {
		t.Fatal("a superseded gateway's command was accepted; a message queued before the " +
			"swap can act on a host that has already moved on")
	}

	// And the new one still works, which is the half that matters more.
	if err := fresh.ReleaseSession(context.Background(), "session-a"); err != nil {
		t.Fatalf("the current gateway was refused: %v", err)
	}
}

// TestReleaseDetachesWithoutEndingTheSession pins the distinction the port had
// to grow: an idle lease must not write an end into the conversation.
func TestReleaseDetachesWithoutEndingTheSession(t *testing.T) {
	p := newPair(t)
	if _, err := p.client.ResumeSession(context.Background(), "session-a", "/w"); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}

	if err := p.client.ReleaseSession(context.Background(), "session-a"); err != nil {
		t.Fatalf("ReleaseSession: %v", err)
	}

	p.child.mu.Lock()
	released, closed := len(p.child.released), len(p.child.closed)
	p.child.mu.Unlock()
	if released != 1 {
		t.Errorf("the child saw %d releases, want 1", released)
	}
	if closed != 0 {
		t.Error("releasing a lease closed the session; DSH writes an end to the session's log " +
			"when that happens, so an idle timeout would mark a conversation finished")
	}
}

// TestApprovalsAreRelayedToTheGatewayAndNeverDecidedByTheHost is the security
// property of the split: the tier that is authenticated to a person is the tier
// that decides.
func TestApprovalsAreRelayedToTheGatewayAndNeverDecidedByTheHost(t *testing.T) {
	p := newPair(t)

	if _, err := p.client.ResumeSession(context.Background(), "session-a", "/w"); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}

	type outcome struct {
		decision harness.PermissionDecision
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		d, err := p.server.RequestPermission(context.Background(), harness.PermissionRequest{
			ID: "call-1", SessionID: "session-a", Tool: "bash", Input: `{"command":"rm -rf /"}`,
		})
		done <- outcome{d, err}
	}()

	// The gateway is asked, which is the whole relay.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(p.approver.seen()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	seen := p.approver.seen()
	if len(seen) != 1 {
		t.Fatalf("the gateway was asked %d times, want 1", len(seen))
	}
	if seen[0].Tool != "bash" {
		t.Errorf("relayed tool = %q, want bash", seen[0].Tool)
	}

	// The request is parked on the gateway, so an answer now has somewhere to
	// land. Without this the test races the append against the block.
	p.approver.awaitParked(t)
	p.approver.answer <- harness.PermissionDecision{OptionID: harness.OptionAllowOnce, DecidedBy: "operator"}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("RequestPermission: %v", got.err)
		}
		if got.decision.OptionID != harness.OptionAllowOnce {
			t.Errorf("decision = %q, want %q", got.decision.OptionID, harness.OptionAllowOnce)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the decision never reached the host")
	}
}

// TestAnUnansweredApprovalIsRefused is the fail-closed rule, tested at the tier
// that now enforces it for a request nobody is connected to answer.
func TestAnUnansweredApprovalIsRefused(t *testing.T) {
	p := newPair(t)
	if _, err := p.client.ResumeSession(context.Background(), "session-a", "/w"); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}

	// The tool asks for approval and nobody ever answers it: the gateway is
	// connected, so the request is relayed, and then ignored — which is the
	// shape of a phone whose screen is off.
	done := make(chan error, 1)
	go func() {
		_, err := p.server.RequestPermission(context.Background(), harness.PermissionRequest{
			ID: "call-1", SessionID: "session-a", Tool: "bash",
			ExpiresAt: time.Now().Add(150 * time.Millisecond),
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an approval with nobody to answer it was allowed; the rule is that " +
				"silence refuses")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an approval with nobody to answer it never resolved")
	}
}

// TestDrainRefusesNewTurnsAndReportsWhatIsLeft pins the deploy protocol at the
// tier that owns it, since a host restart is the only one that can end a turn.
func TestDrainRefusesNewTurnsAndReportsWhatIsLeft(t *testing.T) {
	p := newPair(t)
	if _, err := p.client.ResumeSession(context.Background(), "session-a", "/w"); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}

	status, err := p.client.Drain(context.Background(), "test", false)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !status.Draining {
		t.Error("the host does not report itself as draining after being asked to")
	}

	// A prompt after the drain must be refused, not accepted and then abandoned.
	_, err = p.client.Prompt(context.Background(), "session-a",
		[]harness.PromptBlock{{Type: "text", Text: "hi"}})
	if err == nil {
		t.Fatal("a turn was accepted by a host that is draining")
	}
	if code := codeOf(err); code != "host_draining" {
		t.Errorf("refusal code = %q, want host_draining (got %v)", code, err)
	}
}

// TestInspectingTheHostDoesNotDisplaceTheGateway is the regression test for a
// defect this project shipped and then caught in its own deployment.
//
// `status` and `doctor` both said hello to find out what the host was holding.
// The host treated saying hello as claiming the control role, so asking it a
// question took that role from the running gateway — which lost its connection
// and had to reconnect. A diagnostic that interrupts what it is diagnosing is
// worse than no diagnostic, and the reason it happened is worth keeping: the
// handshake was doing two jobs, and only one of them is about identity.
func TestInspectingTheHostDoesNotDisplaceTheGateway(t *testing.T) {
	serverEnd, clientEnd := net.Pipe()
	child := newStubHarness()
	srv := newServerForTest(t, child)

	go srv.Serve(context.Background(), hostwire.NewConn(serverEnd))
	gateway := newClientForTest(t, clientEnd, newStubApprover())
	if _, err := gateway.ResumeSession(context.Background(), "session-a", "/w"); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}

	// A reader connects, greets, and asks what is true. It must not claim
	// control, and the host must still answer it.
	inspectEnd, inspectClientEnd := net.Pipe()
	go srv.Serve(context.Background(), hostwire.NewConn(inspectEnd))

	inspect := hostwire.NewConn(inspectClientEnd)
	inspect.SetErrorClassifier(hostwire.ErrorOf)
	inspect.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := inspect.Call(ctx, hostwire.MethodHello, hostwire.Hello{
		Protocol: hostwire.Protocol, Caller: "status",
	}, nil); err != nil {
		t.Fatalf("a read-only hello was refused: %v", err)
	}

	var status hostwire.StatusResult
	if err := inspect.Call(ctx, hostwire.MethodStatus, nil, &status); err != nil {
		t.Fatalf("a reader could not read the status: %v", err)
	}
	if status.SessionsHeld != 1 {
		t.Errorf("status reports %d sessions held, want 1", status.SessionsHeld)
	}

	// The gateway is untouched: its connection is still the active one, so a
	// command that needs authority still works.
	if err := gateway.ReleaseSession(ctx, "session-a"); err != nil {
		t.Fatalf("the gateway was displaced by a read-only inspection: %v", err)
	}

	// And the reader cannot act, which is the other half of the rule.
	if err := inspect.Call(ctx, hostwire.MethodSessionRelease,
		hostwire.SessionRef{SessionID: "session-a"}, nil); err == nil {
		t.Error("a read-only connection was allowed to act on the host")
	}
}

/* ------------------------------------------------------------------- helpers */

func newServerForTest(t *testing.T, child harness.Harness) *Server {
	t.Helper()
	srv, err := New(Options{
		NewHarness: func(harness.UpdateSink, harness.PermissionHandler, harness.StateSink) (harness.Harness, error) {
			return child, nil
		},
		Logger:            logx.Discard(),
		TurnTimeout:       time.Minute,
		PermissionTimeout: 3 * time.Second,
		DrainTimeout:      200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agenthost.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	return srv
}

func newClientForTest(t *testing.T, conn net.Conn, approver harness.PermissionHandler) *Client {
	t.Helper()
	client := NewClient(ClientOptions{
		Dial:                func(context.Context) (net.Conn, error) { return conn, nil },
		ReconnectBackoff:    time.Hour,
		MaxReconnectBackoff: time.Hour,
		Logger:              logx.Discard(),
		Permissions:         approver,
		InstanceID:          "test",
	})
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	return client
}

func codeOf(err error) string {
	var wire *hostwire.Error
	if errors.As(err, &wire) {
		return wire.Code
	}
	return fmt.Sprintf("%v", err)
}

// TestClientReportsHostStateToTheGateway keeps the notification path honest: a
// gateway that was never told the child is unwell cannot say so.
func TestClientReportsHostStateToTheGateway(t *testing.T) {
	p := newPair(t)
	p.server.PublishState(harness.StateRestarting, "child exited")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		states := append([]harness.State(nil), p.states...)
		p.mu.Unlock()
		if len(states) > 0 && states[len(states)-1] == harness.StateRestarting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the gateway was never told the child restarted")
}
