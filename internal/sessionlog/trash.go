package sessionlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/errx"
)

// Trash is a holding area for deleted sessions.
//
// Deleting a conversation is the one action in this product that can destroy
// work, and DSH has no such operation at all — the gateway would be removing a
// directory the desktop considers its own. So deletion is a move: the whole
// session directory lands under the gateway's state directory, where it is out
// of every listing and can be put back, byte for byte, until it is purged. The
// alternative — an actual `rm -rf` behind a phone button — is not a thing to
// build on a good day.
type Trash struct {
	store *Store
	root  string
}

// Entry is one trashed session.
type Entry struct {
	SessionID string    `json:"id"`
	Workspace string    `json:"workspace,omitempty"`
	DeletedAt time.Time `json:"deletedAt"`
	// Path is where it lives now, for an operator who would rather move files by
	// hand than trust a phone.
	Path string `json:"-"`
}

// OpenTrash prepares the holding area, creating it if needed.
func OpenTrash(store *Store, root string) (*Trash, error) {
	if store == nil {
		return nil, errors.New("sessionlog: a trash needs a store")
	}
	if root == "" {
		return nil, errors.New("sessionlog: a trash needs a directory")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("sessionlog: create trash: %w", err)
	}
	return &Trash{store: store, root: root}, nil
}

// Root is where trashed sessions live.
func (t *Trash) Root() string { return t.root }

// Move takes a session out of the store.
//
// It refuses while a live process holds the session: moving a directory out from
// under a running agent is how a session ends up half-written in two places. The
// caller surfaces that as "this session is running right now", which is a better
// answer than a success the operator will regret.
func (t *Trash) Move(sessionID string) (Entry, error) {
	dir, err := t.store.Dir(sessionID)
	if err != nil {
		return Entry{}, err
	}
	if t.store.Locked(sessionID) {
		return Entry{}, errx.New(errx.KindConflict, "session_running",
			"that session is being written to right now; stop it, or wait for it to finish")
	}

	// The workspace directory name is preserved inside the trash entry, so
	// restoring needs no manifest: everything about where it came from is in the
	// path.
	workspaceDir := filepath.Base(filepath.Dir(dir))
	at := time.Now().UTC()
	destination := filepath.Join(t.root, at.Format("20060102T150405Z"), workspaceDir, sessionID)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return Entry{}, fmt.Errorf("sessionlog: prepare trash entry: %w", err)
	}
	if err := os.Rename(dir, destination); err != nil {
		return Entry{}, errx.Wrap(err, errx.KindInternal, "trash_failed",
			"the session could not be moved out of the store")
	}

	t.store.forget(sessionID)
	return Entry{SessionID: sessionID, Workspace: workspaceDir, DeletedAt: at, Path: destination}, nil
}

// List returns what is in the trash, newest first.
func (t *Trash) List() []Entry {
	stamps, err := os.ReadDir(t.root)
	if err != nil {
		return nil
	}
	entries := make([]Entry, 0)
	for _, stamp := range stamps {
		if !stamp.IsDir() {
			continue
		}
		at, err := time.Parse("20060102T150405Z", stamp.Name())
		if err != nil {
			continue
		}
		workspaces, err := os.ReadDir(filepath.Join(t.root, stamp.Name()))
		if err != nil {
			continue
		}
		for _, workspace := range workspaces {
			if !workspace.IsDir() {
				continue
			}
			sessions, err := os.ReadDir(filepath.Join(t.root, stamp.Name(), workspace.Name()))
			if err != nil {
				continue
			}
			for _, session := range sessions {
				if !session.IsDir() || !validSessionID(session.Name()) {
					continue
				}
				entries = append(entries, Entry{
					SessionID: session.Name(),
					Workspace: workspace.Name(),
					DeletedAt: at,
					Path:      filepath.Join(t.root, stamp.Name(), workspace.Name(), session.Name()),
				})
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].DeletedAt.After(entries[j].DeletedAt) })
	return entries
}

// Restore puts a session back where it came from.
//
// A session that already exists again — the desktop resumed it, or someone
// restored it on another phone — is left alone: overwriting a live session with
// an older copy is worse than refusing.
func (t *Trash) Restore(sessionID string) (Entry, error) {
	for _, entry := range t.List() {
		if entry.SessionID != sessionID {
			continue
		}
		destination := filepath.Join(t.store.root, entry.Workspace, sessionID)
		if _, err := os.Stat(destination); err == nil {
			return Entry{}, errx.New(errx.KindConflict, "session_exists",
				"a session with that id is already in the store")
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return Entry{}, fmt.Errorf("sessionlog: prepare restore: %w", err)
		}
		if err := os.Rename(entry.Path, destination); err != nil {
			return Entry{}, errx.Wrap(err, errx.KindInternal, "restore_failed",
				"the session could not be put back")
		}
		// The timestamp directory may now be empty; leaving it costs nothing and
		// removing it risks racing a second restore.
		return entry, nil
	}
	return Entry{}, errx.New(errx.KindNotFound, "not_in_trash", "that session is not in the trash")
}

// Purge removes trash entries older than maxAge and returns what it removed for
// good.
//
// This is the only place that deletes anything, and it is deliberately the one
// that a phone cannot reach: what the operator asked for was "get this out of my
// list", and the answer to that is a month of grace, not a cron job with a
// button.
//
// It returns the entries rather than a count because the deletion is permanent:
// a caller that keeps state keyed by session — the curation overlay, for one —
// has to forget them at the same moment, and re-listing the trash would race.
func (t *Trash) Purge(maxAge time.Duration, now time.Time) ([]Entry, error) {
	purged := make([]Entry, 0)
	for _, entry := range t.List() {
		if now.Sub(entry.DeletedAt) < maxAge {
			continue
		}
		if err := os.RemoveAll(entry.Path); err != nil {
			return purged, fmt.Errorf("sessionlog: purge %s: %w", entry.SessionID, err)
		}
		purged = append(purged, entry)
	}
	// Tidy up the directories a purge emptied. `os.Remove` — not `RemoveAll` —
	// is the point: it refuses a directory that still holds something, which is
	// what keeps a session inside its grace period from being swept away with the
	// packaging around it.
	stamps, err := os.ReadDir(t.root)
	if err != nil {
		// The purge above did its work; this is the housekeeping that removes the
		// directories it emptied. Reporting a failure here would be a lie about
		// work that succeeded, and the cost of not tidying is an empty directory
		// left behind.
		return purged, nil //nolint:nilerr // a successful purge with failed tidying is not a failed purge
	}
	for _, stamp := range stamps {
		if !stamp.IsDir() {
			continue
		}
		if _, err := time.Parse("20060102T150405Z", stamp.Name()); err != nil {
			continue
		}
		day := filepath.Join(t.root, stamp.Name())
		workspaces, err := os.ReadDir(day)
		if err != nil {
			continue
		}
		for _, workspace := range workspaces {
			_ = os.Remove(filepath.Join(day, workspace.Name()))
		}
		_ = os.Remove(day)
	}
	return purged, nil
}

// forget drops a session from everything the store remembers about it, so a
// trashed session does not reappear from a cache.
func (s *Store) forget(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, sessionID)
	delete(s.metas, sessionID)
	s.forgetPathLocked(sessionID)
}

// Dir returns the directory a session's log lives in.
func (s *Store) Dir(sessionID string) (string, error) {
	path, err := s.logPath(sessionID)
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}

// SessionsRoot is the directory the store reads, for callers that need to reason
// about where sessions live.
func (s *Store) SessionsRoot() string { return strings.TrimRight(s.root, string(filepath.Separator)) }
