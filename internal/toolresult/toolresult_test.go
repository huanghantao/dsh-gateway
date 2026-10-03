package toolresult

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// A real tail from this machine's own session logs: the markers DSH appends to
// a command's output, in the order its renderer produces them.
const realBashTail = `web/src/views/conversation.ts(466,31): error TS2304: Cannot find name 'outcomeBadge'.
[stderr]
tsc exited
[output truncated; full output: /tmp/dsh-output/9f2c1ab.txt]
[exit code: 2]`

func TestObserveRealMarkers(t *testing.T) {
	out := Observe(realBashTail)

	if !out.HasExitCode || out.ExitCode != 2 {
		t.Fatalf("exit code = (%d, %v), want (2, true)", out.ExitCode, out.HasExitCode)
	}
	if !out.HarnessTruncated {
		t.Error("HarnessTruncated = false, want true: the result names a spill file")
	}
	if out.SpillPath != "/tmp/dsh-output/9f2c1ab.txt" {
		t.Errorf("SpillPath = %q, want the path from the notice", out.SpillPath)
	}
	if !out.Problem() {
		t.Error("Problem() = false, want true for a non-zero exit")
	}
}

func TestObserveExitCodes(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		want    int
		present bool
		problem bool
	}{
		{"clean exit", "ok  github.com/acme/api\n[exit code: 0]", 0, true, false},
		{"failure", "FAIL\n[exit code: 1]", 1, true, true},
		{"no marker", "just output", 0, false, false},
		{"empty", "", 0, false, false},
		// A command that prints something marker-shaped is followed by the real
		// marker, so the last match has to win.
		{"echoed then real", "[exit code: 7]\n[exit code: 0]", 0, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Observe(tc.text)
			if out.HasExitCode != tc.present {
				t.Fatalf("HasExitCode = %v, want %v", out.HasExitCode, tc.present)
			}
			if out.HasExitCode && out.ExitCode != tc.want {
				t.Fatalf("ExitCode = %d, want %d", out.ExitCode, tc.want)
			}
			if out.Problem() != tc.problem {
				t.Fatalf("Problem() = %v, want %v", out.Problem(), tc.problem)
			}
		})
	}
}

func TestObserveStopMarkers(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"partial\n[timed out after 300000ms]", "timed out after 300000ms"},
		{"partial\n[killed by signal: 9]", "killed by signal 9"},
		{"partial\n[stopped: operator cancelled]", "stopped: operator cancelled"},
		{"[sandbox: file access denied under plan mode]", "file access denied under plan mode"},
	}
	for _, tc := range cases {
		out := Observe(tc.text)
		if len(out.Notices) != 1 || out.Notices[0] != tc.want {
			t.Errorf("Observe(%q).Notices = %q, want [%q]", tc.text, out.Notices, tc.want)
		}
		if !out.Problem() {
			t.Errorf("Observe(%q).Problem() = false, want true", tc.text)
		}
	}
}

func TestObserveTruncationWithoutSpillPath(t *testing.T) {
	out := Observe("tail only\n[output truncated; full output: (unavailable)]")
	if !out.HarnessTruncated {
		t.Fatal("HarnessTruncated = false, want true")
	}
	if out.SpillPath != "" {
		t.Fatalf("SpillPath = %q, want empty: (unavailable) is not a path", out.SpillPath)
	}
}

func TestWithError(t *testing.T) {
	out := Observe("no markers here").WithError("FsError", "FS_NOT_OBSERVED")
	if out.ErrorCode != "FS_NOT_OBSERVED" || out.ErrorName != "FsError" {
		t.Fatalf("error = %q/%q, want FsError/FS_NOT_OBSERVED", out.ErrorName, out.ErrorCode)
	}
	if !out.Problem() {
		t.Error("Problem() = false, want true for a structured error")
	}
}

func TestLimitLeavesSmallTextAlone(t *testing.T) {
	text := "a short result"
	got, truncated := Limit(text, 1024)
	if got != text || truncated {
		t.Fatalf("Limit = (%q, %v), want the text unchanged", got, truncated)
	}
}

func TestLimitKeepsBothEnds(t *testing.T) {
	head := strings.Repeat("h", 700)
	tail := strings.Repeat("t", 300)
	got, truncated := Limit(head+strings.Repeat("m", 5000)+tail, 1200)

	if !truncated {
		t.Fatal("truncated = false, want true")
	}
	if !strings.HasPrefix(got, head) {
		t.Error("the beginning of the result was dropped; a reader needs where it started")
	}
	if !strings.HasSuffix(got, tail) {
		t.Error("the end of the result was dropped; that is where the summary and exit status are")
	}
	if !strings.Contains(got, "omitted") {
		t.Error("the elision does not say anything was removed")
	}
	if len(got) > 1200+64 {
		t.Errorf("result is %d bytes, want it bounded near 1200", len(got))
	}
}

func TestLimitDoesNotSplitRunes(t *testing.T) {
	// Three-byte runes throughout, so a byte-aligned cut lands mid-character.
	text := strings.Repeat("界", 400)
	got, truncated := Limit(text, 500)
	if !truncated {
		t.Fatal("truncated = false, want true")
	}
	if !utf8.ValidString(got) {
		t.Fatal("the bounded text is not valid UTF-8: a rune was split")
	}
}

func TestLimitJSONKeepsEveryKey(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"file_path": "/Users/me/code/api/internal/upload/retry.go",
		"content":   strings.Repeat("func x() {}\n", 4000),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, truncated := LimitJSON(string(raw), 2048)
	if !truncated {
		t.Fatal("truncated = false, want true")
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("the bounded payload is not parseable JSON: %v", err)
	}
	if decoded["file_path"] != "/Users/me/code/api/internal/upload/retry.go" {
		t.Error("a short value was lost; the phone describes the call from these fields")
	}
	content, _ := decoded["content"].(string)
	if len(content) >= len(strings.Repeat("func x() {}\n", 4000)) {
		t.Error("the bulk content was not cut")
	}
	if !strings.Contains(content, "omitted") {
		t.Error("the cut is not marked inside the value")
	}
}

func TestLimitJSONIsIdentityUnderBudget(t *testing.T) {
	raw := `{"command":"pwd"}`
	got, truncated := LimitJSON(raw, 1024)
	if got != raw || truncated {
		t.Fatalf("LimitJSON = (%q, %v), want the payload unchanged", got, truncated)
	}
}

func TestLimitJSONFallsBackForOpaqueInput(t *testing.T) {
	// DSH passes malformed model output through as opaque input.
	raw := strings.Repeat("not json at all ", 100)
	got, truncated := LimitJSON(raw, 200)
	if !truncated {
		t.Fatal("truncated = false, want true")
	}
	if len(got) > 264 {
		t.Errorf("result is %d bytes, want it bounded near 200", len(got))
	}
}
