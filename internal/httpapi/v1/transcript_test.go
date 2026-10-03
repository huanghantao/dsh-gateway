package v1

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestTranscriptCarriesToolFactsAndBounds covers the two things a tool row needs
// that a transcript used to drop: what the result said about how the call ended,
// and an honest bound on the bytes a phone is asked to hold.
//
// The interesting case is the middle one. DSH reports a non-zero exit rather
// than erroring, so `isError` is false for a command that failed; a phone that
// only had the boolean showed it as done.
func TestTranscriptCarriesToolFactsAndBounds(t *testing.T) {
	ts := newTestServer(t)
	const id = "session-tool-facts"

	// Long enough that the deployment's budget has to cut it, with the markers a
	// real session log carries.
	output := strings.Repeat("compile output line\n", 120) + "[exit code: 2]"
	writeSessionLog(t, ts, id, "/Users/me/code/api", "run the build",
		`{"type":"tool/call","seq":3,"time":1790519693060,"data":{"callId":"c1","name":"bash","arguments":"{\"command\":\"go build ./...\",\"description\":\"Build everything\"}"}}`,
		`{"type":"tool/result","seq":4,"time":1790519693070,"data":{"message":{"toolCallId":"c1","content":[{"type":"text","text":`+jsonString(output)+`}],"isError":false,"id":"r1"},"error":{"name":"FsError","code":"FS_NOT_OBSERVED"}}}`,
	)

	// A small budget, so the fixture stays readable.
	ts.deps.Config.Limits.ToolOutputBytes = 512
	ts.deps.Config.Limits.ToolInputBytes = 512

	rec := ts.do(http.MethodGet, "/api/v1/sessions/"+id+"/transcript", withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("transcript returned %d: %s", rec.Code, rec.Body.String())
	}

	var page struct {
		Items []transcriptToolJSON `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("transcript is not JSON: %v", err)
	}

	var tool *transcriptToolJSON
	for index := range page.Items {
		if page.Items[index].Role == "tool" {
			tool = &page.Items[index]
		}
	}
	if tool == nil {
		t.Fatalf("no tool item in the transcript: %s", rec.Body.String())
	}

	if tool.Pending {
		t.Error("pending = true, want false: the log recorded a result")
	}
	if tool.EndedAt == "" {
		t.Error("endedAt is missing, so a client cannot show how long the call took")
	}
	if tool.ExitCode == nil || *tool.ExitCode != 2 {
		t.Errorf("exitCode = %v, want 2", tool.ExitCode)
	}
	if tool.Error != "FS_NOT_OBSERVED" {
		t.Errorf("errorCode = %q, want the structured error the log recorded", tool.Error)
	}
	if !tool.Trunc {
		t.Error("outputTruncated = false, want true: the output is longer than the budget")
	}
	if len(tool.Output) > 512+128 {
		t.Errorf("output is %d bytes, want it bounded near 512", len(tool.Output))
	}
	if !strings.Contains(tool.Output, "omitted") {
		t.Error("the bounded output does not say that anything was removed")
	}
	if !strings.HasSuffix(strings.TrimSpace(tool.Output), "[exit code: 2]") {
		t.Error("the tail was cut: the exit status is the last thing a reader looks for")
	}
}

// TestBoundingDoesNotReachTheChangeProjection: the transcript bounds a payload
// on the way out, and the change screen folds the same argument text to build
// its diffs. A bound applied in the store would silently cut every diff short,
// which is the failure this pins.
func TestBoundingDoesNotReachTheChangeProjection(t *testing.T) {
	ts := newTestServer(t)
	const id = "session-bounded-edit"

	before := strings.Repeat("old line\n", 200)
	after := strings.Repeat("new line\n", 200)
	arguments := `{"file_path":"/Users/me/code/api/internal/upload/uploader.go","old_string":` +
		jsonString(before) + `,"new_string":` + jsonString(after) + `}`

	writeSessionLog(t, ts, id, "/Users/me/code/api", "rewrite the uploader",
		`{"type":"tool/call","seq":3,"time":1790519693060,"data":{"callId":"c1","name":"edit","arguments":`+jsonString(arguments)+`}}`,
		`{"type":"tool/result","seq":4,"time":1790519693070,"data":{"message":{"toolCallId":"c1","content":[{"type":"text","text":"updated"}],"isError":false,"id":"r1"}}}`,
	)

	// Small enough that the arguments cannot survive whole.
	ts.deps.Config.Limits.ToolInputBytes = 512

	rec := ts.do(http.MethodGet, "/api/v1/sessions/"+id+"/changes", withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("changes returned %d: %s", rec.Code, rec.Body.String())
	}
	var changes struct {
		Files []struct {
			Added   int `json:"added"`
			Deleted int `json:"deleted"`
			Hunks   []struct {
				Added   int `json:"added"`
				Deleted int `json:"deleted"`
			} `json:"hunks"`
		} `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &changes); err != nil {
		t.Fatalf("changes is not JSON: %v", err)
	}
	if len(changes.Files) != 1 || len(changes.Files[0].Hunks) != 1 {
		t.Fatalf("changes = %s, want one file with one hunk", rec.Body.String())
	}
	if changes.Files[0].Added < 200 || changes.Files[0].Deleted < 200 {
		t.Errorf("the change reports +%d −%d, want the whole edit: bounding reached the store",
			changes.Files[0].Added, changes.Files[0].Deleted)
	}
}

// TestTranscriptStillReportsPendingCall: a call whose result has not been written
// yet is what a phone opening a session mid-turn sees. It has to arrive as
// running rather than as a finished card with no output.
func TestTranscriptStillReportsPendingCall(t *testing.T) {
	ts := newTestServer(t)
	const id = "session-pending"

	writeSessionLog(t, ts, id, "/Users/me/code/api", "start a long build",
		`{"type":"tool/call","seq":3,"time":1790519693060,"data":{"callId":"c1","name":"bash","arguments":"{\"command\":\"sleep 600\"}"}}`,
	)

	rec := ts.do(http.MethodGet, "/api/v1/sessions/"+id+"/transcript", withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("transcript returned %d: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []struct {
			Role    string `json:"role"`
			Pending bool   `json:"pending"`
			EndedAt string `json:"endedAt"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("transcript is not JSON: %v", err)
	}
	for _, item := range page.Items {
		if item.Role != "tool" {
			continue
		}
		if !item.Pending {
			t.Error("pending = false, want true: the log has no result for this call")
		}
		if item.EndedAt != "" {
			t.Errorf("endedAt = %q, want empty for a call that has not ended", item.EndedAt)
		}
		return
	}
	t.Fatal("no tool item was projected")
}

// transcriptToolJSON is the subset of a transcript item these tests read.
type transcriptToolJSON struct {
	Role     string   `json:"role"`
	Tool     string   `json:"tool"`
	Output   string   `json:"output"`
	Pending  bool     `json:"pending"`
	EndedAt  string   `json:"endedAt"`
	ExitCode *int     `json:"exitCode"`
	Error    string   `json:"errorCode"`
	Notices  []string `json:"notices"`
	Trunc    bool     `json:"outputTruncated"`
}

// jsonString encodes a Go string as a JSON string literal, so a fixture can
// embed multi-line tool text in a log line without hand-escaping it.
func jsonString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
