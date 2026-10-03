package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/huanghantao/dsh-gateway/internal/audit"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
)

// decideApprovalRequest is the body of POST /approvals/{id}.
type decideApprovalRequest struct {
	OptionID string `json:"optionId"`
}

// handleListApprovals returns every decision waiting on a human.
func (s *Server) handleListApprovals(w http.ResponseWriter, _ *http.Request) {
	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{
		"approvals": s.deps.Approvals.List(),
	})
}

// handleDecideApproval records an operator's decision.
//
// The decision is attributed to the device that made it, and the audit record is
// written before the agent is allowed to proceed. If the audit write fails the
// decision still stands — refusing to run an approved command because a log file
// is unwritable would be a worse failure — but the error is logged.
func (s *Server) handleDecideApproval(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req decideApprovalRequest
	if err := httpcore.DecodeJSON(w, r, &req); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	if req.OptionID == "" {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindInvalid, "option_required", "an optionId is required"))
		return
	}

	principal, _ := principalFrom(r.Context())

	// Capture what was authorised before resolving it, so the audit record names
	// the tool and its arguments rather than just an opaque id.
	view, _ := s.deps.Approvals.Get(id)

	if err := s.deps.Approvals.Decide(r.Context(), id, req.OptionID, principal.DeviceID); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}

	fields := map[string]any{
		"deviceId":  principal.DeviceID,
		"optionId":  req.OptionID,
		"sessionId": view.SessionID,
		"tool":      view.Tool,
	}
	// The tool's arguments are fingerprinted rather than recorded.
	//
	// They are the most sensitive thing that passes through this process — the
	// body of a Write, the text of an Edit, the full command line of a Bash — and
	// an audit log is kept for months and rotated, not deleted, when the session
	// it belongs to is. Keeping a digest preserves what the log is actually for
	// (tying this decision to that call, and showing two decisions approved the
	// same thing) without turning the log into a second copy of the operator's
	// files.
	if view.Input != "" {
		sum := sha256.Sum256([]byte(view.Input))
		fields["inputBytes"] = len(view.Input)
		fields["inputSha256"] = hex.EncodeToString(sum[:])
	}
	s.deps.Audit.Record(r.Context(), audit.EventApprovalDecided, view.SessionID, fields)

	w.WriteHeader(http.StatusNoContent)
}

// handleListGrants returns the standing authorisations that are currently
// answering approvals without asking.
//
// It is a first-class resource rather than a detail of the approval sheet: an
// authorisation that is in force has to be visible somewhere, or the only way to
// know one exists is to remember giving it.
func (s *Server) handleListGrants(w http.ResponseWriter, _ *http.Request) {
	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{
		"grants": s.deps.Approvals.Grants(),
	})
}

// handleRevokeGrant withdraws a standing authorisation.
//
// The next request that would have matched it asks a human again, which is the
// whole point: revoking has to take effect immediately or it is not a control.
func (s *Server) handleRevokeGrant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if !s.deps.Approvals.RevokeGrant(id) {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindNotFound, "no_such_grant",
				"that authorisation is not in force; it may have expired"))
		return
	}

	principal, _ := principalFrom(r.Context())
	s.deps.Audit.Record(r.Context(), audit.EventApprovalGrantRevoked, "", map[string]any{
		"deviceId": principal.DeviceID,
		"grantId":  id,
	})
	w.WriteHeader(http.StatusNoContent)
}
