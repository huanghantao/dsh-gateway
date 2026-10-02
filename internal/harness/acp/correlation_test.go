package acp

import (
	"testing"

	"github.com/huanghantao/dsh-gateway/internal/harness"
)

// newTestAdapter builds an Adapter with only the fields these tests touch.
//
// It deliberately skips New: the correlation store is pure state, and requiring a
// full option set would obscure what is being tested.
func newTestAdapter() *Adapter {
	return &Adapter{
		sessions:  map[string]harness.Session{},
		toolCalls: map[string]harness.ToolCall{},
	}
}

// TestRememberToolCallPreservesDetailsAcrossCompletion covers the reason the
// correlation store exists.
//
// DSH's `session/request_permission` carries only a call id, and its
// `tool_call_update` carries no title. So the opening `tool_call` update is the
// only place the tool name and arguments ever appear, and a completion update
// must not overwrite them with blanks — otherwise the operator is asked to
// authorise a command the gateway can no longer describe.
func TestRememberToolCallPreservesDetailsAcrossCompletion(t *testing.T) {
	a := newTestAdapter()

	a.rememberToolCall(harness.ToolCall{
		ID:     "call_1",
		Title:  "bash",
		Status: harness.ToolInProgress,
		Input:  `{"command":"rm -rf build"}`,
	})

	// The completion update arrives without a title or arguments, exactly as the
	// live harness sends it.
	a.rememberToolCall(harness.ToolCall{
		ID:     "call_1",
		Status: harness.ToolCompleted,
	})

	got, ok := a.lookupToolCall("call_1")
	if !ok {
		t.Fatal("tool call was forgotten after completion")
	}
	if got.Title != "bash" {
		t.Errorf("Title = %q, want %q; the approval sheet would show an unnamed tool", got.Title, "bash")
	}
	if got.Input == "" {
		t.Error("Input was erased; the approval sheet would show no command to authorise")
	}
	if got.Status != harness.ToolCompleted {
		t.Errorf("Status = %q, want %q", got.Status, harness.ToolCompleted)
	}
}

func TestLookupUnknownToolCall(t *testing.T) {
	a := newTestAdapter()
	if _, ok := a.lookupToolCall("nope"); ok {
		t.Error("lookup of an unknown call id reported success")
	}
}

func TestRememberToolCallIsBounded(t *testing.T) {
	a := newTestAdapter()

	for i := 0; i < maxRememberedToolCalls*3; i++ {
		a.rememberToolCall(harness.ToolCall{
			ID:    string(rune('a'+i%26)) + string(rune('0'+i/26%10)) + string(rune('A'+i/260)),
			Title: "bash",
		})
	}

	a.mu.RLock()
	size := len(a.toolCalls)
	order := len(a.toolCallIDs)
	a.mu.RUnlock()

	if size > maxRememberedToolCalls {
		t.Errorf("store holds %d entries, want at most %d", size, maxRememberedToolCalls)
	}
	if order != size {
		t.Errorf("index holds %d ids but the map holds %d entries; they must not drift", order, size)
	}
}

func TestDropSessionsClearsToolCalls(t *testing.T) {
	a := newTestAdapter()
	a.rememberToolCall(harness.ToolCall{ID: "call_1", Title: "bash"})
	a.sessions["s1"] = harness.Session{}

	a.dropSessions()

	if _, ok := a.lookupToolCall("call_1"); ok {
		t.Error("tool calls survived a child restart; they can never be correlated again")
	}
	if len(a.sessions) != 0 {
		t.Error("sessions survived a child restart")
	}
}
