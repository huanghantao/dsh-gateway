package sessionlog

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/huanghantao/dsh-gateway/internal/errx"
)

// This file projects "what did this session change" out of the session log.
//
// It is a projection and not a workspace diff, deliberately. The gateway never
// reads the files it reports on, which is the property the whole security story
// rests on, and a live diff would silently rewrite itself as the tree moved on
// under a session that had already finished.
//
// It is also, for now, the only answer available. DeepSeek Harness has a richer
// mechanism of its own — `@deepseek-ai/dsh-workspace-changes` records per-turn
// changes from git working-tree snapshots and serves real unified diffs — but
// the summary and the comparisons stay in the Host's memory and its temporary
// directory, disposed with the Session, and the `workspace/changes` event it
// appends to the log carries nothing but a turn number. None of that is
// reachable over ACP. So the gateway reconstructs what it can from the file
// tools' own recorded arguments, which is complete for the edits those tools
// made and silent about anything else.
//
// The field names are the harness's own (`path`, `display`, `added`, `deleted`,
// `binary`, `oversized`, `hunks`), so that a future in-process bridge which can
// reach the real thing is a change of source rather than a change of contract.

// Change sources, reported in Summary.Source.
const (
	// SourceToolCalls means the projection read the file tools' recorded
	// arguments. It covers what `edit` and `write` did and nothing else.
	SourceToolCalls = "tool-calls"
)

// fileTools are the tools whose arguments record a file mutation this build
// understands. Anything else — a `bash` running `sed -i`, a `notebook_edit` with
// its own argument shape — is not guessed at.
var fileTools = map[string]bool{
	"edit":  true,
	"write": true,
}

// Bounds on one projection. A session that edited a thousand files should give
// a phone something it can render, and say what it left out.
const (
	maxChangeFiles    = 200
	maxHunksPerFile   = 40
	maxHunkLines      = 400
	changeContextSize = 3
)

// Hunk is one recorded replacement, rendered as unified-diff lines.
//
// There are no line numbers, because the log does not record where in the file
// an edit landed — only what it replaced. Inventing them would be worse than
// omitting them: a reader who scrolls to the wrong line has been lied to.
type Hunk struct {
	// Seq and CallID tie the hunk to the tool card in the transcript.
	Seq    int64  `json:"seq"`
	CallID string `json:"callId,omitempty"`
	// Tool is the tool that made the change.
	Tool string `json:"tool"`
	// Lines is the hunk body, each line prefixed with "+", "-" or a space.
	Lines []string `json:"lines"`
	// Added and Deleted count the "+" and "-" lines.
	Added   int `json:"added"`
	Deleted int `json:"deleted"`
	// WholeFile marks a write whose previous contents the log does not record.
	// The hunk is then all additions, and the change cannot be undone.
	WholeFile bool `json:"wholeFile,omitempty"`
	// Truncated is true when the hunk was cut to keep the response bounded.
	Truncated bool `json:"truncated,omitempty"`
}

// FileChange is one file the session changed.
type FileChange struct {
	// Path is the file's path as the tools named it.
	Path string `json:"path"`
	// Display is the path as a reader thinks of it: relative to the session's
	// workspace when it is inside one.
	Display string `json:"display"`
	// Added and Deleted are line counts over every hunk below. Deleted counts
	// only what the log recorded, so a file that was overwritten whole reports
	// its additions and no deletions rather than a guess.
	Added   int `json:"added"`
	Deleted int `json:"deleted"`
	// Binary marks content with a NUL byte, which has no lines to compare.
	Binary bool `json:"binary,omitempty"`
	// Edits is how many tool calls touched the file.
	Edits int `json:"edits"`
	// Writes is how many of those were whole-file writes.
	Writes int `json:"writes,omitempty"`
	// Hunks are the changes in the order they were applied.
	Hunks []Hunk `json:"hunks"`
	// Truncated is true when hunks were dropped to keep the response bounded.
	Truncated bool `json:"truncated,omitempty"`
	// Revertible is false when the log does not record enough to undo this
	// file, and Reason says why.
	Revertible bool   `json:"revertible"`
	Reason     string `json:"reason,omitempty"`

	// pureDeletion records that some change removed text without recording a
	// position for it. It is not on the wire: a client is told the file cannot
	// be undone and why, through Reason.
	pureDeletion bool
}

// Edit is one recorded replacement, in the order it was applied.
//
// It is what an undo walks backwards. Old and New are carried rather than
// derived from the rendered hunk so that reversing a change is exact: the hunk
// is trimmed to three lines of context for reading, and undoing from it would
// restore a truncated file.
type Edit struct {
	Path      string
	Tool      string
	CallID    string
	Seq       int64
	Old       string
	New       string
	WholeFile bool
}

// ChangesSummary is what a header line shows.
type ChangesSummary struct {
	// Files is how many files are listed, and Total how many were seen before
	// the cap.
	Files int `json:"files"`
	Total int `json:"total"`
	// Added and Deleted are totals over every file seen, including those the cap
	// omitted.
	Added   int `json:"added"`
	Deleted int `json:"deleted"`
	// Edits is how many recorded changes there were.
	Edits int `json:"edits"`
	// Source names where this came from. See SourceToolCalls.
	Source string `json:"source"`
	// Truncated is true when the file list or any hunk list was cut short.
	Truncated bool `json:"truncated,omitempty"`
}

// Changes is one session's recorded file changes.
type Changes struct {
	Files   []FileChange   `json:"files"`
	Summary ChangesSummary `json:"summary"`
	// Edits is the flat, ordered plan an undo walks. It is deliberately not on
	// the wire: a client reads Files, and the undo path is the server's.
	Edits []Edit `json:"-"`
}

// Changes projects a session's recorded file changes.
//
// A session with no log has changed nothing, so that is answered with an empty
// projection rather than a not-found: the operator who just created a session
// and asked what it had done is owed "nothing yet", not an error about a file
// they never knew existed. An unsupported format still reports itself, because
// there the honest answer is "this build cannot read it".
func (s *Store) Changes(ctx context.Context, sessionID string) (Changes, error) {
	items, meta, err := s.Items(ctx, sessionID)
	switch {
	case err == nil:
		return FoldChanges(items, meta.Workspace), nil
	case errx.KindOf(err) == errx.KindNotFound:
		return FoldChanges(nil, ""), nil
	default:
		return Changes{}, err
	}
}

// FoldChanges builds the projection from transcript items. It is pure, so the
// rules below are a thing a test can hold still without a log on disk.
func FoldChanges(items []Item, workspace string) Changes {
	out := Changes{Summary: ChangesSummary{Source: SourceToolCalls}}
	byPath := map[string]int{} // path -> index in out.Files
	// Counted separately from byPath, because a path the cap left out still
	// happened: a header that reported only what it managed to list would
	// understate the session by exactly the amount it truncated.
	total := map[string]bool{}

	// raw arguments, parsed once per item.
	for _, item := range items {
		if item.Role != RoleTool || !fileTools[item.Tool] {
			continue
		}
		change, ok := parseFileTool(item)
		if !ok {
			continue
		}
		change.Path = normalisePath(change.Path)

		out.Summary.Edits++
		out.Edits = append(out.Edits, change.edit())

		index, seen := byPath[change.Path]
		if !seen {
			total[change.Path] = true
			if len(out.Files) >= maxChangeFiles {
				out.Summary.Truncated = true
				continue
			}
			index = len(out.Files)
			byPath[change.Path] = index
			out.Files = append(out.Files, FileChange{
				Path:    change.Path,
				Display: displayPath(change.Path, workspace),
			})
		}
		file := &out.Files[index]
		file.Edits++
		file.Added += change.added
		file.Deleted += change.deleted
		if change.Binary {
			file.Binary = true
		}
		if change.WholeFile {
			file.Writes++
		}
		if change.pureDeletion {
			file.pureDeletion = true
		}
		// Binary content has no lines to compare, so it is listed, counted and
		// flagged, and given no hunk.
		if !change.Binary {
			if len(file.Hunks) < maxHunksPerFile {
				file.Hunks = append(file.Hunks, change.hunk())
			} else {
				file.Truncated = true
				out.Summary.Truncated = true
			}
		}

		out.Summary.Added += change.added
		out.Summary.Deleted += change.deleted
	}

	for i := range out.Files {
		finishFile(&out.Files[i])
	}
	out.Summary.Files = len(out.Files)
	out.Summary.Total = len(total)
	return out
}

// finishFile decides whether one file can be undone, and says why when it
// cannot.
//
// The rule is deliberately conservative. An undo that guesses is worse than no
// undo at all: the operator presses it precisely when they have stopped trusting
// the agent's changes, so a restore that half-worked would destroy the very
// thing they were trying to get back.
func finishFile(file *FileChange) {
	switch {
	case file.Binary:
		file.Reason = "this file has no lines to compare"
	case file.Writes > 0:
		file.Reason = "the log does not record what this file held before it was written"
	case file.pureDeletion:
		file.Reason = "a change removed text, and the log does not record where it was"
	case file.Truncated:
		file.Reason = "the change list was cut short, so undoing it would be partial"
	default:
		file.Revertible = true
	}
}

// fileToolChange is one parsed tool call.
type fileToolChange struct {
	Path      string
	Tool      string
	CallID    string
	Seq       int64
	Old       string
	New       string
	WholeFile bool
	Binary    bool
	// pureDeletion marks a change that removed text and put nothing in its
	// place, which is a state an undo cannot locate.
	pureDeletion bool
	// deleted is what the log recorded, which for a whole-file write is nothing:
	// the previous contents are not in the log to count.
	deleted   int
	hunkLines []string
	added     int
}

// parseFileTool reads one file-modifying tool call.
func parseFileTool(item Item) (fileToolChange, bool) {
	var args struct {
		FilePath  string `json:"file_path"`
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal([]byte(item.Input), &args); err != nil {
		return fileToolChange{}, false
	}
	path := args.FilePath
	if path == "" {
		path = args.Path
	}
	if path == "" {
		return fileToolChange{}, false
	}

	change := fileToolChange{Path: path, Tool: item.Tool, CallID: item.ID, Seq: item.Seq}
	switch item.Tool {
	case "write":
		change.WholeFile = true
		change.New = args.Content
	case "edit":
		change.Old = args.OldString
		change.New = args.NewString
	default:
		return fileToolChange{}, false
	}

	change.Binary = isBinary(change.Old) || isBinary(change.New)
	if change.Binary {
		return change, true
	}
	// An edit whose replacement is empty removed text. The file no longer holds
	// anything to locate, so undoing it would mean guessing a position — which
	// the undo path refuses, and which the view must therefore not offer.
	change.pureDeletion = change.New == "" && change.Old != ""
	change.hunkLines, change.added, change.deleted = diffLines(change.Old, change.New)
	return change, true
}

// edit renders the parsed call as the undo plan's element.
func (c fileToolChange) edit() Edit {
	return Edit{
		Path:      c.Path,
		Tool:      c.Tool,
		CallID:    c.CallID,
		Seq:       c.Seq,
		Old:       c.Old,
		New:       c.New,
		WholeFile: c.WholeFile,
	}
}

// hunk renders the parsed call for the wire.
func (c fileToolChange) hunk() Hunk {
	lines := c.hunkLines
	truncated := len(lines) > maxHunkLines
	if truncated {
		lines = lines[:maxHunkLines]
	}
	return Hunk{
		Seq:       c.Seq,
		CallID:    c.CallID,
		Tool:      c.Tool,
		Lines:     lines,
		Added:     c.added,
		Deleted:   c.deleted,
		WholeFile: c.WholeFile,
		Truncated: truncated,
	}
}

// isBinary reports whether recorded content holds a NUL byte.
func isBinary(s string) bool { return strings.IndexByte(s, 0) >= 0 }

// diffLines renders the difference between two recorded sides as unified-diff
// lines, each prefixed with "+", "-" or a space, and counts them.
//
// It trims the common prefix and suffix and marks everything between them as
// replaced. That is not a *minimal* diff — a block that moved shows as a
// replacement rather than as two small edits — but it is always a correct one,
// it is linear in the size of the change, and it cannot blow up on the
// multi-megabyte replacement that a full LCS would have to allocate a matrix
// for. A minimal diff is a refinement; a diff that is wrong, or that takes a
// second to compute while someone waits on a phone, is not.
//
// Context is kept to changeContextSize lines on each side, the same three a
// unified diff uses, so one edit's hunk stays readable on a small screen.
func diffLines(old, new string) (lines []string, added, removed int) {
	oldLines := splitLines(old)
	newLines := splitLines(new)

	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}

	contextBefore := min(prefix, changeContextSize)
	contextAfter := min(suffix, changeContextSize)
	oldChanged := oldLines[prefix : len(oldLines)-suffix]
	newChanged := newLines[prefix : len(newLines)-suffix]

	lines = make([]string, 0, contextBefore+len(oldChanged)+len(newChanged)+contextAfter)
	for _, line := range oldLines[prefix-contextBefore : prefix] {
		lines = append(lines, " "+line)
	}
	for _, line := range oldChanged {
		lines = append(lines, "-"+line)
	}
	for _, line := range newChanged {
		lines = append(lines, "+"+line)
	}
	for _, line := range oldLines[len(oldLines)-suffix : len(oldLines)-suffix+contextAfter] {
		lines = append(lines, " "+line)
	}
	return lines, len(newChanged), len(oldChanged)
}

// splitLines splits content into lines, tolerating CRLF and a trailing newline.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	parts := strings.Split(s, "\n")
	for i, line := range parts {
		parts[i] = strings.TrimSuffix(line, "\r")
	}
	return parts
}

// normalisePath is the grouping key: the path exactly as the tool named it.
//
// It is not resolved against the filesystem on purpose. Resolving would mean
// touching the workspace — the one thing this whole path exists not to do — and
// two tools naming the same file differently is a far smaller error than a
// changes screen that refuses to render because a file it names has moved.
func normalisePath(path string) string { return strings.TrimSpace(path) }

// displayPath shortens a path the way a reader thinks of it: relative to the
// session's workspace when it is inside one.
func displayPath(path, workspace string) string {
	if workspace != "" && strings.HasPrefix(path, workspace+"/") {
		return strings.TrimPrefix(path, workspace+"/")
	}
	return path
}

// RevertTargets lists the paths an undo can act on, in application order.
func (c Changes) RevertTargets() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(c.Files))
	for _, file := range c.Files {
		if file.Revertible && !seen[file.Path] {
			seen[file.Path] = true
			out = append(out, file.Path)
		}
	}
	sort.Strings(out)
	return out
}
