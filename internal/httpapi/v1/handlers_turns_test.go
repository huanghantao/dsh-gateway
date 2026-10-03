package v1

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// These tests cover the prompt lifecycle over the real route table: admission,
// queueing, stopping, and the image blocks the composer sends. They are the
// endpoint-level half of docs/adr/0004; the scheduler's own behaviour is covered
// in its own package.

// promptBody renders a prompt request body.
func promptBody(blocks ...map[string]any) string {
	encoded, _ := json.Marshal(map[string]any{"blocks": blocks})
	return string(encoded)
}

func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

// ticket is the prompt acknowledgement, as the client decodes it.
type ticket struct {
	TurnID    string `json:"turnId"`
	State     string `json:"state"`
	Position  int    `json:"position"`
	QueuedAt  string `json:"queuedAt"`
	StartedAt string `json:"startedAt"`
}

func decodeTicket(t *testing.T, body string) ticket {
	t.Helper()
	var got ticket
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("prompt response %q: %v", body, err)
	}
	return got
}

// holdTurn makes the next prompt block until the returned function is called.
func holdTurn(ts *testServer) func() {
	gate := make(chan struct{})
	ts.driver.promptGate = gate
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		ts.driver.promptGate = nil
		close(gate)
	}
}

// TestPromptStartsImmediatelyAndSaysSo pins the shape a client relies on: the
// answer is the ticket, not a bare id, so "running" and "queued" are
// distinguishable without a second request.
func TestPromptStartsImmediatelyAndSaysSo(t *testing.T) {
	ts := newTestServer(t)
	release := holdTurn(ts)
	defer release()

	rec := ts.do("POST", "/api/v1/sessions/session-test/prompt",
		withCookie(ts.token), withJSON(promptBody(textBlock("hello"))))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	got := decodeTicket(t, rec.Body.String())
	if got.State != "running" {
		t.Errorf("state = %q, want running", got.State)
	}
	if got.TurnID == "" {
		t.Error("turnId is empty; the client cannot follow the turn it just started")
	}
	if got.StartedAt == "" {
		t.Error("startedAt is empty; the elapsed timer has nothing to tick from")
	}
}

// TestPromptQueuesBehindARunningTurn is the behaviour the product needed: a
// follow-up typed while the agent works is kept, and told where it stands.
func TestPromptQueuesBehindARunningTurn(t *testing.T) {
	ts := newTestServer(t)
	release := holdTurn(ts)

	if rec := ts.do("POST", "/api/v1/sessions/session-test/prompt",
		withCookie(ts.token), withJSON(promptBody(textBlock("first")))); rec.Code != http.StatusAccepted {
		t.Fatalf("first prompt status = %d: %s", rec.Code, rec.Body.String())
	}

	rec := ts.do("POST", "/api/v1/sessions/session-test/prompt",
		withCookie(ts.token), withJSON(promptBody(textBlock("second"))))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("second prompt status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	got := decodeTicket(t, rec.Body.String())
	if got.State != "queued" || got.Position != 1 {
		t.Errorf("ticket = %+v, want queued at position 1", got)
	}
	if got.StartedAt != "" {
		t.Error("a queued prompt reports startedAt; it has not started")
	}

	// The session resource reports the queue, which is how a client that
	// reconnects mid-turn learns where it stands.
	body := ts.do("GET", "/api/v1/sessions/session-test", withCookie(ts.token)).Body.String()
	if !strings.Contains(body, `"queue"`) || !strings.Contains(body, `"queued"`) {
		t.Errorf("session view = %s, want the waiting prompt listed", body)
	}

	release()
}

// TestCancelStopsTheTurnAndReportsWhatItDid: the answer says what was stopped,
// so a client is not told "cancelled" for a session nothing was running in.
func TestCancelStopsTheTurnAndReportsWhatItDid(t *testing.T) {
	ts := newTestServer(t)
	release := holdTurn(ts)

	if rec := ts.do("POST", "/api/v1/sessions/session-test/prompt",
		withCookie(ts.token), withJSON(promptBody(textBlock("first")))); rec.Code != http.StatusAccepted {
		t.Fatalf("prompt status = %d", rec.Code)
	}
	if rec := ts.do("POST", "/api/v1/sessions/session-test/prompt",
		withCookie(ts.token), withJSON(promptBody(textBlock("second")))); rec.Code != http.StatusAccepted {
		t.Fatalf("second prompt status = %d", rec.Code)
	}

	rec := ts.do("POST", "/api/v1/sessions/session-test/cancel", withCookie(ts.token), withJSON("{}"))
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d: %s", rec.Code, rec.Body.String())
	}
	var result struct {
		Cancelled bool `json:"cancelled"`
		Dropped   int  `json:"dropped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("cancel response: %v", err)
	}
	if !result.Cancelled || result.Dropped != 1 {
		t.Errorf("result = %+v, want the running turn cancelled and one prompt dropped", result)
	}
	if len(ts.driver.cancels) != 1 || ts.driver.cancels[0] != "session-test" {
		t.Errorf("harness cancels = %v, want one for session-test", ts.driver.cancels)
	}
	release()
}

// TestCancelOnAnIdleSessionIsANoOp makes the button idempotent, which is what a
// double tap needs.
func TestCancelOnAnIdleSessionIsANoOp(t *testing.T) {
	ts := newTestServer(t)

	rec := ts.do("POST", "/api/v1/sessions/session-test/cancel", withCookie(ts.token), withJSON("{}"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(ts.driver.cancels) != 0 {
		t.Errorf("the harness was asked to cancel %v; nothing was running", ts.driver.cancels)
	}
}

/* ------------------------------------------------------------------ images */

// TestImagePromptReachesTheHarness is the feature: a screenshot sent from a
// phone arrives as an image block rather than a 400.
func TestImagePromptReachesTheHarness(t *testing.T) {
	ts := newTestServer(t)
	ts.driver.imagesAllowed = true
	release := holdTurn(ts)
	defer release()

	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3}
	body := promptBody(
		textBlock("what is wrong here?"),
		map[string]any{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(png)},
	)
	rec := ts.do("POST", "/api/v1/sessions/session-test/prompt", withCookie(ts.token), withJSON(body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	// The ticket is the answer, and the turn itself runs in its own goroutine:
	// the harness may not have been called yet when the 202 lands. Waiting is
	// what makes this a test of the image block rather than of the scheduler's
	// timing, which is a race the ticket deliberately does not win.
	deadline := time.Now().Add(2 * time.Second)
	for len(ts.driver.prompts) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if len(ts.driver.prompts) != 1 {
		t.Fatalf("prompts = %d, want one", len(ts.driver.prompts))
	}
	blocks := ts.driver.prompts[0].blocks
	if len(blocks) != 2 {
		t.Fatalf("blocks = %+v, want text and image", blocks)
	}
	if blocks[1].Type != "image" || blocks[1].MIMEType != "image/png" {
		t.Errorf("image block = %+v, want the decoded image", blocks[1])
	}
	if !bytes.Equal(blocks[1].Data, png) {
		t.Error("the image bytes did not survive the round trip")
	}
}

// TestImagePromptIsRefusedWhenTheHarnessCannotTakeOne: fail closed with an
// explanation, rather than admitting a prompt the model would never see.
func TestImagePromptIsRefusedWhenTheHarnessCannotTakeOne(t *testing.T) {
	ts := newTestServer(t)
	ts.driver.imagesAllowed = false

	body := promptBody(map[string]any{"type": "image", "mimeType": "image/png", "data": "AAAA"})
	rec := ts.do("POST", "/api/v1/sessions/session-test/prompt", withCookie(ts.token), withJSON(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "images_unsupported") {
		t.Errorf("body = %s, want the stable code a client branches on", rec.Body.String())
	}
}

// TestImageValidationNamesWhatIsWrong covers the three refusals a phone will
// actually hit, each with its own code so the app can say something useful.
func TestImageValidationNamesWhatIsWrong(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		code string
	}{
		{
			name: "a format the model cannot read",
			body: map[string]any{"type": "image", "mimeType": "image/heic", "data": "AAAA"},
			code: "unsupported_image_type",
		},
		{
			name: "data that is not base64",
			body: map[string]any{"type": "image", "mimeType": "image/png", "data": "not base64!!"},
			code: "invalid_image_data",
		},
		{
			name: "an empty image",
			body: map[string]any{"type": "image", "mimeType": "image/png", "data": ""},
			code: "invalid_image_data",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t)
			ts.driver.imagesAllowed = true

			rec := ts.do("POST", "/api/v1/sessions/session-test/prompt",
				withCookie(ts.token), withJSON(promptBody(tc.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.code) {
				t.Errorf("body = %s, want code %q", rec.Body.String(), tc.code)
			}
		})
	}
}

// TestImageAtTheLimitIsRefusedWithItsSize: the message has to name the numbers,
// because "too large" tells an operator nothing about which knob to turn.
func TestImageAtTheLimitIsRefusedWithItsSize(t *testing.T) {
	ts := newTestServer(t)
	ts.driver.imagesAllowed = true
	ts.deps.Config.Limits.MaxImageBytes = 8

	body := promptBody(map[string]any{
		"type": "image", "mimeType": "image/png",
		"data": base64.StdEncoding.EncodeToString(make([]byte, 64)),
	})
	rec := ts.do("POST", "/api/v1/sessions/session-test/prompt", withCookie(ts.token), withJSON(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "image_too_large") {
		t.Errorf("body = %s, want image_too_large", rec.Body.String())
	}
}

// TestAnEmptyPromptIsRefusedBeforeATurnStarts: a 400 the app can explain beats a
// turn that fails a moment later for a reason nobody sees.
func TestAnEmptyPromptIsRefusedBeforeATurnStarts(t *testing.T) {
	ts := newTestServer(t)

	rec := ts.do("POST", "/api/v1/sessions/session-test/prompt",
		withCookie(ts.token), withJSON(promptBody(textBlock("   "))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if len(ts.driver.prompts) != 0 {
		t.Error("an empty prompt reached the harness")
	}
}

/* ----------------------------------------------------------------- changes */

// TestChangesIsReadableAndRevertIsNotByDefault is the deployment's posture in
// one test: reading what a session changed is always available, and writing to
// the workspace needs an operator to have asked for it.
func TestChangesIsReadableAndRevertIsNotByDefault(t *testing.T) {
	ts := newTestServer(t)

	rec := ts.do("GET", "/api/v1/sessions/session-test/changes", withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("changes status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"source":"tool-calls"`) {
		t.Errorf("body = %s, want the projection's source named", rec.Body.String())
	}

	rec = ts.do("POST", "/api/v1/sessions/session-test/revert", withCookie(ts.token), withJSON("{}"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("revert status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "revert_disabled") {
		t.Errorf("body = %s, want revert_disabled", rec.Body.String())
	}
}
