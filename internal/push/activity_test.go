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

// These tests cover the question the notification vocabulary was rebuilt to
// answer: **who** finished, and **what** did it amount to.
//
// The failure they exist to prevent is a lock screen with two identical lines on
// it. Three agents can run inside one session — the main agent and however many
// delegations it hands out — and every one of them used to settle as the same
// anonymous "The agent finished", with no way to tell which had reported and
// nothing at all about the work.

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

// TestASubagentAndTheMainAgentReadDifferently is the core property: two
// notifications, one line each, and the lines are not interchangeable.
//
// A delegation names itself — the model wrote the task a description, and the
// notification carries it — while the main agent is named by its role. A reader
// who sees both on a lock screen can tell which is which without opening
// anything.
func TestASubagentAndTheMainAgentReadDifferently(t *testing.T) {
	// A threshold of zero means every settled call is worth a buzz, which is
	// what makes this test deterministic: it is about what the notifications
	// say, not about when they are sent.
	bus, rec := notifierFor(t, push.NotifierOptions{
		Threshold:        time.Nanosecond,
		IncludeTaskNames: true,
	})

	// One delegation, named by its own description.
	toolStarted(bus, "session-1", "call_task", "subagent", `{"description":"Research dependency versions","prompt":"Find the current versions of X, Y and Z."}`)
	toolEnded(bus, "session-1", "call_task", "", "X 1.2, Y 3.4, Z 5.6", false)

	waitFor(t, func() bool { return len(rec.received()) >= 1 }, "a notification when the child settles")
	child := rec.received()[0]

	// Then the main agent's turn, which did some work of its own.
	longStart := time.Now().Add(-10 * time.Minute)
	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{TurnID: "turn-1", State: "running", StartedAt: &longStart})
	toolStarted(bus, "session-1", "call_bash", "bash", `{"command":"go test ./..."}`)
	toolEnded(bus, "session-1", "call_bash", "", "[exit code: 0]", false)
	toolStarted(bus, "session-1", "call_edit", "edit", `{"file_path":"internal/push/activity.go"}`)
	toolEnded(bus, "session-1", "call_edit", "", "ok", false)
	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{TurnID: "turn-1", State: "completed", StartedAt: &longStart})

	waitFor(t, func() bool { return len(rec.received()) >= 2 }, "a notification when the turn settles")
	parent := rec.received()[1]

	// Which agent: stated on both, and different.
	if child.Actor == nil || child.Actor.Kind != push.ActorSub {
		t.Fatalf("child actor = %+v, want a subagent", child.Actor)
	}
	if parent.Actor == nil || parent.Actor.Kind != push.ActorMain {
		t.Fatalf("parent actor = %+v, want the main agent", parent.Actor)
	}
	if !strings.Contains(child.Title, "Subagent") {
		t.Errorf("child title = %q, want the actor named", child.Title)
	}
	if !strings.Contains(parent.Title, "Main agent") {
		t.Errorf("parent title = %q, want the actor named", parent.Title)
	}
	if child.Title == parent.Title {
		t.Errorf("both titles are %q; the whole point is that they differ", child.Title)
	}
	if strings.Contains(parent.Title, "Subagent") {
		t.Errorf("parent title = %q, must not be mistaken for a delegation", parent.Title)
	}

	// Which task: the child's own description, which is the only place it
	// appears — the settlement message the harness writes names the child by id.
	if !strings.Contains(child.Title, "Research dependency versions") || !strings.Contains(child.Body, "Research dependency versions") {
		t.Errorf("child message = %q / %q, want the delegated task named", child.Title, child.Body)
	}

	// What it amounted to: the parent turn counted two calls and one file.
	if !strings.Contains(parent.Body, "2 tool calls") {
		t.Errorf("parent body = %q, want the tool calls counted", parent.Body)
	}
	if !strings.Contains(parent.Body, "1 file changed") {
		t.Errorf("parent body = %q, want the file change counted", parent.Body)
	}

	// And the two do not collapse into one another on the lock screen.
	if child.Tag == parent.Tag {
		t.Errorf("both notifications share tag %q; one would replace the other", child.Tag)
	}
	if !strings.Contains(child.URL, "session-1") || !strings.Contains(parent.URL, "session-1") {
		t.Errorf("urls = %q / %q, want both to open the session they belong to", child.URL, parent.URL)
	}

	// The structured fields are what a client groups by, so they have to be
	// present and not merely implied by the prose.
	if child.Outcome != "completed" || parent.Outcome != "completed" {
		t.Errorf("outcomes = %q / %q, want both completed", child.Outcome, parent.Outcome)
	}
	if parent.Summary == "" {
		t.Errorf("parent summary is empty; a client cannot build a row from prose alone")
	}
}

// TestAFailedDelegationSaysWhatWentWrong pins the case the harness itself loses:
// dsh's own settlement notice for a failed child names the child and then stops,
// with the failure reason never reaching anyone. The gateway has the tool result,
// so the notification carries it.
func TestAFailedDelegationSaysWhatWentWrong(t *testing.T) {
	bus, rec := notifierFor(t, push.NotifierOptions{
		Threshold:        time.Hour,
		IncludeTaskNames: true,
	})

	toolStarted(bus, "session-1", "call_task", "subagent", `{"description":"Audit the handlers"}`)
	toolEnded(bus, "session-1", "call_task", "", "tool call aborted: permission denied for /etc/hosts", true)

	waitFor(t, func() bool { return len(rec.received()) >= 1 }, "a failure is reported however short")
	message := rec.received()[0]
	if message.Outcome != "failed" {
		t.Errorf("outcome = %q, want failed", message.Outcome)
	}
	if !strings.Contains(message.Title, "failed") {
		t.Errorf("title = %q, want the outcome stated", message.Title)
	}
	if !strings.Contains(message.Title, "Audit the handlers") {
		t.Errorf("title = %q, want the task named", message.Title)
	}
	if !strings.Contains(message.Body, "permission denied") {
		t.Errorf("body = %q, want the reason the child stopped", message.Body)
	}
}

// TestATaskNameIsOmittedWhenTheDeploymentSaysSo is the privacy half: naming a
// delegated task is a switch, and turning it off loses the name rather than
// leaking it into some other field.
func TestATaskNameIsOmittedWhenTheDeploymentSaysSo(t *testing.T) {
	bus, rec := notifierFor(t, push.NotifierOptions{
		Threshold:        time.Nanosecond,
		IncludeTaskNames: false,
	})

	toolStarted(bus, "session-1", "call_task", "subagent", `{"description":"Audit the handlers"}`)
	toolEnded(bus, "session-1", "call_task", "", "done", false)

	waitFor(t, func() bool { return len(rec.received()) >= 1 }, "a notification when the child settles")
	message := rec.received()[0]
	if message.Actor == nil || message.Actor.Kind != push.ActorSub {
		t.Fatalf("actor = %+v, want the subagent still identified", message.Actor)
	}
	if message.Actor.Name != "" {
		t.Errorf("actor name = %q, want it withheld", message.Actor.Name)
	}
	for _, field := range []string{message.Title, message.Body, message.Summary} {
		if strings.Contains(field, "Audit the handlers") {
			t.Errorf("message = %+v, must not name the task when the deployment opted out", message)
		}
	}
	// The actor is still named, so the notification remains distinguishable from
	// the main agent's — which is the line the option does not cross.
	if !strings.Contains(message.Title, "Subagent") {
		t.Errorf("title = %q, want the subagent still distinguishable", message.Title)
	}
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
