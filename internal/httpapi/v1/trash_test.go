package v1

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/huanghantao/dsh-gateway/internal/harness"
)

// writeSessionLog puts a minimal but real session log on disk, so deletion and
// restore have something to move.
//
// extra events are appended after the three that make a log readable, which is
// how a test says something about a session's route without a second fixture
// that would drift from this one.
func writeSessionLog(t *testing.T, ts *testServer, sessionID, workspace, title string, extra ...string) string {
	t.Helper()
	dir := filepath.Join(ts.deps.Sessions.SessionsRoot(), "--workspace--", sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	defer enc.Close()

	var body bytes.Buffer
	lines := append([]string{
		`{"type":"session","version":4,"id":"` + sessionID + `","cwd":"` + workspace + `"}`,
		`{"type":"session/title","seq":1,"time":1790519693049,"data":{"title":"` + title + `"}}`,
		`{"type":"user/message","seq":2,"time":1790519693050,"data":{"content":[{"type":"text","text":"hello"}],"id":"u1","source":{"kind":"user"}}}`,
	}, extra...)
	for _, line := range lines {
		body.Write(enc.EncodeAll([]byte(line+"\n"), nil))
	}
	path := filepath.Join(dir, "session.v4.jsonl.zstd")
	if err := os.WriteFile(path, body.Bytes(), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return path
}

// TestDeletingASessionPutsItInTheTrashAndBack covers the product promise of the
// only destructive button in the app: it destroys nothing.
func TestDeletingASessionPutsItInTheTrashAndBack(t *testing.T) {
	ts := newTestServer(t)
	const id = "session-deleteme"
	path := writeSessionLog(t, ts, id, "/Users/me/code/api", "把发布说明整理成文档")

	rec := ts.do(http.MethodDelete, "/api/v1/sessions/"+id, withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE returned %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the session's log is still in the store after a delete")
	}

	// The trash knows what it holds, by name — a list of ids would be a list
	// nobody can act on.
	trash := ts.do(http.MethodGet, "/api/v1/trash", withCookie(ts.token))
	if trash.Code != http.StatusOK {
		t.Fatalf("GET /trash returned %d", trash.Code)
	}
	var listing struct {
		Trash []struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			Deleted string `json:"deletedAt"`
		} `json:"trash"`
	}
	if err := json.Unmarshal(trash.Body.Bytes(), &listing); err != nil {
		t.Fatalf("/trash is not JSON: %v", err)
	}
	if len(listing.Trash) != 1 || listing.Trash[0].ID != id {
		t.Fatalf("trash = %+v, want the deleted session", listing.Trash)
	}
	if listing.Trash[0].Title != "把发布说明整理成文档" {
		t.Errorf("title = %q, want the session's own title", listing.Trash[0].Title)
	}
	if listing.Trash[0].Deleted == "" {
		t.Error("the entry does not say when it was deleted")
	}

	rec = ts.do(http.MethodPost, "/api/v1/trash/restore", withCookie(ts.token), withJSON(`{"id":"`+id+`"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("restore returned %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the restored log is not back where it was: %v", err)
	}
	if trash := ts.do(http.MethodGet, "/api/v1/trash", withCookie(ts.token)); trash.Code != http.StatusOK {
		t.Fatalf("GET /trash returned %d", trash.Code)
	} else {
		listing.Trash = nil
		_ = json.Unmarshal(trash.Body.Bytes(), &listing)
		if len(listing.Trash) != 0 {
			t.Errorf("the trash still holds %+v after a restore", listing.Trash)
		}
	}
}

// TestDeletingSomethingThatIsNotThere says so rather than pretending.
func TestDeletingSomethingThatIsNotThere(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(http.MethodDelete, "/api/v1/sessions/session-never-existed", withCookie(ts.token))
	if rec.Code != http.StatusNotFound {
		t.Errorf("deleting an unknown session returned %d, want 404", rec.Code)
	}
}

// TestTrashCanBeTurnedOff: a gateway without a transcript projection keeps no
// trash, and must say so instead of half-working.
func TestTrashCanBeTurnedOff(t *testing.T) {
	ts := newTestServer(t)
	ts.deps.Trash = nil
	if rec := ts.do(http.MethodDelete, "/api/v1/sessions/whatever", withCookie(ts.token)); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("DELETE without a trash returned %d, want 503", rec.Code)
	}
	rec := ts.do(http.MethodGet, "/api/v1/trash", withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /trash without a trash returned %d, want an empty list", rec.Code)
	}
}

// TestSessionReceiptReportsWhatHappened: the conversation answers "what was
// said"; this answers "what did it cost me and what did it touch", from the log
// alone.
func TestSessionReceiptReportsWhatHappened(t *testing.T) {
	ts := newTestServer(t)
	const id = "session-receipt"
	writeSessionLog(t, ts, id, "/Users/me/code/api", "why is the upload failing?")

	rec := ts.do(http.MethodGet, "/api/v1/sessions/"+id+"/receipt", withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET receipt returned %d: %s", rec.Code, rec.Body.String())
	}
	var receipt struct {
		ID            string `json:"id"`
		Title         string `json:"title"`
		Workspace     string `json:"workspace"`
		Messages      int    `json:"messages"`
		InputTokens   int    `json:"inputTokens"`
		OutputTokens  int    `json:"outputTokens"`
		ActiveSeconds int    `json:"activeSeconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatalf("receipt is not JSON: %v", err)
	}
	if receipt.ID != id || receipt.Workspace != "/Users/me/code/api" {
		t.Errorf("receipt = %+v, want the session it was asked about", receipt)
	}
	if receipt.Title != "why is the upload failing?" {
		t.Errorf("title = %q, want the session's own title", receipt.Title)
	}
	if receipt.Messages != 1 {
		t.Errorf("messages = %d, want the one prompt the fixture holds", receipt.Messages)
	}
}

// TestReceiptWithoutATranscript says so rather than answering with zeros, which
// would look like a session that did nothing.
func TestReceiptWithoutATranscript(t *testing.T) {
	ts := newTestServer(t)
	ts.deps.Sessions = nil
	rec := ts.do(http.MethodGet, "/api/v1/sessions/whatever/receipt", withCookie(ts.token))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("receipt without a transcript returned %d, want 503", rec.Code)
	}
}

// TestListIncludesSessionsTheGatewayItselfHasOpen covers a hole a phone user
// hits immediately: DSH's listing omits a session another process owns, and the
// process that owns one is usually this gateway — so the session the operator
// just created and ran a turn in was missing from the list they went back to.
func TestListIncludesSessionsTheGatewayItselfHasOpen(t *testing.T) {
	ts := newTestServer(t)
	if ts.deps.Leases == nil {
		t.Skip("this test server has no lease manager")
	}
	const id = "session-open-right-now"
	writeSessionLog(t, ts, id, "/Users/me/code/api", "a session the phone is in")

	// The harness does not list it — that is the behaviour being compensated for.
	ts.deps.Harness.(*fakeHarness).sessions = []harness.SessionInfo{{ID: "session-other", Workspace: "/Users/me/code/api"}}
	ts.deps.Leases.Adopt(harness.Session{Info: harness.SessionInfo{ID: id, Workspace: "/Users/me/code/api"}})

	rec := ts.do(http.MethodGet, "/api/v1/sessions", withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sessions returned %d", rec.Code)
	}
	var page struct {
		Sessions []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("listing is not JSON: %v", err)
	}
	if len(page.Sessions) == 0 || page.Sessions[0].ID != id {
		t.Fatalf("listing = %+v, want the open session first", page.Sessions)
	}
	if page.Sessions[0].Title != "a session the phone is in" {
		t.Errorf("title = %q, want the session's own title", page.Sessions[0].Title)
	}
}

// TestSearchFindsTextInsideATranscript: the list search answers "which session",
// this answers "where in it" — and it says how much it read, because a bounded
// search that stays quiet about the bound answers "nothing found" with the same
// confidence whether it read three sessions or three hundred.
func TestSearchFindsTextInsideATranscript(t *testing.T) {
	ts := newTestServer(t)
	const id = "session-searchable"
	dir := filepath.Join(ts.deps.Sessions.SessionsRoot(), "--workspace--", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	defer enc.Close()
	var body bytes.Buffer
	for _, line := range []string{
		`{"type":"session","version":4,"id":"` + id + `","cwd":"/Users/me/code/api"}`,
		`{"type":"session/title","seq":1,"time":1790519693049,"data":{"title":"缓存问题"}}`,
		`{"type":"user/message","seq":2,"time":1790519693050,"data":{"content":[{"type":"text","text":"构建缓存为什么不生效"}],"id":"u1","source":{"kind":"user"}}}`,
	} {
		body.Write(enc.EncodeAll([]byte(line+"\n"), nil))
	}
	if err := os.WriteFile(filepath.Join(dir, "session.v4.jsonl.zstd"), body.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	rec := ts.do(http.MethodGet, "/api/v1/sessions/search?q=%E7%BC%93%E5%AD%98", withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("search returned %d: %s", rec.Code, rec.Body.String())
	}
	var result struct {
		Query   string `json:"query"`
		Scanned int    `json:"scanned"`
		Matches []struct {
			SessionID string `json:"sessionId"`
			Title     string `json:"title"`
			Snippet   string `json:"snippet"`
			Role      string `json:"role"`
		} `json:"matches"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("search result is not JSON: %v", err)
	}
	if result.Scanned < 1 {
		t.Errorf("scanned = %d, want the sessions it read to be reported", result.Scanned)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("matches = %+v, want one", result.Matches)
	}
	if result.Matches[0].SessionID != id || result.Matches[0].Role != "user" {
		t.Errorf("match = %+v, want the user message of the session", result.Matches[0])
	}

	// An empty query is a client bug, and saying so beats scanning everything.
	if rec := ts.do(http.MethodGet, "/api/v1/sessions/search?q=", withCookie(ts.token)); rec.Code != http.StatusBadRequest {
		t.Errorf("an empty query returned %d, want 400", rec.Code)
	}
}
