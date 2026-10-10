package bridge

import (
	"fmt"
	"strconv"
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

// TestAnUnclosedToolCallCannotGrowTheTableForever bounds the call table.
//
// A completion removes its own entry, but a completion is not guaranteed: a turn
// cancelled mid-call, or a child killed, leaves an opening frame with nothing to
// close it. The entry holds the call's raw arguments — a whole file, for an edit
// — so an unbounded table is not a few bytes per leak.
func TestAnUnclosedToolCallCannotGrowTheTableForever(t *testing.T) {
	bus := events.New(events.Config{Replay: 8, Queue: 8})
	b := New(bus, config.Default().Limits)

	open := func(id string) {
		b.Publish(harness.Update{SessionID: "s1", Kind: harness.UpdateTool, Tool: &harness.ToolCall{
			ID:     id,
			Title:  "edit",
			Status: harness.ToolInProgress,
			Input:  `{"path":"/tmp/notes","content":"a whole file's worth of arguments"}`,
		}})
	}

	total := maxOpenCalls * 3
	for i := 0; i < total; i++ {
		open(fmt.Sprintf("c%d", i))
	}

	count := func() (int, int) {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.open), len(b.order)
	}
	held, ordered := count()
	if held > maxOpenCalls {
		t.Errorf("open calls = %d, want at most %d: nothing closes a call whose end never arrives", held, maxOpenCalls)
	}
	if ordered != held {
		t.Errorf("order = %d and open = %d, want them equal: the cap must count calls that are still open", ordered, held)
	}

	// The oldest go first: their completion, if it ever came, came long ago.
	b.mu.Lock()
	_, oldestHeld := b.open["c0"]
	_, newestHeld := b.open[fmt.Sprintf("c%d", total-1)]
	b.mu.Unlock()
	if oldestHeld {
		t.Error("the oldest call survived the cap, so the newest are what gets dropped")
	}
	if !newestHeld {
		t.Error("the call that was opened last is not tracked, so its closing frame will arrive with no name")
	}

	// And the cap does not break the correlation it exists for: a completion
	// still finds its entry, and leaves the order the size it found it.
	open("c_final")
	before, _ := count()
	b.Publish(harness.Update{SessionID: "s1", Kind: harness.UpdateTool, Tool: &harness.ToolCall{
		ID:     "c_final",
		Status: harness.ToolCompleted,
		Output: "ok",
	}})
	after, orderedAfter := count()
	if after != before-1 {
		t.Errorf("open calls = %d after a completion, want %d", after, before-1)
	}
	if orderedAfter != after {
		t.Errorf("order = %d after a completion and open = %d: a finished call still counts against the cap",
			orderedAfter, after)
	}
}

// TestTakeTurnStatesWhatTheTurnWas pins the whole point of the accumulator: the
// bridge is where an ACP turn's words and work exist, and the settlement is
// where they are stated.
//
// The assertions are the two facts a notification cannot reconstruct: the
// message the turn ended on — not the narration it started with, and not a step
// that committed no text — and the counts of the calls that settled, with a
// command that failed through its exit code counted as a failure.
func TestTakeTurnStatesWhatTheTurnWas(t *testing.T) {
	bus := events.New(events.Config{Replay: 32, Queue: 32})
	b := New(bus, config.Default().Limits)

	b.Publish(harness.Update{SessionID: "s1", Kind: harness.UpdateMessage, MessageID: "m1", Text: "Let me look at the release path first."})
	b.Publish(harness.Update{SessionID: "s1", Kind: harness.UpdateTool, Tool: &harness.ToolCall{
		ID: "c1", Title: "edit", Status: harness.ToolInProgress, Input: `{"file_path":"/tmp/a.go"}`,
	}})
	b.Publish(harness.Update{SessionID: "s1", Kind: harness.UpdateTool, Tool: &harness.ToolCall{
		ID: "c1", Status: harness.ToolCompleted, Output: "written",
	}})
	b.Publish(harness.Update{SessionID: "s1", Kind: harness.UpdateTool, Tool: &harness.ToolCall{
		ID: "c2", Title: "bash", Status: harness.ToolInProgress, Input: `{"command":"make build"}`,
	}})
	output := "boom\n[exit code: 1]"
	b.Publish(harness.Update{SessionID: "s1", Kind: harness.UpdateTool, Tool: &harness.ToolCall{
		ID: "c2", Status: harness.ToolCompleted, Output: output, Result: toolresult.Observe(output),
	}})
	// A step that only called a tool commits a message with no text.
	b.Publish(harness.Update{SessionID: "s1", Kind: harness.UpdateMessage, MessageID: "m2", Text: "   "})
	// A delegated child, counted as a delegation and never announced on its own.
	b.Publish(harness.Update{SessionID: "s1", Kind: harness.UpdateTool, Tool: &harness.ToolCall{
		ID: "c3", Title: "subagent", Status: harness.ToolInProgress, Input: `{"prompt":"audit"}`,
	}})
	b.Publish(harness.Update{SessionID: "s1", Kind: harness.UpdateTool, Tool: &harness.ToolCall{
		ID: "c3", Status: harness.ToolCompleted, Output: "done",
	}})
	b.Publish(harness.Update{SessionID: "s1", Kind: harness.UpdateMessage, MessageID: "m3", Text: "部署完成，线上已经是新版本。"})

	record := b.TakeTurn("s1")
	if record.Closing == nil || record.Closing.Text != "部署完成，线上已经是新版本。" {
		t.Fatalf("closing = %+v, want the turn's last words", record.Closing)
	}
	want := events.Work{Calls: 3, Failed: 1, Delegations: 1, Edits: 1}
	if record.Work != want {
		t.Errorf("work = %+v, want %+v", record.Work, want)
	}

	// Handed over, not copied: a settlement describes one turn, and a record
	// left behind would be quoted as the closing words of whatever came next.
	if next := b.TakeTurn("s1"); next.Closing != nil || next.Work.Calls != 0 {
		t.Errorf("the record survived being taken: %+v", next)
	}
}

// TestTheOpenTurnTableEvictsWhatStoppedTalking is the eviction policy.
//
// The table holds every session whose updates pass through the bridge, and only
// the turns this gateway drives are ever taken from it: a delegated child's
// record is written and never read, because a child's turn is not the gateway's
// to settle. A workflow that fans out to enough children would therefore push
// the parent's record out of the table if eviction went by arrival — and the
// parent's record is the one whose settlement is about to read it. Eviction goes
// by activity instead, so what is forgotten is what has stopped being written.
func TestTheOpenTurnTableEvictsWhatStoppedTalking(t *testing.T) {
	bus := events.New(events.Config{Replay: 8, Queue: 8})
	b := New(bus, config.Default().Limits)

	// The parent turn opens the table and is the oldest entry by arrival.
	b.Publish(harness.Update{SessionID: "parent", Kind: harness.UpdateMessage, MessageID: "m1", Text: "the report nobody may lose"})

	// A parent turn keeps working while its children come and go: a call settles
	// every so often, and the children — which say one thing and stop — churn
	// through the table several times over.
	for i := 0; i < maxTurnRecords*2; i++ {
		b.Publish(harness.Update{SessionID: childID(i), Kind: harness.UpdateMessage, Text: "child"})
		if i%8 != 0 {
			continue
		}
		b.Publish(harness.Update{SessionID: "parent", Kind: harness.UpdateTool, Tool: &harness.ToolCall{
			ID: "call-" + strconv.Itoa(i), Title: "bash", Status: harness.ToolInProgress, Input: `{"command":"go build ./..."}`,
		}})
		b.Publish(harness.Update{SessionID: "parent", Kind: harness.UpdateTool, Tool: &harness.ToolCall{
			ID: "call-" + strconv.Itoa(i), Status: harness.ToolCompleted, Output: "ok",
		}})
	}

	record := b.TakeTurn("parent")
	if record.Closing == nil || record.Closing.Text != "the report nobody may lose" {
		t.Errorf("the parent's record was evicted by its children: %+v", record)
	}
	if want := maxTurnRecords * 2 / 8; record.Work.Calls != want {
		t.Errorf("the parent's own work was lost with %d calls counted, want %d", record.Work.Calls, want)
	}
	if record := b.TakeTurn(childID(0)); record.Closing != nil {
		t.Errorf("the table kept a child that had stopped talking for %d sessions: %+v", maxTurnRecords, record)
	}
}

func childID(index int) string { return "child-" + strconv.Itoa(index) }
