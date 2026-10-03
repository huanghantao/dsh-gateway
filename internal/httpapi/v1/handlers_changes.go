package v1

import (
	"errors"
	"net/http"

	"github.com/huanghantao/dsh-gateway/internal/audit"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
	"github.com/huanghantao/dsh-gateway/internal/sessionlog"
	"github.com/huanghantao/dsh-gateway/internal/workspace"
)

// handleChanges returns what a session changed.
//
// Read-only and always available while the log is readable: it answers from the
// agent's own record of its edits and touches nothing in the workspace.
func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.deps.Sessions == nil {
		_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{
			"files": []any{}, "unsupported": true,
		})
		return
	}

	changes, err := s.deps.Sessions.Changes(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, sessionlog.ErrUnsupportedFormat):
			// A newer DSH wrote this log. The same degradation the transcript
			// uses: say so rather than failing, because the session is still
			// real and only this view is unavailable.
			_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{
				"files":       []any{},
				"unsupported": true,
				"detail":      "this session was written by a newer DeepSeek Harness than the gateway understands",
			})
			return
		default:
			httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
			return
		}
	}

	_ = httpcore.RespondJSON(w, http.StatusOK, changesView(changes, s.deps.Workspace != nil))
}

// changesView renders the projection for a client.
//
// `revertible` is the projection's own verdict — whether the log records enough
// to reverse this file — and `revertEnabled` beside it says whether this
// deployment will act on that. They are deliberately two fields rather than one
// ANDed together: folded into one, every file on a read-only deployment reported
// `revertible: false` with an empty reason, and a client had no way to tell
// "this file cannot be undone" from "nothing here can be". Each field now states
// one fact, and `reason` always explains a false.
//
// A client is expected to require both before offering the control. It is not
// the last line of defence — the endpoint refuses with `revert_disabled` — but
// offering an action that cannot happen is a worse experience than not offering
// it.
func changesView(changes sessionlog.Changes, revertEnabled bool) map[string]any {
	files := make([]map[string]any, 0, len(changes.Files))
	for _, file := range changes.Files {
		files = append(files, map[string]any{
			"path":       file.Path,
			"display":    file.Display,
			"added":      file.Added,
			"deleted":    file.Deleted,
			"edits":      file.Edits,
			"writes":     file.Writes,
			"binary":     file.Binary,
			"hunks":      file.Hunks,
			"truncated":  file.Truncated,
			"revertible": file.Revertible,
			"reason":     file.Reason,
		})
	}
	summary := map[string]any{
		"files":     changes.Summary.Files,
		"total":     changes.Summary.Total,
		"added":     changes.Summary.Added,
		"deleted":   changes.Summary.Deleted,
		"edits":     changes.Summary.Edits,
		"source":    changes.Summary.Source,
		"truncated": changes.Summary.Truncated,
	}
	return map[string]any{
		"files":         files,
		"summary":       summary,
		"revertEnabled": revertEnabled,
	}
}

// revertRequest is the body of POST /sessions/{id}/revert.
type revertRequest struct {
	// Paths restricts the undo. Empty means every reversible file.
	Paths []string `json:"paths,omitempty"`
}

// handleRevert undoes changes this session recorded.
//
// This is the only endpoint in the gateway that writes to an operator's files,
// and it is absent unless the deployment turned it on. Everything about it is
// conservative: it refuses while the agent is working, it refuses a file whose
// recorded change no longer matches exactly, and it reports per file rather than
// pretending the operation was atomic.
func (s *Server) handleRevert(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if s.deps.Workspace == nil {
		// KindUnavailable rather than KindForbidden, matching curation_disabled
		// and transcript_disabled: this is a capability the deployment does not
		// have, not a caller who is not allowed. It also keeps 403 meaning one
		// thing on this API — the CSRF origin gate — which is what both clients
		// and the route-table test rely on.
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindUnavailable, "revert_disabled",
				"this gateway is read-only; an operator must enable changes.revert to allow undo"))
		return
	}
	if s.deps.Sessions == nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindUnavailable, "transcript_disabled",
				"undo needs the session log, which this gateway does not read"))
		return
	}

	var req revertRequest
	if r.ContentLength != 0 {
		if err := httpcore.DecodeJSON(w, r, &req); err != nil {
			httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
			return
		}
	}

	// An undo races a turn by definition: the agent is writing the same files.
	// Refusing is the only honest answer — the alternative is a tree that
	// matches neither what the agent wrote nor what it held before.
	if s.deps.Turns != nil && s.deps.Turns.Busy(id) {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindConflict, "session_busy",
				"the agent is working in this session; stop it before undoing its changes"))
		return
	}
	if s.deps.Follower != nil && s.deps.Follower.Running(id) {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindConflict, "session_running_elsewhere",
				"another process is working in this session; stop it before undoing its changes"))
		return
	}

	changes, err := s.deps.Sessions.Changes(r.Context(), id)
	if err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}

	report, err := s.deps.Workspace.Revert(r.Context(), revertPlan(changes.Edits), req.Paths)
	if err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}

	principal, _ := principalFrom(r.Context())
	paths := make([]string, 0, len(report.Files))
	for _, file := range report.Files {
		if file.Status == workspace.OutcomeReverted {
			paths = append(paths, file.Path)
		}
	}
	s.deps.Audit.Record(r.Context(), audit.EventWorkspaceReverted, id, map[string]any{
		"deviceId": principal.DeviceID,
		"reverted": report.Reverted,
		"refused":  report.Refused,
		"paths":    paths,
	})

	_ = httpcore.RespondJSON(w, http.StatusOK, report)
}

// revertPlan maps the log projection onto the undo path's own vocabulary.
//
// The two types are kept separate on purpose. The projection describes what a
// reader should see; the plan describes what a writer may do. Folding them into
// one type would let a change to the display shape widen what undo is willing to
// touch, which is the wrong direction for that coupling to run.
func revertPlan(edits []sessionlog.Edit) []workspace.Edit {
	out := make([]workspace.Edit, 0, len(edits))
	for _, edit := range edits {
		out = append(out, workspace.Edit{
			Path:      edit.Path,
			Tool:      edit.Tool,
			CallID:    edit.CallID,
			Old:       edit.Old,
			New:       edit.New,
			WholeFile: edit.WholeFile,
		})
	}
	return out
}
