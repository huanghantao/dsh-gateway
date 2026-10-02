package v1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/audit"
	"github.com/huanghantao/dsh-gateway/internal/curation"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
	"github.com/huanghantao/dsh-gateway/internal/idgen"
	"github.com/huanghantao/dsh-gateway/internal/sessionlog"
)

// sessionView is the wire shape of a session.
type sessionView struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Workspace string     `json:"workspace"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
	Leased    bool       `json:"leased"`
	Busy      bool       `json:"busy"`
	Pinned    bool       `json:"pinned"`
	Model     string     `json:"model,omitempty"`
	Effort    string     `json:"reasoningEffort,omitempty"`
	// Archived and Pinned are the operator's own decisions, wherever they were
	// made: this phone, or the desktop whose store the gateway also reads.
	Archived bool `json:"archived,omitempty"`
	// ArchivedOnDesk says the desktop is the reason, so a client can explain it
	// and not offer an undo that would only surprise the other screen.
	ArchivedOnDesk bool `json:"archivedOnDesk,omitempty"`
	// Preview is the first thing the operator typed, for the many sessions that
	// never got a title: a list of "Untitled session" rows tells a reader
	// nothing, while the opening prompt is what they recognise.
	Preview string `json:"preview,omitempty"`
	// MessageCount is how many conversation rows the session holds. It is the
	// cheapest honest signal of whether there is any work in there.
	MessageCount int `json:"messageCount,omitempty"`
	// HistoryAvailable is false when no readable log exists, so the UI can hide
	// the "load earlier" affordance instead of showing a spinner forever.
	HistoryAvailable bool `json:"historyAvailable"`
}

// sessionPage is a page of sessions.
type sessionPage struct {
	Sessions   []sessionView `json:"sessions"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

// listScanLimit bounds how many harness sessions one list request will look at
// while it is filtering out archived rows and looking for a search term.
//
// The harness pages at a hundred, and an operator with a few hundred sessions
// who has archived most of them would otherwise receive pages that hold almost
// nothing. Five pages is far more than the first screen needs and still bounds
// the work one request can do.
const listScanLimit = 500

// handleListSessions merges the harness's authoritative list with the metadata
// only the session log carries.
//
// DSH's own listing returns an id and a cwd and nothing else, so this merge is
// what gives the phone a titled, timestamped conversation list. It is also where
// the operator's own curation is applied: archived sessions are out of the
// default view, a search term filters what is left, and both are decided here
// because only here is the title — and therefore the preview that stands in for
// it — known.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	workspace := r.URL.Query().Get("workspace")
	if workspace != "" && !s.workspaceAllowed(workspace) {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindInvalid, "unknown_workspace", "that workspace is not configured"))
		return
	}

	state := r.URL.Query().Get("state")
	switch state {
	case "", "active", "archived", "all":
	default:
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindInvalid, "unknown_state", "state must be active, archived or all"))
		return
	}
	archivedWanted := state == "archived"
	hideArchived := state == "" || state == "active"

	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	limit := s.listLimit(r)

	out := sessionPage{Sessions: make([]sessionView, 0, limit)}
	cursor := r.URL.Query().Get("cursor")
	scanned := 0
	for {
		page, ok := s.listCache.get(workspace, cursor, s.now())
		if !ok {
			var err error
			page, err = s.deps.Harness.ListSessions(r.Context(), workspace, cursor)
			if err != nil {
				httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
				return
			}
			s.listCache.put(workspace, cursor, page, s.now())
		}
		scanned += len(page.Sessions)
		for _, info := range page.Sessions {
			view := s.viewSession(r.Context(), info)
			if hideArchived && view.Archived {
				continue
			}
			if archivedWanted && !view.Archived {
				continue
			}
			if query != "" && !view.matches(query) {
				continue
			}
			out.Sessions = append(out.Sessions, view)
		}

		cursor = page.NextCursor
		if len(out.Sessions) >= limit || cursor == "" || scanned >= listScanLimit {
			break
		}
	}
	// DSH's listing omits a session another process owns, and the process that
	// owns one is usually this gateway: the session the operator just started is
	// missing from the list they are looking at, which reads as "it vanished".
	// Merged back in at the front, because a session you are in the middle of is
	// the one you are looking for.
	if s.deps.Leases != nil {
		seen := make(map[string]bool, len(out.Sessions))
		for _, view := range out.Sessions {
			seen[view.ID] = true
		}
		owned := make([]sessionView, 0, 4)
		for _, snapshot := range s.deps.Leases.List() {
			if seen[snapshot.SessionID] {
				continue
			}
			view := s.viewSession(r.Context(), harness.SessionInfo{
				ID:        snapshot.SessionID,
				Workspace: s.workspaceForSession(r.Context(), snapshot.SessionID),
			})
			if hideArchived && view.Archived {
				continue
			}
			if archivedWanted && !view.Archived {
				continue
			}
			if query != "" && !view.matches(query) {
				continue
			}
			owned = append(owned, view)
		}
		// Newest first among the owned ones: a list of everything this gateway is
		// running, most recent on top.
		// UpdatedAt is optional on the wire — a session with no readable log yet
		// has none — so the comparison has to tolerate a nil on either side.
		sort.SliceStable(owned, func(i, j int) bool {
			left, right := owned[i].UpdatedAt, owned[j].UpdatedAt
			switch {
			case left == nil:
				return false
			case right == nil:
				return true
			default:
				return left.After(*right)
			}
		})
		out.Sessions = append(owned, out.Sessions...)
	}

	out.NextCursor = cursor
	if len(out.Sessions) > limit {
		out.Sessions = out.Sessions[:limit]
	}
	_ = httpcore.RespondJSON(w, http.StatusOK, out)
}

// listLimit reads the page size, defaulting to the configured transcript page.
func (s *Server) listLimit(r *http.Request) int {
	limit := s.deps.Config.Transcript.PageSize
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > listScanLimit {
		limit = listScanLimit
	}
	return limit
}

// matches reports whether a view answers a search term, lower-cased by the caller.
//
// It searches what the reader sees: the title, the opening prompt that stands in
// for a missing title, the workspace, and the id for the times a session is
// quoted in a bug report.
func (v sessionView) matches(query string) bool {
	for _, field := range []string{v.Title, v.Preview, v.Workspace, v.ID} {
		if field != "" && strings.Contains(strings.ToLower(field), query) {
			return true
		}
	}
	return false
}

// viewSession builds a session view from every source that knows something.
func (s *Server) viewSession(ctx context.Context, info harness.SessionInfo) sessionView {
	v := sessionView{
		ID:        info.ID,
		Title:     info.Title,
		Workspace: info.Workspace,
	}

	if snap, ok := s.deps.Leases.Get(info.ID); ok {
		v.Leased = true
		v.Busy = snap.Busy
		applyConfig(&v, snap.Config)
	}

	if s.deps.Curation != nil {
		decision := s.deps.Curation.Decision(info.ID)
		v.Pinned = decision.Pinned
		v.Archived = decision.Archived
		v.ArchivedOnDesk = decision.ArchivedOnDesk
	}

	// A session someone else is running has no lease here, and the log is the
	// only thing that says so. Without this the list reports a desk session as
	// idle while the agent is writing in it.
	if !v.Busy && s.deps.Follower != nil && s.deps.Follower.Running(info.ID) {
		v.Busy = true
	}

	if s.deps.Sessions != nil {
		meta, err := s.deps.Sessions.Meta(ctx, info.ID)
		switch {
		case err == nil:
			v.HistoryAvailable = true
			if meta.Title != "" {
				v.Title = meta.Title
			}
			if meta.Workspace != "" {
				v.Workspace = meta.Workspace
			}
			if !meta.CreatedAt.IsZero() {
				t := meta.CreatedAt
				v.CreatedAt = &t
			}
			if !meta.UpdatedAt.IsZero() {
				t := meta.UpdatedAt
				v.UpdatedAt = &t
			}
			if v.Model == "" && meta.Model != "" {
				v.Model = meta.Model
			}
			// A session the gateway is not holding has no ACP option to read,
			// and the log is then the only record of the route it ran on. Empty
			// is meaningful in both places — it is ACP's "provider default" —
			// so this only fills a gap, and never overrules a live session.
			if v.Effort == "" && meta.Effort != "" {
				v.Effort = meta.Effort
			}
			v.Preview = meta.Preview
			v.MessageCount = meta.Messages
		case errors.Is(err, sessionlog.ErrUnsupportedFormat):
			// A newer DSH wrote this log. The session is still real and usable;
			// only its history is unreadable, so say so rather than failing.
			v.HistoryAvailable = false
		default:
			v.HistoryAvailable = false
		}
	}
	return v
}

// applyConfig copies model and reasoning effort out of config options.
//
// Both are reported as the value ids a client sends back, not as display names.
// `defaults` in `GET /models` is an id, `POST` and `PATCH` take ids, and a
// session that named its model one way while the picker offered it another
// could only be preselected by guessing: the app had the display name in hand
// and a list of opaque ids to match it against, so the Settings picker sat on
// "Leave unchanged" while the session's model was known all along. The app
// renders the name from the catalog it already has.
func applyConfig(v *sessionView, options []harness.ConfigOption) {
	for _, o := range options {
		switch o.ID {
		case "model":
			v.Model = o.Current
		case "reasoning_effort":
			v.Effort = o.Current
		}
	}
}

// createSessionRequest is the body of POST /sessions.
type createSessionRequest struct {
	Workspace       string `json:"workspace"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

// handleCreateSession creates a session and leases it.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if err := httpcore.DecodeJSON(w, r, &req); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	if !s.workspaceAllowed(req.Workspace) {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindInvalid, "unknown_workspace",
				"pick one of the configured workspaces"))
		return
	}

	session, err := s.deps.Harness.NewSession(r.Context(), req.Workspace)
	if err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	s.cacheConfig(session.Config)
	s.deps.Leases.Adopt(session)
	// A list held a moment ago does not contain this session, and the operator
	// who just created it is looking at the list behind the sheet.
	s.listCache.invalidate()

	// The caller's choice wins; the configured default fills in when the caller
	// did not make one. That is what "Gateway default" in the app means, and it
	// is why the default lives here rather than in each client.
	model := req.Model
	if model == "" {
		model = s.deps.Config.Session.DefaultModel
	}
	effort := req.ReasoningEffort
	if effort == "" {
		effort = s.deps.Config.Session.DefaultReasoningEffort
	}

	// Apply the preferred route before the first prompt, because the profile
	// default may point at a provider the operator has no credential for.
	if err := s.applyPreferredOptions(r.Context(), session.Info.ID, model, effort); err != nil {
		s.deps.Logger.Warn("could not apply session options",
			"session", session.Info.ID, "error", err.Error())
	}

	principal, _ := principalFrom(r.Context())
	s.deps.Audit.Record(r.Context(), audit.EventSessionOpened, session.Info.ID, map[string]any{
		"deviceId":  principal.DeviceID,
		"workspace": req.Workspace,
		"new":       true,
	})

	_ = httpcore.RespondJSON(w, http.StatusCreated, s.viewSession(r.Context(), session.Info))
}

// acquireLeaseRequest is the body of POST /sessions/{id}/lease.
type acquireLeaseRequest struct {
	Workspace string `json:"workspace,omitempty"`
	Pinned    bool   `json:"pinned,omitempty"`
}

// handleAcquireLease attaches a session, releasing DSH's write lock from any
// other process if needed.
func (s *Server) handleAcquireLease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// The body is optional; an empty one is valid and common.
	var req acquireLeaseRequest
	if r.ContentLength != 0 {
		if err := httpcore.DecodeJSON(w, r, &req); err != nil {
			httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
			return
		}
	}

	workspace := req.Workspace
	if workspace == "" {
		workspace = s.workspaceForSession(r.Context(), id)
	}
	if workspace == "" {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindInvalid, "workspace_required",
				"the workspace is unknown; supply it so the harness can verify the session"))
		return
	}

	info, err := s.deps.Leases.Acquire(r.Context(), id, workspace)
	if err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	if req.Pinned {
		s.deps.Leases.SetPinned(id, true)
	}

	principal, _ := principalFrom(r.Context())
	s.deps.Audit.Record(r.Context(), audit.EventSessionOpened, id, map[string]any{
		"deviceId":  principal.DeviceID,
		"workspace": workspace,
	})

	_ = httpcore.RespondJSON(w, http.StatusOK, s.viewSession(r.Context(), info))
}

// handleReleaseLease detaches a session and hands it back to the desktop.
func (s *Server) handleReleaseLease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	force := r.URL.Query().Get("force") == "true"

	if err := s.deps.Leases.Release(r.Context(), id, "requested", force); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	principal, _ := principalFrom(r.Context())
	s.deps.Audit.Record(r.Context(), audit.EventSessionReleased, id, map[string]any{
		"deviceId": principal.DeviceID,
		"reason":   "requested",
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleGetSession returns one session's metadata.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info := harness.SessionInfo{ID: id, Workspace: s.workspaceForSession(r.Context(), id)}
	_ = httpcore.RespondJSON(w, http.StatusOK, s.viewSession(r.Context(), info))
}

// patchSessionRequest changes a session's configuration or its curation.
type patchSessionRequest struct {
	Model           *string `json:"model,omitempty"`
	ReasoningEffort *string `json:"reasoningEffort,omitempty"`
	// Archived and Pinned are the operator's decisions about the list, not about
	// the session's configuration: they need no lease, because a session nobody
	// has attached to is exactly the kind that gets archived.
	Archived *bool `json:"archived,omitempty"`
	Pinned   *bool `json:"pinned,omitempty"`
}

// handlePatchSession changes model or reasoning effort, or archives and pins.
func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req patchSessionRequest
	if err := httpcore.DecodeJSON(w, r, &req); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	if req.Model == nil && req.ReasoningEffort == nil && req.Archived == nil && req.Pinned == nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindInvalid, "nothing_to_change",
				"supply model, reasoningEffort, archived or pinned"))
		return
	}

	if req.Archived != nil || req.Pinned != nil {
		if s.deps.Curation == nil {
			httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
				errx.New(errx.KindUnavailable, "curation_disabled", "this gateway keeps no curation state"))
			return
		}
		if err := s.curate(r.Context(), []string{id}, req.Archived, req.Pinned); err != nil {
			httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
			return
		}
		if req.Model == nil && req.ReasoningEffort == nil {
			decision := s.deps.Curation.Decision(id)
			_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{
				"archived": decision.Archived, "pinned": decision.Pinned,
				"archivedOnDesk": decision.ArchivedOnDesk,
			})
			return
		}
	}

	if !s.deps.Leases.IsLeased(id) {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindConflict, "session_not_leased",
				"attach the session before changing its configuration"))
		return
	}

	var options []harness.ConfigOption
	var err error
	if req.Model != nil {
		if options, err = s.deps.Harness.SetConfigOption(r.Context(), id, "model", *req.Model); err != nil {
			httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
			return
		}
	}
	if req.ReasoningEffort != nil {
		if options, err = s.deps.Harness.SetConfigOption(r.Context(), id, "reasoning_effort", *req.ReasoningEffort); err != nil {
			httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
			return
		}
	}

	s.cacheConfig(options)
	s.deps.Leases.SetConfig(id, options)
	s.deps.Leases.Touch(id)

	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{"config": configView(options)})
}

// handleTranscript returns the read-only history projection.
func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.deps.Sessions == nil {
		_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{"items": []any{}, "unsupported": true})
		return
	}

	limit := s.deps.Config.Transcript.PageSize
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)

	// Reading history is use of the session, so it keeps the lease warm. It does
	// not *take* a lease: the whole point of the projection is that a phone can
	// browse a session the desktop still holds.
	if s.deps.Leases.IsLeased(id) {
		s.deps.Leases.Touch(id)
	}

	page, err := s.deps.Sessions.Transcript(r.Context(), id, before, limit)
	if err != nil {
		if errors.Is(err, sessionlog.ErrUnsupportedFormat) {
			// Expected after a DSH upgrade. Answer with a usable empty page so the
			// client shows "open this on your desktop" instead of an error toast.
			_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{
				"items":       []any{},
				"unsupported": true,
				"detail":      "this transcript was written by a newer DeepSeek Harness than the gateway understands",
			})
			return
		}
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	_ = httpcore.RespondJSON(w, http.StatusOK, page)
}

// promptRequest is the body of POST /sessions/{id}/prompt.
type promptRequest struct {
	Blocks []promptBlock `json:"blocks"`
}

type promptBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// promptResponse acknowledges a prompt. The turn's progress arrives on the event
// stream, because a turn can outlive any sensible HTTP response.
type promptResponse struct {
	TurnID string `json:"turnId"`
}

// handlePrompt admits a prompt and returns immediately.
//
// The turn itself runs on a detached context. Tying it to the request would mean
// a phone that locks its screen cancels the agent mid-edit, which is precisely
// the behaviour a remote-control client must not have.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req promptRequest
	if err := httpcore.DecodeJSON(w, r, &req); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}

	blocks, err := s.toHarnessBlocks(req.Blocks)
	if err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}

	if !s.deps.Leases.IsLeased(id) {
		workspace := s.workspaceForSession(r.Context(), id)
		if workspace == "" {
			httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
				errx.New(errx.KindConflict, "session_not_leased",
					"attach the session before prompting it"))
			return
		}
		if _, err := s.deps.Leases.Acquire(r.Context(), id, workspace); err != nil {
			httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
			return
		}
	}

	if s.deps.Leases.IsBusy(id) {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindConflict, "prompt_in_flight",
				"a turn is already running for this session; wait for it or cancel it"))
		return
	}

	turnID := idgen.New("turn")
	principal, _ := principalFrom(r.Context())

	s.deps.Leases.SetBusy(id, true)
	s.deps.Bus.Publish(events.TypeTurnState, id, map[string]any{
		"turnId": turnID, "state": "running",
	})
	s.deps.Audit.Record(r.Context(), audit.EventPromptSent, id, map[string]any{
		"deviceId": principal.DeviceID,
		"turnId":   turnID,
		"blocks":   len(blocks),
	})

	// Detached on purpose: a turn can run for many minutes and must survive the
	// phone locking its screen or losing its radio. runTurn gives itself its own
	// bounded context. Inheriting the request's would cancel the agent mid-edit
	// the moment the client walked away.
	go s.runTurn(id, turnID, blocks) //nolint:gosec,contextcheck // the request context must not outlive the request

	_ = httpcore.RespondJSON(w, http.StatusAccepted, promptResponse{TurnID: turnID})
}

// runTurn executes one turn and reports its outcome on the event stream.
func (s *Server) runTurn(sessionID, turnID string, blocks []harness.PromptBlock) {
	// Detached from any request, then bounded by the configured prompt timeout so
	// that a wedged harness cannot pin a lease forever.
	ctx, cancel := context.WithTimeout(context.Background(), s.deps.Config.Session.PromptTimeout.Std())
	defer cancel()

	stopReason, err := s.deps.Harness.Prompt(ctx, sessionID, blocks)

	s.deps.Leases.SetBusy(sessionID, false)
	s.deps.Leases.Touch(sessionID)

	state := "completed"
	detail := ""
	if err != nil {
		state = "failed"
		detail = err.Error()
	} else if stopReason == "cancelled" {
		state = "cancelled"
	}

	s.deps.Bus.Publish(events.TypeTurnState, sessionID, map[string]any{
		"turnId": turnID, "state": state, "stopReason": stopReason, "detail": detail,
	})
	if err != nil {
		s.deps.Logger.Warn("turn failed", "session", sessionID, "turn", turnID, "error", err.Error())
	}
}

// handleCancel interrupts the in-flight turn.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if err := s.deps.Harness.Cancel(r.Context(), id); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	principal, _ := principalFrom(r.Context())
	s.deps.Audit.Record(r.Context(), audit.EventPromptCancelled, id, map[string]any{
		"deviceId": principal.DeviceID,
	})
	w.WriteHeader(http.StatusAccepted)
}

// handleModels returns the cached model catalog, plus the gateway's own defaults.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	options := s.cfgCache.get()
	if id := r.URL.Query().Get("sessionId"); id != "" {
		if snap, ok := s.deps.Leases.Get(id); ok && len(snap.Config) > 0 {
			options = snap.Config
		}
	}
	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{
		"config": configView(options),
		// The gateway's defaults, not the harness's: what a create request that
		// omits model or reasoningEffort will be given. A picker preselects
		// these so that "the default" is visible instead of implied.
		"defaults": map[string]any{
			"model":           s.deps.Config.Session.DefaultModel,
			"reasoningEffort": s.deps.Config.Session.DefaultReasoningEffort,
		},
	})
}

// curateRequest is the body of POST /sessions/curate.
type curateRequest struct {
	IDs []string `json:"ids"`
	// Exactly one of these is set: the request either archives or pins.
	Archived *bool `json:"archived,omitempty"`
	Pinned   *bool `json:"pinned,omitempty"`
}

// handleCurateSessions archives or pins a set of sessions in one request.
//
// One request rather than one per session, because the gesture that produces it
// is "these 187" and a phone on a cellular link should not send 187 of anything.
func (s *Server) handleCurateSessions(w http.ResponseWriter, r *http.Request) {
	if s.deps.Curation == nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindUnavailable, "curation_disabled", "this gateway keeps no curation state"))
		return
	}
	var req curateRequest
	if err := httpcore.DecodeJSON(w, r, &req); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	if len(req.IDs) == 0 {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindInvalid, "no_sessions", "supply the ids to change"))
		return
	}
	if len(req.IDs) > listScanLimit {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindInvalid, "too_many_sessions",
				fmt.Sprintf("at most %d sessions per request", listScanLimit)))
		return
	}
	if (req.Archived == nil) == (req.Pinned == nil) {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindInvalid, "one_decision_at_a_time", "set either archived or pinned, not both"))
		return
	}

	if err := s.curate(r.Context(), req.IDs, req.Archived, req.Pinned); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}

	// The answer is the state of every id asked about, so a client can update
	// rows (or undo) without a second round trip.
	states := make([]map[string]any, 0, len(req.IDs))
	for _, id := range req.IDs {
		decision := s.deps.Curation.Decision(id)
		states = append(states, map[string]any{
			"id": id, "archived": decision.Archived, "pinned": decision.Pinned,
			"archivedOnDesk": decision.ArchivedOnDesk,
		})
	}
	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{"sessions": states})
}

// curate applies an archive or pin decision and records it in the audit log.
func (s *Server) curate(ctx context.Context, ids []string, archived, pinned *bool) error {
	var (
		changed int
		err     error
		action  string
	)
	switch {
	case archived != nil:
		changed, err = s.deps.Curation.Archive(ids, *archived)
		action = "session.archived"
		if !*archived {
			action = "session.unarchived"
		}
	default:
		changed, err = s.deps.Curation.Pin(ids, *pinned)
		action = "session.pinned"
		if !*pinned {
			action = "session.unpinned"
		}
	}
	if err != nil {
		return errx.Wrap(err, errx.KindInternal, "curation_failed", "the change could not be stored")
	}
	if changed == 0 {
		return nil
	}
	principal, _ := principalFrom(ctx)
	s.deps.Audit.Record(ctx, audit.Event(action), "", map[string]any{
		"deviceId": principal.DeviceID,
		"sessions": changed,
	})
	return nil
}

// curationRules is the rule set triage runs under, defaulted when unconfigured.
//
// The zero value is meaningful here rather than a bug waiting to happen: a
// gateway that configures nothing should get the portable rules, not a rule set
// with no title evidence and no scratch directories, which would silently stop
// suggesting anything at all.
func (s *Server) curationRules() curation.Rules {
	rules := s.deps.CurationRules
	if rules.TestTitles == nil && len(rules.TempRoots) == 0 {
		return curation.DefaultRules()
	}
	if rules.TestTitles == nil {
		rules.TestTitles = curation.DefaultRules().TestTitles
	}
	if len(rules.TempRoots) == 0 {
		rules.TempRoots = curation.DefaultRules().TempRoots
	}
	return rules
}

// handleTriage suggests sessions that can be put away.
//
// The classification is the server's, not the client's, for two reasons: the
// rules need every session's metadata, which only the gateway has cheaply, and a
// heuristic that decides what disappears from someone's list should be one
// implementation that tests can hold still rather than a copy per client.
func (s *Server) handleTriage(w http.ResponseWriter, r *http.Request) {
	if s.deps.Curation == nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindUnavailable, "curation_disabled", "this gateway keeps no curation state"))
		return
	}

	summary := curation.Candidates{}
	candidates := make([]curation.Candidate, 0)
	cursor := ""
	for scanned := 0; scanned < listScanLimit; {
		page, ok := s.listCache.get("", cursor, s.now())
		if !ok {
			var err error
			page, err = s.deps.Harness.ListSessions(r.Context(), "", cursor)
			if err != nil {
				httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
				return
			}
			s.listCache.put("", cursor, page, s.now())
		}
		scanned += len(page.Sessions)

		batch := make([]curation.Session, 0, len(page.Sessions))
		for _, info := range page.Sessions {
			view := s.viewSession(r.Context(), info)
			summary.Scanned++
			if view.Archived {
				summary.Archived++
				continue
			}
			batch = append(batch, curation.Session{
				ID:        view.ID,
				Title:     view.Title,
				Preview:   view.Preview,
				Workspace: view.Workspace,
				Messages:  view.MessageCount,
				// A session whose history could not be read is not a session that
				// is empty; only the rules that need no content may judge it.
				LogReadable: view.HistoryAvailable,
			})
		}
		candidates = append(candidates, curation.TriageWith(batch, s.curationRules())...)

		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}

	for _, candidate := range candidates {
		switch candidate.Verdict {
		case curation.VerdictTest:
			summary.Test++
		case curation.VerdictTemp:
			summary.Temp++
		case curation.VerdictDraft:
			summary.Draft++
		}
	}
	summary.Candidates = len(candidates)
	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{
		"summary":    summary,
		"candidates": candidates,
	})
}

// --- helpers ----------------------------------------------------------------

// configCache is stored on the Server; declared here to keep the session
// handlers together.
func (s *Server) cacheConfig(options []harness.ConfigOption) {
	if len(options) == 0 {
		return
	}
	s.cfgCache.set(options)
}

func (s *Server) workspaceAllowed(path string) bool {
	if path == "" {
		return false
	}
	for _, ws := range s.workspaces {
		// Compare cleaned paths so that "/a/b/" and "/a/b" are the same root, but
		// never by prefix: "/a/bc" must not match the root "/a/b".
		if strings.TrimRight(ws.Path, "/") == strings.TrimRight(path, "/") {
			return true
		}
	}
	return false
}

// workspaceForSession finds a session's workspace from the best available source.
func (s *Server) workspaceForSession(ctx context.Context, sessionID string) string {
	if s.deps.Sessions != nil {
		if meta, err := s.deps.Sessions.Meta(ctx, sessionID); err == nil && meta.Workspace != "" {
			return meta.Workspace
		}
	}
	// A session created moments ago has no readable log yet, so fall back to the
	// harness listing.
	if page, err := s.deps.Harness.ListSessions(ctx, "", ""); err == nil {
		for _, info := range page.Sessions {
			if info.ID == sessionID {
				return info.Workspace
			}
		}
	}
	return ""
}

// applyPreferredOptions sets model and reasoning effort on a fresh session.
func (s *Server) applyPreferredOptions(ctx context.Context, sessionID, model, effort string) error {
	var last []harness.ConfigOption
	if model != "" {
		options, err := s.deps.Harness.SetConfigOption(ctx, sessionID, "model", model)
		if err != nil {
			return err
		}
		last = options
	}
	if effort != "" {
		options, err := s.deps.Harness.SetConfigOption(ctx, sessionID, "reasoning_effort", effort)
		if err != nil {
			return err
		}
		last = options
	}
	if len(last) > 0 {
		s.cacheConfig(last)
		s.deps.Leases.SetConfig(sessionID, last)
	}
	return nil
}

// toHarnessBlocks validates and converts prompt blocks.
func (s *Server) toHarnessBlocks(in []promptBlock) ([]harness.PromptBlock, error) {
	if len(in) == 0 {
		return nil, errx.New(errx.KindInvalid, "empty_prompt", "a prompt must contain at least one block")
	}
	out := make([]harness.PromptBlock, 0, len(in))
	total := 0
	for _, b := range in {
		switch b.Type {
		case "", "text":
			total += len(b.Text)
			if total > s.deps.Config.Limits.MaxPromptBytes {
				return nil, errx.New(errx.KindInvalid, "prompt_too_large",
					"the prompt exceeds the configured size limit")
			}
			out = append(out, harness.PromptBlock{Type: "text", Text: b.Text})
		default:
			// Image prompts arrive in a later revision. Rejecting explicitly beats
			// silently dropping an attachment the operator believes was sent.
			return nil, errx.New(errx.KindInvalid, "unsupported_block",
				"prompt block type "+b.Type+" is not supported yet")
		}
	}
	return out, nil
}
