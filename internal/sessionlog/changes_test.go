package sessionlog

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests drive FoldChanges directly. It is a pure function over transcript
// items, so the rules about what counts as a change — and about what can be
// undone — are asserted without a log on disk.

func toolCall(seq int64, id, tool string, args map[string]any) Item {
	encoded, _ := json.Marshal(args)
	return Item{ID: id, Seq: seq, Role: RoleTool, Tool: tool, Input: string(encoded)}
}

func userMessage(seq int64, text string) Item {
	return Item{ID: "u", Seq: seq, Role: RoleUser, Text: text}
}

// TestEditBecomesAFileChange is the base case: one edit, one file, one hunk with
// counts a reader can trust.
func TestEditBecomesAFileChange(t *testing.T) {
	workspace := "/Users/me/code/api"
	changes := FoldChanges([]Item{
		userMessage(1, "fix the retry"),
		toolCall(2, "c1", "edit", map[string]any{
			"file_path":  workspace + "/internal/cache/store.go",
			"old_string": "a\nb\nc",
			"new_string": "a\nx\nc",
		}),
	}, workspace)

	if len(changes.Files) != 1 {
		t.Fatalf("files = %+v, want one", changes.Files)
	}
	file := changes.Files[0]
	if file.Display != "internal/cache/store.go" {
		t.Errorf("display = %q, want the path relative to the workspace", file.Display)
	}
	if file.Added != 1 || file.Deleted != 1 {
		t.Errorf("added/deleted = %d/%d, want 1/1", file.Added, file.Deleted)
	}
	if file.Edits != 1 || file.Writes != 0 {
		t.Errorf("edits/writes = %d/%d, want 1/0", file.Edits, file.Writes)
	}
	if !file.Revertible {
		t.Errorf("revertible = false (%s); an edit is exactly what can be undone", file.Reason)
	}
	if len(file.Hunks) != 1 {
		t.Fatalf("hunks = %+v, want one", file.Hunks)
	}
	body := strings.Join(file.Hunks[0].Lines, "\n")
	if !strings.Contains(body, "-b") || !strings.Contains(body, "+x") {
		t.Errorf("hunk = %q, want the replaced line marked on both sides", body)
	}
	if file.Hunks[0].CallID != "c1" {
		t.Errorf("callId = %q, want the tool call it came from", file.Hunks[0].CallID)
	}
}

// TestSeveralEditsToOneFileAggregate: the screen is about files, so a file
// edited five times is one row with five hunks, in the order they happened.
func TestSeveralEditsToOneFileAggregate(t *testing.T) {
	path := "/ws/main.go"
	changes := FoldChanges([]Item{
		toolCall(1, "c1", "edit", map[string]any{"file_path": path, "old_string": "one", "new_string": "two"}),
		toolCall(2, "c2", "edit", map[string]any{"file_path": path, "old_string": "two", "new_string": "three"}),
		toolCall(3, "c3", "edit", map[string]any{"file_path": path, "old_string": "three", "new_string": "four"}),
	}, "/ws")

	if len(changes.Files) != 1 {
		t.Fatalf("files = %+v, want one row for one file", changes.Files)
	}
	file := changes.Files[0]
	if file.Edits != 3 || len(file.Hunks) != 3 {
		t.Fatalf("edits/hunks = %d/%d, want 3/3", file.Edits, len(file.Hunks))
	}
	if file.Hunks[0].CallID != "c1" || file.Hunks[2].CallID != "c3" {
		t.Errorf("hunks are out of order: %q then %q", file.Hunks[0].CallID, file.Hunks[2].CallID)
	}

	// The undo plan is the flat sequence, in application order, because an undo
	// has to walk it backwards.
	if len(changes.Edits) != 3 {
		t.Fatalf("edits = %+v, want three", changes.Edits)
	}
	if changes.Edits[0].New != "two" || changes.Edits[2].New != "four" {
		t.Errorf("plan = %+v, want application order", changes.Edits)
	}
}

// TestWholeFileWriteIsListedButNotUndoable: the log records what was written and
// not what it replaced, so the count is honest and the undo is refused.
func TestWholeFileWriteIsListedButNotUndoable(t *testing.T) {
	changes := FoldChanges([]Item{
		toolCall(1, "c1", "write", map[string]any{
			"file_path": "/ws/new.go",
			"content":   "package main\n\nfunc main() {}\n",
		}),
	}, "/ws")

	file := changes.Files[0]
	if file.Writes != 1 {
		t.Errorf("writes = %d, want 1", file.Writes)
	}
	if file.Added != 3 {
		t.Errorf("added = %d, want the three lines written", file.Added)
	}
	if file.Deleted != 0 {
		t.Errorf("deleted = %d, want 0: the log records no previous contents to count", file.Deleted)
	}
	if file.Revertible {
		t.Error("a whole-file write was reported as undoable")
	}
	if file.Reason == "" {
		t.Error("an undoable=false file carries no reason")
	}
	if !file.Hunks[0].WholeFile {
		t.Error("the hunk is not marked as a whole-file write")
	}
	for _, line := range file.Hunks[0].Lines {
		if !strings.HasPrefix(line, "+") {
			t.Errorf("line = %q, want a whole-file write to be all additions", line)
		}
	}
}

// TestPureDeletionIsNotUndoable: text that was removed leaves nothing to locate,
// and offering an undo that would have to guess is worse than not offering one.
func TestPureDeletionIsNotUndoable(t *testing.T) {
	changes := FoldChanges([]Item{
		toolCall(1, "c1", "edit", map[string]any{
			"file_path":  "/ws/main.go",
			"old_string": "gone\n",
			"new_string": "",
		}),
	}, "/ws")

	file := changes.Files[0]
	if file.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", file.Deleted)
	}
	if file.Revertible {
		t.Error("a deletion was reported as undoable; the log records no position for it")
	}
}

// TestBinaryContentIsFlaggedAndNotCompared keeps a screenshot out of a diff.
func TestBinaryContentIsFlaggedAndNotCompared(t *testing.T) {
	changes := FoldChanges([]Item{
		toolCall(1, "c1", "write", map[string]any{
			"file_path": "/ws/logo.png",
			"content":   "\x89PNG\x00\x01\x02",
		}),
	}, "/ws")

	file := changes.Files[0]
	if !file.Binary {
		t.Error("binary = false, want a NUL byte to be recognised")
	}
	if len(file.Hunks) != 0 {
		t.Errorf("hunks = %+v, want none for binary content", file.Hunks)
	}
	if file.Revertible {
		t.Error("binary content was reported as undoable")
	}
}

// TestOtherToolsAndMalformedArgumentsAreIgnored: a bash command may well have
// changed files, and this projection does not claim to know that. What it must
// not do is crash or invent a change.
func TestOtherToolsAndMalformedArgumentsAreIgnored(t *testing.T) {
	changes := FoldChanges([]Item{
		toolCall(1, "c1", "bash", map[string]any{"command": "sed -i s/a/b/ main.go"}),
		toolCall(2, "c2", "read", map[string]any{"file_path": "/ws/main.go"}),
		{ID: "c3", Seq: 3, Role: RoleTool, Tool: "edit", Input: "{not json"},
		toolCall(4, "c4", "edit", map[string]any{"new_string": "x"}),
	}, "/ws")

	if len(changes.Files) != 0 {
		t.Errorf("files = %+v, want none: none of these record a change this build understands", changes.Files)
	}
	if changes.Summary.Edits != 0 {
		t.Errorf("edits = %d, want 0", changes.Summary.Edits)
	}
}

// TestPathOutsideTheWorkspaceIsKeptAbsolute: the agent can edit outside its
// working directory, and a change screen that hid those would be lying by
// omission.
func TestPathOutsideTheWorkspaceIsKeptAbsolute(t *testing.T) {
	changes := FoldChanges([]Item{
		toolCall(1, "c1", "edit", map[string]any{
			"file_path": "/etc/hosts", "old_string": "a", "new_string": "b",
		}),
	}, "/ws")

	if len(changes.Files) != 1 {
		t.Fatalf("files = %+v, want the outside file listed", changes.Files)
	}
	if changes.Files[0].Display != "/etc/hosts" {
		t.Errorf("display = %q, want the absolute path", changes.Files[0].Display)
	}
}

// TestHunkKeepsThreeLinesOfContext pins the shape a reader scans: enough context
// to recognise the place, not the whole file.
func TestHunkKeepsThreeLinesOfContext(t *testing.T) {
	long := "c1\nc2\nc3\nc4\nc5\nc6"
	changes := FoldChanges([]Item{
		toolCall(1, "c1", "edit", map[string]any{
			"file_path":  "/ws/main.go",
			"old_string": long + "\nOLD\n" + long,
			"new_string": long + "\nNEW\n" + long,
		}),
	}, "/ws")

	lines := changes.Files[0].Hunks[0].Lines
	context := 0
	for _, line := range lines {
		if strings.HasPrefix(line, " ") {
			context++
		}
	}
	if context != 6 {
		t.Errorf("context lines = %d, want 3 on each side of the change: %q", context, lines)
	}
	if len(lines) != 8 {
		t.Errorf("hunk has %d lines, want 3 + 1 + 1 + 3: %q", len(lines), lines)
	}
}

// TestFileCapIsReportedNotSilent: a session that touched a thousand files must
// say it is showing a slice, because a reader who cannot tell will assume they
// are looking at everything.
func TestFileCapIsReportedNotSilent(t *testing.T) {
	items := make([]Item, 0, maxChangeFiles+10)
	for i := range maxChangeFiles + 10 {
		items = append(items, toolCall(int64(i+1), "c", "write", map[string]any{
			"file_path": "/ws/file" + itoaTest(i) + ".go",
			"content":   "x\n",
		}))
	}
	changes := FoldChanges(items, "/ws")

	if len(changes.Files) != maxChangeFiles {
		t.Errorf("files = %d, want the cap %d", len(changes.Files), maxChangeFiles)
	}
	if changes.Summary.Total <= maxChangeFiles {
		t.Errorf("total = %d, want the true count so the header can say what was left out", changes.Summary.Total)
	}
	if !changes.Summary.Truncated {
		t.Error("truncated = false; the reader has no way to know the list is a slice")
	}
}

// itoaTest avoids pulling strconv in for one loop.
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestNoChangesIsAnEmptyProjectionNotAnError pins the shape a client renders for
// a session that only answered a question.
func TestNoChangesIsAnEmptyProjectionNotAnError(t *testing.T) {
	changes := FoldChanges([]Item{userMessage(1, "what does this do?")}, "/ws")

	if len(changes.Files) != 0 {
		t.Errorf("files = %+v, want none", changes.Files)
	}
	if changes.Summary.Source != SourceToolCalls {
		t.Errorf("source = %q, want %q", changes.Summary.Source, SourceToolCalls)
	}
}
