package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/app/lifecycle"
)

// eventFrame is one frame read off the stream, decoded loosely.
//
// The stream tests deliberately decode into a map rather than the server's own
// structs: a test that used the same type the handler marshals would pass even
// if the field names drifted away from the documented contract, which is the
// one thing these tests exist to pin.
type eventFrame struct {
	Seq       uint64          `json:"seq"`
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	Data      json.RawMessage `json:"data"`
}

// dialEvents opens the event stream against a running test server.
func dialEvents(t *testing.T, ts *testServer, query string) (*websocket.Conn, context.Context) {
	t.Helper()

	srv := httptest.NewServer(ts.handler)
	t.Cleanup(srv.Close)

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/events"
	if query != "" {
		url += "?" + query
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Cookie": []string{"dsh_gw_session=" + ts.token},
			"Origin": []string{srv.URL},
		},
	})
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn, ctx
}

// readFrame reads one frame, failing the test on a timeout.
func readFrame(t *testing.T, ctx context.Context, conn *websocket.Conn) eventFrame {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, payload, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var frame eventFrame
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatalf("decode frame %s: %v", payload, err)
	}
	return frame
}

// TestHelloNamesTheSequenceSpace pins the field the whole cursor contract hangs
// on. Without it a client cannot tell "I am up to date" from "my bookmark
// belongs to a process that no longer exists", which is the silent failure this
// contract was introduced to remove.
func TestHelloNamesTheSequenceSpace(t *testing.T) {
	ts := newTestServer(t)
	conn, ctx := dialEvents(t, ts, "")

	frame := readFrame(t, ctx, conn)
	if frame.Type != "hello" {
		t.Fatalf("first frame is %q, want hello", frame.Type)
	}
	var hello struct {
		Generation string `json:"generation"`
		ReplayFrom uint64 `json:"replayFrom"`
	}
	if err := json.Unmarshal(frame.Data, &hello); err != nil {
		t.Fatalf("decode hello: %v", err)
	}
	if hello.Generation == "" {
		t.Fatal("hello carries no generation; a client cannot tell a redeploy from a gap")
	}
	if want := ts.bus.Generation(); hello.Generation != want {
		t.Errorf("generation = %q, want the bus's own %q", hello.Generation, want)
	}
}

// TestStaleCursorFromAPreviousRunIsResynchronised is the regression test for the
// incident this change came from.
//
// A phone that was on `seq: 900` when the gateway was redeployed reconnects
// holding that number. The old code asked "is this cursor older than what I
// still hold?", which is false, so it replayed nothing, sent no resync, and
// returned a connection that looked healthy — while every frame it then
// delivered was discarded by the client as stale, because the new process
// numbers its events from one again. The conversation froze and nothing said so.
func TestStaleCursorFromAPreviousRunIsResynchronised(t *testing.T) {
	first := newTestServer(t)
	// Make the first run's watermark unmistakably higher than the second's.
	for i := 0; i < 50; i++ {
		first.bus.Publish(events.TypeSessionMessage, "session-test", map[string]any{"n": i})
	}

	// The same deployment, restarted: a new process, a new sequence space.
	second := newTestServer(t)

	// The phone reconnects with what it held, and the generation it belonged to.
	conn, ctx := dialEvents(t, second, "since=900&generation="+first.bus.Generation())
	defer conn.CloseNow()

	if frame := readFrame(t, ctx, conn); frame.Type != "hello" {
		t.Fatalf("first frame is %q, want hello", frame.Type)
	}

	resync := readFrame(t, ctx, conn)
	if resync.Type != "resync" {
		t.Fatalf("second frame is %q, want resync: a cursor the gateway cannot honour "+
			"must never produce a stream that merely looks continuous", resync.Type)
	}
	var reason struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(resync.Data, &reason); err != nil {
		t.Fatalf("decode resync: %v", err)
	}
	if reason.Reason != string(events.ResyncGenerationChanged) {
		t.Errorf("resync reason = %q, want %q", reason.Reason, events.ResyncGenerationChanged)
	}

	snap := readFrame(t, ctx, conn)
	if snap.Type != "snapshot" {
		t.Fatalf("third frame is %q, want snapshot: a resync without the state that "+
			"replaces the client's is only half an answer", snap.Type)
	}
	var body struct {
		Generation string `json:"generation"`
		Seq        uint64 `json:"seq"`
		Harness    struct {
			State string `json:"state"`
		} `json:"harness"`
		Turns     []json.RawMessage `json:"turns"`
		Approvals []json.RawMessage `json:"approvals"`
	}
	if err := json.Unmarshal(snap.Data, &body); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if body.Generation != second.bus.Generation() {
		t.Errorf("snapshot generation = %q, want the live one %q",
			body.Generation, second.bus.Generation())
	}
	if body.Harness.State == "" {
		t.Error("snapshot carries no harness state; the one fact a client cannot " +
			"reconstruct after a redeploy is whether the agent is up")
	}
	if body.Approvals == nil {
		t.Error("snapshot carries no approvals list; a client cannot tell \"none pending\" " +
			"from \"this server does not report them\"")
	}
}

// TestCursorAheadOfTheStreamIsResynchronised covers the client that sends a
// bookmark and no generation — every client installed before this contract
// existed. Its cursor claims events this process never published, which must be
// treated as unusable rather than as "nothing to send".
func TestCursorAheadOfTheStreamIsResynchronised(t *testing.T) {
	ts := newTestServer(t)
	ts.bus.Publish(events.TypeSessionMessage, "session-test", nil)

	conn, ctx := dialEvents(t, ts, "since=900")
	defer conn.CloseNow()

	if frame := readFrame(t, ctx, conn); frame.Type != "hello" {
		t.Fatalf("first frame is %q, want hello", frame.Type)
	}
	resync := readFrame(t, ctx, conn)
	if resync.Type != "resync" {
		t.Fatalf("second frame is %q, want resync", resync.Type)
	}
	var reason struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(resync.Data, &reason)
	if reason.Reason != string(events.ResyncAheadOfStream) {
		t.Errorf("resync reason = %q, want %q", reason.Reason, events.ResyncAheadOfStream)
	}
}

// TestReconnectWithinOneRunIsReplayedNotResynchronised is the other half: the
// ordinary case must stay cheap. A client whose cursor is inside this process's
// retained window gets exactly the frames it missed and is told nothing is
// wrong.
func TestReconnectWithinOneRunIsReplayedNotResynchronised(t *testing.T) {
	ts := newTestServer(t)

	// The first connection watches one event go by, which is what gives it a
	// real cursor rather than an invented one. A fresh connection is sent the
	// hello and a snapshot of the present, and deliberately *not* a resync: it
	// has no stale view to repair.
	first, firstCtx := dialEvents(t, ts, "")
	if frame := readFrame(t, firstCtx, first); frame.Type != "hello" {
		t.Fatalf("first frame is %q, want hello", frame.Type)
	}
	if frame := readFrame(t, firstCtx, first); frame.Type != "snapshot" {
		t.Fatalf("second frame on a fresh connection is %q, want snapshot (a resync here "+
			"would make every first load refetch state it is about to fetch anyway)", frame.Type)
	}
	ts.bus.Publish(events.TypeSessionMessage, "session-test", map[string]any{"n": 1})
	seen := readFrame(t, firstCtx, first)
	if seen.Type != string(events.TypeSessionMessage) {
		t.Fatalf("live frame is %q, want session.message", seen.Type)
	}
	_ = first.CloseNow()

	// Two more events happen while nobody is watching. This is the phone's
	// ordinary case: the screen locked and a turn kept running.
	ts.bus.Publish(events.TypeSessionMessage, "session-test", map[string]any{"n": 2})
	ts.bus.Publish(events.TypeSessionMessage, "session-test", map[string]any{"n": 3})

	conn, ctx := dialEvents(t, ts,
		"since="+strconv.FormatUint(seen.Seq, 10)+"&generation="+ts.bus.Generation())
	defer conn.CloseNow()

	if frame := readFrame(t, ctx, conn); frame.Type != "hello" {
		t.Fatalf("first frame is %q, want hello", frame.Type)
	}

	// Exactly the frames the client missed, in order, and no resync before
	// them: an ordinary reconnect must stay cheap.
	for want := seen.Seq + 1; want <= ts.bus.Watermark(); want++ {
		frame := readFrame(t, ctx, conn)
		if frame.Type != string(events.TypeSessionMessage) {
			t.Fatalf("frame %d is %q, want a replayed session.message (a resync here "+
				"would mean the ordinary reconnect was treated as unrecoverable)", frame.Seq, frame.Type)
		}
		if frame.Seq != want {
			t.Errorf("replayed seq = %d, want %d", frame.Seq, want)
		}
	}
}

// TestSubscribingToASessionDeliversItsTurnState covers the case a phone hits
// every time it opens an old conversation that turns out to be mid-turn: the
// frames that announced the turn are long past the replay window, so without a
// snapshot for that session the client shows an idle session that is not idle.
func TestSubscribingToASessionDeliversItsTurnState(t *testing.T) {
	ts := newTestServer(t)
	conn, ctx := dialEvents(t, ts, "")
	if frame := readFrame(t, ctx, conn); frame.Type != "hello" {
		t.Fatalf("first frame is %q, want hello", frame.Type)
	}
	// The connection-wide snapshot that every fresh connection gets, so the next
	// one is unambiguously the session-scoped reply to the subscribe.
	if frame := readFrame(t, ctx, conn); frame.Type != "snapshot" {
		t.Fatalf("second frame is %q, want the opening snapshot", frame.Type)
	}

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"subscribe","sessionId":"session-test"}`)); err != nil {
		t.Fatalf("write subscribe: %v", err)
	}

	frame := readFrame(t, ctx, conn)
	if frame.Type != "snapshot" {
		t.Fatalf("frame after subscribe is %q, want a session snapshot", frame.Type)
	}
	if frame.SessionID != "session-test" {
		t.Fatalf("snapshot sessionId = %q, want session-test", frame.SessionID)
	}
	var snap struct {
		Turns []struct {
			SessionID string `json:"sessionId"`
		} `json:"turns"`
		Approvals []json.RawMessage `json:"approvals"`
	}
	if err := json.Unmarshal(frame.Data, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.Approvals == nil {
		t.Error("snapshot carries no approvals field; a client cannot tell \"none pending\" " +
			"from \"this server does not report them\"")
	}
}

// TestDrainingIsReportedAndRefusesNewWork pins the deploy protocol from the
// client's side: the frame that says "I am leaving" arrives, and a prompt sent
// afterwards is refused with a retryable answer rather than accepted by a
// process that is about to close the agent.
func TestDrainingIsReportedAndRefusesNewWork(t *testing.T) {
	ts := newTestServer(t)
	conn, ctx := dialEvents(t, ts, "")
	if frame := readFrame(t, ctx, conn); frame.Type != "hello" {
		t.Fatalf("first frame is %q, want hello", frame.Type)
	}

	ts.life.Drain(lifecycle.ReasonDeploy)

	sawDraining := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !sawDraining {
		if frame := readFrame(t, ctx, conn); frame.Type == "gateway.draining" {
			sawDraining = true
		}
	}
	if !sawDraining {
		t.Error("no gateway.draining frame arrived; a redeploy would be indistinguishable " +
			"from a hang until a request failed")
	}

	rec := ts.do(http.MethodGet, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz while draining = %d, want 503: a deploy polls readiness to learn "+
			"when the old process is gone, and draining means it is still here", rec.Code)
	}

	rec = ts.do(http.MethodPost, "/api/v1/sessions/session-test/prompt",
		withCookie(ts.token), withJSON(`{"blocks":[{"type":"text","text":"are you there?"}]}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("prompt while draining = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "gateway_draining") {
		t.Errorf("refusal body does not name the code a client branches on: %s", rec.Body.String())
	}

	// Releasing is the direction the drain is trying to move sessions in, so it
	// must keep working.
	release := ts.do(http.MethodDelete, "/api/v1/sessions/session-test/lease", withCookie(ts.token))
	if release.Code == http.StatusServiceUnavailable {
		t.Error("releasing a lease was refused while draining; the drain is trying to give " +
			"sessions back, and refusing that works against it")
	}
}
