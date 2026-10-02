package v1

import (
	"net/http"

	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
)

// trashView is one deleted session as a client sees it.
type trashView struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	Preview   string `json:"preview,omitempty"`
	DeletedAt string `json:"deletedAt,omitempty"`
}

// handleDeleteSession moves a session to the trash.
//
// Not a DELETE that removes anything: the session directory is moved into the
// gateway's own state directory, out of every listing and restorable byte for
// byte for a month. A phone button that destroys a conversation is not something
// this product should have, and DSH has no delete of its own to defer to.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if s.deps.Trash == nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindUnavailable, "trash_disabled", "this gateway keeps no trash"))
		return
	}
	id := r.PathValue("id")

	entry, err := s.deps.Trash.Move(id)
	if err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	// A cached listing does not know the session is gone.
	s.listCache.invalidate()

	principal, _ := principalFrom(r.Context())
	s.deps.Audit.Record(r.Context(), "session.deleted", id, map[string]any{
		"deviceId": principal.DeviceID,
	})

	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{
		"deleted":   true,
		"id":        entry.SessionID,
		"deletedAt": entry.DeletedAt,
	})
}

// handleListTrash returns what is in the trash, newest first.
func (s *Server) handleListTrash(w http.ResponseWriter, r *http.Request) {
	if s.deps.Trash == nil {
		_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{"trash": []trashView{}})
		return
	}
	entries := s.deps.Trash.List()
	views := make([]trashView, 0, len(entries))
	for _, entry := range entries {
		view := trashView{ID: entry.SessionID, DeletedAt: entry.DeletedAt.UTC().Format("2006-01-02T15:04:05Z")}
		// A title makes the list recognisable, and the log is right there in the
		// trash — reading it is the same code path as reading a live session.
		if s.deps.Sessions != nil {
			// The store reads by session id from the sessions root, which no
			// longer holds this one; the trash entry itself is what has the file.
			view.Title, view.Preview = s.trashedTitles(entry.SessionID)
		}
		views = append(views, view)
	}
	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{"trash": views})
}

// handleRestoreTrash puts a session back where it came from.
func (s *Server) handleRestoreTrash(w http.ResponseWriter, r *http.Request) {
	if s.deps.Trash == nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindUnavailable, "trash_disabled", "this gateway keeps no trash"))
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := httpcore.DecodeJSON(w, r, &req); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	if req.ID == "" {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindInvalid, "session_required", "which session should come back?"))
		return
	}
	entry, err := s.deps.Trash.Restore(req.ID)
	if err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	s.listCache.invalidate()

	principal, _ := principalFrom(r.Context())
	s.deps.Audit.Record(r.Context(), "session.restored", req.ID, map[string]any{
		"deviceId": principal.DeviceID,
	})
	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{"restored": true, "id": entry.SessionID})
}

// trashedTitles reads a trashed session's own log for something to show.
//
// The store cannot find it — it is no longer under the sessions root — so the
// trash entry's path is read directly, through the same parser, which is also why
// a restored session comes back with its title intact.
func (s *Server) trashedTitles(sessionID string) (string, string) {
	for _, entry := range s.deps.Trash.List() {
		if entry.SessionID != sessionID {
			continue
		}
		meta, err := s.deps.Sessions.MetaAt(entry.Path, sessionID)
		if err != nil {
			return "", ""
		}
		return meta.Title, meta.Preview
	}
	return "", ""
}
