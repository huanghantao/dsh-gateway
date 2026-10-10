package toolresult

import "testing"

// TestCallNameStripsTheRoute pins the normalisation everything else depends on.
//
// A tool arrives named for the route it came through as often as for itself:
// `mcp__filesystem__edit` from an MCP server, `bash: subagent` from a wrapper.
// Classifying on the raw string would miss every one of those, and — the reason
// this is one function rather than a convention — it would miss them
// *differently* on the two producers, because only the ACP stream carries the
// decoration at all. A log row records the name the model called.
func TestCallNameStripsTheRoute(t *testing.T) {
	cases := map[string]string{
		"edit":                    "edit",
		"Edit":                    "edit",
		"  bash  ":                "bash",
		"mcp__filesystem__edit":   "edit",
		"mcp__server__subagent":   "subagent",
		"bash: subagent":          "subagent",
		"MCP__server__write":      "write",
		"mcp__server__tool__edit": "edit",
		"something:else:workflow": "workflow",
		"":                        "",
	}
	for name, want := range cases {
		if got := CallName(name); got != want {
			t.Errorf("CallName(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestFileToolAndDelegationToolAreTheOnlyLists pins which calls a turn's record
// counts as a file change and as a delegation.
//
// It is one list for the whole gateway: the change screen, the ACP bridge and
// the log watcher all ask this. Three copies used to exist — a map in the
// projection and two switches in the notifier — with a comment admitting that a
// tool added to one and not the others "shows up as a test that disagrees".
func TestFileToolAndDelegationToolAreTheOnlyLists(t *testing.T) {
	files := map[string]bool{
		"edit":                  true,
		"write":                 true,
		"mcp__filesystem__edit": true,
		"bash":                  false,
		"read":                  false,
		"notebook_edit":         false, // its arguments are its own shape
		"subagent":              false,
	}
	for name, want := range files {
		if got := FileTool(name); got != want {
			t.Errorf("FileTool(%q) = %v, want %v", name, got, want)
		}
	}

	delegations := map[string]bool{
		"subagent":              true,
		"subagent_fork":         true,
		"workflow":              true,
		"mcp__agents__subagent": true,
		"bash":                  false,
		"edit":                  false,
		"task":                  false,
	}
	for name, want := range delegations {
		if got := DelegationTool(name); got != want {
			t.Errorf("DelegationTool(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestFailedReadsTheResultNotOnlyTheError is the signal a summary depends on.
//
// DSH reports a non-zero exit as a status rather than an error, so a count built
// from the boolean alone calls a failed build a success. Measured on this
// machine's own logs, 29 of 2,868 results carried a non-zero exit with
// `isError: false` — every one of them reported "done".
func TestFailedReadsTheResultNotOnlyTheError(t *testing.T) {
	zero, one := 0, 1
	cases := []struct {
		name    string
		isError bool
		facts   Facts
		want    bool
	}{
		{"a clean result", false, Facts{}, false},
		{"a clean exit code", false, Facts{ExitCode: &zero}, false},
		{"the harness said it failed", true, Facts{}, true},
		{"the command exited non-zero", false, Facts{ExitCode: &one}, true},
		{"an error name and a clean exit", true, Facts{ExitCode: &zero}, true},
	}
	for _, tc := range cases {
		if got := Failed(tc.isError, tc.facts); got != tc.want {
			t.Errorf("%s: Failed(%v, %+v) = %v, want %v", tc.name, tc.isError, tc.facts, got, tc.want)
		}
	}
}
