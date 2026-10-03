package v1

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/turns"
	"github.com/huanghantao/dsh-gateway/internal/audit"
	"github.com/huanghantao/dsh-gateway/internal/curation"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
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
	// HistoryAvailable is false when no readable log exists for the session. It
	// is also what the transcript's LogReadable is set from.
	HistoryAvailable bool `json:"historyAvailable"`
	// Turn is the prompt running in this session right now, when this gateway is
	// the one running it. It carries the start time, which is what a client that
	// reconnects mid-turn needs: the turn.state event that announced it may be
	// long past the replay window.
	Turn *turns.Ticket `json:"turn,omitempty"`
	// Queue are the prompts waiting behind Turn, oldest first. Each carries its
	// position, so a client can show a follow-up creeping forward.
	Queue []turns.Ticket `json:"queue,omitempty"`
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
	//
	// Two sources answer for those sessions, and both are needed: the leases
	// this gateway holds cover a session while a phone is attached to it, and
	// the handles the harness holds cover the leases that went away without the
	// attachment going away — an expired lease, or a redeploy, which leave DSH's
	// attachment in place on purpose (a release is bookkeeping; see the agent
	// host's handleRelease).
	if candidates := s.mergeBackCandidates(); len(candidates) > 0 {
		seen := make(map[string]bool, len(out.Sessions)+len(candidates))
		for _, view := range out.Sessions {
			seen[view.ID] = true
		}
		owned := make([]sessionView, 0, len(candidates))
		for _, candidate := range candidates {
			if seen[candidate.id] {
				continue
			}
			seen[candidate.id] = true
			// Resolved here rather than while the candidate list was built: it
			// costs a log read and, for a session created moments ago, a harness
			// round trip, so only the rows that survive the merge pay for it.
			// Named apart from the page's own `workspace`, which is the filter
			// below rather than this row's answer.
			rowWorkspace := candidate.workspace
			if rowWorkspace == "" {
				rowWorkspace = s.workspaceForSession(r.Context(), candidate.id)
			}
			view := s.viewSession(r.Context(), harness.SessionInfo{ID: candidate.id, Workspace: rowWorkspace})
			if hideArchived && view.Archived {
				continue
			}
			if archivedWanted && !view.Archived {
				continue
			}
			if query != "" && !view.matches(query) {
				continue
			}
			// A page that asked for one workspace must not collect rows from
			// another — the same filter the harness page above was served
			// under. A row whose workspace could not be read is kept: unknown
			// is not the same as elsewhere.
			if workspace != "" && view.Workspace != "" && view.Workspace != workspace {
				continue
			}
			owned = append(owned, view)
		}
		// Newest first among the merged ones: a list of everything this gateway
		// is running, most recent on top.
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

// mergeCandidate is a session the harness's own listing cannot report, with the
// workspace when the source that named it already knows one.
type mergeCandidate struct {
	id        string
	workspace string
}

// mergeBackCandidates lists the sessions the harness's own listing cannot report.
//
// A lease says this gateway is driving the session; a held handle says the
// harness still owns it. Both are candidates for the merge, and a leased session
// is normally held as well, so the caller de-duplicates by id.
//
// A lease carries no workspace — the log or the harness listing is asked for it
// later, and only for rows that survive — while a held session was handed over
// with the cwd DSH bound it to.
func (s *Server) mergeBackCandidates() []mergeCandidate {
	var out []mergeCandidate
	if s.deps.Leases != nil {
		for _, snapshot := range s.deps.Leases.List() {
			out = append(out, mergeCandidate{id: snapshot.SessionID})
		}
	}
	if s.deps.Held != nil {
		for _, info := range s.deps.Held.HeldSessions() {
			out = append(out, mergeCandidate{id: info.ID, workspace: info.Workspace})
		}
	}
	return out
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

	// The queue is read for every session, leased or not: the scheduler holds
	// state only while something is running or waiting, so this is a map lookup
	// on the common path rather than a per-row cost.
	if s.deps.Turns != nil {
		queue := s.deps.Turns.Queue(info.ID)
		v.Turn = queue.Running
		v.Queue = queue.Queued
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
	if !s.admit(w, r) {
		return
	}
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
	// Attaching is how a client *starts* driving a session — it takes DSH's
	// single-writer lock and may create deliverable state — so it is new work,
	// and a drain refuses it for the same reason it refuses a prompt. Releasing
	// a lease is not gated: the drain is trying to give sessions back.
	if !s.admit(w, r) {
		return
	}
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
	// Bounded on the way out, never in the store: the change projection folds
	// the same argument text to build diffs, so a trimmed copy there would
	// silently cut a diff short.
	_ = httpcore.RespondJSON(w, http.StatusOK, transcriptView(page, s.deps.Config.Limits))
}

// promptRequest is the body of POST /sessions/{id}/prompt.
type promptRequest struct {
	Blocks []promptBlock `json:"blocks"`
}

// promptBlock is one element of a prompt.
//
// Text and image are the two the harness understands. Data is base64 because
// that is what a JSON body can carry, and it is bounded by limits.maxImageBytes
// after decoding — a limit on the encoded form would be a limit that means a
// different thing for every image size.
type promptBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

// handlePrompt admits a prompt and returns immediately.
//
// The turn itself is owned by the scheduler, which runs it on a detached
// context: tying it to the request would mean a phone that locks its screen
// cancels the agent mid-edit, which is precisely the behaviour a remote-control
// client must not have.
//
// A prompt that arrives while a turn is running is queued rather than refused,
// so a follow-up typed while watching the agent work is kept. The response says
// which of the two happened.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	if !s.admit(w, r) {
		return
	}
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

	ticket, err := s.deps.Turns.Submit(r.Context(), id, blocks)
	if err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}

	principal, _ := principalFrom(r.Context())
	s.deps.Audit.Record(r.Context(), audit.EventPromptSent, id, map[string]any{
		"deviceId": principal.DeviceID,
		"turnId":   ticket.ID,
		"state":    ticket.State,
		"blocks":   len(blocks),
	})

	_ = httpcore.RespondJSON(w, http.StatusAccepted, ticket)
}

// handleCancel interrupts the in-flight turn and discards anything queued
// behind it.
//
// The answer reports what was actually stopped. Cancelling a session with
// nothing running is a no-op, not an error: a double tap on a phone must be
// harmless, and the agent must not be told to stop a turn that belongs to
// another process.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	result, err := s.deps.Turns.Cancel(r.Context(), id)
	if err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}

	principal, _ := principalFrom(r.Context())
	s.deps.Audit.Record(r.Context(), audit.EventPromptCancelled, id, map[string]any{
		"deviceId": principal.DeviceID,
		"turn":     result.Cancelled,
		"dropped":  result.Dropped,
	})
	_ = httpcore.RespondJSON(w, http.StatusOK, result)
}

// handleDropQueued removes one prompt waiting behind a running turn.
//
// It is separate from cancel on purpose: dropping a follow-up the operator no
// longer wants must not stop the work already in progress.
func (s *Server) handleDropQueued(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	turnID := r.PathValue("turnId")

	dropped, err := s.deps.Turns.Drop(id, turnID)
	if err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	if !dropped {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindNotFound, "no_such_turn",
				"that prompt is not queued; it may have started or been dropped already"))
		return
	}

	principal, _ := principalFrom(r.Context())
	s.deps.Audit.Record(r.Context(), audit.EventPromptCancelled, id, map[string]any{
		"deviceId": principal.DeviceID,
		"turnId":   turnID,
		"dropped":  1,
	})
	_ = httpcore.RespondJSON(w, http.StatusOK, s.deps.Turns.Queue(id))
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

// acceptedImageTypes are the formats a prompt may carry.
//
// A closed set rather than `image/*`: naming them is what lets a format the
// model cannot read be refused *before* the turn starts, where the answer is a
// 400 the app can explain, rather than after, where it is a failed turn the
// operator has to interpret. HEIC is absent on purpose — it is what an iPhone
// gallery produces, and the app converts it before sending.
var acceptedImageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// toHarnessBlocks validates and converts prompt blocks.
//
// The rules are the ones the ACP adapter applies, checked one layer earlier: a
// prompt the harness cannot carry is answered with a 400 the app can explain,
// instead of being admitted as a turn that fails a moment later for a reason the
// operator never sees.
func (s *Server) toHarnessBlocks(in []promptBlock) ([]harness.PromptBlock, error) {
	if len(in) == 0 {
		return nil, errx.New(errx.KindInvalid, "empty_prompt", "a prompt must contain at least one block")
	}
	out := make([]harness.PromptBlock, 0, len(in))
	textBytes := 0
	meaningful := 0
	for _, b := range in {
		switch b.Type {
		case "", "text":
			textBytes += len(b.Text)
			if textBytes > s.deps.Config.Limits.MaxPromptBytes {
				return nil, errx.New(errx.KindInvalid, "prompt_too_large",
					"the prompt exceeds the configured size limit")
			}
			// A blank text block beside an image is normal — the composer sends
			// its caption even when there is none — so it is carried and dropped
			// later rather than refused here.
			if strings.TrimSpace(b.Text) != "" {
				meaningful++
			}
			out = append(out, harness.PromptBlock{Type: "text", Text: b.Text})

		case "image":
			block, err := s.imageBlock(b)
			if err != nil {
				return nil, err
			}
			out = append(out, block)
			meaningful++

		default:
			return nil, errx.New(errx.KindInvalid, "unsupported_block",
				"prompt block type "+b.Type+" is not supported")
		}
	}
	if meaningful == 0 {
		return nil, errx.New(errx.KindInvalid, "empty_prompt",
			"a prompt must contain at least one non-empty block")
	}
	return out, nil
}

// imageBlock validates one image block and decodes it.
func (s *Server) imageBlock(b promptBlock) (harness.PromptBlock, error) {
	if !s.deps.Harness.Capabilities().CanPromptImages {
		// Fail closed, with an explanation: the alternative is a prompt the model
		// silently never sees.
		return harness.PromptBlock{}, errx.New(errx.KindInvalid, "images_unsupported",
			"the connected harness is not configured to accept images")
	}

	mime := strings.ToLower(strings.TrimSpace(b.MIMEType))
	if !acceptedImageTypes[mime] {
		return harness.PromptBlock{}, errx.New(errx.KindInvalid, "unsupported_image_type",
			"an image must be png, jpeg, webp or gif")
	}

	data, err := decodeBase64(b.Data)
	if err != nil {
		return harness.PromptBlock{}, errx.New(errx.KindInvalid, "invalid_image_data",
			"the image is not valid base64")
	}
	if len(data) == 0 {
		return harness.PromptBlock{}, errx.New(errx.KindInvalid, "invalid_image_data",
			"the image is empty")
	}
	if limit := s.imageLimit(); int64(len(data)) > limit {
		return harness.PromptBlock{}, errx.New(errx.KindInvalid, "image_too_large",
			fmt.Sprintf("the image is %d bytes; the limit is %d", len(data), limit))
	}
	return harness.PromptBlock{Type: "image", MIMEType: mime, Data: data}, nil
}

// imageLimit is the per-image bound: the configured one, or the request body
// limit when none is set. The body limit is enforced before a handler runs, so
// it is a real ceiling either way.
func (s *Server) imageLimit() int64 {
	if n := s.deps.Config.Limits.MaxImageBytes; n > 0 {
		return int64(n)
	}
	return s.deps.Config.Limits.MaxBodyBytes
}

// decodeBase64 accepts padded and unpadded standard base64 alike.
//
// Browsers emit padding, and a client that trims it is making a reasonable guess
// about a format that does not need it. Refusing that would be a compatibility
// bug with no security value.
func decodeBase64(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("empty base64")
	}
	if data, err := base64.StdEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
}
