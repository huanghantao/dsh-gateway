package workspace_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/workspace"
)

// These tests cover the one path in the gateway that writes to an operator's
// files. Every one of them is about a refusal as much as about a success: the
// undo is pressed precisely when someone has stopped trusting what happened to
// their tree, so a restore that half-worked would destroy the thing they were
// trying to recover.

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// newReverter builds one rooted at a fresh temporary workspace.
func newReverter(t *testing.T) (*workspace.Reverter, string) {
	t.Helper()
	root := t.TempDir()
	// TempDir on macOS is behind a symlink (/var -> /private/var), and the
	// reverter resolves before comparing, so the root has to be resolved too or
	// every path looks like it is outside.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	return workspace.New(workspace.Options{Roots: []string{resolved}, Logger: logx.Discard()}), resolved
}

// TestRevertRestoresAnEditedFile is the feature working.
func TestRevertRestoresAnEditedFile(t *testing.T) {
	reverter, root := newReverter(t)
	path := filepath.Join(root, "main.go")
	// The file holds what the agent's edit left behind; the undo puts back what
	// the log recorded before it.
	write(t, path, "a\nB\nc\n")

	report, err := reverter.Revert(context.Background(), []workspace.Edit{{
		Path: path, Tool: "edit", CallID: "c1",
		Old: "a\nb\nc\n", New: "a\nB\nc\n",
	}}, nil)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if report.Reverted != 1 || report.Refused != 0 {
		t.Fatalf("report = %+v, want one file reverted", report)
	}
	if got := read(t, path); got != "a\nb\nc\n" {
		t.Errorf("content = %q, want the recorded original", got)
	}
	if report.Files[0].Display != "main.go" {
		t.Errorf("display = %q, want the path relative to the workspace", report.Files[0].Display)
	}
}

// TestRevertRefusesWhenTheFileHasMovedOn is the safety property: an edit whose
// recorded text is no longer there means someone else has been in the file, and
// guessing is how an undo destroys work.
func TestRevertRefusesWhenTheFileHasMovedOn(t *testing.T) {
	reverter, root := newReverter(t)
	path := filepath.Join(root, "main.go")
	write(t, path, "someone rewrote this\n")

	report, err := reverter.Revert(context.Background(), []workspace.Edit{{
		Path: path, Tool: "edit", Old: "a\nb\nc\n", New: "a\nB\nc\n",
	}}, nil)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if report.Refused != 1 {
		t.Fatalf("report = %+v, want a refusal", report)
	}
	if got := read(t, path); got != "someone rewrote this\n" {
		t.Errorf("content = %q, want the file untouched", got)
	}
	if report.Files[0].Reason == "" {
		t.Error("a refusal carries no reason")
	}
}

// TestRevertRefusesAnAmbiguousMatch: replacing the first of two identical blocks
// would silently corrupt the file, and the operator would have no way to tell.
func TestRevertRefusesAnAmbiguousMatch(t *testing.T) {
	reverter, root := newReverter(t)
	path := filepath.Join(root, "main.go")
	write(t, path, "same\nsame\n")

	report, err := reverter.Revert(context.Background(), []workspace.Edit{{
		Path: path, Tool: "edit", Old: "original", New: "same",
	}}, nil)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if report.Refused != 1 {
		t.Fatalf("report = %+v, want a refusal", report)
	}
	if !strings.Contains(report.Files[0].Reason, "2 times") {
		t.Errorf("reason = %q, want it to say how many places matched", report.Files[0].Reason)
	}
}

// TestRevertRefusesAWholeFileWrite: the previous contents are not in the log, so
// there is nothing to restore.
func TestRevertRefusesAWholeFileWrite(t *testing.T) {
	reverter, root := newReverter(t)
	path := filepath.Join(root, "main.go")
	write(t, path, "written by the agent\n")

	report, err := reverter.Revert(context.Background(), []workspace.Edit{{
		Path: path, Tool: "write", WholeFile: true, New: "written by the agent\n",
	}}, nil)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if report.Refused != 1 {
		t.Fatalf("report = %+v, want a refusal", report)
	}
	if got := read(t, path); got != "written by the agent\n" {
		t.Errorf("content = %q, want the file untouched", got)
	}
}

// TestRevertRefusesADeletion: text that was removed leaves nothing to locate.
func TestRevertRefusesADeletion(t *testing.T) {
	reverter, root := newReverter(t)
	path := filepath.Join(root, "main.go")
	write(t, path, "kept\n")

	report, err := reverter.Revert(context.Background(), []workspace.Edit{{
		Path: path, Tool: "edit", Old: "removed\n", New: "",
	}}, nil)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if report.Refused != 1 {
		t.Fatalf("report = %+v, want a refusal", report)
	}
}

// TestRevertConfinesItselfToTheAllowlist is the boundary that makes this
// package safe to have at all.
func TestRevertConfinesItselfToTheAllowlist(t *testing.T) {
	reverter, _ := newReverter(t)
	outside := filepath.Join(t.TempDir(), "secrets.txt")
	write(t, outside, "not in the workspace\n")

	report, err := reverter.Revert(context.Background(), []workspace.Edit{{
		Path: outside, Tool: "edit", Old: "in the workspace", New: "not in the workspace",
	}}, nil)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if report.Refused != 1 {
		t.Fatalf("report = %+v, want a refusal", report)
	}
	if got := read(t, outside); got != "not in the workspace\n" {
		t.Errorf("content = %q, want the file outside the workspace untouched", got)
	}
}

// TestRevertRefusesASymlinkOutOfTheWorkspace is the case a path-prefix check
// alone would miss: the link is inside, the file it names is not.
func TestRevertRefusesASymlinkOutOfTheWorkspace(t *testing.T) {
	reverter, root := newReverter(t)
	outside := filepath.Join(t.TempDir(), "secrets.txt")
	write(t, outside, "outside\n")

	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	report, err := reverter.Revert(context.Background(), []workspace.Edit{{
		Path: link, Tool: "edit", Old: "inside", New: "outside",
	}}, nil)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if report.Refused != 1 {
		t.Fatalf("report = %+v, want a refusal for a link that leaves the workspace", report)
	}
	if got := read(t, outside); got != "outside\n" {
		t.Errorf("content = %q, want the target untouched", got)
	}
}

// TestRevertWalksBackwardsThroughSeveralEdits is why the order is not an
// optimisation: each step has to find the text the *previous* step left behind.
func TestRevertWalksBackwardsThroughSeveralEdits(t *testing.T) {
	reverter, root := newReverter(t)
	path := filepath.Join(root, "main.go")
	write(t, path, "three\n")

	report, err := reverter.Revert(context.Background(), []workspace.Edit{
		{Path: path, Tool: "edit", Old: "one\n", New: "two\n"},
		{Path: path, Tool: "edit", Old: "two\n", New: "three\n"},
	}, nil)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if report.Reverted != 1 || report.Files[0].Replacements != 2 {
		t.Fatalf("report = %+v, want both changes reversed", report)
	}
	if got := read(t, path); got != "one\n" {
		t.Errorf("content = %q, want the state before the first change", got)
	}
}

// TestRevertOnlyTouchesThePathsAskedFor, so a client can undo one file out of a
// session that changed twenty.
//
// The answer is about the request and nothing else: a session that changed two
// hundred files and was asked about one should not come back with two hundred
// rows, most of them reporting that nobody asked for them.
func TestRevertOnlyTouchesThePathsAskedFor(t *testing.T) {
	reverter, root := newReverter(t)
	keep := filepath.Join(root, "keep.go")
	undo := filepath.Join(root, "undo.go")
	write(t, keep, "new\n")
	write(t, undo, "new\n")

	report, err := reverter.Revert(context.Background(), []workspace.Edit{
		{Path: keep, Tool: "edit", Old: "old\n", New: "new\n"},
		{Path: undo, Tool: "edit", Old: "old\n", New: "new\n"},
	}, []string{undo})
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if report.Reverted != 1 || report.Skipped != 0 || len(report.Files) != 1 {
		t.Fatalf("report = %+v, want a one-row answer about the file that was asked for", report)
	}
	if report.Files[0].Path != undo {
		t.Errorf("reported on %q, want only the requested file", report.Files[0].Path)
	}
	if got := read(t, keep); got != "new\n" {
		t.Errorf("keep.go = %q, want it left alone", got)
	}
	if got := read(t, undo); got != "old\n" {
		t.Errorf("undo.go = %q, want it restored", got)
	}
}

// TestRevertReportsARequestedPathTheLogNeverTouched: silence would read as
// "nothing needed doing", when the truth is that this session never changed the
// file and the request was probably a mistake.
func TestRevertReportsARequestedPathTheLogNeverTouched(t *testing.T) {
	reverter, root := newReverter(t)
	touched := filepath.Join(root, "touched.go")
	stranger := filepath.Join(root, "stranger.go")
	write(t, touched, "new\n")
	write(t, stranger, "untouched\n")

	report, err := reverter.Revert(context.Background(), []workspace.Edit{
		{Path: touched, Tool: "edit", Old: "old\n", New: "new\n"},
	}, []string{stranger})
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if report.Refused != 1 || len(report.Files) != 1 {
		t.Fatalf("report = %+v, want one refusal naming the path", report)
	}
	if report.Files[0].Reason == "" {
		t.Error("the refusal carries no reason")
	}
	if got := read(t, stranger); got != "untouched\n" {
		t.Errorf("stranger.go = %q, want it untouched", got)
	}
}

// TestRevertKeepsGoingAfterARefusal: a per-file report is more useful than an
// operation that stops at the first problem, because the operator needs to know
// which files are back and which are not.
func TestRevertKeepsGoingAfterARefusal(t *testing.T) {
	reverter, root := newReverter(t)
	broken := filepath.Join(root, "broken.go")
	fine := filepath.Join(root, "fine.go")
	write(t, broken, "nothing like what was recorded\n")
	write(t, fine, "new\n")

	report, err := reverter.Revert(context.Background(), []workspace.Edit{
		{Path: broken, Tool: "edit", Old: "old\n", New: "gone\n"},
		{Path: fine, Tool: "edit", Old: "old\n", New: "new\n"},
	}, nil)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if report.Refused != 1 || report.Reverted != 1 {
		t.Fatalf("report = %+v, want one refusal and one success", report)
	}
	if got := read(t, fine); got != "old\n" {
		t.Errorf("fine.go = %q, want it still restored", got)
	}
}

// TestRevertPreservesTheFileMode: a script that came back without its execute
// bit would be a silent regression.
func TestRevertPreservesTheFileMode(t *testing.T) {
	reverter, root := newReverter(t)
	path := filepath.Join(root, "run.sh")
	write(t, path, "new\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := reverter.Revert(context.Background(), []workspace.Edit{{
		Path: path, Tool: "edit", Old: "old\n", New: "new\n",
	}}, nil); err != nil {
		t.Fatalf("Revert: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755 preserved", info.Mode().Perm())
	}
}

// TestRevertWithNoAllowlistRefusesEverything: a gateway configured with no
// workspaces cannot write anywhere, and saying so beats writing somewhere
// unintended.
func TestRevertWithNoAllowlistRefusesEverything(t *testing.T) {
	reverter := workspace.New(workspace.Options{Logger: logx.Discard()})
	path := filepath.Join(t.TempDir(), "main.go")
	write(t, path, "new\n")

	report, err := reverter.Revert(context.Background(), []workspace.Edit{{
		Path: path, Tool: "edit", Old: "old\n", New: "new\n",
	}}, nil)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if report.Refused != 1 {
		t.Fatalf("report = %+v, want a refusal", report)
	}
}

// TestRevertOfAnAlreadyRestoredFileIsReportedNotRewritten: pressing undo twice
// must be harmless, and the second press has to say what it found.
func TestRevertOfAnAlreadyRestoredFileIsReportedNotRewritten(t *testing.T) {
	reverter, root := newReverter(t)
	path := filepath.Join(root, "main.go")
	write(t, path, "old\n")

	report, err := reverter.Revert(context.Background(), []workspace.Edit{{
		Path: path, Tool: "edit", Old: "old\n", New: "new\n",
	}}, nil)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	// The recorded replacement is gone, so this is a refusal: the file no longer
	// contains what the change replaced. It is *not* the already-restored path,
	// which only applies while the text still matches.
	if report.Refused != 1 {
		t.Fatalf("report = %+v, want a refusal on a second press", report)
	}
	if got := read(t, path); got != "old\n" {
		t.Errorf("content = %q, want it unchanged", got)
	}
}
