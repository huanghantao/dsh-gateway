package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/lifecycle"
	"github.com/huanghantao/dsh-gateway/internal/app/questions"
	"github.com/huanghantao/dsh-gateway/internal/audit"
	"github.com/huanghantao/dsh-gateway/internal/config"
)

// askBody is one question request as the answerer plugin posts it.
const askBody = `{
  "id": "ask-1",
  "sessionId": "session-test",
  "questions": [
    {
      "id": "style",
      "header": "回答风格",
      "question": "你希望我平时回答的风格是？",
      "options": [
        {"label": "简洁直接 (Recommended)", "description": "先给结论。", "recommended": true},
        {"label": "详细解释", "description": "把推导讲清楚。"}
      ]
    },
    {"id": "extras", "question": "还要什么？", "multiSelect": true}
  ]
}`

// bridgeRequest builds the answerer's request.
func bridgeRequest(ctx context.Context, body string) *http.Request {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, PathQuestions, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testBridgeToken)
	return req
}

// pendingQuestion waits for a question to appear in the broker, so a test never
// sleeps on a race it can observe.
func pendingQuestion(t *testing.T, ts *testServer, id string) questions.View {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if view, ok := ts.deps.Questions.Get(id); ok {
			return view
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("question %s never became pending", id)
	return questions.View{}
}

// answerViaAPI answers a question the way the phone does.
func answerViaAPI(t *testing.T, ts *testServer, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/questions/"+id, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", sameOrigin)
	req.AddCookie(&http.Cookie{Name: "dsh_gw_session", Value: ts.token})
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)
	return rec
}

/* ------------------------------------------------------------------ tests */

// The whole capability, in one test: the answerer posts a question, the phone
// lists it, the phone answers it, and the answer comes back down the same
// connection to the model.
func TestQuestionRoundTripFromTheAnswererToThePhoneAndBack(t *testing.T) {
	ts := newTestServer(t)

	type reply struct {
		rec *httptest.ResponseRecorder
	}
	done := make(chan reply, 1)
	go func() {
		rec := httptest.NewRecorder()
		ts.handler.ServeHTTP(rec, bridgeRequest(context.Background(), askBody))
		done <- reply{rec}
	}()

	view := pendingQuestion(t, ts, "ask-1")
	if len(view.Items) != 2 {
		t.Fatalf("the question reached the broker as %+v", view.Items)
	}
	if !view.Items[0].Options[0].Recommended {
		t.Error("the recommended option lost its flag on the way in")
	}

	// The phone's list is the same thing, in the shape the app decodes.
	listed := ts.do(http.MethodGet, "/api/v1/questions", withCookie(ts.token))
	if listed.Code != http.StatusOK {
		t.Fatalf("GET /questions returned %d: %s", listed.Code, listed.Body.String())
	}
	var body struct {
		Questions []questions.View `json:"questions"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the question list: %v", err)
	}
	if len(body.Questions) != 1 || body.Questions[0].ID != "ask-1" {
		t.Fatalf("listed questions = %+v, want the pending one", body.Questions)
	}

	answered := answerViaAPI(t, ts, "ask-1",
		`{"answers":[{"id":"style","selected":["简洁直接 (Recommended)"],"custom":"简短即可"}]}`)
	if answered.Code != http.StatusNoContent {
		t.Fatalf("answering returned %d: %s", answered.Code, answered.Body.String())
	}

	select {
	case got := <-done:
		if got.rec.Code != http.StatusOK {
			t.Fatalf("the bridge answered %d: %s", got.rec.Code, got.rec.Body.String())
		}
		var response struct {
			Outcome string             `json:"outcome"`
			Answers []questions.Answer `json:"answers"`
			Reason  string             `json:"reason"`
		}
		if err := json.Unmarshal(got.rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode the bridge reply: %v", err)
		}
		if response.Outcome != string(questions.OutcomeAnswered) {
			t.Fatalf("outcome = %q (%s), want answered", response.Outcome, response.Reason)
		}
		if len(response.Answers) != 2 {
			t.Fatalf("answers = %+v, want one per question", response.Answers)
		}
		if response.Answers[0].Custom != "简短即可" {
			t.Errorf("the typed answer did not survive: %+v", response.Answers[0])
		}
		// The question the operator skipped is present and empty, which is what
		// the model reads as "they passed on this one".
		if response.Answers[1].ID != "extras" || len(response.Answers[1].Selected) != 0 {
			t.Errorf("skipped answer = %+v", response.Answers[1])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the bridge never returned after the question was answered")
	}

	if _, ok := ts.deps.Questions.Get("ask-1"); ok {
		t.Error("the question is still pending after being answered")
	}
}

// A question is read and typed, not tapped, so its wait must outlive the
// gateway's generic request deadline. Without that, every question would be
// withdrawn at the read timeout while still on screen.
func TestQuestionBridgeOutlivesTheRequestDeadline(t *testing.T) {
	ts := newTestServer(t)

	// A read timeout far shorter than the wait the test then exercises.
	ts.deps.Config.Limits.ReadTimeout = config.Duration(40 * time.Millisecond)
	ts.handler = ts.Handler()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		ts.handler.ServeHTTP(rec, bridgeRequest(context.Background(), askBody))
		done <- rec
	}()
	pendingQuestion(t, ts, "ask-1")

	// Well past the handler deadline, and past the point where a naive
	// implementation would have withdrawn the question as "the asker left".
	time.Sleep(120 * time.Millisecond)
	if _, ok := ts.deps.Questions.Get("ask-1"); !ok {
		t.Fatal("the question was withdrawn when the generic request deadline passed")
	}

	if rec := answerViaAPI(t, ts, "ask-1", `{"answers":[{"id":"style","selected":["详细解释"]}]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("answering returned %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "详细解释") {
			t.Fatalf("the bridge returned %d: %s", rec.Code, rec.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the bridge never returned: a deadline took a question that a person answered")
	}
}

// The other half of the same rule: a disconnect is not a deadline. When the
// answerer goes away — a stopped turn, a dead child — the question goes with it.
func TestQuestionBridgeWithdrawsWhenTheAskerDisappears(t *testing.T) {
	ts := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		ts.handler.ServeHTTP(rec, bridgeRequest(ctx, askBody))
		done <- rec
	}()
	pendingQuestion(t, ts, "ask-1")

	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := ts.deps.Questions.Get("ask-1"); !ok {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, ok := ts.deps.Questions.Get("ask-1"); ok {
		t.Fatal("a question outlived the answerer that asked it")
	}

	// The question is gone, so the phone cannot answer it — and says so the way
	// the app already understands.
	if rec := answerViaAPI(t, ts, "ask-1", `{"answers":[]}`); rec.Code != http.StatusConflict {
		t.Fatalf("answering a withdrawn question returned %d, want 409", rec.Code)
	}
}

func TestQuestionAnsweringIsValidated(t *testing.T) {
	cases := []struct {
		name string
		id   string
		body string
		// pending starts an answerer that is waiting on a question. A case that
		// answers nothing pending leaves it off: a parked bridge request would
		// hold the test open until the gateway's own request deadline.
		pending bool
		status  int
		code    string
	}{
		{
			name:    "an option that was never offered",
			id:      "ask-1",
			body:    `{"answers":[{"id":"style","selected":["多给代码"]}]}`,
			pending: true,
			status:  http.StatusBadRequest,
			code:    "unknown_option",
		},
		{
			name:    "a question this request did not ask",
			id:      "ask-1",
			body:    `{"answers":[{"id":"nope","selected":[]}]}`,
			pending: true,
			status:  http.StatusBadRequest,
			code:    "unknown_question",
		},
		{
			name:   "a question that is not pending at all",
			id:     "ask-nothing",
			body:   `{"answers":[]}`,
			status: http.StatusConflict,
			code:   "question_closed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t)
			done := make(chan struct{})
			if tc.pending {
				go func() {
					defer close(done)
					rec := httptest.NewRecorder()
					ts.handler.ServeHTTP(rec, bridgeRequest(context.Background(), askBody))
				}()
				pendingQuestion(t, ts, "ask-1")
			} else {
				close(done)
			}

			rec := answerViaAPI(t, ts, tc.id, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if body := rec.Body.String(); !strings.Contains(body, tc.code) {
				t.Fatalf("body = %s, want the code %q", body, tc.code)
			}

			// Whatever was rejected, the question must still be answerable: the
			// operator is looking at the card and will try again.
			if tc.id == "ask-1" {
				if _, ok := ts.deps.Questions.Get("ask-1"); !ok {
					t.Fatal("a rejected answer closed the question")
				}
				answerViaAPI(t, ts, "ask-1", `{"answers":[]}`)
			}
			<-done
		})
	}
}

// The bridge is a credential wall of its own: it takes no device cookie, and the
// token it does take is the one this process minted.
func TestQuestionBridgeRefusesEveryOtherCaller(t *testing.T) {
	ts := newTestServer(t)

	cases := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "no credential at all", mutate: func(*http.Request) {}},
		{name: "a device cookie", mutate: func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "dsh_gw_session", Value: ts.token})
		}},
		{name: "an empty bearer token", mutate: func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer ")
		}},
		{name: "the wrong token", mutate: func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+testBridgeToken+"x")
		}},
		{name: "a token of the right length and the wrong bytes", mutate: func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+strings.Repeat("0", len(testBridgeToken)))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
				PathQuestions, strings.NewReader(askBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "")
			tc.mutate(req)

			rec := httptest.NewRecorder()
			ts.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
			}
			if _, ok := ts.deps.Questions.Get("ask-1"); ok {
				t.Fatal("an unauthenticated request parked a question")
			}
		})
	}
}

// A snapshot is what a reconnecting phone renders from, so a question that is
// waiting for an answer has to be in it — otherwise the card vanishes on every
// reconnect and the agent stays blocked with nothing on screen.
func TestSnapshotCarriesPendingQuestions(t *testing.T) {
	ts := newTestServer(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		ts.handler.ServeHTTP(rec, bridgeRequest(context.Background(), askBody))
	}()
	pendingQuestion(t, ts, "ask-1")

	frame := ts.snapshot(nil, 0)
	if len(frame.Questions) != 1 || frame.Questions[0].ID != "ask-1" {
		t.Fatalf("snapshot questions = %+v", frame.Questions)
	}

	// Scoped to another session, the question is not this client's business.
	if other := ts.snapshot([]string{"another-session"}, 0); len(other.Questions) != 0 {
		t.Fatalf("a scoped snapshot leaked another session's question: %+v", other.Questions)
	}
	if mine := ts.snapshot([]string{"session-test"}, 0); len(mine.Questions) != 1 {
		t.Fatalf("a scoped snapshot lost its own session's question: %+v", mine.Questions)
	}

	answerViaAPI(t, ts, "ask-1", `{"answers":[]}`)
	<-done
}

// The answers are what the operator typed, so the audit log records a digest and
// a count rather than a second copy of the words.
func TestAnsweringAQuestionIsAuditedWithoutCopyingTheAnswer(t *testing.T) {
	ts := newTestServer(t)

	const secret = "my account number is 1234"
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		ts.handler.ServeHTTP(rec, bridgeRequest(context.Background(), askBody))
	}()
	pendingQuestion(t, ts, "ask-1")

	rec := answerViaAPI(t, ts, "ask-1",
		`{"answers":[{"id":"style","selected":["详细解释"],"custom":"`+secret+`"}]}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("answering returned %d: %s", rec.Code, rec.Body.String())
	}
	<-done

	raw, err := os.ReadFile(filepath.Join(ts.stateDir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("read the audit log: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Error("the audit log contains the answer verbatim; a typed answer is as sensitive as a tool's arguments")
	}

	var record struct {
		Event  string         `json:"event"`
		Fields map[string]any `json:"fields"`
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var candidate struct {
			Event  string         `json:"event"`
			Fields map[string]any `json:"fields"`
		}
		if err := json.Unmarshal([]byte(line), &candidate); err != nil {
			continue
		}
		if candidate.Event == string(audit.EventQuestionAnswered) {
			record, found = candidate, true
		}
	}
	if !found {
		t.Fatal("the answer was not audited at all")
	}
	if got, _ := record.Fields["questionId"].(string); got != "ask-1" {
		t.Errorf("audited questionId = %q", got)
	}
	if got, _ := record.Fields["sessionId"].(string); got != "session-test" {
		t.Errorf("audited sessionId = %q", got)
	}
	if sum, _ := record.Fields["answersSha256"].(string); sum == "" {
		t.Error("no answersSha256: a digest is what ties the record to what was said")
	}
	if got, _ := record.Fields["answered"].(float64); got != 1 {
		t.Errorf("answered = %v, want 1", record.Fields["answered"])
	}
	if got, _ := record.Fields["skipped"].(float64); got != 1 {
		t.Errorf("skipped = %v, want 1", record.Fields["skipped"])
	}
}

// A malformed request from the answerer is its own bug, and it must not park a
// question the operator could see.
func TestQuestionBridgeRejectsAMalformedAsk(t *testing.T) {
	ts := newTestServer(t)

	for _, body := range []string{
		`{`,
		`{"id":"","questions":[{"id":"a","question":"?"}]}`,
		`{"id":"ask-x","questions":[]}`,
		`{"id":"ask-x","questions":[{"id":"","question":"?"}]}`,
	} {
		rec := httptest.NewRecorder()
		ts.handler.ServeHTTP(rec, bridgeRequest(context.Background(), body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("a malformed ask returned %d: %s", rec.Code, rec.Body.String())
		}
	}
	if len(ts.deps.Questions.List()) != 0 {
		t.Fatal("a malformed ask parked a question")
	}
}

// The bridge is the one route a client cannot reach with a device credential, so
// it must not become a way to sidestep the drain either.
func TestQuestionBridgeRefusesWhileDraining(t *testing.T) {
	ts := newTestServer(t)
	ts.life.Drain(lifecycle.ReasonDeploy)

	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, bridgeRequest(context.Background(), askBody))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 while draining", rec.Code)
	}
	if len(ts.deps.Questions.List()) != 0 {
		t.Fatal("a draining gateway parked a question")
	}
}
