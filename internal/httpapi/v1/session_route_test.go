package v1

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestSessionRouteComesFromTheLogForASessionNobodyHolds covers the sessions the
// gateway is not holding: the desktop's, and its own between leases.
//
// ACP reveals a session's model and effort only while the session is attached
// to this process, so for every other session the log is the only record. The
// parser reads it out of `request/header`, and this pins that the value reaches
// the wire: without it the phone has nothing to show but "Default model" and
// "Default effort", which is exactly what the operator sees on the Settings
// screen after the gateway restarts.
func TestSessionRouteComesFromTheLogForASessionNobodyHolds(t *testing.T) {
	ts := newTestServer(t)

	writeSessionLog(t, ts, "session-test", "/Users/me/code/api", "route test",
		`{"type":"request/header","seq":3,"time":1790519692980,"data":{"header":{"config":{"provider":"command-code","model":"deepseek/deepseek-v4.1-flash","reasoningEffort":"max","maxTokens":128000},"tools":[]},"reason":"initial"}}`,
	)

	rec := ts.do(http.MethodGet, "/api/v1/sessions", withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sessions returned %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Sessions []struct {
			ID              string `json:"id"`
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoningEffort"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(body.Sessions))
	}

	got := body.Sessions[0]
	if got.Model != "deepseek/deepseek-v4.1-flash" {
		t.Errorf("model = %q, want the model the log ran on", got.Model)
	}
	if got.ReasoningEffort != "max" {
		t.Errorf("reasoningEffort = %q, want the effort the log ran on", got.ReasoningEffort)
	}
}
