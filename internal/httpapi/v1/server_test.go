package v1

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/approvals"
	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/app/lease"
	"github.com/huanghantao/dsh-gateway/internal/app/lifecycle"
	"github.com/huanghantao/dsh-gateway/internal/app/turns"
	"github.com/huanghantao/dsh-gateway/internal/audit"
	"github.com/huanghantao/dsh-gateway/internal/authn/devicetoken"
	"github.com/huanghantao/dsh-gateway/internal/authn/ratelimit"
	"github.com/huanghantao/dsh-gateway/internal/config"
	"github.com/huanghantao/dsh-gateway/internal/curation"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/pairing"
	"github.com/huanghantao/dsh-gateway/internal/sessionlog"
)

// fakeHarness satisfies harness.Harness without spawning anything.
//
// The API tests care about admission — who may reach a handler — not about what
// the handler does with a real agent. A fake keeps them fast and deterministic,
// and its zero values are the "not ready" answers a caller must cope with.
type fakeHarness struct {
	// mu guards everything below it.
	//
	// A turn runs on its own goroutine — that is the point of the scheduler — so
	// the test that starts one and the fake that records it are concurrent by
	// construction. Without this the race detector reports the test helper
	// writing the gate while the turn goroutine reads it, which is a real race
	// in the test rather than in the code under test, and a flaky one at that.
	mu       sync.Mutex
	state    harness.State
	sessions []harness.SessionInfo
	config   []harness.ConfigOption
	// listCalls counts how often the server enumerated sessions, which is the
	// cost the listing cache exists to remove.
	listCalls int
	// setCalls records what the server asked the harness to change. Without it
	// a test can only assert that a session was created, not which model it was
	// told to use — and that is the whole question.
	setCalls []setConfigCall
	// promptGate, when non-nil, holds Prompt open until a value is sent. A turn
	// that settles instantly cannot show a queue, which is most of what the
	// prompt endpoints are for.
	promptGate chan struct{}
	// prompts records every prompt that reached the harness, in order.
	prompts []promptCall
	// imagesAllowed is what Capabilities reports.
	imagesAllowed bool
	// cancels records every Cancel.
	cancels []string
}

// promptCall is one harness.Prompt invocation.
type promptCall struct {
	sessionID string
	blocks    []harness.PromptBlock
}

// setConfigCall is one harness.SetConfigOption invocation.
type setConfigCall struct {
	option string
	value  string
}

func (f *fakeHarness) Start(context.Context) error { return nil }
func (f *fakeHarness) Close(context.Context) error { return nil }

func (f *fakeHarness) State() harness.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeHarness) Capabilities() harness.Capabilities {
	f.mu.Lock()
	defer f.mu.Unlock()
	return harness.Capabilities{
		ProtocolVersion: 1, CanList: true, CanResume: true, CanClose: true,
		CanPromptImages: f.imagesAllowed,
	}
}

func (f *fakeHarness) ListSessions(context.Context, string, string) (harness.SessionPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return harness.SessionPage{Sessions: f.sessions}, nil
}

func (f *fakeHarness) NewSession(_ context.Context, workspace string) (harness.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return harness.Session{
		Info:   harness.SessionInfo{ID: "session-test", Workspace: workspace},
		Config: f.config,
	}, nil
}

func (f *fakeHarness) ResumeSession(_ context.Context, id, workspace string) (harness.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return harness.Session{
		Info:   harness.SessionInfo{ID: id, Workspace: workspace},
		Config: f.config,
	}, nil
}

func (f *fakeHarness) CloseSession(context.Context, string) error   { return nil }
func (f *fakeHarness) ReleaseSession(context.Context, string) error { return nil }

func (f *fakeHarness) SetConfigOption(_ context.Context, _, option, value string) ([]harness.ConfigOption, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls = append(f.setCalls, setConfigCall{option: option, value: value})
	return f.config, nil
}

// HoldPrompt makes the next prompt block until the returned function is called.
//
// It lives on the fake rather than in a test helper because the gate is shared
// between the test goroutine and the turn goroutine, so the lock that guards it
// has to be the fake's.
func (f *fakeHarness) HoldPrompt() func() {
	gate := make(chan struct{})
	f.mu.Lock()
	f.promptGate = gate
	f.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.promptGate = nil
			f.mu.Unlock()
			close(gate)
		})
	}
}

func (f *fakeHarness) Prompt(ctx context.Context, sessionID string, blocks []harness.PromptBlock) (string, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, promptCall{sessionID: sessionID, blocks: blocks})
	gate := f.promptGate
	f.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "end_turn", nil
}

func (f *fakeHarness) promptCalls() []promptCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]promptCall(nil), f.prompts...)
}

func (f *fakeHarness) setConfigCalls() []setConfigCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]setConfigCall(nil), f.setCalls...)
}

func (f *fakeHarness) cancelCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancels...)
}

func (f *fakeHarness) listCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

func (f *fakeHarness) clearSetCalls() {
	f.mu.Lock()
	f.setCalls = nil
	f.mu.Unlock()
}

func (f *fakeHarness) resetListCalls() {
	f.mu.Lock()
	f.listCalls = 0
	f.mu.Unlock()
}

func (f *fakeHarness) setState(state harness.State) {
	f.mu.Lock()
	f.state = state
	f.mu.Unlock()
}

func (f *fakeHarness) Cancel(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, sessionID)
	return nil
}

// testServer bundles the API with the pieces a test needs to drive it.
type testServer struct {
	*Server
	// handler is Server.Handler(): the route table wrapped in the production
	// middleware chain.
	//
	// Tests go through it rather than the bare mux on purpose. The outer chain
	// is what installs the request id that every problem document carries, so a
	// test against the raw mux would silently skip it and could not notice if it
	// stopped working.
	handler http.Handler
	// bus is the event bus behind the API, so a test can publish the frames a
	// real harness would have published — including one from "before a restart".
	bus *events.Bus
	// life is the gateway's own state, so a test can drive a redeploy rather
	// than only describe one.
	life   *lifecycle.Lifecycle
	auth   *devicetoken.Authenticator
	pair   *pairing.Service
	token  string // a credential for an enrolled device
	logger *logx.Logger
	// driver is the fake harness behind the API, so a test can hold a turn open
	// or inspect what reached the agent.
	driver *fakeHarness
	// stateDir is where the gateway keeps devices, the audit log and the pairing
	// secret, so a test can read back what the gateway wrote down about a
	// request rather than only what it answered.
	stateDir string
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	dir := t.TempDir()
	workspace := t.TempDir()
	now := time.Now

	cfg := config.Default()
	cfg.StateDir = dir
	cfg.PublicURL = "https://gateway.test"
	cfg.Workspaces = []string{workspace}

	logger := logx.Discard()

	devices, err := devicetoken.Open(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatalf("devicetoken.Open: %v", err)
	}
	auth := devicetoken.New(devices, time.Hour, now)

	limiter := ratelimit.New(ratelimit.Config{
		PerSecond: 1000, Burst: 1000, // effectively unlimited, so tests are not about the limiter
		MaxFailures: 5, LockoutBase: time.Second, MaxLockout: time.Minute,
	}, now)

	pairSvc, err := pairing.Open(dir, 10*time.Minute, auth, limiter, now)
	if err != nil {
		t.Fatalf("pairing.Open: %v", err)
	}

	auditLog, err := audit.Open(audit.Config{Path: filepath.Join(dir, "audit.jsonl")}, logger, now)
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	t.Cleanup(func() { _ = auditLog.Close() })

	curationStore, err := curation.Open(curation.Options{StateDir: dir, Logger: logx.Discard()})
	if err != nil {
		t.Fatalf("curation.Open: %v", err)
	}

	// A real store and trash, so deletion can be exercised end to end: the point
	// of the trash is what happens to files, and a fake would test nothing.
	sessionsDir := t.TempDir()
	historyStore, err := sessionlog.New(sessionsDir, logx.Discard())
	if err != nil {
		t.Fatalf("sessionlog.New: %v", err)
	}
	t.Cleanup(historyStore.Close)
	trashStore, err := sessionlog.OpenTrash(historyStore, filepath.Join(dir, "trash"))
	if err != nil {
		t.Fatalf("sessionlog.OpenTrash: %v", err)
	}

	resolver, err := httpcore.NewClientIPResolver([]string{"127.0.0.1/32", "::1/128"})
	if err != nil {
		t.Fatalf("NewClientIPResolver: %v", err)
	}

	bus := events.New(events.Config{Replay: 64, Queue: 64})
	driver := &fakeHarness{
		state: harness.StateReady,
		sessions: []harness.SessionInfo{
			{ID: "session-test", Workspace: workspace},
		},
		config: []harness.ConfigOption{{
			ID: "model", Name: "Model", Current: "m1",
			Options: []harness.ConfigOptionValue{{ID: "m1", Name: "Model One"}},
		}},
	}

	// The scheduler is built before the lease, which consults it: "is a turn
	// running" has exactly one answer, and this is where it lives.
	scheduler := turns.New(turns.Options{
		Harness:    driver,
		Bus:        bus,
		Logger:     logger,
		Timeout:    time.Minute,
		QueueDepth: cfg.Session.PromptQueueDepth,
		Now:        now,
	})

	leases := lease.New(lease.Options{
		Harness: driver, IdleTimeout: time.Minute, Logger: logger, Now: now, Busy: scheduler.Busy,
	})
	broker := approvals.New(approvals.Options{Timeout: time.Minute, Bus: bus, Logger: logger, Now: now})
	t.Cleanup(broker.Close)

	// The gateway's own state, so a test can drive a deploy rather than only
	// describe one.
	life := lifecycle.New(bus, logger)

	srv, err := New(Deps{
		Config:    cfg,
		Logger:    logger,
		Bus:       bus,
		Lifecycle: life,
		Leases:    leases,
		Approvals: broker,
		Turns:     scheduler,
		Harness:   driver,
		// The store is real so deletion has files to move; the projector's own
		// behaviour is covered in its own package.
		Sessions:  historyStore,
		Trash:     trashStore,
		Curation:  curationStore,
		Auth:      auth,
		Pairing:   pairSvc,
		Audit:     auditLog,
		Limiter:   limiter,
		Resolver:  resolver,
		Version:   "test",
		StartedAt: now(),
		Ready:     func() bool { return true },
		// The real path, relative to the test's state directory: a catalog that
		// outlives the process is behavior worth exercising in every test that
		// attaches a session, not only in the one that names it.
		CatalogPath: filepath.Join(dir, "models.json"),
	})
	if err != nil {
		t.Fatalf("v1.New: %v", err)
	}

	// Enrol one device so authenticated routes have something to accept.
	_, token, err := auth.Enroll(context.Background(), "test device", "test-agent")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	return &testServer{
		Server:   srv,
		handler:  srv.Handler(),
		bus:      bus,
		life:     life,
		auth:     auth,
		pair:     pairSvc,
		token:    token,
		logger:   logger,
		driver:   driver,
		stateDir: dir,
	}
}

// do issues a request against the route table, with optional credentials.
func (ts *testServer) do(method, path string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)
	return rec
}

func withCookie(token string) func(*http.Request) {
	return func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "dsh_gw_session", Value: token})
	}
}

// withOrigin sets only the Origin header.
//
// Host is deliberately left alone: the server compares Origin's host against the
// request's Host, which is the browser's own rule, and httptest.NewRequest
// already sets Host to "example.com". So Origin == sameOrigin describes a
// same-origin request, and any other value describes a cross-site one — which is
// precisely the distinction under test. An earlier version of this helper copied
// Origin into Host, which made every "cross-origin" request look same-origin and
// quietly turned the test into a no-op.
func withOrigin(origin string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Origin", origin) }
}

// sameOrigin matches the Host httptest.NewRequest sets by default.
const sameOrigin = "https://example.com"

// withJSON sends a JSON body from the same origin, which is what every mutating
// route requires: the server compares Origin's host against the request's Host
// (httptest sets that to example.com), and the body has to be a real reader
// because DecodeJSON also enforces the length limit.
func withJSON(body string) func(*http.Request) {
	return func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", sameOrigin)
	}
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// concretePath turns a route pattern into a requestable path.
func concretePath(pattern string) string {
	return pathParam.ReplaceAllString(pattern, "testid")
}

// TestNoRouteIsReachableWithoutACredential walks the real route table.
//
// This is the test that matters most in this package. Every endpoint here can
// either drive an agent that runs shell commands or read the operator's session
// history, so an endpoint that forgets its credential check is a full compromise
// — and the omission is invisible in review, because the handler itself looks
// exactly like every other handler.
//
// Iterating the table rather than a hand-written list means a newly added route
// is covered the moment it is declared, without anyone remembering to extend a
// test.
func TestNoRouteIsReachableWithoutACredential(t *testing.T) {
	ts := newTestServer(t)

	for _, rt := range ts.routes() {
		if rt.public {
			continue
		}
		t.Run(rt.method+" "+rt.pattern, func(t *testing.T) {
			rec := ts.do(rt.method, concretePath(rt.pattern))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("unauthenticated %s %s returned %d, want 401 — this endpoint "+
					"can drive an agent that runs shell commands",
					rt.method, rt.pattern, rec.Code)
			}
		})
	}
}

// TestTheSetOfPublicRoutesIsExactlyWhatWasIntended pins the exceptions.
//
// A public route is a deliberate hole in the credential wall. Listing them here
// means widening that set is a decision someone has to make twice: once in the
// table and once here, with a reason written down.
func TestTheSetOfPublicRoutesIsExactlyWhatWasIntended(t *testing.T) {
	ts := newTestServer(t)

	expected := map[string]string{
		"GET /healthz":        "liveness",
		"GET /readyz":         "readiness",
		"GET /api/v1/healthz": "liveness alias",
		"GET /api/v1/readyz":  "readiness alias",
		"POST /api/v1/pair":   "enrols the first credential",
	}

	seen := map[string]bool{}
	for _, rt := range ts.routes() {
		key := rt.method + " " + rt.pattern
		if !rt.public {
			continue
		}
		seen[key] = true
		if _, ok := expected[key]; !ok {
			t.Errorf("%s is public but is not on the intended list; if that is deliberate, "+
				"add it here with the reason it is safe to expose", key)
		}
		if strings.TrimSpace(rt.reason) == "" {
			t.Errorf("%s is public but states no reason", key)
		}
	}
	for key := range expected {
		if !seen[key] {
			t.Errorf("%s was expected to be public but is not", key)
		}
	}
}

// TestTheEventStreamAuthenticatesOrRefuses covers the one route that cannot use
// the shared middleware.
//
// A browser WebSocket handshake cannot carry an Authorization header, so the
// event stream verifies the credential inside its own handler. That makes it the
// single place where the standard wrapper is bypassed, and therefore the one
// most worth asserting directly.
func TestTheEventStreamAuthenticatesOrRefuses(t *testing.T) {
	ts := newTestServer(t)

	rec := ts.do(http.MethodGet, "/api/v1/events")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated event stream returned %d, want 401", rec.Code)
	}
	var problem httpcore.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("response is not a problem document: %v", err)
	}
	if problem.Code != "authentication_required" {
		t.Errorf("problem code = %q, want authentication_required", problem.Code)
	}

	// A bad credential must be refused too, not merely a missing one.
	rec = ts.do(http.MethodGet, "/api/v1/events", withCookie("not-a-real-token"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("event stream with an unknown credential returned %d, want 401", rec.Code)
	}
}

// TestMutatingRoutesRejectAForeignOrigin covers the CSRF control.
//
// The session cookie is SameSite=Strict, but that is a browser behaviour and not
// a server-side guarantee. The server check is what actually holds if a browser
// is older, or if the request is made by something that is not a browser.
func TestMutatingRoutesRejectAForeignOrigin(t *testing.T) {
	ts := newTestServer(t)

	mutating := 0
	for _, rt := range ts.routes() {
		switch rt.method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			continue
		}
		if rt.pattern == "/api/v1/pair" {
			continue // pairing has no credential to protect yet
		}
		mutating++

		t.Run(rt.method+" "+rt.pattern, func(t *testing.T) {
			// A valid credential, but the request claims to come from elsewhere.
			rec := ts.do(rt.method, concretePath(rt.pattern),
				withCookie(ts.token), withOrigin("https://evil.example"))
			if rec.Code != http.StatusForbidden {
				t.Errorf("cross-origin %s %s returned %d, want 403", rt.method, rt.pattern, rec.Code)
			}

			// The same request from our own origin must get past admission. It
			// may still fail on its merits — a bogus session id is a 404 or 409,
			// not a 403 — so the assertion is only that it is not a rejection of
			// the *origin*.
			rec = ts.do(rt.method, concretePath(rt.pattern),
				withCookie(ts.token), withOrigin(sameOrigin))
			if rec.Code == http.StatusForbidden {
				t.Errorf("same-origin %s %s was refused as cross-origin", rt.method, rt.pattern)
			}
			if rec.Code == http.StatusUnauthorized {
				t.Errorf("same-origin %s %s did not accept a valid credential", rt.method, rt.pattern)
			}
		})
	}

	if mutating == 0 {
		t.Fatal("no mutating routes found; the origin check would never be exercised")
	}
}

// TestOriginIsIgnoredOnSafeMethods records that reads are not origin-gated.
//
// A GET carries no state change, and requiring an Origin on reads would break
// clients that legitimately omit it — a native app, or a plain curl health
// check. Pinning it here keeps a future "harden everything" change from silently
// breaking those.
func TestOriginIsIgnoredOnSafeMethods(t *testing.T) {
	ts := newTestServer(t)

	for _, rt := range ts.routes() {
		if rt.method != http.MethodGet {
			continue
		}
		rec := ts.do(rt.method, concretePath(rt.pattern), withCookie(ts.token))
		if rec.Code == http.StatusForbidden {
			t.Errorf("GET %s was origin-gated; reads must not require an Origin", rt.pattern)
		}
	}
}

// TestCredentialFailuresSayOnlyWhatTheClientNeeds covers the error surface.
//
// Two classes of failure are deliberately reported differently, and the
// distinction is worth stating because getting it wrong in either direction is a
// bug:
//
//   - No credential at all is "authentication_required". A client that has never
//     paired learns it needs to pair, which is the difference between a working
//     onboarding flow and a mysterious 401. It reveals nothing: the caller
//     already knows whether it sent a credential.
//   - A credential that was presented and rejected is always
//     "unknown_credential" — the same answer for a token that never existed, one
//     that expired, and one that was revoked. Those must not be distinguishable,
//     or the endpoint becomes an oracle for which devices exist and which of
//     them the operator has cut off.
func TestCredentialFailuresSayOnlyWhatTheClientNeeds(t *testing.T) {
	ts := newTestServer(t)

	problemCode := func(t *testing.T, rec *httptest.ResponseRecorder) string {
		t.Helper()
		var problem httpcore.Problem
		if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
			t.Fatalf("response is not a problem document: %v", err)
		}
		return problem.Code
	}

	t.Run("no credential asks the client to pair", func(t *testing.T) {
		rec := ts.do(http.MethodGet, "/api/v1/me")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if code := problemCode(t, rec); code != "authentication_required" {
			t.Errorf("code = %q, want authentication_required", code)
		}
	})

	t.Run("every rejected credential is reported identically", func(t *testing.T) {
		cases := map[string]string{
			"a well-formed but unknown token": "0000000000000000000000000000000000000000000000000000000000000000",
			"a short token":                   "abc",
			"a non-hex token":                 "zzzz",
		}

		for name, token := range cases {
			rec := ts.do(http.MethodGet, "/api/v1/me", withCookie(token))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s returned %d, want 401", name, rec.Code)
				continue
			}
			if code := problemCode(t, rec); code != "unknown_credential" {
				t.Errorf("%s produced code %q, want unknown_credential; a rejected "+
					"credential must not reveal whether the device exists, expired, or "+
					"was revoked", name, code)
			}
		}
	})

	t.Run("an empty cookie value counts as no credential", func(t *testing.T) {
		// A cookie jar that cleared a value rather than removing the cookie is a
		// client bug; reporting it as "pair" is more useful than "unknown".
		rec := ts.do(http.MethodGet, "/api/v1/me", withCookie(""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if code := problemCode(t, rec); code != "authentication_required" {
			t.Errorf("code = %q, want authentication_required", code)
		}
	})
}

// TestPairingEnrolsADevice covers the one public route that changes state.
func TestPairingEnrolsADevice(t *testing.T) {
	ts := newTestServer(t)

	code, _ := ts.pair.Current()

	body := strings.NewReader(`{"code":"` + code + `","deviceName":"iPhone"}`)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/pair", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("pair returned %d, want 201: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Device struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"device"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("pair response is not JSON: %v", err)
	}
	if resp.Token == "" {
		t.Error("pair returned no token; a native app has nothing to store")
	}
	if resp.Device.Name != "iPhone" {
		t.Errorf("device name = %q, want iPhone", resp.Device.Name)
	}

	// The response must also set the cookie, so a browser needs no extra step.
	if len(rec.Result().Cookies()) == 0 {
		t.Error("pair set no cookie; a browser would have to read the token out of the body")
	}

	// The freshly minted token must actually work.
	me := ts.do(http.MethodGet, "/api/v1/me", withCookie(resp.Token))
	if me.Code != http.StatusOK {
		t.Errorf("the token pair just issued was rejected by /me: %d", me.Code)
	}
}

// TestPairingRejectsAWrongCode is the negative half of the same story, and the
// half that matters when someone is guessing.
func TestPairingRejectsAWrongCode(t *testing.T) {
	ts := newTestServer(t)

	body := strings.NewReader(`{"code":"WRONGCODE","deviceName":"attacker"}`)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/pair", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("pairing with a wrong code returned %d, want 401", rec.Code)
	}
	// No cookie on a failed attempt, or a failed guess would still hand out a
	// session.
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a failed pairing set a cookie")
	}
}

// TestProblemDocumentsAreWellFormed checks the error contract clients depend on.
func TestProblemDocumentsAreWellFormed(t *testing.T) {
	ts := newTestServer(t)

	rec := ts.do(http.MethodGet, "/api/v1/me")
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}

	var problem httpcore.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("not a problem document: %v", err)
	}
	if problem.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", problem.Status)
	}
	if !strings.HasPrefix(problem.Type, httpcore.ProblemNamespace) {
		t.Errorf("type = %q, want it to start with %q", problem.Type, httpcore.ProblemNamespace)
	}
	if problem.Instance == "" {
		t.Error("problem has no instance; a user-reported failure could not be traced to a log line")
	}
	// A 401 must advertise how to authenticate.
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Error("401 response does not advertise the authentication scheme")
	}
}

// TestCredentialCanBePresentedAsABearerToken covers the native-app path.
//
// The pairing response returns the token in its body precisely so a future app
// can store it in a keychain instead of relying on a cookie jar. If the bearer
// path were untested, that promise would rot.
func TestCredentialCanBePresentedAsABearerToken(t *testing.T) {
	ts := newTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+ts.token)
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("bearer authentication returned %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// A malformed Authorization header must fail rather than silently falling
	// back to the cookie, which would mask a client bug.
	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	req.AddCookie(&http.Cookie{Name: "dsh_gw_session", Value: ts.token})
	rec = httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a malformed Authorization header fell back to the cookie (%d); "+
			"that would hide a client bug", rec.Code)
	}
}

// TestRevokingTheCallingDeviceLocksItOutImmediately covers the panic button.
func TestRevokingTheCallingDeviceLocksItOutImmediately(t *testing.T) {
	ts := newTestServer(t)

	me := ts.do(http.MethodGet, "/api/v1/me", withCookie(ts.token))
	if me.Code != http.StatusOK {
		t.Fatalf("baseline /me returned %d", me.Code)
	}

	// The field is `id`, not `deviceId`: /me returns the same flat shape as an
	// entry in /devices, so one client decoder handles both.
	var identity struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &identity); err != nil {
		t.Fatalf("cannot read device identity: %v", err)
	}
	if identity.ID == "" {
		t.Fatalf("/me returned no id; body was %s", me.Body.String())
	}

	if rec := ts.do(http.MethodDelete, "/api/v1/devices/"+identity.ID, withCookie(ts.token)); rec.Code != http.StatusNoContent {
		t.Fatalf("revoking the calling device returned %d, want 204: %s", rec.Code, rec.Body.String())
	}

	if rec := ts.do(http.MethodGet, "/api/v1/me", withCookie(ts.token)); rec.Code != http.StatusUnauthorized {
		t.Errorf("a revoked device was still accepted (%d); revocation must be immediate, "+
			"because it is the only way to cut off a device that has been lost", rec.Code)
	}
}

// TestReadinessReflectsTheHarness covers the probe a supervisor depends on.
func TestReadinessReflectsTheHarness(t *testing.T) {
	ts := newTestServer(t)

	rec := ts.do(http.MethodGet, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("ready /readyz returned %d, want 200", rec.Code)
	}

	// A harness that is not ready must be reported as such, or a load balancer
	// would send traffic to a gateway that cannot serve a single prompt.
	ts.deps.Ready = func() bool { return false }
	ts.deps.Harness.(*fakeHarness).setState(harness.StateRestarting)

	rec = ts.do(http.MethodGet, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unready /readyz returned %d, want 503", rec.Code)
	}

	// Liveness stays 200 regardless: the process is fine, its dependency is not.
	if rec := ts.do(http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("liveness returned %d while unhealthy, want 200", rec.Code)
	}
}

// TestUnknownWorkspaceIsRefusedAtTheEdge covers the allowlist.
//
// The ACP client hands the harness an absolute cwd, so accepting a client-chosen
// path here would point an agent with shell access at any directory on the
// machine.
func TestUnknownWorkspaceIsRefusedAtTheEdge(t *testing.T) {
	ts := newTestServer(t)

	body := strings.NewReader(`{"workspace":"/etc"}`)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/sessions", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", sameOrigin)
	req.Host = "example.com"
	req.AddCookie(&http.Cookie{Name: "dsh_gw_session", Value: ts.token})
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("creating a session in /etc returned %d, want 400: %s", rec.Code, rec.Body.String())
	}

	var problem httpcore.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("not a problem document: %v", err)
	}
	if problem.Code != "unknown_workspace" {
		t.Errorf("problem code = %q, want unknown_workspace", problem.Code)
	}
}

// TestNewSessionStartsWithTheGatewayDefaults pins what "Gateway default" means.
//
// The app's new-session sheet sends no model and no reasoning effort when the
// user leaves those pickers alone, and the profile default the harness would
// otherwise use is not necessarily what the operator wants (it once pointed at a
// provider with no credential). So the gateway fills the gap from its own
// configuration — and, just as importantly, only fills it: an explicit choice
// from the caller must survive untouched.
func TestNewSessionStartsWithTheGatewayDefaults(t *testing.T) {
	ts := newTestServer(t)
	driver := ts.deps.Harness.(*fakeHarness)

	const model = `["command-code","deepseek/deepseek-v4.1-flash"]`
	ts.deps.Config.Session.DefaultModel = model
	ts.deps.Config.Session.DefaultReasoningEffort = "max"

	workspace := ts.deps.Config.Workspaces[0]

	rec := ts.do(http.MethodPost, "/api/v1/sessions",
		withCookie(ts.token), withJSON(fmt.Sprintf(`{"workspace":%q}`, workspace)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /sessions returned %d, want 201: %s", rec.Code, rec.Body.String())
	}
	want := []setConfigCall{{option: "model", value: model}, {option: "reasoning_effort", value: "max"}}
	if !reflect.DeepEqual(driver.setConfigCalls(), want) {
		t.Errorf("harness was asked to set %+v, want %+v", driver.setConfigCalls(), want)
	}

	// An explicit caller choice is not overridden by the configured default.
	driver.clearSetCalls()
	rec = ts.do(http.MethodPost, "/api/v1/sessions",
		withCookie(ts.token),
		withJSON(fmt.Sprintf(`{"workspace":%q,"model":"m1","reasoningEffort":"off"}`, workspace)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /sessions returned %d, want 201: %s", rec.Code, rec.Body.String())
	}
	want = []setConfigCall{{option: "model", value: "m1"}, {option: "reasoning_effort", value: "off"}}
	if !reflect.DeepEqual(driver.setConfigCalls(), want) {
		t.Errorf("harness was asked to set %+v, want the caller's own choice %+v", driver.setConfigCalls(), want)
	}
}

// TestModelsReportsTheGatewayDefaults covers the other half of the contract: the
// picker can only preselect what will happen if the API tells it what that is.
func TestModelsReportsTheGatewayDefaults(t *testing.T) {
	ts := newTestServer(t)
	ts.deps.Config.Session.DefaultModel = "m1"
	ts.deps.Config.Session.DefaultReasoningEffort = "max"

	rec := ts.do(http.MethodGet, "/api/v1/models", withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /models returned %d, want 200", rec.Code)
	}
	var body struct {
		Defaults struct {
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoningEffort"`
		} `json:"defaults"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET /models is not JSON: %v", err)
	}
	if body.Defaults.Model != "m1" || body.Defaults.ReasoningEffort != "max" {
		t.Errorf("GET /models defaults = %+v, want {m1 max}", body.Defaults)
	}
}

// fakeFollower reports which sessions another DSH process is running.
type fakeFollower map[string]bool

func (f fakeFollower) Running(sessionID string) bool { return f[sessionID] }

// TestSessionsReportASessionAnotherProcessRuns covers the list's half of the
// follower. A session being written at the desk has no lease here, so before
// this the phone showed a busy session as idle — and the reader had no way to
// tell "nothing is happening" from "something is happening somewhere else".
func TestSessionsReportASessionAnotherProcessRuns(t *testing.T) {
	ts := newTestServer(t)
	ts.deps.Follower = fakeFollower{"session-test": true}

	rec := ts.do(http.MethodGet, "/api/v1/sessions", withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sessions returned %d, want 200", rec.Code)
	}
	var body struct {
		Sessions []struct {
			ID     string `json:"id"`
			Busy   bool   `json:"busy"`
			Leased bool   `json:"leased"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET /sessions is not JSON: %v", err)
	}
	if len(body.Sessions) == 0 {
		t.Fatal("the test harness lists one session; the response listed none")
	}
	for _, session := range body.Sessions {
		if session.ID != "session-test" {
			t.Errorf("unexpected session %q in the list", session.ID)
			continue
		}
		if !session.Busy {
			t.Error("a session another process is running was reported as idle")
		}
		if session.Leased {
			t.Error("a session this gateway does not hold was reported as leased")
		}
	}

	// And the inverse: a session nobody is running stays idle.
	ts.deps.Follower = fakeFollower{}
	rec = ts.do(http.MethodGet, "/api/v1/sessions", withCookie(ts.token))
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET /sessions is not JSON: %v", err)
	}
	for _, session := range body.Sessions {
		if session.Busy {
			t.Errorf("session %q reported busy with no lease and no follower", session.ID)
		}
	}
}

// TestDecisionOnAnUnknownApprovalIsAConflict records the fail-closed edge.
func TestDecisionOnAnUnknownApprovalIsAConflict(t *testing.T) {
	ts := newTestServer(t)

	body := strings.NewReader(`{"optionId":"allow-once"}`)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/approvals/apr_does-not-exist", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", sameOrigin)
	req.Host = "example.com"
	req.AddCookie(&http.Cookie{Name: "dsh_gw_session", Value: ts.token})
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("deciding an unknown approval returned %d, want 409", rec.Code)
	}
}

// The fake must keep satisfying the interface as it evolves; a compile error
// here is a clearer signal than a runtime failure inside a handler.
var _ harness.Harness = (*fakeHarness)(nil)

// TestMeHasTheSameShapeAsADeviceListEntry pins the consistency that a real
// browser test proved was missing.
//
// An earlier revision returned deviceId/deviceName/pairedAt from /me while
// /devices used id/name/createdAt. The mobile app decoded both with one
// function, so pairing succeeded and the very next request failed with "the
// server returned an unexpected principal" — a confusing symptom a long way from
// its cause. One entity gets one shape, and this test says so.
func TestMeHasTheSameShapeAsADeviceListEntry(t *testing.T) {
	ts := newTestServer(t)

	me := ts.do(http.MethodGet, "/api/v1/me", withCookie(ts.token))
	if me.Code != http.StatusOK {
		t.Fatalf("/me returned %d", me.Code)
	}
	var principal map[string]any
	if err := json.Unmarshal(me.Body.Bytes(), &principal); err != nil {
		t.Fatalf("/me is not JSON: %v", err)
	}

	list := ts.do(http.MethodGet, "/api/v1/devices", withCookie(ts.token))
	var devices struct {
		Devices []map[string]any `json:"devices"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &devices); err != nil {
		t.Fatalf("/devices is not JSON: %v", err)
	}
	if len(devices.Devices) != 1 {
		t.Fatalf("expected exactly one enrolled device, got %d", len(devices.Devices))
	}
	entry := devices.Devices[0]

	// Every field a device-list entry carries must also appear on /me, with the
	// same name. Additions to /me are fine — limits and features live there — but
	// a rename on one side and not the other is what broke the app.
	for key := range entry {
		if _, ok := principal[key]; !ok {
			t.Errorf("/devices entries carry %q but /me does not; a client decoding "+
				"both with one function would fail on whichever it reached second", key)
		}
	}

	// The old, inconsistent names must be gone rather than kept alongside, or the
	// next client author has two plausible choices and will pick wrong.
	for _, stale := range []string{"deviceId", "deviceName", "pairedAt"} {
		if _, ok := principal[stale]; ok {
			t.Errorf("/me still exposes %q; the device shape uses id/name/createdAt", stale)
		}
	}
}

// TestSessionListIsCachedButNeverStaleForTheOperator pins both halves of the
// listing cache: a refresh is cheap, and a session the operator just created is
// in the very next list.
func TestSessionListIsCachedButNeverStaleForTheOperator(t *testing.T) {
	ts := newTestServer(t)

	// Two reads in a row: the second must not reach the harness. The fake counts
	// its calls, so this is observation rather than timing.
	driver := ts.deps.Harness.(*fakeHarness)
	driver.resetListCalls()
	for i := 0; i < 5; i++ {
		if rec := ts.do(http.MethodGet, "/api/v1/sessions", withCookie(ts.token)); rec.Code != http.StatusOK {
			t.Fatalf("GET /sessions returned %d", rec.Code)
		}
	}
	if driver.listCallCount() != 1 {
		t.Errorf("five list requests asked the harness %d time(s), want 1", driver.listCallCount())
	}

	// A page with no filter is cached once, and the cache is dropped the moment a
	// session is created through this API.
	rec := ts.do(http.MethodPost, "/api/v1/sessions",
		withCookie(ts.token), withJSON(fmt.Sprintf(`{"workspace":%q}`, ts.deps.Config.Workspaces[0])))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /sessions returned %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if rec := ts.do(http.MethodGet, "/api/v1/sessions", withCookie(ts.token)); rec.Code != http.StatusOK {
		t.Fatalf("GET /sessions returned %d", rec.Code)
	}
	if driver.listCallCount() != 2 {
		t.Errorf("the list was served from a cache the create should have dropped (harness calls: %d)", driver.listCallCount())
	}
}

// TestSessionListCacheExpires keeps a listing from living forever if nothing
// invalidates it: a session started on the desktop has no other way in.
func TestSessionListCacheExpires(t *testing.T) {
	ts := newTestServer(t)
	driver := ts.deps.Harness.(*fakeHarness)
	driver.resetListCalls()

	now := time.Now()
	ts.now = func() time.Time { return now }

	if rec := ts.do(http.MethodGet, "/api/v1/sessions", withCookie(ts.token)); rec.Code != http.StatusOK {
		t.Fatalf("GET /sessions returned %d", rec.Code)
	}
	now = now.Add(sessionListTTL + time.Second)
	if rec := ts.do(http.MethodGet, "/api/v1/sessions", withCookie(ts.token)); rec.Code != http.StatusOK {
		t.Fatalf("GET /sessions returned %d", rec.Code)
	}
	if driver.listCallCount() != 2 {
		t.Errorf("harness calls = %d, want 2: the listing must expire", driver.listCallCount())
	}
}

// sessionIDsFrom lists the ids in a session list response.
func sessionIDsFrom(t *testing.T, body []byte) []string {
	t.Helper()
	var page struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("session list is not JSON: %v", err)
	}
	ids := make([]string, 0, len(page.Sessions))
	for _, session := range page.Sessions {
		ids = append(ids, session.ID)
	}
	return ids
}

// TestArchivingHidesASessionAndUndoBringsItBack is the product promise of the
// tidy-up flow, at the API: a session can leave the list without being destroyed,
// and coming back is one request.
func TestArchivingHidesASessionAndUndoBringsItBack(t *testing.T) {
	ts := newTestServer(t)
	driver := ts.deps.Harness.(*fakeHarness)
	driver.sessions = []harness.SessionInfo{
		{ID: "session-keep", Workspace: ts.deps.Config.Workspaces[0]},
		{ID: "session-noise", Workspace: ts.deps.Config.Workspaces[0]},
	}

	list := func(query string) []string {
		rec := ts.do(http.MethodGet, "/api/v1/sessions"+query, withCookie(ts.token))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /sessions%s returned %d", query, rec.Code)
		}
		return sessionIDsFrom(t, rec.Body.Bytes())
	}

	if got := list(""); len(got) != 2 {
		t.Fatalf("before archiving the list holds %v, want both sessions", got)
	}

	rec := ts.do(http.MethodPatch, "/api/v1/sessions/session-noise",
		withCookie(ts.token), withJSON(`{"archived":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("archiving returned %d, want 200: %s", rec.Code, rec.Body.String())
	}

	active := list("")
	if len(active) != 1 || active[0] != "session-keep" {
		t.Errorf("after archiving the default list holds %v, want only session-keep", active)
	}
	archived := list("?state=archived")
	if len(archived) != 1 || archived[0] != "session-noise" {
		t.Errorf("?state=archived holds %v, want session-noise", archived)
	}
	if all := list("?state=all"); len(all) != 2 {
		t.Errorf("?state=all holds %v, want both", all)
	}

	// Undo, which is what the toast in the UI sends.
	rec = ts.do(http.MethodPatch, "/api/v1/sessions/session-noise",
		withCookie(ts.token), withJSON(`{"archived":false}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("undo returned %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := list(""); len(got) != 2 {
		t.Errorf("after the undo the list holds %v, want both sessions back", got)
	}
}

// TestCurateTakesASetInOneRequest covers the gesture the tidy-up sheet makes.
func TestCurateTakesASetInOneRequest(t *testing.T) {
	ts := newTestServer(t)
	driver := ts.deps.Harness.(*fakeHarness)
	driver.sessions = []harness.SessionInfo{
		{ID: "session-a", Workspace: ts.deps.Config.Workspaces[0]},
		{ID: "session-b", Workspace: ts.deps.Config.Workspaces[0]},
		{ID: "session-c", Workspace: ts.deps.Config.Workspaces[0]},
	}

	rec := ts.do(http.MethodPost, "/api/v1/sessions/curate",
		withCookie(ts.token), withJSON(`{"ids":["session-a","session-b"],"archived":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("curate returned %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Sessions []struct {
			ID       string `json:"id"`
			Archived bool   `json:"archived"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("curate response is not JSON: %v", err)
	}
	if len(body.Sessions) != 2 {
		t.Fatalf("curate answered for %d sessions, want 2", len(body.Sessions))
	}
	for _, session := range body.Sessions {
		if !session.Archived {
			t.Errorf("%s came back unarchived", session.ID)
		}
	}

	rec = ts.do(http.MethodGet, "/api/v1/sessions", withCookie(ts.token))
	if got := sessionIDsFrom(t, rec.Body.Bytes()); len(got) != 1 || got[0] != "session-c" {
		t.Errorf("after a bulk archive the list holds %v, want only session-c", got)
	}

	// One decision per request: asking for both is a client bug, not a guess.
	rec = ts.do(http.MethodPost, "/api/v1/sessions/curate",
		withCookie(ts.token), withJSON(`{"ids":["session-c"],"archived":true,"pinned":true}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("archiving and pinning at once returned %d, want 400", rec.Code)
	}
}

// TestSearchFindsSessionsByTitleAndWorkspace: the list is 478 sessions deep, and
// the reader remembers a word, not a date.
func TestSearchFindsSessionsByTitleAndWorkspace(t *testing.T) {
	ts := newTestServer(t)
	driver := ts.deps.Harness.(*fakeHarness)
	driver.sessions = []harness.SessionInfo{
		{ID: "session-one", Workspace: "/Users/me/code/api"},
		{ID: "session-two", Workspace: "/Users/me/code/web"},
	}
	ts.deps.Config.Workspaces = []string{"/Users/me/code/api", "/Users/me/code/web"}
	ts.workspaces = nil
	for _, path := range ts.deps.Config.Workspaces {
		ts.workspaces = append(ts.workspaces, describeWorkspace(path))
	}

	rec := ts.do(http.MethodGet, "/api/v1/sessions?q=code%2Fweb", withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("search returned %d", rec.Code)
	}
	if got := sessionIDsFrom(t, rec.Body.Bytes()); len(got) != 1 || got[0] != "session-two" {
		t.Errorf("search matched %v, want only session-two", got)
	}

	rec = ts.do(http.MethodGet, "/api/v1/sessions?q=nothing-matches-this", withCookie(ts.token))
	if got := sessionIDsFrom(t, rec.Body.Bytes()); len(got) != 0 {
		t.Errorf("a term nothing matches returned %v", got)
	}
}

// TestTriageEndpointReportsWhatItWouldArchive: the rules themselves are unit
// tested in the curation package; this is the wiring, and that a session in a
// scratch directory is recognised even when no log metadata is available.
func TestTriageEndpointReportsWhatItWouldArchive(t *testing.T) {
	ts := newTestServer(t)
	driver := ts.deps.Harness.(*fakeHarness)
	// The real session gets a workspace that is not a scratch directory: the test
	// server's own workspaces live under the OS temp root, which the triage
	// correctly treats as scratch.
	driver.sessions = []harness.SessionInfo{
		{ID: "session-real", Workspace: "/Users/me/code/api"},
		{ID: "session-tmp", Workspace: "/var/folders/ty/xyz/T/TestSomething123/001"},
	}

	rec := ts.do(http.MethodGet, "/api/v1/sessions/triage", withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("triage returned %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Summary struct {
			Scanned int `json:"scanned"`
			Temp    int `json:"temp"`
		} `json:"summary"`
		Candidates []struct {
			ID      string `json:"id"`
			Verdict string `json:"verdict"`
			Reason  string `json:"reason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("triage response is not JSON: %v", err)
	}
	if body.Summary.Scanned != 2 {
		t.Errorf("scanned = %d, want 2", body.Summary.Scanned)
	}
	if body.Summary.Temp != 1 {
		t.Errorf("summary = %+v, want exactly one temporary-directory candidate", body.Summary)
	}
	if len(body.Candidates) != 1 || body.Candidates[0].ID != "session-tmp" {
		t.Fatalf("candidates = %+v, want session-tmp", body.Candidates)
	}
	if body.Candidates[0].Reason == "" {
		t.Error("a candidate without a reason cannot be explained to the operator")
	}
}

// TestApprovalAuditFingerprintsTheArguments is the guard on a deliberate
// privacy decision: the audit log records *that* a tool call was authorised and
// a digest of what was authorised, never the arguments themselves.
//
// This matters because the arguments are the most sensitive thing that passes
// through the process — the body of a Write, the text of an Edit, the full
// command line of a Bash — and the log is rotated rather than deleted, and is
// not cleared when the session it refers to is. docs/security.md states this in
// as many words, and a previous revision stated the opposite while writing the
// raw arguments out, so the claim needs a test behind it.
func TestApprovalAuditFingerprintsTheArguments(t *testing.T) {
	ts := newTestServer(t)

	const (
		approvalID = "apr_fingerprint"
		toolInput  = `{"command":"echo hunter2-please-do-not-log-me"}`
	)
	// A secret that must not appear anywhere in the file, so the assertion can
	// be a plain "not present" rather than a field-by-field check that a future
	// edit could route around.
	const secret = "hunter2-please-do-not-log-me"

	done := make(chan struct{})
	go func() {
		defer close(done)
		// RequestPermission blocks until a human answers, which the request
		// below does. The decision is what is under test, not the return value.
		_, _ = ts.deps.Approvals.RequestPermission(context.Background(), harness.PermissionRequest{
			ID:          approvalID,
			SessionID:   "session-audit",
			ToolCallID:  "call-1",
			Tool:        "bash",
			Input:       toolInput,
			Options:     []harness.PermissionOption{{ID: "allow-once", Name: "Allow", Kind: "allow"}},
			RequestedAt: time.Now(),
			ExpiresAt:   time.Now().Add(time.Minute),
		})
	}()

	// Wait for the broker to have published it, or the decision races the
	// request and lands as a 409.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := ts.deps.Approvals.Get(approvalID); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the approval was never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}

	body := strings.NewReader(`{"optionId":"allow-once"}`)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v1/approvals/"+approvalID, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", sameOrigin)
	req.Host = "example.com"
	req.AddCookie(&http.Cookie{Name: "dsh_gw_session", Value: ts.token})
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("deciding the approval returned %d: %s", rec.Code, rec.Body.String())
	}
	<-done

	raw, err := os.ReadFile(filepath.Join(ts.stateDir, "audit.jsonl"))
	if err != nil {
		t.Fatalf("read the audit log: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Error("the audit log contains the tool's arguments verbatim; it must record a digest instead")
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
		if candidate.Event == string(audit.EventApprovalDecided) {
			record, found = candidate, true
		}
	}
	if !found {
		t.Fatal("the decision was not audited at all")
	}
	if got, _ := record.Fields["tool"].(string); got != "bash" {
		t.Errorf("audited tool = %q, want bash: the log has to name what was authorised", got)
	}
	sum, _ := record.Fields["inputSha256"].(string)
	if sum == "" {
		t.Fatal("no inputSha256: a digest is what ties the decision to the call it authorised")
	}
	if want := fmt.Sprintf("%x", sha256.Sum256([]byte(toolInput))); sum != want {
		t.Errorf("inputSha256 = %q, want %q", sum, want)
	}
	if got, ok := record.Fields["inputBytes"].(float64); !ok || int(got) != len(toolInput) {
		t.Errorf("inputBytes = %v, want %d", record.Fields["inputBytes"], len(toolInput))
	}
}
