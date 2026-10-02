package v1

import (
	"net/http"

	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
)

// handleSessionReceipt answers "what did this session actually do".
//
// The conversation answers "what was said"; this answers the question someone
// asks a day later — which files changed, how many tokens went through, how long
// it ran. Everything is derived from the log already on disk, so it cannot drift
// from the conversation it describes.
func (s *Server) handleSessionReceipt(w http.ResponseWriter, r *http.Request) {
	if s.deps.Sessions == nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindUnavailable, "transcript_disabled", "this gateway does not read session logs"))
		return
	}
	id := r.PathValue("id")
	receipt, err := s.deps.Sessions.Receipt(r.Context(), id)
	if err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}

	// The model and effort come from the log's own model/selection event, which is
	// what the session actually ran with — not from the gateway's defaults, which
	// are what it *would* run with next.
	_ = httpcore.RespondJSON(w, http.StatusOK, receipt)
}
