package v1

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/huanghantao/dsh-gateway/internal/harness"
)

// heldStub is the harness's own account of what it is still holding a handle
// for. In production that is the agent host's client; here it is a list a test
// controls, which is what lets these tests be about the merge rather than about
// DSH's attachment rules.
type heldStub struct{ sessions []harness.SessionInfo }

func (h *heldStub) HeldSessions() []harness.SessionInfo {
	return append([]harness.SessionInfo(nil), h.sessions...)
}

// listRow asks for a page and returns one row from it.
func listRow(t *testing.T, ts *testServer, path, id string) (sessionView, bool) {
	t.Helper()
	rec := ts.do(http.MethodGet, path, withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s returned %d: %s", path, rec.Code, rec.Body.String())
	}
	var page sessionPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	for _, view := range page.Sessions {
		if view.ID == id {
			return view, true
		}
	}
	return sessionView{}, false
}

// TestSessionListShowsASessionTheHarnessStillHolds is the report this merge was
// written for.
//
// DSH's `session/list` skips every session that is live in the process answering
// it, and the agent host holds a session for as long as its child lives — a
// release is bookkeeping, because detaching is not something DSH offers. So once
// the lease covering a session went away (an idle timeout, or a redeploy) the
// phone had no row for a session whose agent was still working, and no way to
// reach it either: the list is where an id comes from.
func TestSessionListShowsASessionTheHarnessStillHolds(t *testing.T) {
	ts := newTestServer(t)
	const id = "session-held"
	workspace := ts.deps.Config.Workspaces[0]
	writeSessionLog(t, ts, id, workspace, "子agent通知优化")

	// Not in the harness's listing and not leased: held is the only source that
	// knows this session exists.
	ts.deps.Held = &heldStub{sessions: []harness.SessionInfo{{ID: id, Workspace: workspace}}}

	row, ok := listRow(t, ts, "/api/v1/sessions", id)
	if !ok {
		t.Fatal("a session the harness still holds is missing from the list, which is " +
			"the whole of the operator's complaint: it vanished")
	}
	if row.Archived {
		t.Error("the held row came back archived")
	}
	if !row.HistoryAvailable {
		t.Error("the held row reports no history although its log is readable")
	}
	if row.Title != "子agent通知优化" {
		t.Errorf("title = %q, want the title from the log: a held session is a session, "+
			"not a placeholder row", row.Title)
	}
	if row.Leased {
		t.Error("the held row says it is leased; nothing is driving it, which is exactly " +
			"why it needed merging back")
	}
}

// TestAMergedBackSessionStillObeysTheListFilters keeps the merge from being a
// side door around the operator's own decisions and the page's own scope.
func TestAMergedBackSessionStillObeysTheListFilters(t *testing.T) {
	ts := newTestServer(t)
	const id = "session-held"
	workspace := ts.deps.Config.Workspaces[0]
	writeSessionLog(t, ts, id, workspace, "a conversation")
	ts.deps.Held = &heldStub{sessions: []harness.SessionInfo{{ID: id, Workspace: workspace}}}

	// Archived means archived, wherever the row came from: a held session that
	// ignored curation would make archiving look broken.
	rec := ts.do(http.MethodPost, "/api/v1/sessions/curate",
		withCookie(ts.token), withJSON(`{"ids":["`+id+`"],"archived":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("curate returned %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := listRow(t, ts, "/api/v1/sessions", id); ok {
		t.Error("an archived session came back into the active list through the merge")
	}
	if _, ok := listRow(t, ts, "/api/v1/sessions?state=archived", id); !ok {
		t.Error("an archived session is missing from the archive view")
	}

	// A page that asked for one workspace must not collect a row from another.
	// No log is written for this one, so the workspace it carries is the one the
	// harness reported.
	ts.deps.Held = &heldStub{sessions: []harness.SessionInfo{
		{ID: "session-elsewhere", Workspace: "/somewhere/else"},
	}}
	if _, ok := listRow(t, ts, "/api/v1/sessions", "session-elsewhere"); !ok {
		t.Error("an unfiltered list dropped a held session from another workspace")
	}
	scoped := "/api/v1/sessions?workspace=" + url.QueryEscape(workspace)
	if _, ok := listRow(t, ts, scoped, "session-elsewhere"); ok {
		t.Error("a page scoped to one workspace carried a session from another")
	}
}
