// Package curation decides which sessions a reader sees, and which they never
// want to see again.
//
// DSH stores sessions as directories nobody can rename, move or hide, so a phone
// that lists everything the disk holds ends up with a pile of test runs beside
// the work that matters. Two sources of opinion fix that, and this package is
// where they meet:
//
//   - **DSH's own store** (`<dsh.home>/storages/workspace.json`) already carries
//     `archivedSessionIds` and `pinnedSessionIds`. The desktop can archive and
//     pin today, so the phone must not show what the desktop has put away.
//   - **An overlay of the gateway's own** (`<stateDir>/curation.json`), because
//     archiving from the phone must not rewrite a file another process owns and
//     is actively editing. The overlay can both add and remove: "archived" and
//     "unarchived", "pinned" and "unpinned", so undoing a desktop decision from
//     the phone is possible without touching the desktop's file.
//
// Reads of DSH's store are cached by size and mtime — it is small, but it is
// read once per session per list request. Writes are atomic, and only ever touch
// the gateway's own file.
package curation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/atomicfile"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// overlayFile is the gateway's own file, kept beside the other gateway state.
const overlayFile = "curation.json"

// dshStoreFile is DSH's workspace store, relative to the harness home.
var dshStoreFile = filepath.Join("storages", "workspace.json")

// overlay is the on-disk shape of the gateway's decisions.
//
// Four lists rather than two, because "the desktop archived this" and "the
// operator un-archived it on the phone" are different facts, and the second must
// win without editing the first.
type overlay struct {
	Archived   []string `json:"archived,omitempty"`
	Unarchived []string `json:"unarchived,omitempty"`
	Pinned     []string `json:"pinned,omitempty"`
	Unpinned   []string `json:"unpinned,omitempty"`
}

// Decision explains why a session is in (or out of) the list, which a UI needs
// in order to say "archived" rather than simply hiding something.
type Decision struct {
	Archived bool `json:"archived"`
	Pinned   bool `json:"pinned"`
	// ArchivedOnDesk and PinnedOnDesk say that DSH's own store is the reason, so
	// a client can say "archived on the desktop" and not offer an undo it cannot
	// perform.
	ArchivedOnDesk bool `json:"archivedOnDesk,omitempty"`
	PinnedOnDesk   bool `json:"pinnedOnDesk,omitempty"`
}

// Options configures a Store.
type Options struct {
	// StateDir holds the gateway's overlay, and is never shared with DSH.
	StateDir string
	// DSHHome is the harness home that owns workspace.json. Empty disables
	// reading it, which is what a test wants when there is no desktop.
	DSHHome string
	Logger  *logx.Logger
	Now     func() time.Time
}

// Store answers curation questions and records the gateway's own decisions.
type Store struct {
	opts Options

	mu      sync.Mutex
	current overlay
	// dshCache is DSH's last-read lists, keyed by the file's size and mtime.
	dshSize    int64
	dshModTime time.Time
	dshLists   overlay
}

// Open builds a Store, creating nothing until something is written.
func Open(opts Options) (*Store, error) {
	if opts.StateDir == "" {
		return nil, errors.New("curation: a state directory is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Store{opts: opts}

	raw, err := os.ReadFile(s.overlayPath())
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.current); err != nil {
			// A corrupt overlay must not take the gateway down: the worst case of
			// ignoring it is that archived sessions reappear, which is visible and
			// fixable, unlike a gateway that will not start.
			s.debug("ignoring an unreadable overlay", err)
			s.current = overlay{}
		}
	case os.IsNotExist(err):
	default:
		return nil, fmt.Errorf("curation: read overlay: %w", err)
	}
	return s, nil
}

// Decision reports what is known about one session.
func (s *Store) Decision(sessionID string) Decision {
	desk := s.deskLists()
	s.mu.Lock()
	local := s.current
	s.mu.Unlock()

	decision := Decision{}
	if contains(desk.Archived, sessionID) {
		decision.Archived, decision.ArchivedOnDesk = true, true
	}
	if contains(desk.Pinned, sessionID) {
		decision.Pinned, decision.PinnedOnDesk = true, true
	}
	if contains(local.Archived, sessionID) {
		decision.Archived = true
	}
	if contains(local.Unarchived, sessionID) {
		decision.Archived, decision.ArchivedOnDesk = false, false
	}
	if contains(local.Pinned, sessionID) {
		decision.Pinned = true
	}
	if contains(local.Unpinned, sessionID) {
		decision.Pinned, decision.PinnedOnDesk = false, false
	}
	return decision
}

// Hide reports whether a session should be out of the default list.
func (s *Store) Hide(sessionID string) bool { return s.Decision(sessionID).Archived }

// Archive records a decision about a set of sessions and returns how many
// changed. Setting archived=false undoes an archive, whoever made it.
func (s *Store) Archive(sessionIDs []string, archived bool) (int, error) {
	return s.update(sessionIDs, func(o *overlay, id string) bool {
		if archived {
			if contains(o.Archived, id) {
				return false
			}
			o.Archived = append(o.Archived, id)
			o.Unarchived = remove(o.Unarchived, id)
			return true
		}
		if contains(o.Unarchived, id) {
			return false
		}
		o.Unarchived = append(o.Unarchived, id)
		o.Archived = remove(o.Archived, id)
		return true
	})
}

// Pin records a pin, or removes one.
func (s *Store) Pin(sessionIDs []string, pinned bool) (int, error) {
	return s.update(sessionIDs, func(o *overlay, id string) bool {
		if pinned {
			if contains(o.Pinned, id) {
				return false
			}
			o.Pinned = append(o.Pinned, id)
			o.Unpinned = remove(o.Unpinned, id)
			return true
		}
		if contains(o.Unpinned, id) {
			return false
		}
		o.Unpinned = append(o.Unpinned, id)
		o.Pinned = remove(o.Pinned, id)
		return true
	})
}

// update applies a change to every id and writes once, so a bulk action is one
// atomic file write rather than one per session.
func (s *Store) update(sessionIDs []string, change func(*overlay, string) bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := overlay{
		Archived:   append([]string(nil), s.current.Archived...),
		Unarchived: append([]string(nil), s.current.Unarchived...),
		Pinned:     append([]string(nil), s.current.Pinned...),
		Unpinned:   append([]string(nil), s.current.Unpinned...),
	}
	changed := 0
	for _, id := range sessionIDs {
		if id == "" {
			continue
		}
		if change(&next, id) {
			changed++
		}
	}
	if changed == 0 {
		return 0, nil
	}
	next.Archived = sortedUnique(next.Archived)
	next.Unarchived = sortedUnique(next.Unarchived)
	next.Pinned = sortedUnique(next.Pinned)
	next.Unpinned = sortedUnique(next.Unpinned)

	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("curation: encode overlay: %w", err)
	}
	if err := atomicfile.WriteFileSecret(s.overlayPath(), raw); err != nil {
		return 0, fmt.Errorf("curation: write overlay: %w", err)
	}
	s.current = next
	return changed, nil
}

// deskLists returns DSH's own archived and pinned ids, re-reading the file when
// it changed.
func (s *Store) deskLists() overlay {
	if s.opts.DSHHome == "" {
		return overlay{}
	}
	path := filepath.Join(s.opts.DSHHome, dshStoreFile)
	info, err := os.Stat(path)
	if err != nil {
		return overlay{}
	}

	s.mu.Lock()
	if s.dshSize == info.Size() && s.dshModTime.Equal(info.ModTime()) {
		lists := s.dshLists
		s.mu.Unlock()
		return lists
	}
	s.mu.Unlock()

	raw, err := os.ReadFile(path) //nolint:gosec // the harness home the operator configured
	if err != nil {
		s.debug("could not read the harness workspace store", err)
		return overlay{}
	}
	var parsed struct {
		Global struct {
			ArchivedSessionIDs []string `json:"archivedSessionIds"`
			PinnedSessionIDs   []string `json:"pinnedSessionIds"`
		} `json:"global"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		s.debug("could not parse the harness workspace store", err)
		return overlay{}
	}

	lists := overlay{Archived: parsed.Global.ArchivedSessionIDs, Pinned: parsed.Global.PinnedSessionIDs}

	s.mu.Lock()
	s.dshSize, s.dshModTime, s.dshLists = info.Size(), info.ModTime(), lists
	s.mu.Unlock()
	return lists
}

// overlayPath is the gateway's own file.
func (s *Store) overlayPath() string { return filepath.Join(s.opts.StateDir, overlayFile) }

func (s *Store) debug(msg string, err error) {
	if s.opts.Logger == nil {
		return
	}
	s.opts.Logger.Debug("curation: "+msg, "error", err.Error())
}

func contains(list []string, id string) bool {
	for _, item := range list {
		if item == id {
			return true
		}
	}
	return false
}

func remove(list []string, id string) []string {
	out := list[:0]
	for _, item := range list {
		if item != id {
			out = append(out, item)
		}
	}
	return out
}

func sortedUnique(list []string) []string {
	seen := make(map[string]struct{}, len(list))
	out := make([]string, 0, len(list))
	for _, item := range list {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}
