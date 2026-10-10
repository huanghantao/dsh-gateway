package sessionlog

import (
	"context"
	"testing"
	"time"
)

// testSession2 is a second session id, so a test that writes its own log cannot
// collide with the shared fixture's.
const testSession2 = "session-22222222-3333-4444-8555-666666666666"

// turnLog is a session with two finished turns, written the way DSH writes one:
// every row carries the turn it belongs to, and the boundaries carry a timestamp.
//
// Turn 1 is the session's earlier life — an answer, an edit, a resumed turn
// boundary — and turn 2 is the turn a notification would be about: a narration,
// two calls (one delegation, one command that failed through its exit code), an
// empty step, and the report it ended on.
func turnLog() []string {
	return []string{
		`{"type":"session","version":4,"id":"session-22222222-3333-4444-8555-666666666666","createdAt":1790519692979,"cwd":"/Users/me/code/api"}`,
		`{"type":"turn/start","seq":2,"time":1790519692979,"data":{"turn":1}}`,
		`{"type":"user/message","seq":3,"time":1790519693046,"data":{"content":[{"type":"text","text":"deploy it"}],"id":"m-user-1","source":{"kind":"user"}}}`,
		`{"type":"assistant/message","seq":4,"time":1790519700000,"data":{"turn":1,"step":1,"message":{"role":"assistant","id":"m-asst-1","content":[{"type":"text","text":"the first turn's answer"}],"source":{"kind":"model","model":"deepseek-flash"}}}}`,
		`{"type":"tool/call","seq":5,"time":1790519700100,"data":{"turn":1,"step":1,"callId":"call_old","name":"edit","arguments":"{\"file_path\":\"/tmp/old.go\"}"}}`,
		`{"type":"tool/result","seq":6,"time":1790519700200,"data":{"turn":1,"step":1,"message":{"role":"tool","toolCallId":"call_old","id":"m-tool-old","content":[{"type":"text","text":"written"}]}}}`,
		`{"type":"turn/end","seq":7,"time":1790519700300,"data":{"turn":1,"reason":{"kind":"completed"}}}`,

		`{"type":"turn/start","seq":8,"time":1790746400000,"data":{"turn":2}}`,
		`{"type":"user/message","seq":9,"time":1790746400100,"data":{"content":[{"type":"text","text":"redeploy please"}],"id":"m-user-2","source":{"kind":"user"}}}`,
		`{"type":"assistant/message","seq":10,"time":1790746400200,"data":{"turn":2,"step":1,"message":{"role":"assistant","id":"m-asst-2","content":[{"type":"text","text":"Let me look at the release path first."}],"source":{"kind":"model","model":"deepseek-flash"}}}}`,
		`{"type":"tool/call","seq":11,"time":1790746400300,"data":{"turn":2,"step":1,"callId":"call_sub","name":"mcp__agents__subagent","arguments":"{\"prompt\":\"audit\"}"}}`,
		`{"type":"tool/result","seq":12,"time":1790746400400,"data":{"turn":2,"step":1,"message":{"role":"tool","toolCallId":"call_sub","id":"m-tool-sub","content":[{"type":"text","text":"done"}]}}}`,
		`{"type":"tool/call","seq":13,"time":1790746400500,"data":{"turn":2,"step":2,"callId":"call_build","name":"bash","arguments":"{\"command\":\"make build\"}"}}`,
		// A command that failed: DSH reports this as a status, not as an error.
		`{"type":"tool/result","seq":14,"time":1790746400600,"data":{"turn":2,"step":2,"message":{"role":"tool","toolCallId":"call_build","id":"m-tool-build","content":[{"type":"text","text":"boom\n[exit code: 1]"}]}}}`,
		// A step that only called a tool commits a message with no text. It is
		// not the turn's sign-off, and reading it as one would erase the report.
		`{"type":"assistant/message","seq":15,"time":1790746400700,"data":{"turn":2,"step":3,"message":{"role":"assistant","id":"m-asst-3","content":[{"type":"tool-call","id":"call_late","name":"bash","arguments":"{\"command\":\"echo hi\"}"}],"source":{"kind":"model","model":"deepseek-flash"}}}}`,
		// A call whose result never arrived. It is not work the turn did.
		`{"type":"tool/call","seq":16,"time":1790746400800,"data":{"turn":2,"step":3,"callId":"call_late","name":"bash","arguments":"{\"command\":\"echo hi\"}"}}`,
		`{"type":"assistant/message","seq":17,"time":1790746400900,"data":{"turn":2,"step":4,"message":{"role":"assistant","id":"m-asst-4","content":[{"type":"text","text":"部署完成，线上已经是新版本。"}],"source":{"kind":"model","model":"deepseek-flash"}}}}`,
		`{"type":"turn/end","seq":18,"time":1790746401000,"data":{"turn":2,"reason":{"kind":"completed"}}}`,
	}
}

// TestLastTurnStatesTheTurnThatJustEnded is the reason this function exists.
//
// A follower reads a projection it did not write, and the rows of every turn the
// session ever ran are in it. The record it states must be the *last* turn's:
// the answer it ended on, the work it did, and none of the earlier turns' —
// which is what makes a notification about a followed turn a description of the
// turn rather than of the part of it this process happened to watch.
func TestLastTurnStatesTheTurnThatJustEnded(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--Users-me-code-api--", testSession2, turnLog())
	s := newTestStore(t, root)

	ctx := context.Background()
	items, meta, err := s.Items(ctx, testSession2)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if !meta.TurnStartedAt.Equal(fromMillis(1790746400000)) {
		t.Fatalf("TurnStartedAt = %v, want the log's own turn/start time", meta.TurnStartedAt)
	}

	record := LastTurn(items, meta.TurnStartedAt)
	if record.Closing == nil {
		t.Fatal("the turn's closing message was not stated")
	}
	if record.Closing.Text != "部署完成，线上已经是新版本。" {
		t.Errorf("closing = %q, want the message the turn ended on", record.Closing.Text)
	}
	if record.Closing.Model != "deepseek-flash" {
		t.Errorf("closing model = %q, want the model that wrote it", record.Closing.Model)
	}
	// Two settled calls belong to this turn: the delegation and the command that
	// failed through its exit code. The pending call is not work the turn did,
	// and the previous turn's edit is not this turn's.
	want := struct{ calls, failed, delegations, edits int }{calls: 2, failed: 1, delegations: 1, edits: 0}
	got := record.Work
	if got.Calls != want.calls || got.Failed != want.failed || got.Delegations != want.delegations || got.Edits != want.edits {
		t.Errorf("work = %+v, want %d calls, %d failed, %d delegations, %d edits",
			got, want.calls, want.failed, want.delegations, want.edits)
	}
}

// TestLastTurnWithoutABoundaryReadsTheWholeProjection is the honest fallback.
//
// A log whose boundaries carry no timestamp cannot say where its last turn
// began, and the alternative to reading the whole projection would be inventing
// a start — which would silently drop rows that do belong to the turn.
func TestLastTurnWithoutABoundaryReadsTheWholeProjection(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--Users-me-code-api--", testSession2, turnLog())
	s := newTestStore(t, root)

	items, _, err := s.Items(context.Background(), testSession2)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	record := LastTurn(items, time.Time{})
	if record.Closing == nil || record.Closing.Text != "部署完成，线上已经是新版本。" {
		t.Errorf("closing = %+v, want the last message in the projection", record.Closing)
	}
	// Every settled call in the session, since nothing says where the turn began.
	if want := 3; record.Work.Calls != want {
		t.Errorf("calls = %d, want %d", record.Work.Calls, want)
	}
}

// TestTurnStartedAtSurvivesTheTurnItBelongsTo: the settlement describes the turn
// that just ended, so the start it states has to be that turn's — clearing it at
// `turn/end` would leave every settlement with nothing to measure from.
func TestTurnStartedAtSurvivesTheTurnItBelongsTo(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--Users-me-code-api--", testSession2, turnLog())
	s := newTestStore(t, root)

	meta, err := s.Meta(context.Background(), testSession2)
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.TurnRunning {
		t.Error("a finished turn is reported as running")
	}
	if meta.TurnCount != 2 {
		t.Errorf("TurnCount = %d, want 2", meta.TurnCount)
	}
	if want := fromMillis(1790746400000); !meta.TurnStartedAt.Equal(want) {
		t.Errorf("TurnStartedAt = %v, want turn 2's start %v", meta.TurnStartedAt, want)
	}
}
