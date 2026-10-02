package sessionlog

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// writeLog builds a session log from event lines, in the multi-frame shape DSH
// actually produces: the file is a concatenation of independently compressed
// zstd frames, not a single frame. A decoder that reads only the first frame
// silently returns just the session header, which is exactly the failure this
// fixture is designed to catch.
func writeLog(t *testing.T, root, workspace, sessionID string, events []string) {
	t.Helper()

	dir := filepath.Join(root, workspace, sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	defer enc.Close()

	var buf bytes.Buffer
	for _, e := range events {
		buf.Write(enc.EncodeAll([]byte(e+"\n"), nil))
	}
	if err := os.WriteFile(filepath.Join(dir, logFileName), buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
}

// appendLog adds one more frame, the way DSH flushes: appended, never rewritten.
func appendLog(t *testing.T, root, workspace, sessionID string, events ...string) {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	defer enc.Close()

	var buf bytes.Buffer
	for _, e := range events {
		buf.Write(enc.EncodeAll([]byte(e+"\n"), nil))
	}
	path := filepath.Join(root, workspace, sessionID, logFileName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer file.Close()
	if _, err := file.Write(buf.Bytes()); err != nil {
		t.Fatalf("append log: %v", err)
	}
}

func newTestStore(t *testing.T, root string) *Store {
	t.Helper()
	s, err := New(root, logx.Discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

const testSession = "session-11111111-2222-4333-8444-555555555555"

func sampleEvents() []string {
	return []string{
		`{"type":"session","version":4,"id":"session-11111111-2222-4333-8444-555555555555","createdAt":1790519692979,"cwd":"/Users/me/code/api"}`,
		`{"type":"turn/start","seq":4,"time":1790519692979,"data":{"turn":1}}`,
		`{"type":"user/message","seq":8,"time":1790519693046,"data":{"content":[{"type":"text","text":"why is the upload failing?"}],"id":"m-user-1","source":{"kind":"user"}}}`,
		// DSH records its own injections as user-role messages. These must not
		// become bubbles: the skill catalog alone is tens of kilobytes and would
		// bury a two-line exchange.
		`{"type":"user/message","seq":9,"time":1790519693050,"data":{"content":[{"type":"text","text":"Current runtime context. This snapshot supersedes earlier ones."}],"id":"m-ctx-1","source":{"kind":"runtime-context","form":"snapshot"}}}`,
		`{"type":"user/message","seq":10,"time":1790519693051,"data":{"content":[{"type":"text","text":"<system-reminder>A skill is a reusable set of task-specific instructions."}],"id":"m-skills-1","source":{"kind":"skill-catalog","form":"catalog"}}}`,
		`{"type":"session/title","seq":13,"time":1790519693049,"data":{"title":"why is the upload failing?","source":{"kind":"fallback"}}}`,
		`{"type":"step/start","seq":14,"time":1790519693050,"data":{}}`,
		`{"type":"assistant/message","seq":15,"time":1790519695735,"data":{"turn":1,"step":1,"message":{"role":"assistant","id":"m-asst-1","content":[{"type":"reasoning","text":"The uploader retries without a backoff."},{"type":"text","text":"Because the retry has no backoff."}],"source":{"kind":"model","provider":"command-code","model":"deepseek/deepseek-v4.1-flash"}},"usage":{"inputTokens":5195,"outputTokens":42,"totalTokens":10317,"cacheReadTokens":5120}}}`,
		`{"type":"assistant/message","seq":16,"time":1790519695736,"data":{"turn":1,"step":1,"message":{"role":"assistant","id":"m-asst-2","content":[{"type":"tool-call","id":"call_abc","name":"bash","arguments":"{\"command\":\"grep -n retry uploader.go\"}"}],"source":{"kind":"model","model":"deepseek/deepseek-v4.1-flash"}}}}`,
		`{"type":"tool/call","seq":17,"time":1790519695740,"data":{"turn":1,"step":1,"callId":"call_abc","name":"bash","arguments":"{\"command\":\"grep -n retry uploader.go\"}"}}`,
		`{"type":"tool/result","seq":18,"time":1790519695900,"data":{"turn":1,"step":1,"message":{"role":"tool","toolCallId":"call_abc","isError":true,"id":"m-tool-1","content":[{"type":"text","text":"no such file: uploader.go"}]}}}`,
		`{"type":"tool/call","seq":19,"time":1790519696000,"data":{"turn":1,"step":2,"callId":"call_def","name":"bash","arguments":"{\"command\":\"ls\"}"}}`,
		// Deliberately never resolved, to exercise the pending path.
		`{"type":"model/selection","seq":25,"time":1790746430414,"data":{"provider":"command-code","model":"deepseek/deepseek-v4.1-flash","reasoningEffort":"max"}}`,
		`{"type":"turn/end","seq":26,"time":1790746430500,"data":{"turn":1,"reason":{"kind":"completed"}}}`,
		// An event type this build has never heard of must be skipped, not fatal.
		`{"type":"some/future/event","seq":27,"time":1790746430600,"data":{"anything":true}}`,
	}
}

func TestProjectTranscript(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--Users-me-code-api--", testSession, sampleEvents())
	s := newTestStore(t, root)

	ctx := context.Background()

	meta, err := s.Meta(ctx, testSession)
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.Title != "why is the upload failing?" {
		t.Errorf("Title = %q", meta.Title)
	}
	if meta.Workspace != "/Users/me/code/api" {
		t.Errorf("Workspace = %q", meta.Workspace)
	}
	if meta.Model != "deepseek/deepseek-v4.1-flash" {
		t.Errorf("Model = %q", meta.Model)
	}
	if meta.Effort != "max" {
		t.Errorf("Effort = %q", meta.Effort)
	}
	if meta.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", meta.TurnCount)
	}
	if meta.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	page, err := s.Transcript(ctx, testSession, 0, 100)
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}

	// user, assistant, tool(call_abc), tool(call_def). Three things must NOT
	// appear: the second assistant message contains only a tool-call block (or
	// every tool use would be duplicated), and the two injected user-role
	// messages are harness bookkeeping rather than conversation.
	if len(page.Items) != 4 {
		for _, it := range page.Items {
			t.Logf("  item seq=%d role=%s tool=%s text=%q", it.Seq, it.Role, it.Tool, truncate(it.Text))
		}
		t.Fatalf("got %d items, want 4", len(page.Items))
	}

	if got := page.Items[0]; got.Role != RoleUser || got.Text != "why is the upload failing?" {
		t.Errorf("item 0 = %+v", got)
	}

	asst := page.Items[1]
	if asst.Role != RoleAssistant {
		t.Fatalf("item 1 role = %q", asst.Role)
	}
	if asst.Text != "Because the retry has no backoff." {
		t.Errorf("assistant text = %q", asst.Text)
	}
	if asst.Thinking != "The uploader retries without a backoff." {
		t.Errorf("assistant thinking = %q", asst.Thinking)
	}
	if asst.Usage == nil || asst.Usage.OutputTokens != 42 || asst.Usage.CacheReadTokens != 5120 {
		t.Errorf("assistant usage = %+v", asst.Usage)
	}

	// The result event carries no tool name, so this asserts the call/result
	// correlation works — without it the UI shows an unnamed tool completing.
	call := page.Items[2]
	if call.Role != RoleTool || call.Tool != "bash" {
		t.Errorf("tool item = %+v", call)
	}
	if call.Output != "no such file: uploader.go" {
		t.Errorf("tool output = %q", call.Output)
	}
	if !call.IsError {
		t.Error("tool IsError = false, want true")
	}
	if call.Pending {
		t.Error("resolved tool call is still marked pending")
	}
	if call.Input == "" {
		t.Error("tool input is empty; the command would not be visible")
	}

	// The unresolved call must still be present and flagged, so the UI can show a
	// spinner rather than pretending nothing happened.
	unresolved := page.Items[3]
	if unresolved.Tool != "bash" || !unresolved.Pending {
		t.Errorf("unresolved tool item = %+v", unresolved)
	}
}

// TestModelComesFromRequestHeader pins the event an ACP session actually writes.
//
// `model/selection` is appended by DSH's web session controller, so a session
// this gateway created over ACP never has one: its route lives only in
// `request/header`. Reading just the former left every such session looking as
// though it had no model at all, which the phone renders as "Default model".
func TestModelComesFromRequestHeader(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, []string{
		`{"type":"session","version":4,"id":"` + testSession + `","createdAt":1790519692979,"cwd":"/Users/me/code/api"}`,
		`{"type":"request/header","seq":2,"time":1790519692980,"data":{"header":{"config":{"provider":"command-code","model":"deepseek/deepseek-v4.1-flash","reasoningEffort":"max","maxTokens":128000},"tools":[{"name":"bash"}]},"reason":"initial"}}`,
		// A later header wins: it is the route in force now, which is what a
		// session resumed on another model writes.
		`{"type":"request/header","seq":3,"time":1790519792980,"data":{"header":{"config":{"provider":"volcengine","model":"glm-5.3-flash","reasoningEffort":"off"},"tools":[]},"reason":"change"}}`,
	})
	s := newTestStore(t, root)

	meta, err := s.Meta(context.Background(), testSession)
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.Model != "glm-5.3-flash" {
		t.Errorf("Model = %q, want the last header's model", meta.Model)
	}
	if meta.Effort != "off" {
		t.Errorf("Effort = %q, want the last header's effort", meta.Effort)
	}
}

func TestTranscriptPaging(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	s := newTestStore(t, root)

	ctx := context.Background()

	first, err := s.Transcript(ctx, testSession, 0, 2)
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	if len(first.Items) != 2 {
		t.Fatalf("first page has %d items, want 2", len(first.Items))
	}
	if first.NextBefore == 0 {
		t.Fatal("NextBefore is 0; paging backwards is impossible")
	}
	if first.Total != 4 {
		t.Errorf("Total = %d, want 4", first.Total)
	}

	second, err := s.Transcript(ctx, testSession, first.NextBefore, 2)
	if err != nil {
		t.Fatalf("Transcript page 2: %v", err)
	}
	if len(second.Items) != 2 {
		t.Fatalf("second page has %d items, want 2", len(second.Items))
	}
	// Total counts the whole transcript, not the page: it is how a client knows
	// how much history is left behind the page it holds.
	if second.Total != 4 {
		t.Errorf("second page Total = %d, want 4", second.Total)
	}
	// Pages must not overlap, and the oldest page must come back oldest-first.
	if second.Items[len(second.Items)-1].Seq >= first.Items[0].Seq {
		t.Errorf("pages overlap: second ends at seq %d, first starts at %d",
			second.Items[len(second.Items)-1].Seq, first.Items[0].Seq)
	}

	// Every item has now been delivered, so the store must report the transcript
	// as exhausted. NextBefore == 0 is the "nothing older" signal; a client that
	// ignored it and asked again with before=0 would be handed the newest page a
	// second time, so this assertion is what prevents that loop.
	if second.NextBefore != 0 {
		t.Errorf("NextBefore = %d after the oldest page, want 0", second.NextBefore)
	}

	// The two pages together must account for the whole transcript, with no item
	// seen twice and none skipped.
	seen := map[int64]int{}
	for _, it := range append(append([]Item(nil), first.Items...), second.Items...) {
		seen[it.Seq]++
	}
	if len(seen) != first.Total {
		t.Errorf("pages covered %d distinct items, want %d", len(seen), first.Total)
	}
	for seq, n := range seen {
		if n != 1 {
			t.Errorf("seq %d appeared %d times", seq, n)
		}
	}
}

func TestUnsupportedFormatDegrades(t *testing.T) {
	root := t.TempDir()
	events := sampleEvents()
	// Rewrite the header as a future schema version.
	events[0] = `{"type":"session","version":99,"id":"` + testSession + `","createdAt":1,"cwd":"/tmp"}`
	writeLog(t, root, "--ws--", testSession, events)
	s := newTestStore(t, root)

	_, err := s.Transcript(context.Background(), testSession, 0, 10)
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("err = %v, want ErrUnsupportedFormat", err)
	}

	// The list reads metadata instead of rows, and must refuse the same log: a
	// version it cannot read is not a partial read to serve the readable half of.
	if _, err := s.Meta(context.Background(), testSession); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("Meta err = %v, want ErrUnsupportedFormat", err)
	}
}

func TestSessionIDValidation(t *testing.T) {
	root := t.TempDir()
	// A real session to prove the store works, then hostile ids to prove it does
	// not walk out of the root.
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	s := newTestStore(t, root)

	valid := []string{
		testSession,
		"435fdb8d-017e-404f-af20-833609f43ab9",
	}
	for _, id := range valid {
		if !validSessionID(id) {
			t.Errorf("validSessionID(%q) = false, want true", id)
		}
	}

	hostile := []string{
		"../../etc/passwd",
		"..",
		".",
		"a/b",
		"a\\b",
		"-leading-dash",
		"",
		"has space",
		"nul\x00byte",
		"session-1/../../..",
	}
	for _, id := range hostile {
		if validSessionID(id) {
			t.Errorf("validSessionID(%q) = true, want false", id)
		}
		if _, err := s.Transcript(context.Background(), id, 0, 10); err == nil {
			t.Errorf("Transcript(%q) succeeded, want rejection", id)
		}
	}
}

func TestMissingSessionIsNotFound(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	_, err := s.Transcript(context.Background(), testSession, 0, 10)
	if err == nil {
		t.Fatal("Transcript of a missing session succeeded")
	}
}

func TestCacheInvalidatesOnChange(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	s := newTestStore(t, root)
	ctx := context.Background()

	first, err := s.Transcript(ctx, testSession, 0, 100)
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}

	// Appending is what DSH does; the size changes and the cache must not serve
	// the stale projection.
	path := filepath.Join(root, "--ws--", testSession, logFileName)
	appendEvents(t, path, []string{
		`{"type":"user/message","seq":30,"time":1790746431000,"data":{"content":[{"type":"text","text":"and now?"}],"id":"m-user-2"}}`,
	})
	// Filesystem mtime granularity can be coarse; make the change unambiguous.
	if err := os.Chtimes(path, time.Now().Add(time.Second), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	second, err := s.Transcript(ctx, testSession, 0, 100)
	if err != nil {
		t.Fatalf("Transcript after append: %v", err)
	}
	if len(second.Items) != len(first.Items)+1 {
		t.Fatalf("after append: %d items, want %d", len(second.Items), len(first.Items)+1)
	}
	if last := second.Items[len(second.Items)-1]; last.Text != "and now?" {
		t.Errorf("newest item = %+v", last)
	}
}

// appendEvents adds more frames to an existing log, mirroring DSH's append-only
// multi-frame write pattern.
func appendEvents(t *testing.T, path string, events []string) {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	defer enc.Close()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	for _, e := range events {
		if _, err := f.Write(enc.EncodeAll([]byte(e+"\n"), nil)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// TestMetaIsCachedPerSession is the session list's cost model in one test.
//
// The list reads every session's metadata on every request, and an operator has
// hundreds of them. Reading each log once per request is what made the list take
// half a second; a size-and-mtime match must therefore answer without reading.
func TestMetaIsCachedPerSession(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	s := newTestStore(t, root)

	ctx := context.Background()
	if _, err := s.Meta(ctx, testSession); err != nil {
		t.Fatalf("Meta: %v", err)
	}
	after := s.Decodes()

	for i := 0; i < 50; i++ {
		if _, err := s.Meta(ctx, testSession); err != nil {
			t.Fatalf("Meta: %v", err)
		}
	}
	if got := s.Decodes(); got != after {
		t.Errorf("50 cached reads cost %d more decode(s), want none", got-after)
	}
}

// TestMetaRereadsOnlyWhatChanged keeps the other half honest: a cache that never
// expires would show a session's old title forever.
func TestMetaRereadsOnlyWhatChanged(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	s := newTestStore(t, root)

	ctx := context.Background()
	first, err := s.Meta(ctx, testSession)
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	before := s.Decodes()

	// A later frame, appended the way DSH appends one: a new title.
	appendLog(t, root, "--ws--", testSession,
		`{"type":"session/title","seq":30,"time":1790746430700,"data":{"title":"uploads retry forever"}}`)

	second, err := s.Meta(ctx, testSession)
	if err != nil {
		t.Fatalf("Meta after a change: %v", err)
	}
	if got := s.Decodes(); got != before+1 {
		t.Errorf("a changed log cost %d decode(s), want exactly one", got-before)
	}
	if second.Title == first.Title {
		t.Errorf("title still %q after the log recorded a new one", second.Title)
	}

	// And the new answer is cached in turn.
	third := s.Decodes()
	if _, err := s.Meta(ctx, testSession); err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if got := s.Decodes(); got != third {
		t.Errorf("the re-read was not cached: %d more decode(s)", got-third)
	}
}

// TestTranscriptReadAnswersMetaToo: opening a conversation and then listing
// sessions must not fold the same file twice.
func TestTranscriptReadAnswersMetaToo(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	s := newTestStore(t, root)

	ctx := context.Background()
	if _, err := s.Transcript(ctx, testSession, 0, 50); err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	before := s.Decodes()
	if _, err := s.Meta(ctx, testSession); err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if got := s.Decodes(); got != before {
		t.Errorf("metadata after a transcript read cost %d decode(s), want none", got-before)
	}
}

// TestMetaCarriesPreviewAndCount covers what a list row needs when a session has
// no title: the first thing the operator typed, and how much is in there.
func TestMetaCarriesPreviewAndCount(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	s := newTestStore(t, root)

	meta, err := s.Meta(context.Background(), testSession)
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.Preview != "why is the upload failing?" {
		t.Errorf("Preview = %q, want the operator's first message", meta.Preview)
	}
	// Two user rows are injections and must not count; tools are not messages;
	// one assistant message carries text and one is a bare tool call.
	if meta.Messages != 2 {
		t.Errorf("Messages = %d, want 2 (one prompt, one answer)", meta.Messages)
	}
	if meta.TurnCount != 1 || meta.TurnRunning {
		t.Errorf("turn bookkeeping regressed: count=%d running=%v", meta.TurnCount, meta.TurnRunning)
	}
}

// TestPreviewIgnoresInjectionsAndTruncates: DSH writes runtime context and a
// skill catalogue as user-role messages, and either would be a useless preview.
func TestPreviewIgnoresInjectionsAndTruncates(t *testing.T) {
	root := t.TempDir()
	long := ""
	for i := 0; i < 200; i++ {
		long += "字"
	}
	writeLog(t, root, "--ws--", testSession, []string{
		`{"type":"session","version":4,"id":"session-11111111-2222-4333-8444-555555555555","cwd":"/tmp/ws"}`,
		`{"type":"user/message","seq":1,"time":1790519692979,"data":{"content":[{"type":"text","text":"Current runtime context."}],"id":"ctx","source":{"kind":"runtime-context"}}}`,
		`{"type":"user/message","seq":2,"time":1790519692980,"data":{"content":[{"type":"text","text":"` + long + `"}],"id":"first","source":{"kind":"user"}}}`,
		`{"type":"user/message","seq":3,"time":1790519692981,"data":{"content":[{"type":"text","text":"a second question"}],"id":"second","source":{"kind":"user"}}}`,
	})
	s := newTestStore(t, root)

	meta, err := s.Meta(context.Background(), testSession)
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if len(meta.Preview) > 160 {
		t.Errorf("Preview is %d bytes, want it truncated", len(meta.Preview))
	}
	if meta.Preview == "" || meta.Preview == "Current runtime context." {
		t.Errorf("Preview = %q, want the first real prompt", meta.Preview)
	}
	if meta.Preview[len(meta.Preview)-3:] != "…" {
		t.Errorf("Preview = %q, want an ellipsis marking the cut", meta.Preview)
	}
	// It keeps the *first* prompt, not the latest.
	if meta.Preview == "a second question" {
		t.Error("Preview took the newest prompt; a preview is what the session opened with")
	}
}
