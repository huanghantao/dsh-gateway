package v1

import (
	"net/http"
	"strings"

	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
	"github.com/huanghantao/dsh-gateway/internal/sessionlog"
)

// searchMatch is a match plus what the client needs in order to act on it.
type searchMatch struct {
	sessionlog.Match
	// Archived marks a match inside a session the operator put away. Archived
	// sessions are still searched — the point is to find what was written, and
	// hiding a match because someone tidied their list would be the wrong kind of
	// quiet — but a result that looked like a live session and was not would be
	// worse.
	Archived bool `json:"archived,omitempty"`
}

// searchResponse reports what was read as well as what was found: a bounded
// search that did not say it was bounded would answer "nothing found" with the
// same confidence whether it read three sessions or three hundred.
type searchResponse struct {
	Query     string        `json:"query"`
	Scanned   int           `json:"scanned"`
	Matches   []searchMatch `json:"matches"`
	Truncated bool          `json:"truncated"`
}

// handleSearchSessions looks for text inside session transcripts.
func (s *Server) handleSearchSessions(w http.ResponseWriter, r *http.Request) {
	if s.deps.Sessions == nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindUnavailable, "transcript_disabled", "this gateway does not read session logs"))
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindInvalid, "query_required", "what should be looked for?"))
		return
	}
	workspace := r.URL.Query().Get("workspace")
	if workspace != "" && !s.workspaceAllowed(workspace) {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindInvalid, "unknown_workspace", "that workspace is not configured"))
		return
	}

	opts := sessionlog.DefaultSearchOptions()
	opts.Workspace = workspace
	matches, scanned, err := s.deps.Sessions.Search(r.Context(), query, opts)
	if err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}

	results := make([]searchMatch, 0, len(matches))
	for _, match := range matches {
		archived := false
		if s.deps.Curation != nil {
			archived = s.deps.Curation.Decision(match.SessionID).Archived
		}
		results = append(results, searchMatch{Match: match, Archived: archived})
	}

	_ = httpcore.RespondJSON(w, http.StatusOK, searchResponse{
		Query:   query,
		Scanned: scanned,
		Matches: results,
		// The bound was reached, so there may be more: saying so is the
		// difference between "that is all there is" and "that is all I looked at".
		Truncated: scanned >= opts.Sessions || len(matches) >= opts.Matches,
	})
}
