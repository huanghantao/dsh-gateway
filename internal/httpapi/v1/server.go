// Package v1 implements the gateway's versioned HTTP and WebSocket API.
//
// This is the driving adapter: it translates the wire contract in docs/api.md
// into calls on the application services. It owns no domain logic. Anything that
// decides what should happen lives in internal/app or internal/harness; anything
// that decides how a failure is rendered lives in internal/httpcore.
package v1

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/approvals"
	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/app/lease"
	"github.com/huanghantao/dsh-gateway/internal/app/lifecycle"
	"github.com/huanghantao/dsh-gateway/internal/app/turns"
	"github.com/huanghantao/dsh-gateway/internal/audit"
	"github.com/huanghantao/dsh-gateway/internal/authn"
	"github.com/huanghantao/dsh-gateway/internal/authn/devicetoken"
	"github.com/huanghantao/dsh-gateway/internal/authn/ratelimit"
	"github.com/huanghantao/dsh-gateway/internal/config"
	"github.com/huanghantao/dsh-gateway/internal/curation"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/pairing"
	"github.com/huanghantao/dsh-gateway/internal/push"
	"github.com/huanghantao/dsh-gateway/internal/sessionlog"
	"github.com/huanghantao/dsh-gateway/internal/workspace"
)

// Follower reports which sessions another DSH process is running right now.
//
// The gateway's own leases only describe the sessions it drives. A session
// being written at the desk has no lease, so without this the phone would show
// it as idle however busy it is.
type Follower interface {
	Running(sessionID string) bool
}

// Deps is everything the API needs. It is a struct rather than a long argument
// list so that adding a service is a one-line change at the composition root.
type Deps struct {
	Config    config.Config
	Logger    *logx.Logger
	Bus       *events.Bus
	Leases    *lease.Manager
	Approvals *approvals.Broker
	// Turns admits and runs prompts. It is the single answer to "is a turn
	// running", which is why the lease consults it rather than keeping a flag.
	Turns    *turns.Scheduler
	Harness  harness.Harness
	Sessions *sessionlog.Store
	// Trash holds deleted sessions. Nil means deletion is not offered at all.
	Trash *sessionlog.Trash
	// Curation is the operator's archive and pin lists, from this phone and from
	// the desktop. Nil means nothing is ever archived.
	Curation *curation.Store
	// CurationRules decides what triage calls a test rather than a conversation.
	// The zero value means the portable defaults, which is what a deployment that
	// configures nothing should get.
	CurationRules curation.Rules
	// Push delivers notifications to registered browsers. Nil means this
	// gateway never notifies anyone.
	Push *push.Service
	// Webhooks are chat channels that receive the same notifications. They are
	// how a phone that cannot reach Google's push service still gets told.
	Webhooks []push.Webhook
	// Workspace undoes recorded changes. Nil means this gateway is read-only
	// over the workspace, which is the default and the whole security posture:
	// an undo is the only thing here that writes, so its absence is a code path
	// that does not exist rather than one that is switched off.
	Workspace *workspace.Reverter
	// Follower may be nil, in which case only leased sessions are ever busy.
	Follower Follower
	Auth     *devicetoken.Authenticator
	Pairing  *pairing.Service
	Audit    *audit.Logger
	Limiter  *ratelimit.Limiter
	Resolver *httpcore.ClientIPResolver
	// Version is reported by /healthz for deployment diagnostics.
	Version string
	// StartedAt is when the process came up.
	StartedAt time.Time
	// Lifecycle is the gateway's own state. It is what refuses new work while
	// the process is draining, and it is never nil in production — a nil one
	// would mean a redeploy accepts prompts it cannot finish.
	Lifecycle *lifecycle.Lifecycle
	// Ready reports whether the harness has completed its handshake.
	Ready func() bool
	// CatalogPath is where the model catalog observed from the harness is
	// remembered between runs. Empty keeps it in memory only, which is what a
	// test with no state directory wants.
	CatalogPath string
}

// Server holds the API's dependencies and its route table.
type Server struct {
	deps Deps

	// workspaces is the resolved allowlist. It is computed once at construction
	// so that a request never resolves a path from client input.
	workspaces []Workspace

	// cfgCache remembers the model catalog from the last attached session.
	cfgCache configCache

	// listCache remembers the harness's session listing for a moment, so a
	// refresh does not pay for a full enumeration again.
	listCache sessionListCache
	// now is injectable so a test can age the cache without sleeping.
	now func() time.Time
}

// New builds the API.
func New(deps Deps) (*Server, error) {
	s := &Server{deps: deps, now: time.Now}
	// A catalog remembered from an earlier run, so the model picker has
	// something to offer before any session has attached.
	if deps.CatalogPath != "" {
		s.cfgCache.open(deps.CatalogPath, deps.Logger)
	}
	for _, path := range deps.Config.Workspaces {
		s.workspaces = append(s.workspaces, describeWorkspace(path))
	}
	return s, nil
}

// route is one entry in the API's table.
//
// The table is data rather than a sequence of mux.Handle calls so that the
// authentication policy is inspectable. "Which endpoints can be reached without
// a device credential?" is then a question a test can ask of the real table,
// instead of one a reviewer has to answer by reading every line and noticing
// which ones are missing a wrapper.
type route struct {
	method  string
	pattern string
	handler http.Handler

	// public marks a route reachable with no device credential. Every public
	// route must state why it is safe, and the set is pinned by a test.
	public bool
	reason string
}

// routes is the API's complete route table.
//
// Authentication is applied per route rather than globally: a global wrapper
// would make a new endpoint authenticated by default, which sounds safer, but it
// also means the exceptions live in a second place and drift. Here, adding an
// endpoint without a credential check is a line that visibly says `public: true`.
func (s *Server) routes() []route {
	authed := func(h http.HandlerFunc) http.Handler {
		return s.rateLimited(s.requireAuth(h))
	}
	open := func(h http.HandlerFunc) http.Handler {
		return s.rateLimited(h)
	}

	return []route{
		// Liveness probes. They expose a status string and nothing an attacker
		// can act on, and they must stay reachable so a supervisor can tell a
		// dead process from a sick one.
		{method: "GET", pattern: "/healthz", handler: http.HandlerFunc(s.handleHealthz),
			public: true, reason: "liveness probe; exposes only a status string"},
		{method: "GET", pattern: "/readyz", handler: http.HandlerFunc(s.handleReadyz),
			public: true, reason: "readiness probe; exposes only a status string"},
		// Served under the API prefix too, because the contract named /api/v1 as
		// the base and then listed unprefixed paths. Accepting both removes a
		// guess the client should not have to make.
		{method: "GET", pattern: "/api/v1/healthz", handler: http.HandlerFunc(s.handleHealthz),
			public: true, reason: "liveness probe, API-prefixed alias"},
		{method: "GET", pattern: "/api/v1/readyz", handler: http.HandlerFunc(s.handleReadyz),
			public: true, reason: "readiness probe, API-prefixed alias"},

		// Pairing is unauthenticated by definition — it is how a device obtains
		// its first credential. It is protected instead by a short-lived derived
		// code, a rate limit, and an escalating lockout.
		{method: "POST", pattern: "/api/v1/pair", handler: open(s.handlePair),
			public: true, reason: "enrols the first credential; guarded by a derived one-time code, rate limit, and lockout"},

		{method: "GET", pattern: "/api/v1/me", handler: authed(s.handleMe)},

		{method: "GET", pattern: "/api/v1/devices", handler: authed(s.handleListDevices)},
		{method: "DELETE", pattern: "/api/v1/devices/{id}", handler: authed(s.handleRevokeDevice)},

		{method: "GET", pattern: "/api/v1/workspaces", handler: authed(s.handleWorkspaces)},
		{method: "GET", pattern: "/api/v1/models", handler: authed(s.handleModels)},

		// Notifications. The key is public by design: it is what a browser needs
		// in order to subscribe at all.
		{method: "GET", pattern: "/api/v1/push/key", handler: authed(s.handlePushKey)},
		{method: "POST", pattern: "/api/v1/push/subscribe", handler: authed(s.handlePushSubscribe)},
		{method: "POST", pattern: "/api/v1/push/unsubscribe", handler: authed(s.handlePushUnsubscribe)},
		{method: "POST", pattern: "/api/v1/push/test", handler: authed(s.handlePushTest)},

		{method: "GET", pattern: "/api/v1/sessions", handler: authed(s.handleListSessions)},
		{method: "POST", pattern: "/api/v1/sessions", handler: authed(s.handleCreateSession)},
		// Literal patterns win over the "{id}" wildcard in Go's mux, so these are
		// reached before a session with one of these names could shadow them.
		{method: "GET", pattern: "/api/v1/sessions/triage", handler: authed(s.handleTriage)},
		// Searching inside transcripts, not just titles: the list search answers
		// "which session", this answers "where in it".
		{method: "GET", pattern: "/api/v1/sessions/search", handler: authed(s.handleSearchSessions)},
		{method: "POST", pattern: "/api/v1/sessions/curate", handler: authed(s.handleCurateSessions)},
		{method: "GET", pattern: "/api/v1/trash", handler: authed(s.handleListTrash)},
		{method: "POST", pattern: "/api/v1/trash/restore", handler: authed(s.handleRestoreTrash)},
		{method: "GET", pattern: "/api/v1/sessions/{id}", handler: authed(s.handleGetSession)},
		{method: "GET", pattern: "/api/v1/sessions/{id}/receipt", handler: authed(s.handleSessionReceipt)},
		{method: "PATCH", pattern: "/api/v1/sessions/{id}", handler: authed(s.handlePatchSession)},
		// Deletion is a move into the gateway's own trash, restorable for a
		// month: the one button here that could destroy work does not.
		{method: "DELETE", pattern: "/api/v1/sessions/{id}", handler: authed(s.handleDeleteSession)},
		{method: "POST", pattern: "/api/v1/sessions/{id}/lease", handler: authed(s.handleAcquireLease)},
		{method: "DELETE", pattern: "/api/v1/sessions/{id}/lease", handler: authed(s.handleReleaseLease)},
		{method: "GET", pattern: "/api/v1/sessions/{id}/transcript", handler: authed(s.handleTranscript)},
		// What the session changed. Reading is always available; the undo beside
		// it is not, and answers 403 when the deployment is read-only.
		{method: "GET", pattern: "/api/v1/sessions/{id}/changes", handler: authed(s.handleChanges)},
		{method: "POST", pattern: "/api/v1/sessions/{id}/revert", handler: authed(s.handleRevert)},
		{method: "POST", pattern: "/api/v1/sessions/{id}/prompt", handler: authed(s.handlePrompt)},
		{method: "POST", pattern: "/api/v1/sessions/{id}/cancel", handler: authed(s.handleCancel)},
		// Dropping one queued follow-up is not stopping the turn, so it is its
		// own verb rather than a query parameter on cancel.
		{method: "DELETE", pattern: "/api/v1/sessions/{id}/queue/{turnId}", handler: authed(s.handleDropQueued)},

		{method: "GET", pattern: "/api/v1/approvals", handler: authed(s.handleListApprovals)},
		{method: "POST", pattern: "/api/v1/approvals/{id}", handler: authed(s.handleDecideApproval)},
		// Standing authorisations are a resource, not a detail of the sheet: the
		// only way to know one is in force is to be able to look.
		{method: "GET", pattern: "/api/v1/approvals/grants", handler: authed(s.handleListGrants)},
		{method: "DELETE", pattern: "/api/v1/approvals/grants/{id}", handler: authed(s.handleRevokeGrant)},

		// The event stream checks the same credential itself, inside the handler,
		// because a WebSocket handshake cannot carry a custom header from a
		// browser: the credential rides in the cookie and is verified before the
		// upgrade is accepted. It is therefore not routed through requireAuth,
		// but it is not public either — see handleEvents.
		{method: "GET", pattern: "/api/v1/events", handler: open(s.handleEvents),
			reason: "authenticates inside the handler, before the upgrade; a browser WebSocket cannot send an Authorization header"},
	}
}

// Register mounts every route on mux.
func (s *Server) Register(mux *http.ServeMux) {
	for _, rt := range s.routes() {
		mux.Handle(rt.method+" "+rt.pattern, rt.handler)
	}
}

// Handler returns the fully wrapped API handler, outermost layer first.
//
// The order is the security policy:
//
//	request id        every log line is traceable
//	client address    resolves the real peer before anything trusts it
//	recover           a panic in one request never kills the tunnel
//	access log        records the outcome of everything below
//	security headers  applied even to failures
//	body limit        bounds memory before a handler reads anything
//	timeout           bounds handler execution
//
// Authentication is deliberately per-route rather than global, so that adding an
// endpoint without a credential check is a visible omission in Register instead
// of a silent hole.
func (s *Server) Handler() http.Handler {
	chain := httpcore.Chain(
		httpcore.RequestID(),
		httpcore.ClientAddress(s.deps.Resolver),
		httpcore.Recover(s.deps.Logger),
		httpcore.AccessLog(s.deps.Logger),
		httpcore.SecurityHeaders(s.deps.Config.SecureCookies()),
		httpcore.BodyLimit(s.deps.Config.Limits.MaxBodyBytes),
		httpcore.Timeout(s.deps.Config.Limits.ReadTimeout.Std()),
	)

	mux := http.NewServeMux()
	s.Register(mux)
	return chain(mux)
}

// --- middleware -------------------------------------------------------------

type ctxKeyPrincipal struct{}

// principalFrom returns the authenticated device.
func principalFrom(ctx context.Context) (authn.Principal, bool) {
	p, ok := ctx.Value(ctxKeyPrincipal{}).(authn.Principal)
	return p, ok
}

// requireAuth rejects any request without a valid device credential.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := s.extractCredential(r)
		if !ok {
			httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
				errx.New(errx.KindUnauthenticated, "authentication_required",
					"this endpoint requires a paired device"))
			return
		}

		principal, err := s.deps.Auth.Authenticate(r.Context(), cred)
		if err != nil {
			// Record the failure against the source address so that a scanner
			// cannot probe credentials indefinitely.
			key := clientKey(r)
			s.deps.Limiter.Fail(key)
			s.deps.Audit.Record(r.Context(), audit.EventAuthFailed, "", map[string]any{
				"clientIp": key,
				"reason":   errx.CodeOf(err),
			})
			httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
			return
		}

		// A valid credential on a mutating request must also come from our own
		// origin, or a page on another site could drive the agent using the
		// operator's cookie.
		if isMutating(r.Method) && !s.originAllowed(r) {
			httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
				errx.New(errx.KindForbidden, "origin_rejected",
					"this request did not originate from the gateway"))
			return
		}

		ctx := context.WithValue(r.Context(), ctxKeyPrincipal{}, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// admit refuses a request that would start new work while the gateway is
// draining. It reports whether the caller should continue.
//
// This is the counterpart, at the door, of the drain itself. Without it a
// redeploy has a race it cannot win: the drain waits for the turns it knows
// about, and a prompt admitted a millisecond before the harness is closed is a
// turn nobody waited for — accepted with a 202, then destroyed. The refusal is
// 503 rather than a conflict because it is temporary by construction, and a
// client is meant to retry it.
func (s *Server) admit(w http.ResponseWriter, r *http.Request) bool {
	if s.deps.Lifecycle == nil || s.deps.Lifecycle.Serving() {
		return true
	}
	httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
		lifecycle.ErrDraining())
	return false
}

// rateLimited applies the per-client token bucket.
func (s *Server) rateLimited(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, retryAfter := s.deps.Limiter.Allow(clientKey(r))
		if !ok {
			seconds := int(retryAfter.Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", itoa(seconds))
			httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
				errx.New(errx.KindRateLimited, "rate_limited", "too many requests; slow down"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// extractCredential finds a device credential on the request.
//
// Browsers use the session cookie. Native apps use a bearer token, which is why
// the pairing response returns the token in its body as well as setting a cookie:
// a future app has no cookie jar to rely on.
func (s *Server) extractCredential(r *http.Request) (authn.Credential, bool) {
	if header := r.Header.Get("Authorization"); header != "" {
		scheme, value, found := strings.Cut(header, " ")
		if found && strings.EqualFold(scheme, "Bearer") && strings.TrimSpace(value) != "" {
			return authn.Credential{Kind: authn.KindBearer, Value: strings.TrimSpace(value)}, true
		}
		// A malformed Authorization header is a client bug, not an invitation to
		// fall back to the cookie: silently ignoring it would mask the bug.
		return authn.Credential{}, false
	}

	if cookie, err := r.Cookie(s.deps.Config.Auth.CookieName); err == nil && cookie.Value != "" {
		return authn.Credential{Kind: authn.KindCookie, Value: cookie.Value}, true
	}
	return authn.Credential{}, false
}

// originAllowed reports whether a mutating request came from the gateway's own
// origin.
//
// Absent Origin is accepted: native clients and same-origin navigations may omit
// it, and the credential is already required. When Origin *is* present it must
// match the Host the browser used, which is what stops a cross-site form post
// from carrying the operator's cookie into a state-changing call.
func (s *Server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	// A browser never sends more than one Origin; multiple values mean a
	// hand-crafted request.
	if strings.Contains(origin, ",") {
		return false
	}

	host := r.Host
	if host == "" {
		return false
	}
	// Compare scheme-insensitively on host, mirroring how the browser derives
	// Origin from the page it loaded.
	trimmed := origin
	if i := strings.Index(trimmed, "://"); i >= 0 {
		trimmed = trimmed[i+3:]
	}
	return strings.EqualFold(trimmed, host)
}

// isMutating reports whether a method changes state.
func isMutating(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// clientKey is the rate-limiting identity of a request. It is the resolved client
// address, so all devices behind one home NAT share a bucket, which is correct:
// the threat model is an attacker on the internet, not an attacker in the house.
func clientKey(r *http.Request) string {
	if ip := httpcore.ClientIPFrom(r.Context()); ip != nil {
		return ip.String()
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
