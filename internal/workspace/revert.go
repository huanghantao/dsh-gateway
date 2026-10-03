// Package workspace owns every write this gateway makes to an operator's files.
//
// Nothing else in the tree may open a file for writing. That is the point of
// having the package at all: the gateway's central security property is that it
// is read-only over the workspace, and a property that is claimed in a document
// is worth much less than one that is enforced by there being a single, small,
// auditable place where the opposite can happen.
//
// The only capability here is an undo of a change the session log recorded. It
// is off unless the operator turns it on (`changes.revert.enabled`), and a
// deployment that leaves it off constructs no Reverter at all — the code path is
// absent rather than disabled, which is a stronger statement and a simpler one
// to check.
//
// An undo never guesses. Every replacement it makes is an exact, unique match
// against the content the log recorded, and anything else is reported as
// refused. The operator presses undo precisely when they have stopped trusting
// what happened to their tree; a restore that half-worked would destroy the very
// thing they were trying to recover.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/huanghantao/dsh-gateway/internal/atomicfile"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// Edit is one recorded replacement, as the undo path needs it.
//
// It is deliberately a plain value with no reference to the log projection that
// produces it: an undo needs to know what to look for and what to put back, and
// nothing about where that was written down.
type Edit struct {
	// Path is the file, as the tool that changed it named it.
	Path string
	// Tool and CallID identify the change for the report and the audit log.
	Tool   string
	CallID string
	// Old and New are the two sides the change replaced: an undo puts Old back
	// where New is.
	Old string
	New string
	// WholeFile marks a change that overwrote the file, whose previous contents
	// are not recorded and therefore cannot be restored.
	WholeFile bool
}

// Outcome is what happened to one file.
type Outcome string

const (
	// OutcomeReverted means the file was restored and written.
	OutcomeReverted Outcome = "reverted"
	// OutcomeSkipped means the undo would write what the file already holds: the
	// change had already been reversed by hand.
	OutcomeSkipped Outcome = "skipped"
	// OutcomeRefused means the undo was attempted and declined, with a reason.
	// Nothing was written.
	OutcomeRefused Outcome = "refused"
)

// Result is one file's outcome.
type Result struct {
	Path string `json:"path"`
	// Display is the path shortened for a reader, when the caller supplied a
	// workspace to shorten against.
	Display string  `json:"display,omitempty"`
	Status  Outcome `json:"status"`
	// Replacements is how many recorded changes were reversed.
	Replacements int `json:"replacements,omitempty"`
	// Reason explains a refusal, and is empty otherwise.
	Reason string `json:"reason,omitempty"`
}

// Report is the whole operation's outcome.
//
// There is no transaction, and that is a decision rather than an omission: a
// half-reverted tree with a per-file report tells the operator exactly which
// files are back and which are not, while a rollback would hide the ones it
// touched on the way.
type Report struct {
	Files []Result `json:"files"`
	// Reverted, Refused and Skipped count the files in each state.
	Reverted int `json:"reverted"`
	Refused  int `json:"refused"`
	Skipped  int `json:"skipped"`
}

// Options configures a Reverter.
type Options struct {
	// Roots is the allowlist of directories an undo may write inside. A path
	// outside every root is refused, which is the same boundary every read path
	// already enforces.
	Roots []string
	// Logger may be nil.
	Logger *logx.Logger
}

// Reverter undoes recorded changes inside an allowlist.
type Reverter struct {
	// roots are the allowlist entries exactly as configured, and resolvedRoots
	// the same set with symlinks followed. Two lists rather than one because they
	// answer different questions: a check has to compare resolved paths, while a
	// label has to shorten the path the rest of the app is showing — and on a
	// platform where /tmp is a link to /private/tmp those are not the same
	// string, so a report shortened against the resolved root names nothing.
	roots         []string
	resolvedRoots []string
	logger        *logx.Logger
}

// New builds a Reverter.
func New(opts Options) *Reverter {
	if opts.Logger == nil {
		opts.Logger = logx.Discard()
	}
	roots := make([]string, 0, len(opts.Roots))
	resolvedRoots := make([]string, 0, len(opts.Roots))
	for _, root := range opts.Roots {
		roots = append(roots, filepath.Clean(root))
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			resolvedRoots = append(resolvedRoots, resolved)
			continue
		}
		resolvedRoots = append(resolvedRoots, filepath.Clean(root))
	}
	return &Reverter{roots: roots, resolvedRoots: resolvedRoots, logger: opts.Logger}
}

// Revert undoes edits, newest first, optionally restricted to paths.
//
// The order matters and is not an optimisation: two edits to the same file are
// recorded as a sequence, and applying them in the order they happened would
// look for the *later* change's text in a file that no longer holds it. Walking
// backwards is what makes each step's match the unique one the log recorded.
//
// A restricted request is answered about what was asked for and nothing else. A
// session that changed two hundred files and was asked to undo one should not
// come back with two hundred rows, most of them saying "not asked for": a report
// is a thing a person reads, and the useful length of it is the length of the
// request.
func (r *Reverter) Revert(ctx context.Context, edits []Edit, paths []string) (Report, error) {
	// Group by path, preserving the order the files were first changed in.
	order := make([]string, 0, len(edits))
	byPath := map[string][]Edit{}
	for _, edit := range edits {
		if _, seen := byPath[edit.Path]; !seen {
			order = append(order, edit.Path)
		}
		byPath[edit.Path] = append(byPath[edit.Path], edit)
	}

	if len(paths) > 0 {
		wanted := make(map[string]bool, len(paths))
		for _, path := range paths {
			wanted[path] = true
		}
		requested := make([]string, 0, len(paths))
		for _, path := range order {
			if wanted[path] {
				requested = append(requested, path)
			}
		}
		order = requested
	}

	report := Report{Files: make([]Result, 0, len(order))}
	for _, path := range order {
		if err := ctx.Err(); err != nil {
			return report, err
		}

		result := r.revertFile(path, byPath[path])
		switch result.Status {
		case OutcomeReverted:
			report.Reverted++
		case OutcomeRefused:
			report.Refused++
		case OutcomeSkipped:
			report.Skipped++
		}
		report.Files = append(report.Files, result)
	}

	// A path that was asked for and has no recorded change is worth a row of its
	// own: silence would read as "nothing needed doing", when the truth is that
	// this session never touched it and the request was probably a mistake.
	if len(paths) > 0 {
		for _, path := range paths {
			if _, known := byPath[path]; known {
				continue
			}
			report.Refused++
			report.Files = append(report.Files, Result{
				Path:    path,
				Display: displayOf(path, r.roots),
				Status:  OutcomeRefused,
				Reason:  "the log records no change to this file",
			})
		}
	}
	return report, nil
}

// revertFile undoes one file's changes, or explains why it will not.
func (r *Reverter) revertFile(path string, edits []Edit) Result {
	result := Result{Path: path, Display: displayOf(path, r.roots)}

	resolved, err := r.resolve(path)
	if err != nil {
		// The message rather than the error: a reason is read by a person, and
		// `outside_workspace: that file is outside every configured workspace`
		// says the same thing twice with a wire code in front of it.
		result.Status = OutcomeRefused
		result.Reason = errx.PublicMessage(err)
		return result
	}

	// A whole-file write cannot be undone from the log at all, so the file is
	// refused before anything is read: a partial undo of a file whose beginning
	// cannot be restored is worse than none.
	for _, edit := range edits {
		if edit.WholeFile {
			result.Status = OutcomeRefused
			result.Reason = "the log does not record what this file held before it was overwritten"
			return result
		}
	}

	// resolved came from resolve(), which confines it to an allowlisted root and
	// refuses a symlink that leaves one. The path is not attacker-controlled
	// beyond that check: it is a file the agent itself edited.
	content, err := os.ReadFile(resolved) //nolint:gosec // confined by resolve()
	if err != nil {
		result.Status = OutcomeRefused
		result.Reason = "the file could not be read"
		if !errors.Is(err, fs.ErrNotExist) {
			r.logger.Warn("revert: read failed", "path", resolved, "error", err.Error())
		}
		return result
	}

	reversed := 0
	text := string(content)
	// Backwards: see the note on Revert.
	for i := len(edits) - 1; i >= 0; i-- {
		edit := edits[i]
		next, err := undo(text, edit)
		if err != nil {
			result.Status = OutcomeRefused
			result.Reason = err.Error()
			return result
		}
		text = next
		reversed++
	}

	if reversed == 0 {
		result.Status = OutcomeRefused
		result.Reason = "no recorded change to undo"
		return result
	}
	if text == string(content) {
		// Everything already matched what the undo would write, which means the
		// work had already been undone by hand. Saying so beats a silent write.
		result.Status = OutcomeSkipped
		result.Reason = "the file already matches what this change would restore"
		return result
	}

	// The file's own mode is preserved rather than invented: a script that lost
	// its execute bit would be a silent, infuriating regression.
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(resolved); err == nil {
		mode = info.Mode().Perm()
	}
	if err := atomicfile.WriteFile(resolved, []byte(text), mode); err != nil {
		result.Status = OutcomeRefused
		result.Reason = "the file could not be written"
		r.logger.Warn("revert: write failed", "path", resolved, "error", err.Error())
		return result
	}

	result.Status = OutcomeReverted
	result.Replacements = reversed
	r.logger.Info("reverted a recorded change",
		"path", resolved, "changes", reversed)
	return result
}

// undo reverses one replacement in text.
func undo(text string, edit Edit) (string, error) {
	if edit.New == "" {
		// The change removed text. Where it used to sit cannot be recovered from
		// "nothing is here any more", and inserting it somewhere plausible would
		// corrupt the file in a way that is hard to notice and harder to undo.
		return "", errors.New("the change deleted text, and the log does not record where it was")
	}
	switch count := strings.Count(text, edit.New); {
	case count == 0:
		return "", errors.New("the file no longer contains what this change replaced")
	case count > 1:
		return "", fmt.Errorf("what this change replaced appears %d times in the file, so undoing it would be a guess", count)
	}
	return strings.Replace(text, edit.New, edit.Old, 1), nil
}

// resolve checks that a path is inside an allowlisted root and returns it
// resolved.
//
// The check is on the symlink-resolved path, so a symlink inside the allowlist
// pointing outside it is refused rather than followed. That is the same rule the
// read side applies, and it has to be applied again here because this is the
// side that writes.
func (r *Reverter) resolve(path string) (string, error) {
	if len(r.resolvedRoots) == 0 {
		return "", errors.New("this gateway has no workspace allowlist configured")
	}
	absolute := path
	if !filepath.IsAbs(absolute) {
		return "", errors.New("the recorded path is not absolute, so it cannot be checked against the workspace allowlist")
	}

	// The file itself is resolved, not just its directory, so that a symlink
	// inside the allowlist pointing outside it is refused rather than followed.
	// Resolving only the directory would also let the write replace the link with
	// a regular file, which is a silent change to something the operator did not
	// ask about.
	resolved, err := filepath.EvalSymlinks(absolute)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		// A path that does not exist cannot be written to either, and the read
		// below will refuse it with a clearer reason than this could.
		dir, dirErr := filepath.EvalSymlinks(filepath.Dir(absolute))
		if dirErr != nil {
			return "", errors.New("the file's directory could not be resolved")
		}
		resolved = filepath.Join(dir, filepath.Base(absolute))
	default:
		return "", errors.New("the file could not be resolved")
	}

	for _, root := range r.resolvedRoots {
		if resolved == root || strings.HasPrefix(resolved, root+string(filepath.Separator)) {
			return resolved, nil
		}
	}
	// Refused rather than failed: the agent may legitimately have edited a file
	// outside the workspace, so this is a normal answer to give a caller, not an
	// error that aborts the rest of the operation.
	return "", errx.New(errx.KindForbidden, "outside_workspace",
		"that file is outside every configured workspace")
}

// displayOf shortens a path against the allowlist, for a report a phone renders.
//
// It shortens the path as given rather than its resolved form, so that a report
// names the same file the change screen did: a reader comparing the two should
// not have to work out that /private/tmp/gw/ws/x and /tmp/gw/ws/x are one file.
func displayOf(path string, roots []string) string {
	for _, root := range roots {
		if strings.HasPrefix(path, root+string(filepath.Separator)) {
			return strings.TrimPrefix(path, root+string(filepath.Separator))
		}
	}
	return path
}
