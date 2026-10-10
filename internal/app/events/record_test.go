package events

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestWorkCountsASettledCallOnce pins the arithmetic behind a card's middle line.
//
// It is a method on the payload rather than a function in the notifier because
// two producers count the same turn from different material — the bridge from
// the harness's update stream, the watcher from a session log's rows — and a
// reader must not be able to tell which one counted. The name is normalised
// first, so an MCP-namespaced call is classified the same way on both routes.
func TestWorkCountsASettledCallOnce(t *testing.T) {
	var work Work
	work.Count("bash", false)
	work.Count("mcp__filesystem__edit", false)
	work.Count("mcp__agents__subagent", true)
	work.Count("workflow", false)

	want := Work{Calls: 4, Failed: 1, Delegations: 2, Edits: 1}
	if work != want {
		t.Errorf("work = %+v, want %+v", work, want)
	}
}

// TestRecordIsAbsentWhenThereIsNothingToSay pins the wire shape: a settlement
// that states no record must not grow a `record` key, because a client (and a
// notification) has to be able to tell "this turn said nothing" from "this
// producer does not state records" by the frame's shape alone.
func TestRecordIsAbsentWhenThereIsNothingToSay(t *testing.T) {
	plain, err := json.Marshal(TurnState{TurnID: "turn-1", State: "completed"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(plain), "record") {
		t.Errorf("a settlement with no record carries one: %s", plain)
	}

	// A turn that only talked: a closing message and no work.
	talked, err := json.Marshal(TurnState{
		TurnID: "turn-1", State: "completed",
		Record: TurnRecord{Closing: &Closing{Text: "已提交并推送。", Model: "deepseek-flash"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"closing":{"text":"已提交并推送。","model":"deepseek-flash"`} {
		if !strings.Contains(string(talked), want) {
			t.Errorf("a closing message did not reach the wire: %s", talked)
		}
	}
	if strings.Contains(string(talked), `"work"`) {
		t.Errorf("a turn that did no work reported work: %s", talked)
	}

	// A turn that ended on a tool call: work and no closing message.
	worked, err := json.Marshal(TurnState{
		TurnID: "turn-1", State: "completed",
		Record: TurnRecord{Work: Work{Calls: 6}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(worked), `"work":{"calls":6}`) {
		t.Errorf("work did not reach the wire: %s", worked)
	}
	if strings.Contains(string(worked), `"closing"`) {
		t.Errorf("a turn with no closing message carried one: %s", worked)
	}
}
