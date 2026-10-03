// Package toolresult reads what a tool call left behind, and bounds it for a phone.
//
// Two mechanical jobs, both of them about telling the truth on a small screen.
//
// # Facts
//
// A recorded result says more than `isError`. DeepSeek Harness ends a command's
// output with its own markers — `[exit code: N]`, `[timed out after Nms]`,
// `[killed by signal: N]`, `[sandbox: …]` — and appends a notice of its own when
// it truncated the output, naming the file that holds the rest. A session log
// may additionally record a structured `{name, code}` error beside the message.
//
// None of that reaches a reader through a boolean, and the boolean is not even
// the right question: DSH reports a non-zero exit rather than erroring, so a
// command that failed is recorded with `isError: false`. Measured over this
// machine's own session logs, 29 of 2,868 results carried a non-zero exit code
// with `isError: false` — every one of them rendered "done" on a phone.
//
// # Bounds
//
// Tool arguments and results are unbounded on the wire today. The same
// measurement found single results of 57 KiB and raw log lines of 139 KiB, both
// of which a phone on cellular is handed whole. A reader who is handed a prefix
// must be told that is what they got, so every bounding function here reports
// whether it cut anything.
//
// Policy — how many bytes a deployment is willing to send — lives in
// `config.Limits`. This package only applies it.
package toolresult

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

/* --------------------------------------------------------------------- facts */

// markers are the lines DSH appends to a result, each on a line of its own.
//
// Taken from the bash tool's own renderer: stdout, then a marked stderr section,
// then these. They are matched anywhere in the text rather than only at the end,
// because a command may print something after what looks like a marker, and
// because a truncated stream puts its notice mid-text when stderr follows.
var (
	exitMarker    = regexp.MustCompile(`^\[exit code: (-?\d+)\]$`)
	timeoutMarker = regexp.MustCompile(`^\[timed out after (\d+)ms\]$`)
	signalMarker  = regexp.MustCompile(`^\[killed by signal: (.+)\]$`)
	stopMarker    = regexp.MustCompile(`^\[stopped: (.+)\]$`)
	sandboxMarker = regexp.MustCompile(`^\[sandbox: (.+)\]$`)

	truncatedMarker = regexp.MustCompile(`^\[output truncated; full output: (.*)\]$`)
	droppedMarker   = regexp.MustCompile(`^\[some output was dropped from memory; full output: (.*)\]$`)

	// missingSpillPath is what DSH writes when it truncated the output and has
	// nowhere to put the rest.
	missingSpillPath = "(unavailable)"
)

// Outcome is what a result says about how the call ended.
//
// Zero value means "the result said nothing", which is different from "the call
// succeeded": callers must not read an empty Outcome as a clean exit.
type Outcome struct {
	// ExitCode is the command's exit status. Only meaningful with HasExitCode.
	ExitCode    int
	HasExitCode bool

	// ErrorName and ErrorCode are DSH's structured error, recorded beside the
	// message in a session log. They are empty for the live ACP path, which
	// carries a status and no error object.
	ErrorName string
	ErrorCode string

	// Notices are the harness's own stop markers, verbatim and already readable:
	// "timed out after 300000ms", "killed by signal 9", "file access denied
	// under plan mode". They are passed through rather than re-phrased, because
	// the vocabulary belongs to the harness and translating it a second time
	// here would let the two drift.
	Notices []string

	// HarnessTruncated is true when DSH says it cut the output itself.
	HarnessTruncated bool
	// SpillPath is where DSH put the full text, when it said.
	SpillPath string
}

// Problem reports whether the result describes something a reader should look
// at: a non-zero exit, a structured error, or a stop the harness called out.
func (o Outcome) Problem() bool {
	return (o.HasExitCode && o.ExitCode != 0) || o.ErrorCode != "" || o.ErrorName != "" || len(o.Notices) > 0
}

// Facts is an Outcome in the vocabulary of the wire.
//
// It is embedded by both the live event payload and the transcript projection,
// which is the point: one definition means the phone reads one set of field
// names for a fact whether the call happened a second ago or a week ago, and a
// field added here cannot reach one route and miss the other.
type Facts struct {
	// ExitCode is absent when the result did not say; a pointer, because zero is
	// a real exit status and must not be confused with silence.
	ExitCode *int `json:"exitCode,omitempty"`
	// ErrorName and ErrorCode are DSH's structured error, when the log recorded
	// one. The live ACP path has no equivalent and leaves them empty.
	ErrorName string `json:"errorName,omitempty"`
	ErrorCode string `json:"errorCode,omitempty"`
	// Notices are the harness's own stop markers, verbatim.
	Notices []string `json:"notices,omitempty"`
	// HarnessTruncated says DSH cut the output itself; SpillPath is where it put
	// the rest, when it had somewhere to put it.
	HarnessTruncated bool   `json:"harnessTruncated,omitempty"`
	SpillPath        string `json:"spillPath,omitempty"`
}

// Facts renders an Outcome for the wire. A nil-able exit code is the one place
// the two differ.
func (o Outcome) Facts() Facts {
	facts := Facts{
		ErrorName:        o.ErrorName,
		ErrorCode:        o.ErrorCode,
		Notices:          o.Notices,
		HarnessTruncated: o.HarnessTruncated,
		SpillPath:        o.SpillPath,
	}
	if o.HasExitCode {
		code := o.ExitCode
		facts.ExitCode = &code
	}
	return facts
}

// Trim reports which of a call's two payloads the deployment bounded.
//
// It travels beside the text rather than inside it, so a card can say "this is
// what was shown, not all of it" without parsing a marker out of the content it
// is warning about.
type Trim struct {
	InputTruncated  bool `json:"inputTruncated,omitempty"`
	OutputTruncated bool `json:"outputTruncated,omitempty"`
}

// Bound applies a deployment's byte budgets to one call's arguments and result.
//
// One function for both producers, so the live frame and the transcript show the
// same bytes for the same call.
func Bound(input, output string, inputBytes, outputBytes int) (string, string, Trim) {
	boundedInput, inputCut := LimitJSON(input, inputBytes)
	boundedOutput, outputCut := Limit(output, outputBytes)
	return boundedInput, boundedOutput, Trim{InputTruncated: inputCut, OutputTruncated: outputCut}
}

// Observe reads the markers DSH leaves in a result's text.
//
// The last match of each kind wins: a command whose own output contains
// something that looks like a marker is followed by the real one.
func Observe(text string) Outcome {
	var out Outcome
	if text == "" {
		return out
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		switch {
		case exitMarker.MatchString(trimmed):
			if n, err := strconv.Atoi(exitMarker.FindStringSubmatch(trimmed)[1]); err == nil {
				out.ExitCode, out.HasExitCode = n, true
			}
		case timeoutMarker.MatchString(trimmed):
			out.Notices = replaceNotice(out.Notices, "timed out after "+timeoutMarker.FindStringSubmatch(trimmed)[1]+"ms")
		case signalMarker.MatchString(trimmed):
			out.Notices = replaceNotice(out.Notices, "killed by signal "+strings.TrimSpace(signalMarker.FindStringSubmatch(trimmed)[1]))
		case stopMarker.MatchString(trimmed):
			out.Notices = replaceNotice(out.Notices, "stopped: "+strings.TrimSpace(stopMarker.FindStringSubmatch(trimmed)[1]))
		case sandboxMarker.MatchString(trimmed):
			out.Notices = replaceNotice(out.Notices, strings.TrimSpace(sandboxMarker.FindStringSubmatch(trimmed)[1]))
		case truncatedMarker.MatchString(trimmed):
			out.HarnessTruncated, out.SpillPath = true, spillPath(truncatedMarker.FindStringSubmatch(trimmed)[1])
		case droppedMarker.MatchString(trimmed):
			out.HarnessTruncated, out.SpillPath = true, spillPath(droppedMarker.FindStringSubmatch(trimmed)[1])
		}
	}
	return out
}

// WithError adds the structured error a session log recorded.
func (o Outcome) WithError(name, code string) Outcome {
	o.ErrorName, o.ErrorCode = strings.TrimSpace(name), strings.TrimSpace(code)
	return o
}

// replaceNotice keeps one notice per kind: a retried stop marker updates rather
// than accumulating, so a reader never sees the same sentence twice.
func replaceNotice(notices []string, next string) []string {
	kind := strings.Fields(next)[0]
	for index, existing := range notices {
		if strings.HasPrefix(existing, kind) {
			notices[index] = next
			return notices
		}
	}
	return append(notices, next)
}

// spillPath normalises the path a truncation notice names. DSH writes
// "(unavailable)" when it has nowhere to put the full text; that is a fact about
// the output, not a path, and conflating the two would have a client trying to
// open a file called "(unavailable)".
func spillPath(raw string) string {
	path := strings.TrimSpace(raw)
	if path == "" || path == missingSpillPath {
		return ""
	}
	return path
}

/* -------------------------------------------------------------------- bounds */

// elision is the line placed where text was removed. It carries the count so a
// reader can tell a two-line cut from a two-hundred-line one.
func elision(omitted int, unit string) string {
	return fmt.Sprintf("\n… [%d %s omitted] …\n", omitted, unit)
}

// Limit keeps a bounded head and tail of text, and reports whether it cut.
//
// Head *and* tail, rather than a prefix: the two places a reader looks on a
// phone are the beginning, where the failure starts, and the end, where the
// summary and the exit status are. Cutting at byte boundaries would split a
// UTF-8 rune, so both cuts are moved to the nearest boundary.
func Limit(text string, max int) (string, bool) {
	if max <= 0 || len(text) <= max {
		return text, false
	}
	// A third of the budget for the tail: the end of a command's output is
	// usually denser than its beginning (a summary, an error, an exit).
	head := max * 2 / 3
	tail := max - head

	headEnd := boundaryAtOrBefore(text, head)
	tailStart := boundaryAtOrAfter(text, len(text)-tail)
	if tailStart <= headEnd {
		return text[:headEnd] + elision(len(text)-headEnd, "bytes"), true
	}
	omitted := tailStart - headEnd
	return text[:headEnd] + elision(omitted, "bytes") + text[tailStart:], true
}

// boundaryAtOrBefore returns the largest index <= at that starts a rune.
func boundaryAtOrBefore(text string, at int) int {
	if at >= len(text) {
		return len(text)
	}
	for at > 0 && !utf8Start(text[at]) {
		at--
	}
	return at
}

// boundaryAtOrAfter returns the smallest index >= at that starts a rune.
func boundaryAtOrAfter(text string, at int) int {
	if at <= 0 {
		return 0
	}
	for at < len(text) && !utf8Start(text[at]) {
		at++
	}
	return at
}

// utf8Start reports whether b begins a UTF-8 sequence.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// jsonClipBudget is how much of a single string value survives the first pass.
// It is deliberately generous: the fields a phone reads to *describe* a call —
// a path, a command, a description, a search pattern — are short, and it is the
// bulk content (a whole file, a long diff) that has to go.
const jsonClipBudget = 4 << 10

// LimitJSON bounds a tool's argument payload while keeping it parseable.
//
// Truncating the JSON text would be simpler and worse: the client parses these
// arguments to say what a call did — a path, a command, which two strings an
// edit replaced — and half a JSON document parses as nothing at all. So the
// payload is decoded, its long strings are clipped, and it is encoded again.
// Every key survives; a reader still learns what the call was about, and the
// honest "this was cut" is carried by the returned flag rather than by a broken
// document.
//
// A payload that is not JSON at all — DSH passes malformed model output through
// as opaque input — falls back to a plain text bound.
func LimitJSON(raw string, max int) (string, bool) {
	if max <= 0 || len(raw) <= max {
		return raw, false
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return Limit(raw, max)
	}

	// Each round halves the per-string budget, so a payload that is over budget
	// because of many medium strings converges too, not just one huge one.
	budget := jsonClipBudget
	for round := 0; round < 8; round++ {
		encoded, err := json.Marshal(clipStrings(value, budget))
		if err != nil {
			break
		}
		if len(encoded) <= max {
			return string(encoded), true
		}
		budget /= 2
	}

	// Nothing worked: a structure of tens of thousands of tiny values cannot be
	// trimmed by clipping. A plain text bound is worse to read but still true.
	return Limit(raw, max)
}

// clipStrings returns a copy of a decoded JSON value with every string longer
// than budget cut to a head and a tail.
func clipStrings(value any, budget int) any {
	switch typed := value.(type) {
	case string:
		if len(typed) <= budget {
			return typed
		}
		head := boundaryAtOrBefore(typed, budget*2/3)
		tail := boundaryAtOrAfter(typed, len(typed)-budget/3)
		if tail <= head {
			return typed[:head] + elision(len(typed)-head, "chars")
		}
		return typed[:head] + elision(tail-head, "chars") + typed[tail:]
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = clipStrings(item, budget)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = clipStrings(item, budget)
		}
		return out
	default:
		return value
	}
}
