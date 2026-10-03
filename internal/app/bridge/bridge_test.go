package bridge

import (
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/config"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/toolresult"
)

// TestToolStatusVocabulary pins the mapping from the harness's lifecycle words
// onto the wire's, and above all what an unrecognised one becomes.
//
// The default is the interesting case. Reading a word this build has never seen
// as "still running" keeps the card on screen and honest; reading it as
// "completed" — which is what the bridge used to do — closes a call that has not
// finished and hides the result that is still coming.
func TestToolStatusVocabulary(t *testing.T) {
	cases := []struct {
		in   harness.ToolStatus
		want events.ToolStatus
	}{
		{harness.ToolInProgress, events.ToolInProgress},
		{harness.ToolCompleted, events.ToolCompleted},
		{harness.ToolFailed, events.ToolFailed},
		{"pending", events.ToolInProgress},
		{"", events.ToolInProgress},
	}
	for _, tc := range cases {
		if got := toolStatusOf(tc.in); got != tc.want {
			t.Errorf("toolStatusOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestClosingFrameKeepsTheNameAndCarriesTheFacts covers the two things the
// completing update cannot supply by itself: the tool's name, which DSH omits
// from it, and the result facts, which are the only place a non-zero exit status
// exists at all.
func TestClosingFrameKeepsTheNameAndCarriesTheFacts(t *testing.T) {
	bus := events.New(events.Config{Replay: 8, Queue: 8})
	sub := bus.Subscribe(events.Cursor{Generation: bus.Generation()}, nil).Subscription
	defer sub.Close()
	b := New(bus, config.Default().Limits)

	b.Publish(harness.Update{SessionID: "s1", Kind: harness.UpdateTool, Tool: &harness.ToolCall{
		ID:     "c1",
		Title:  "bash",
		Status: harness.ToolInProgress,
		Input:  `{"command":"go build ./..."}`,
	}})
	output := "compile failed\n[exit code: 2]"
	b.Publish(harness.Update{SessionID: "s1", Kind: harness.UpdateTool, Tool: &harness.ToolCall{
		ID:     "c1",
		Status: harness.ToolCompleted,
		Output: output,
		Result: toolresult.Observe(output),
	}})

	frames := make([]events.ToolData, 0, 2)
	deadline := time.After(2 * time.Second)
	for len(frames) < 2 {
		select {
		case e := <-sub.Events():
			if e.Type != events.TypeSessionTool {
				continue
			}
			data, ok := e.Data.(events.ToolData)
			if !ok {
				t.Fatalf("session.tool payload = %T, want events.ToolData", e.Data)
			}
			frames = append(frames, data)
		case <-deadline:
			t.Fatalf("only %d of 2 tool frames arrived", len(frames))
		}
	}

	start := frames[0]
	if start.Phase != events.ToolStarted || start.Status != events.ToolInProgress || start.Tool != "bash" {
		t.Errorf("start frame = %+v", start)
	}
	end := frames[1]
	if end.Phase != events.ToolEnded || end.Status != events.ToolCompleted {
		t.Errorf("end frame = %+v, want a completed call", end)
	}
	if end.Tool != "bash" {
		t.Errorf("tool = %q, want the name the opening frame taught the bridge", end.Tool)
	}
	if end.Input == "" {
		t.Error("the closing frame dropped the arguments the opening frame carried")
	}
	if end.ExitCode == nil || *end.ExitCode != 2 {
		t.Errorf("exitCode = %v, want 2 from the result's own marker", end.ExitCode)
	}
	if end.IsError {
		t.Error("IsError = true, want false: a non-zero exit is reported, not errored")
	}
}
