package v1

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/huanghantao/dsh-gateway/internal/app/questions"
	"github.com/huanghantao/dsh-gateway/internal/audit"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
)

// PathQuestions is the agent-facing route the answerer plugin posts to.
//
// It is exported because two places must agree on it and neither can derive it
// from the other: the route table below, and the composition root, which writes
// the plugin's endpoint file before any server exists.
const PathQuestions = "/internal/questions"

// answerQuestionRequest is the body of POST /questions/{id}.
type answerQuestionRequest struct {
	// Answers are the operator's answers. A question left out is treated as
	// skipped, which is what the sheet's own "next" does.
	Answers []questions.Answer `json:"answers"`
}

// askRequest is the body the answerer posts.
type askRequest struct {
	// ID is the answerer's id for this ask, stable across its retries.
	ID string `json:"id"`
	// SessionID is the DSH session that asked.
	SessionID string `json:"sessionId"`
	// Questions are the questions, in the model's order.
	Questions []questions.Item `json:"questions"`
}

// askResponse is what the answerer receives.
type askResponse struct {
	// Outcome is "answered" or "unanswered".
	Outcome string `json:"outcome"`
	// Answers is present when Outcome is "answered".
	Answers []questions.Answer `json:"answers,omitempty"`
	// Reason explains an unanswered outcome: "timeout", "cancelled", "shutdown".
	Reason string `json:"reason,omitempty"`
}

// handleListQuestions returns every question waiting on a human.
func (s *Server) handleListQuestions(w http.ResponseWriter, _ *http.Request) {
	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{
		"questions": s.deps.Questions.List(),
	})
}

// handleAnswerQuestion records the operator's answers.
//
// The audit record is written after the answer is delivered, because unlike an
// approval there is nothing to gate: the decision *is* the answer, and refusing
// to deliver it because a log file is unwritable would lose the one thing the
// operator just took the trouble to type.
func (s *Server) handleAnswerQuestion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req answerQuestionRequest
	if err := httpcore.DecodeJSON(w, r, &req); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}

	principal, _ := principalFrom(r.Context())
	// Read before answering, so the audit record can name the session.
	view, _ := s.deps.Questions.Get(id)

	if err := s.deps.Questions.Answer(id, req.Answers, principal.DeviceID); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}

	fields := map[string]any{
		"deviceId":   principal.DeviceID,
		"sessionId":  view.SessionID,
		"questionId": id,
		"answered":   answeredCount(req.Answers),
		"skipped":    len(view.Items) - answeredCount(req.Answers),
	}
	// The answers themselves are fingerprinted rather than recorded, for the
	// same reason an approval's arguments are: a free-text answer can be
	// anything the operator typed, and an audit log is kept for months. The
	// digest still ties this decision to the words it was made of.
	if digest, size, ok := fingerprintAnswers(req.Answers); ok {
		fields["answersBytes"] = size
		fields["answersSha256"] = digest
	}
	s.deps.Audit.Record(r.Context(), audit.EventQuestionAnswered, view.SessionID, fields)

	w.WriteHeader(http.StatusNoContent)
}

// handleQuestionBridge is the agent-facing half of the question capability.
//
// It is a long poll rather than a callback: the plugin that runs inside the
// harness holds one HTTP request open for as long as the question is pending, so
// the "the asker went away" signal is a real disconnect — a stopped turn, a dead
// child — and needs no separate withdrawal call.
//
// The wait deliberately outlives the generic request deadline. A question is
// meant to be read and typed, and the gateway's own read timeout is measured in
// seconds; honouring it here would withdraw every question at the minute mark and
// tell the model nobody answered one that is still on screen. So the broker's
// wait runs on a context detached from that deadline, while still noticing a
// disconnect: context.Cause distinguishes the two, because a deadline reports
// DeadlineExceeded and a closed connection reports the server's cancellation.
func (s *Server) handleQuestionBridge(w http.ResponseWriter, r *http.Request) {
	if !s.admit(w, r) {
		return
	}

	var req askRequest
	if err := httpcore.DecodeJSON(w, r, &req); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}

	// The wait is deliberately not bounded by the request's own deadline; see
	// bridgeWaitContext.
	wait := bridgeWaitContext(r)
	result, err := s.deps.Questions.Request(wait, questions.Request{ //nolint:contextcheck // the wait outlives the generic request deadline on purpose
		ID:        req.ID,
		SessionID: req.SessionID,
		Items:     req.Questions,
	})
	if err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}

	response := askResponse{Outcome: string(result.Outcome), Answers: result.Answers, Reason: result.Reason}
	_ = httpcore.RespondJSON(w, http.StatusOK, response)
}

// bridgeWaitContext returns a context that survives the generic request deadline
// but not a client disconnect. See handleQuestionBridge.
func bridgeWaitContext(r *http.Request) context.Context {
	request := r.Context()
	ctx, cancel := context.WithCancel(context.WithoutCancel(request))
	// The watcher ends with the request: net/http cancels a request's context
	// when its handler returns, which is what stops this goroutine.
	context.AfterFunc(request, func() {
		// A deadline that fired is the gateway's own bound, not the asker
		// leaving; withdrawing on it would end a question nobody has finished
		// reading. Any other cancellation is the connection going away.
		if errors.Is(context.Cause(request), context.DeadlineExceeded) {
			return
		}
		cancel()
	})
	return ctx
}

// bridgeAuthorized authenticates the answerer plugin.
//
// The plugin runs inside the DSH child, in an environment the gateway itself
// composed, so it presents a bearer token that exists only in the endpoint file
// the gateway wrote 0600 for this process. There is no device credential here and
// no pairing: this route is not for people, and a client without the token is
// answered exactly as if it did not exist.
//
// The comparison is constant time. An empty token never authenticates anything,
// which matters because "questions are switched off" must not degrade into
// "anything with an empty Authorization header is trusted".
func (s *Server) bridgeAuthorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validBridgeToken(s.deps.QuestionToken, bearerFrom(r)) {
			httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
				errx.New(errx.KindUnauthenticated, "authentication_required",
					"the question bridge requires the token from the harness endpoint file"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// answeredCount counts the answers that say something.
func answeredCount(answers []questions.Answer) int {
	count := 0
	for _, answer := range answers {
		if len(answer.Selected) > 0 || answer.Custom != "" {
			count++
		}
	}
	return count
}

// fingerprintAnswers digests an answer set, and reports its size.
func fingerprintAnswers(answers []questions.Answer) (digest string, size int, ok bool) {
	encoded, err := json.Marshal(answers)
	if err != nil {
		return "", 0, false
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), len(encoded), true
}
