package push_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/config"
	"github.com/huanghantao/dsh-gateway/internal/push"
	"github.com/huanghantao/dsh-gateway/internal/toolresult"
)

// limitsForTests is wide enough that nothing a test sends is truncated: these
// tests are about what a notification says, not about the byte budgets.
var limitsForTests = config.Limits{ToolInputBytes: 4096, ToolOutputBytes: 4096}

// These tests cover what a notification says, and one thing it must never say.
//
// The vocabulary is three facts — who settled, how it ended, what the work
// amounted to — because the failure it was rebuilt to prevent is a lock screen of
// identical "The agent finished" lines that cannot be told apart or acted on.
//
// Where those facts come from has changed and is worth stating plainly, because
// these tests are the boundary of it: a settlement states its own record (see
// events.TurnRecord), so the notifier no longer folds tool frames to count, nor
// keeps messages to quote. The counting is tested where it happens — the
// projection's in internal/sessionlog, the update stream's in
// internal/app/bridge, the arithmetic in internal/app/events — and what is left
// here is what a notification renders and what it must never render.
//
// The one thing it must never say is that a delegated child finished. A session
// can run several agents, but only one of them is the thing the operator is
// waiting for: the child settles inside the parent's turn, and the parent keeps
// working afterwards, so announcing the child is an interruption that reports
// "done" over work that is not. The delegation survives as a count in the
// parent's summary, which is what these tests pin.

// oneNotification waits for the single message a test expects, and fails if more
// than one ever arrived.
//
// It is the barrier the silence assertions need. The bus hands this notifier its
// frames in order and a delivery happens inside the handler, so by the time a
// later notification is received every earlier frame has already been dealt with
// — which is what turns "the child was not announced" into a fact rather than a
// race against the goroutine.
func oneNotification(t *testing.T, rec *receiver, what string) push.Message {
	t.Helper()
	waitFor(t, func() bool { return len(rec.received()) >= 1 }, what)
	if got := len(rec.received()); got != 1 {
		t.Fatalf("received %d notifications, want exactly 1 (%s): %+v", got, what, rec.received())
	}
	return rec.received()[0]
}

// TestADelegationIsCountedButNeverAnnounced is the policy in one test.
//
// Two children settle while the turn that handed out the work is still running —
// one that finished and one that failed. Neither is news: the operator asked for
// the turn, not for the child, and the child failing does not end it. What is
// news is the turn, and by then the delegations are part of what it says it did.
func TestADelegationIsCountedButNeverAnnounced(t *testing.T) {
	// A threshold of a nanosecond means time is never the reason a message was
	// withheld, so what is asserted here is the policy and not the threshold.
	bus, rec := notifierFor(t, push.NotifierOptions{Threshold: time.Nanosecond})

	longStart := time.Now().Add(-10 * time.Minute)

	// Two delegated children, each a session of its own with a record of its
	// own. `subagent` is the only thing that says what they are: a child's log,
	// turns and record look exactly like a session a person opened.
	for _, child := range []struct {
		session string
		closing string
	}{
		{"session-child-ok", "X 1.2, Y 3.4, Z 5.6"},
		{"session-child-bad", "tool call aborted: permission denied for /etc/hosts"},
	} {
		bus.Publish(events.TypeTurnState, child.session, events.TurnState{
			TurnID: "turn-child", State: "completed", StartedAt: &longStart,
			Subagent: true, Record: said(child.closing, ""),
		})
	}

	// The turn that handed the work out settles, and its record is where the
	// delegations and the failure are counted — by whoever owned it, not here.
	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{
		TurnID: "turn-1", State: "completed", StartedAt: &longStart,
		Record: events.TurnRecord{Work: events.Work{Calls: 2, Delegations: 2, Failed: 1}},
	})

	message := oneNotification(t, rec, "a notification when the turn that delegated settles")
	if message.Actor == nil || message.Actor.Kind != push.ActorMain {
		t.Errorf("actor = %+v, want the main agent: a child is never what settles a session", message.Actor)
	}
	if !strings.Contains(message.Title, "Main agent") {
		t.Errorf("title = %q, want the main agent named", message.Title)
	}
	// What the turn amounted to: the work it handed out, and the child that
	// failed doing it.
	if !strings.Contains(message.Body, "2 delegations") {
		t.Errorf("body = %q, want the delegations counted", message.Body)
	}
	if !strings.Contains(message.Body, "1 failure") {
		t.Errorf("body = %q, want the failed child counted", message.Body)
	}
	if !strings.Contains(message.Summary, "2 delegations") {
		t.Errorf("summary = %q, want the countable phrase a client can render", message.Summary)
	}
	// And the children are still not named by what they had to say: a child's
	// record belongs to the child's session, and nothing may quote it under the
	// parent's.
	for _, field := range []string{message.Title, message.Body, message.Summary, message.Answer} {
		for _, leaked := range []string{"X 1.2, Y 3.4, Z 5.6", "permission denied"} {
			if strings.Contains(field, leaked) {
				t.Errorf("message = %+v, must not carry the child's own text %q", message, leaked)
			}
		}
	}
}

// toolStarted and toolEnded publish one call's lifecycle the way the bridge and
// the watcher do.
func toolStarted(bus *events.Bus, sessionID, callID, tool, input string) {
	bus.Publish(events.TypeSessionTool, sessionID, events.NewToolData(events.ToolFacts{
		Phase:  events.ToolStarted,
		CallID: callID,
		Tool:   tool,
		Status: events.ToolInProgress,
		Input:  input,
	}, limitsForTests))
}

func toolEnded(bus *events.Bus, sessionID, callID, tool, output string, failed bool) {
	status := events.ToolCompleted
	if failed {
		status = events.ToolFailed
	}
	bus.Publish(events.TypeSessionTool, sessionID, events.NewToolData(events.ToolFacts{
		Phase:   events.ToolEnded,
		CallID:  callID,
		Tool:    tool,
		Status:  status,
		Output:  output,
		IsError: failed,
	}, limitsForTests))
}

// toolExited publishes a settled call whose result carries an exit code, which
// is how DSH reports a command that failed: as a status, not as an error.
func toolExited(bus *events.Bus, sessionID, callID, tool string, code int) {
	bus.Publish(events.TypeSessionTool, sessionID, events.NewToolData(events.ToolFacts{
		Phase:  events.ToolEnded,
		CallID: callID,
		Tool:   tool,
		Status: events.ToolCompleted,
		Output: "[exit code: " + strconv.Itoa(code) + "]",
		Facts:  toolresult.Outcome{ExitCode: code, HasExitCode: true}.Facts(),
	}, limitsForTests))
}

// TestTheTurnSummaryRendersWhatTheTurnReported is the "what did it do?" half for
// a main-agent turn: the numbers come from the settlement's own record, and a
// failure among them is shown rather than hidden.
func TestTheTurnSummaryRendersWhatTheTurnReported(t *testing.T) {
	bus, rec := notifierFor(t, push.NotifierOptions{Threshold: time.Nanosecond})

	longStart := time.Now().Add(-10 * time.Minute)
	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{
		TurnID: "turn-1", State: "completed", StartedAt: &longStart,
		Record: events.TurnRecord{Work: events.Work{Calls: 3, Edits: 1, Failed: 1}},
	})

	waitFor(t, func() bool { return len(rec.received()) >= 1 }, "a notification for the settled turn")
	message := rec.received()[0]
	for _, want := range []string{"3 tool calls", "1 file changed", "1 failure"} {
		if !strings.Contains(message.Body, want) {
			t.Errorf("body = %q, want it to contain %q", message.Body, want)
		}
	}
	if !strings.Contains(message.Summary, "3 tool calls") {
		t.Errorf("summary = %q, want the countable phrase a client can render", message.Summary)
	}
}
