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
// What it must never say is that a delegated child finished. A session can run
// several agents, but only one of them is the thing the operator is waiting for:
// the child settles inside the parent's turn, and the parent keeps working
// afterwards, so announcing the child is an interruption that reports "done"
// over work that is not. The delegation survives as a count in the parent's
// summary, which is what these tests pin.

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
	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{TurnID: "turn-1", State: "running", StartedAt: &longStart})

	toolStarted(bus, "session-1", "call_ok", "subagent", `{"description":"Research dependency versions","prompt":"Find the current versions of X, Y and Z."}`)
	toolEnded(bus, "session-1", "call_ok", "", "X 1.2, Y 3.4, Z 5.6", false)
	toolStarted(bus, "session-1", "call_bad", "subagent_fork", `{"description":"Audit the handlers"}`)
	toolEnded(bus, "session-1", "call_bad", "", "tool call aborted: permission denied for /etc/hosts", true)

	// The turn ends, which is the first thing worth a notification — and the
	// ordering above makes it the proof that the two settlements were handled
	// and produced nothing.
	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{TurnID: "turn-1", State: "completed", StartedAt: &longStart})

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
	// And the children are still not named by what they were asked to do: that
	// description was only ever carried by a notification about the child.
	for _, field := range []string{message.Title, message.Body, message.Summary} {
		for _, leaked := range []string{"Research dependency versions", "Audit the handlers", "permission denied"} {
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

// TestTheTurnSummaryCountsWhatTheTurnActuallyDid is the "what did it do?" half
// for a main-agent turn: the numbers come from the calls that settled during it,
// and a failure among them is counted rather than hidden.
func TestTheTurnSummaryCountsWhatTheTurnActuallyDid(t *testing.T) {
	bus, rec := notifierFor(t, push.NotifierOptions{Threshold: time.Nanosecond})

	longStart := time.Now().Add(-10 * time.Minute)
	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{TurnID: "turn-1", State: "running", StartedAt: &longStart})

	toolStarted(bus, "session-1", "call_1", "bash", `{"command":"go build ./..."}`)
	toolExited(bus, "session-1", "call_1", "bash", 0)
	// A command that exited non-zero is not an error to the harness — it is a
	// status — so a summary built from `isError` alone would call this clean.
	toolStarted(bus, "session-1", "call_2", "bash", `{"command":"go test ./..."}`)
	toolExited(bus, "session-1", "call_2", "bash", 1)
	toolStarted(bus, "session-1", "call_3", "write", `{"file_path":"notes.md"}`)
	toolEnded(bus, "session-1", "call_3", "", "written", false)

	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{TurnID: "turn-1", State: "completed", StartedAt: &longStart})

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

// TestACallFromAPreviousTurnIsNotCounted guards the ledger's one failure mode:
// a tool frame that arrives after its turn settled — the ACP bridge and the log
// watcher do not interleave perfectly — must not be reported as the next turn's
// work.
func TestACallFromAPreviousTurnIsNotCounted(t *testing.T) {
	bus, rec := notifierFor(t, push.NotifierOptions{Threshold: time.Nanosecond})

	longStart := time.Now().Add(-10 * time.Minute)
	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{TurnID: "turn-1", State: "running", StartedAt: &longStart})
	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{TurnID: "turn-1", State: "completed", StartedAt: &longStart})

	// The straggler: a call that belongs to the turn that just ended.
	toolStarted(bus, "session-1", "call_late", "bash", `{"command":"echo late"}`)
	toolEnded(bus, "session-1", "call_late", "", "[exit code: 0]", false)

	waitFor(t, func() bool { return len(rec.received()) >= 1 }, "the settled turn is still reported")
	first := rec.received()[0]
	if strings.Contains(first.Body, "tool call") {
		t.Errorf("body = %q, want no work counted: the turn had already settled", first.Body)
	}
}
