package agenthost

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/huanghantao/dsh-gateway/internal/agenthost/hostwire"
)

// TestHeldSessionsOutliveAReleaseThatCannotDetach is the property the phone's
// session list depends on.
//
// DSH permits exactly one active attachment per session and offers no detach, so
// a release — a lease expiring, or an operator handing the session back — leaves
// the handle in the host. That is deliberate: `session/close` would write a
// synthetic end into the session's own log. What must not happen is the gateway
// forgetting the session, because DSH's own listing cannot show a session that is
// live in the child, and the row would then be missing from a list where the
// agent is still working.
func TestHeldSessionsOutliveAReleaseThatCannotDetach(t *testing.T) {
	p := newPair(t)
	ctx := context.Background()

	if _, err := p.client.ResumeSession(ctx, "session-a", "/w"); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	if held := p.client.HeldSessions(); len(held) != 1 || held[0].ID != "session-a" {
		t.Fatalf("held after attach = %+v, want session-a", held)
	}
	if held := p.client.HeldSessions(); held[0].Workspace != "/w" {
		t.Errorf("held workspace = %q, want /w: the host knows the cwd DSH bound the "+
			"session to, and the list should not have to guess it", held[0].Workspace)
	}

	// The child cannot detach, which is what DSH's adapter reports.
	p.child.mu.Lock()
	p.child.releaseErr = errors.New("release_unsupported: this adapter holds its sessions for the life of the process")
	p.child.mu.Unlock()

	if err := p.client.ReleaseSession(ctx, "session-a"); err == nil {
		t.Fatal("the release reported success although the child refused to detach")
	}
	if held := p.client.HeldSessions(); len(held) != 1 {
		t.Fatalf("held after a failed release = %+v, want the session still there: a lease "+
			"ending is not the agent letting go", held)
	}

	// Closing is the deletion path, and it really does let go.
	if err := p.client.CloseSession(ctx, "session-a"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if held := p.client.HeldSessions(); len(held) != 0 {
		t.Errorf("held after close = %+v, want none", held)
	}
}

// TestResumingASessionTheHostHoldsDoesNotAskTheChildAgain covers the other half
// of the same story: a session whose lease was lost has to be attachable again,
// or the row comes back to the phone and cannot be used.
//
// DSH refuses a second attachment ("session is already active"), so a resume that
// reached the child would fail — and the failure would read as "somebody else has
// this session" when the somebody is this gateway's own host.
func TestResumingASessionTheHostHoldsDoesNotAskTheChildAgain(t *testing.T) {
	p := newPair(t)
	ctx := context.Background()

	first, err := p.client.ResumeSession(ctx, "session-a", "/w")
	if err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	again, err := p.client.ResumeSession(ctx, "session-a", "/w")
	if err != nil {
		t.Fatalf("re-attaching a session the host holds failed: %v", err)
	}
	if again.Info.ID != first.Info.ID || again.Info.Workspace != first.Info.Workspace {
		t.Errorf("re-attach answered %+v, want the held handle %+v", again.Info, first.Info)
	}

	p.child.mu.Lock()
	asked := len(p.child.resumes)
	p.child.mu.Unlock()
	if asked != 1 {
		t.Errorf("the child was asked to attach %d time(s), want 1: the second resume "+
			"belongs to the handle the host already has", asked)
	}
}

// TestAFreshGatewayLearnsWhatTheHostHolds is what makes the merge survive a
// redeploy.
//
// The whole point of the agent host is that a gateway restart does not disturb
// the child or its sessions; the sessions stay attached, so `session/list` still
// cannot report them. A gateway that came up with an empty idea of what is held
// would show the operator a list that had quietly lost the conversations the host
// is running.
func TestAFreshGatewayLearnsWhatTheHostHolds(t *testing.T) {
	serverEnd, clientEnd := net.Pipe()
	child := newStubHarness()
	srv := newServerForTest(t, child)
	srvEnd := hostwire.NewConn(serverEnd)
	go srv.Serve(context.Background(), srvEnd)

	first := newClientForTest(t, clientEnd, newStubApprover())
	if _, err := first.ResumeSession(context.Background(), "session-a", "/w"); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	// The gateway is replaced. The child and its sessions are not.
	_ = first.Close(context.Background())
	_ = srvEnd.Close()

	secondEnd, secondClientEnd := net.Pipe()
	go srv.Serve(context.Background(), hostwire.NewConn(secondEnd))
	second := newClientForTest(t, secondClientEnd, newStubApprover())

	held := second.HeldSessions()
	if len(held) != 1 || held[0].ID != "session-a" {
		t.Fatalf("a fresh gateway sees held = %+v, want session-a: without it the session "+
			"is missing from the list and the harness cannot report it", held)
	}
	if held[0].Workspace != "/w" {
		t.Errorf("held workspace = %q, want /w", held[0].Workspace)
	}
}
