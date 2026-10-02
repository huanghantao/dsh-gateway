package sessionwatch_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/sessionlog"
	"github.com/huanghantao/dsh-gateway/internal/sessionwatch"
)

// fixture is a sessions root with one session, written the way DSH writes it:
// one zstd frame per flush, appended to the same file.
type fixture struct {
	t       *testing.T
	root    string
	id      string
	path    string
	seq     int64
	encoder *zstd.Encoder
}

func newFixture(t *testing.T, id string) *fixture {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	t.Cleanup(func() { _ = enc.Close() })

	root := t.TempDir()
	dir := filepath.Join(root, "--workspace--", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f := &fixture{
		t:       t,
		root:    root,
		id:      id,
		path:    filepath.Join(dir, "session.v4.jsonl.zstd"),
		encoder: enc,
	}
	// The header line, which carries the schema version the reader checks.
	f.flush(map[string]any{"type": "session", "version": sessionlog.SupportedVersion, "id": id, "cwd": "/tmp/ws"})
	return f
}

// flush appends one complete zstd frame holding the given events.
func (f *fixture) flush(events ...map[string]any) {
	f.t.Helper()
	var body []byte
	for _, event := range events {
		f.seq++
		if _, ok := event["seq"]; !ok {
			event["seq"] = f.seq
		}
		if _, ok := event["time"]; !ok {
			event["time"] = time.Now().UnixMilli()
		}
		line, err := json.Marshal(event)
		if err != nil {
			f.t.Fatalf("marshal: %v", err)
		}
		body = append(body, line...)
		body = append(body, '\n')
	}
	f.write(f.encoder.EncodeAll(body, nil))
}

// write appends raw bytes, so a test can leave a frame half-written.
func (f *fixture) write(raw []byte) {
	f.t.Helper()
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		f.t.Fatalf("open log: %v", err)
	}
	defer file.Close()
	if _, err := file.Write(raw); err != nil {
		f.t.Fatalf("append log: %v", err)
	}
}

// frame encodes events without writing them, for the half-written case.
func (f *fixture) frame(events ...map[string]any) []byte {
	f.t.Helper()
	var body []byte
	for _, event := range events {
		f.seq++
		event["seq"] = f.seq
		event["time"] = time.Now().UnixMilli()
		line, err := json.Marshal(event)
		if err != nil {
			f.t.Fatalf("marshal: %v", err)
		}
		body = append(body, line...)
		body = append(body, '\n')
	}
	return f.encoder.EncodeAll(body, nil)
}

// sibling writes one more session into the same root, holding one message.
//
// The store keeps only a handful of projections, so a test that needs a log to
// be read again has to give it neighbours — which is what a real sessions root
// has, and why a projection is usually gone by the next sweep.
func (f *fixture) sibling(t *testing.T, id string) {
	t.Helper()
	dir := filepath.Join(f.root, "--workspace--", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	header := map[string]any{"type": "session", "version": sessionlog.SupportedVersion, "id": id, "cwd": "/tmp/ws"}
	message := userMessage("neighbour")
	message["seq"] = 1
	message["time"] = time.Now().UnixMilli()

	var body []byte
	for _, event := range []map[string]any{header, message} {
		line, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		body = append(body, line...)
		body = append(body, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "session.v4.jsonl.zstd"), f.encoder.EncodeAll(body, nil), 0o600); err != nil {
		t.Fatalf("write sibling session: %v", err)
	}
}

// watcher builds a watcher over the fixture, with an ownership predicate.
func (f *fixture) watcher(t *testing.T, owned func(string) bool) (*sessionwatch.Watcher, *events.Bus, *events.Subscription) {
	t.Helper()
	watcher, _, bus, sub := f.watcherWithStore(t, owned)
	return watcher, bus, sub
}

// watcherWithStore is watcher, plus the store it reads through: a test that
// cares how much a sweep cost asks the store's decodes counter.
func (f *fixture) watcherWithStore(t *testing.T, owned func(string) bool) (*sessionwatch.Watcher, *sessionlog.Store, *events.Bus, *events.Subscription) {
	t.Helper()
	bus := events.New(events.Config{Replay: 128, Queue: 128})
	store, err := sessionlog.New(f.root, logx.Discard())
	if err != nil {
		t.Fatalf("sessionlog.New: %v", err)
	}
	t.Cleanup(store.Close)

	sub, _ := bus.Subscribe(0, nil)
	t.Cleanup(sub.Close)

	watcher, err := sessionwatch.New(sessionwatch.Options{
		Store:    store,
		Bus:      bus,
		Owned:    owned,
		Interval: time.Millisecond,
		Logger:   logx.Discard(),
	})
	if err != nil {
		t.Fatalf("sessionwatch.New: %v", err)
	}
	return watcher, store, bus, sub
}

// drain collects everything published so far.
func drain(sub *events.Subscription) []events.Event {
	var out []events.Event
	for {
		select {
		case e := <-sub.Events():
			out = append(out, e)
		default:
			return out
		}
	}
}

// typesOf lists the frame types in order.
func typesOf(got []events.Event) []events.Type {
	out := make([]events.Type, 0, len(got))
	for _, e := range got {
		out = append(out, e.Type)
	}
	return out
}

func userMessage(text string) map[string]any {
	return map[string]any{
		"type": "user/message",
		"data": map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"source":  map[string]any{"kind": "user"},
			"id":      "u-" + text,
		},
	}
}

func assistantMessage(id, text string) map[string]any {
	return map[string]any{
		"type": "assistant/message",
		"data": map[string]any{
			"message": map[string]any{
				"id":      id,
				"content": []map[string]any{{"type": "text", "text": text}},
				"source":  map[string]any{"model": "provider/model"},
			},
			"usage": map[string]any{"inputTokens": 10, "outputTokens": 2, "totalTokens": 12},
		},
	}
}

// TestFirstSightIsSilent is the rule that keeps a gateway restart from firing
// every session's history at the phone as if it were happening now.
func TestFirstSightIsSilent(t *testing.T) {
	f := newFixture(t, "session-one")
	f.flush(userMessage("hello"), assistantMessage("a1", "hi there"))

	watcher, _, sub := f.watcher(t, nil)
	watcher.Sweep(context.Background())

	if got := drain(sub); len(got) != 0 {
		t.Fatalf("first sweep published %v, want nothing", typesOf(got))
	}
}

// TestAppendedEventsArePublished covers the whole point: rows that appear in a
// log another process is writing reach the bus.
func TestAppendedEventsArePublished(t *testing.T) {
	f := newFixture(t, "session-two")
	f.flush(userMessage("hello"))

	watcher, _, sub := f.watcher(t, nil)
	watcher.Sweep(context.Background())
	drain(sub)

	f.flush(assistantMessage("a1", "hi there"))
	watcher.Sweep(context.Background())

	got := drain(sub)
	if len(got) == 0 {
		t.Fatal("an appended assistant message published nothing")
	}
	var message map[string]any
	for _, e := range got {
		if e.Type == events.TypeSessionMessage {
			message = e.Data.(map[string]any)
		}
	}
	if message == nil {
		t.Fatalf("no session.message among %v", typesOf(got))
	}
	if message["id"] != "a1" || message["role"] != "assistant" || message["text"] != "hi there" {
		t.Errorf("message = %v, want the appended assistant message", message)
	}
	if message["model"] != "provider/model" {
		t.Errorf("model = %v, want the message's model", message["model"])
	}
	if _, ok := message["usage"]; !ok {
		t.Error("the message carried usage in the log but none was published")
	}
}

// TestPromptTypedElsewhereIsPublished: a prompt typed at the desk is a row the
// phone must show, and its role is what keeps it from rendering as an answer.
func TestPromptTypedElsewhereIsPublished(t *testing.T) {
	f := newFixture(t, "session-three")

	watcher, _, sub := f.watcher(t, nil)
	watcher.Sweep(context.Background())

	f.flush(userMessage("typed at the desk"))
	watcher.Sweep(context.Background())

	var found bool
	for _, e := range drain(sub) {
		if e.Type != events.TypeSessionMessage {
			continue
		}
		data := e.Data.(map[string]any)
		if data["role"] != "user" || data["text"] != "typed at the desk" {
			t.Errorf("published %v, want a user message", data)
		}
		found = true
	}
	if !found {
		t.Error("a prompt written to the log by another process published nothing")
	}
}

// TestToolCallBecomesTwoFrames pins the lifecycle: the call is a start, the
// result closes the same card, and the call id pairs them.
func TestToolCallBecomesTwoFrames(t *testing.T) {
	f := newFixture(t, "session-four")

	watcher, _, sub := f.watcher(t, nil)
	watcher.Sweep(context.Background())

	f.flush(map[string]any{
		"type": "tool/call",
		"data": map[string]any{"callId": "c1", "name": "bash", "arguments": `{"command":"ls"}`},
	})
	watcher.Sweep(context.Background())
	started := drain(sub)
	if len(started) != 1 || started[0].Type != events.TypeSessionTool {
		t.Fatalf("tool call published %v, want one session.tool", typesOf(started))
	}
	if data := started[0].Data.(map[string]any); data["phase"] != "start" || data["callId"] != "c1" || data["tool"] != "bash" {
		t.Errorf("start frame = %v", data)
	}

	f.flush(map[string]any{
		"type": "tool/result",
		"data": map[string]any{"message": map[string]any{
			"toolCallId": "c1",
			"content":    []map[string]any{{"type": "text", "text": "listing"}},
			"isError":    false,
			"id":         "r1",
		}},
	})
	watcher.Sweep(context.Background())

	var end map[string]any
	for _, e := range drain(sub) {
		if e.Type == events.TypeSessionTool {
			end = e.Data.(map[string]any)
		}
	}
	if end == nil || end["phase"] != "end" || end["callId"] != "c1" {
		t.Fatalf("end frame = %v, want the result closing c1", end)
	}
	if end["output"] != "listing" {
		t.Errorf("output = %v, want the result text", end["output"])
	}
}

// TestOwnedSessionsAreSilentButStayCurrent is the anti-duplication rule: while
// the gateway drives a session its activity already arrives over ACP, and a
// later handover to the desk must not replay the whole turn.
func TestOwnedSessionsAreSilentButStayCurrent(t *testing.T) {
	f := newFixture(t, "session-five")
	f.flush(userMessage("hello"))

	owned := true
	watcher, _, sub := f.watcher(t, func(string) bool { return owned })
	watcher.Sweep(context.Background())
	drain(sub)

	// The gateway drives a turn: the log grows, the bus must not hear about it.
	f.flush(
		assistantMessage("a1", "driven by the gateway"),
		map[string]any{"type": "turn/start", "data": map[string]any{"turn": 1}},
		map[string]any{"type": "turn/end", "data": map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}},
	)
	watcher.Sweep(context.Background())
	if got := drain(sub); len(got) != 0 {
		t.Fatalf("an owned session published %v, want nothing", typesOf(got))
	}

	// The desk takes it over and writes the next message. Only that one is news.
	owned = false
	f.flush(assistantMessage("a2", "written at the desk"))
	watcher.Sweep(context.Background())

	got := drain(sub)
	var texts []string
	for _, e := range got {
		if e.Type == events.TypeSessionMessage {
			texts = append(texts, e.Data.(map[string]any)["text"].(string))
		}
	}
	if len(texts) != 1 || texts[0] != "written at the desk" {
		t.Fatalf("after the handover published %v, want only the newest message", texts)
	}
}

// TestHalfWrittenFrameIsRetried: DSH appends a frame per flush, so a reader can
// catch the file mid-write. That must cost one silent tick, not a lost message.
func TestHalfWrittenFrameIsRetried(t *testing.T) {
	f := newFixture(t, "session-six")

	watcher, _, sub := f.watcher(t, nil)
	watcher.Sweep(context.Background())

	frame := f.frame(assistantMessage("a1", "cut in half"))
	f.write(frame[:len(frame)/2])
	watcher.Sweep(context.Background())
	if got := drain(sub); len(got) != 0 {
		t.Fatalf("a partial frame published %v, want nothing", typesOf(got))
	}

	f.write(frame[len(frame)/2:])
	watcher.Sweep(context.Background())

	var found bool
	for _, e := range drain(sub) {
		if e.Type == events.TypeSessionMessage && e.Data.(map[string]any)["text"] == "cut in half" {
			found = true
		}
	}
	if !found {
		t.Error("the message was lost when its frame arrived in two pieces")
	}
}

// TestRootBiggerThanTheMemoryBoundIsNotReread pins what a real sessions root
// costs. The watcher keeps projections for a bounded number of sessions, but it
// has to remember that it has *seen* every one of them: a forgotten log is
// decoded again on the next sweep, so a home with a few hundred sessions decodes
// all of them once a second, forever. That sweep takes longer than the interval,
// which is how it turns into a core held at 100%.
func TestRootBiggerThanTheMemoryBoundIsNotReread(t *testing.T) {
	f := newFixture(t, "session-aaa")
	f.flush(userMessage("hello"))
	// Many more logs than the watcher keeps projections for.
	for i := 0; i < 96; i++ {
		f.sibling(t, fmt.Sprintf("session-b-%03d", i))
	}

	watcher, store, _, sub := f.watcherWithStore(t, nil)
	watcher.Sweep(context.Background())
	seeded := store.Decodes()
	if seeded != 97 {
		t.Fatalf("the seeding sweep read %d logs, want one per session", seeded)
	}
	drain(sub)

	for i := 0; i < 3; i++ {
		watcher.Sweep(context.Background())
	}
	if got := store.Decodes(); got != seeded {
		t.Errorf("settled sweeps re-read %d logs, want none", got-seeded)
	}
}

// TestEvictedProjectionIsSeededNotReplayed is the other half of that bound: a
// session whose projection eviction let go must not have its whole history
// re-sent to the phone as if it were new. The read that finds the append is the
// one that re-establishes the baseline; the append after it is news again.
func TestEvictedProjectionIsSeededNotReplayed(t *testing.T) {
	f := newFixture(t, "session-aaa")
	f.flush(userMessage("hello"))
	// Read after this one, so its projection is the first to go.
	for i := 0; i < 96; i++ {
		f.sibling(t, fmt.Sprintf("session-b-%03d", i))
	}

	watcher, _, _, sub := f.watcherWithStore(t, nil)
	watcher.Sweep(context.Background())
	drain(sub)

	f.flush(assistantMessage("a1", "written after eviction"))
	watcher.Sweep(context.Background())
	if got := drain(sub); len(got) != 0 {
		t.Fatalf("a re-seeded session published %v, want nothing", typesOf(got))
	}

	f.flush(assistantMessage("a2", "and this one is news"))
	watcher.Sweep(context.Background())

	var texts []string
	for _, e := range drain(sub) {
		if e.Type == events.TypeSessionMessage {
			texts = append(texts, e.Data.(map[string]any)["text"].(string))
		}
	}
	if len(texts) != 1 || texts[0] != "and this one is news" {
		t.Fatalf("after re-seeding published %v, want only the next message", texts)
	}
}

// TestRunStopsWithItsContext keeps the goroutine honest.
func TestRunStopsWithItsContext(t *testing.T) {
	f := newFixture(t, "session-eight")
	watcher, _, _ := f.watcher(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watcher.Run(ctx)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return when its context was cancelled")
	}
}
