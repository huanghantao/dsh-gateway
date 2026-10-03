// Package agenthost, this file: the gateway's side of the seam.
//
// It implements harness.Harness — the same driven port the in-process ACP
// adapter implements — so that everything above it is unchanged. That is the
// whole return on having had a port in the first place: the HTTP API, the lease
// manager and the turn scheduler do not know whether the agent is a child
// process down a pipe or a socket across the room, and this file is where the
// difference is absorbed.
package agenthost

import (
	"context"

	"fmt"
	"math/rand"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/agenthost/hostwire"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// ClientOptions configures a Client.
type ClientOptions struct {
	// Dial opens a connection to the host. It is a function rather than an
	// address so that a test can hand over one end of a socketpair, and so that
	// the unix/TCP choice is a deployment decision rather than a code path.
	Dial func(ctx context.Context) (net.Conn, error)
	// InstanceID identifies this gateway process to the host, for logs.
	InstanceID string
	Logger     *logx.Logger

	// StateSink receives the child's lifecycle transitions, the same way the
	// in-process adapter reports them.
	StateSink harness.StateSink
	// Updates receives agent activity.
	Updates harness.UpdateSink
	// Permissions is asked for every tool that needs a person. It is the same
	// broker the in-process adapter was given, unchanged: the host relays, the
	// gateway decides.
	Permissions harness.PermissionHandler

	// ReconnectBackoff and MaxReconnectBackoff bound the redial delay. Jitter is
	// applied, so a host restart does not bring a fleet of reconnects down on it
	// in lockstep.
	ReconnectBackoff    time.Duration
	MaxReconnectBackoff time.Duration
	// CallTimeout bounds a request that is not a turn. A turn has no timeout of
	// its own here: it is bounded by the host.
	CallTimeout time.Duration

	Now func() time.Time
}

// Client is the gateway's handle on the agent host.
//
// It owns one connection at a time and re-establishes it after a failure, which
// is what turns "the host restarted" from a fatal error into a pause. It is safe
// for concurrent use.
type Client struct {
	opts   ClientOptions
	logger *logx.Logger
	now    func() time.Time

	mu     sync.Mutex
	conn   *hostwire.Conn
	state  harness.State
	detail string
	// epoch is this client's fencing token, minted once per process and never
	// reused. The host refuses a command carrying one older than the connection
	// it is on, which is what stops a stale gateway from acting after it has
	// been replaced.
	epoch uint64
	// hostEpoch names the host process, so a caller can tell a reconnect from a
	// host restart.
	hostEpoch string
	// caps is what the harness advertised at the host's own handshake.
	caps harness.Capabilities

	// awaiting correlates a turn this client submitted with its outcome. A
	// gateway that reconnects mid-turn registers here too, which is how it
	// rejoins work it did not start in this process.
	awaiting map[string]chan hostwire.TurnResult

	// held is every session the host is still holding a handle for, keyed by id.
	//
	// DSH's own session listing cannot report these: it skips every session that
	// is live in the process answering it, and that process is the host's child.
	// The handle outlives the lease that attached it — a release is bookkeeping,
	// because detaching is not something DSH offers — so without this set a
	// session would drop out of the phone's list the moment its lease expired
	// and stay out until the child restarted, even while its agent was working.
	//
	// Replaced wholesale from the host's snapshot on every (re)connect, which is
	// what keeps it true across a gateway restart, and maintained by the calls
	// that attach or drop a handle.
	held map[string]harness.SessionInfo

	stop     chan struct{}
	stopOnce sync.Once
}

// ErrNotConnected is returned when the gateway asks for work before the host has
// accepted a connection. It is retryable, and the HTTP layer renders it as 503.
var ErrNotConnected = errx.New(errx.KindUnavailable, "host_unavailable",
	"the agent host is not connected")

// NewClient builds a client. Start must be called before it is used.
func NewClient(opts ClientOptions) *Client {
	if opts.Logger == nil {
		opts.Logger = logx.Discard()
	}
	if opts.ReconnectBackoff <= 0 {
		opts.ReconnectBackoff = 500 * time.Millisecond
	}
	if opts.MaxReconnectBackoff <= 0 {
		opts.MaxReconnectBackoff = 30 * time.Second
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = 2 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Client{
		opts:     opts,
		logger:   opts.Logger,
		now:      opts.Now,
		state:    harness.StateStarting,
		epoch:    epochFromClock(opts.Now),
		awaiting: map[string]chan hostwire.TurnResult{},
		held:     map[string]harness.SessionInfo{},
		stop:     make(chan struct{}),
	}
}

// Start connects, and keeps reconnecting until Close.
//
// It returns once the first connection is established, so a gateway that cannot
// reach its host fails at startup rather than at the first prompt. A host that
// goes away later is a pause, not a failure: the supervisor redials and reports
// the state so a client can say "the agent host is restarting" instead of
// hanging.
func (c *Client) Start(ctx context.Context) error {
	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}
	// The first connection knows nothing, and that is the awkward case rather
	// than the empty one: the host is the process a redeploy does not replace, so
	// it may already be holding sessions and running turns this one never saw.
	// Rejoining here rather than only on a reconnect is what lets a gateway that
	// has just started list — and follow — the work that outlived the gateway
	// before it.
	c.rejoin(conn) //nolint:contextcheck // rejoin bounds its own snapshot call in the client's lifetime, not the caller's request
	// The supervisor outlives every request — it is what keeps the connection up
	// for as long as this client lives — so it is rooted in the client's own
	// lifetime rather than in the context that happened to start it.
	go c.supervise(conn) //nolint:contextcheck,gosec // see above
	return nil
}

// Close stops reconnecting and ends the connection.
func (c *Client) Close(context.Context) error {
	c.stopOnce.Do(func() { close(c.stop) })
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		return conn.Close()
	}
	return nil
}

// connect dials, says hello, and installs the notification handlers.
func (c *Client) connect(ctx context.Context) (*hostwire.Conn, error) {
	raw, err := c.opts.Dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent host: dial: %w", err)
	}
	conn := hostwire.NewConn(raw)
	// A handler on this end can fail — an approval broker that is closing, for
	// instance — and the refusal must reach the host with its meaning intact.
	conn.SetErrorClassifier(toWireError)

	callCtx, cancel := context.WithTimeout(ctx, c.opts.CallTimeout)
	defer cancel()

	// Handlers first, then the reader, then the handshake.
	//
	// The order is not cosmetic. The host adopts a connection as soon as it
	// answers the hello, and it may send a state notification or a relayed
	// permission request in the same breath. A client that registered its
	// handlers after the hello returned would answer those with "unknown
	// method" — a permission prompt would silently never reach the phone and the
	// tool would be refused on timeout. Installing everything before the first
	// byte is read makes that window not exist.
	conn.Handle(hostwire.MethodUpdate, func(r *hostwire.Request) error {
		var update hostwire.Update
		if err := r.Decode(&update); err != nil {
			return err
		}
		if c.opts.Updates != nil {
			c.opts.Updates.Publish(fromWireUpdate(update))
		}
		return nil
	})
	conn.Handle(hostwire.MethodState, func(r *hostwire.Request) error {
		var event hostwire.StateEvent
		if err := r.Decode(&event); err != nil {
			return err
		}
		c.setState(harness.State(event.State), event.Detail)
		return nil
	})
	conn.Handle(hostwire.MethodTurnResult, func(r *hostwire.Request) error {
		var result hostwire.TurnResult
		if err := r.Decode(&result); err != nil {
			return err
		}
		c.deliver(result)
		return nil
	})
	// A relayed approval arrives as a notification and is answered with a
	// command of its own. The two are decoupled on purpose: the answer must not
	// be tied to the connection the question arrived on, or a gateway that
	// reconnected while a tool waited could never answer it.
	conn.Handle(hostwire.MethodPermissionRequest, func(r *hostwire.Request) error { //nolint:contextcheck // the answer is bounded by the connection's own context, not by the request that carried the question
		var req hostwire.PermissionRequest
		if err := r.Decode(&req); err != nil {
			return err
		}
		// On its own goroutine: `decide` blocks until a person answers, and the
		// reader must not stop behind it. The connection's context bounds the
		// wait, so a host that goes away does not leave a decision outstanding.
		go func() {
			decision, err := c.decide(r.Context(), req)
			if err != nil {
				c.logger.Warn("an approval could not be decided",
					"approval", req.ID, "session", req.SessionID, "error", err.Error())
				// A decision that cannot be made must not leave the tool
				// waiting: silence refuses, which is the same rule the
				// in-process broker applies.
				decision = hostwire.PermissionDecisionParams{
					SessionID: req.SessionID,
					RequestID: req.ID,
					OptionID:  harness.OptionRejectOnce,
				}
			}
			c.sendDecision(r.Context(), decision)
		}()
		return nil
	})
	conn.Handle(hostwire.MethodDrainingNotification, func(r *hostwire.Request) error {
		var event hostwire.DrainingEvent
		if err := r.Decode(&event); err != nil {
			return err
		}
		c.logger.Warn("the agent host is draining",
			"reason", event.Reason, "turnsRunning", event.TurnsRunning)
		return nil
	})

	conn.Start()

	var hello hostwire.HelloResult
	if err := conn.Call(callCtx, hostwire.MethodHello, hostwire.Hello{
		Protocol:   hostwire.Protocol,
		Caller:     "gateway",
		InstanceID: c.opts.InstanceID,
		Epoch:      c.epoch,
		// This is the gateway: it exists to drive the agent, so it takes the
		// control role. A diagnostic tool says hello without this and reads.
		ClaimsControl: true,
	}, &hello); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("agent host: hello: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.hostEpoch = hello.HostEpoch
	c.caps = fromWireCapabilities(hello.Capabilities)
	c.mu.Unlock()

	c.logger.Info("connected to the agent host",
		"host_epoch", hello.HostEpoch, "pid", hello.HostPID,
		"agent", hello.Capabilities.AgentName)
	return conn, nil
}

// supervise keeps a live connection, redialling with jittered backoff.
func (c *Client) supervise(conn *hostwire.Conn) {
	backoff := c.opts.ReconnectBackoff

	for {
		select {
		case <-c.stop:
			return
		case <-conn.Done():
		}

		if c.stopping() {
			return
		}
		cause := conn.Err()
		c.logger.Error("lost the agent host", "error", errorText(cause), "backoff", backoff.String())
		// The harness state moves to restarting rather than failed: what the
		// gateway knows is that it cannot reach the host, not that the host is
		// broken. The distinction matters to the phone, which shows one as a
		// pause and the other as something to act on.
		c.setState(harness.StateRestarting, errorText(cause))

		select {
		case <-c.stop:
			return
		case <-time.After(jitter(backoff)):
		}

		ctx, cancel := context.WithTimeout(context.Background(), c.opts.CallTimeout)
		next, err := c.connect(ctx)
		cancel()
		if err != nil {
			c.logger.Warn("agent host reconnect failed", "error", err.Error())
			backoff = nextBackoff(backoff, c.opts.MaxReconnectBackoff)
			continue
		}
		// A new connection means the host may not be the one we were talking to,
		// so what we believed about the child's state is no longer evidence.
		c.rejoin(next)
		backoff = c.opts.ReconnectBackoff
		conn = next
	}
}

// rejoin re-establishes the gateway's view after a reconnect.
//
// It is what makes a gateway restart survivable end to end. A fresh connection
// knows nothing: turns may have settled while it was away, and turns may still
// be running that this process never submitted. Both are resolved from one
// snapshot.
func (c *Client) rejoin(conn *hostwire.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.CallTimeout)
	defer cancel()

	var snap hostwire.SnapshotResult
	if err := conn.Call(ctx, hostwire.MethodSnapshot, nil, &snap); err != nil {
		c.logger.Warn("could not read the host snapshot after reconnecting", "error", err.Error())
		return
	}
	if snap.HostEpoch != c.hostEpoch {
		// The host itself was replaced, so every turn this gateway was awaiting
		// is gone. Saying so is better than waiting for an answer that cannot
		// come.
		c.logger.Warn("the agent host restarted; turns in flight were interrupted",
			"was", c.hostEpoch, "now", snap.HostEpoch)
		c.mu.Lock()
		c.hostEpoch = snap.HostEpoch
		awaiting := c.awaiting
		c.awaiting = map[string]chan hostwire.TurnResult{}
		c.mu.Unlock()
		for _, ch := range awaiting {
			ch <- hostwire.TurnResult{Err: &hostwire.Error{
				Kind:    hostwire.KindUnavailable,
				Code:    "host_restarted",
				Message: "the agent host was restarted and the turn did not survive it",
			}}
		}
	}
	c.setState(harness.State(snap.State.State), snap.State.Detail)

	// What the host holds is its own answer to give, and it is the authority:
	// a reconnect may follow a gateway restart (the child kept its sessions) or
	// a host restart (it kept none, and DSH's listing can show them again).
	c.replaceHeld(snap.Sessions)

	for _, held := range snap.Sessions {
		if held.Turn == nil {
			continue
		}
		c.logger.Info("rejoined a turn that was already running",
			"session", held.Info.ID, "turn", held.Turn.TurnID)
		// A turn nobody has submitted in this process, running now. The host
		// owns its outcome, so the gateway only has to wait for it — which is
		// what makes the phone's "turn running" survive a redeploy.
		go c.awaitTurn(held.Turn.TurnID, held.Turn.SessionID)
	}
}

// awaitTurn observes a turn this process did not submit until it settles.
//
// The observation is what the gateway owes a client after a redeploy: the turn
// survived, and something has to notice when it ends rather than leaving the
// phone on "running" forever. The outcome is not routed to a caller — there is
// no caller, because the process that submitted it is gone — so it is logged and
// the client's own refetch reconciles the rest. Reattaching it to the turn
// *scheduler* is the natural next step and is deliberately not attempted here:
// the scheduler's state is per-process by construction, and inventing a second
// way for a turn to be admitted is how two definitions of "busy" appear.
func (c *Client) awaitTurn(turnID, sessionID string) {
	conn := c.current()
	if conn == nil {
		return
	}
	// No deadline: a turn is bounded by the host, and imposing a second bound
	// here would cancel work for the convenience of the observer.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var result hostwire.TurnResult
	err := conn.Call(ctx, hostwire.MethodTurnAwait,
		hostwire.TurnAwaitParams{TurnID: turnID}, &result)
	switch {
	case err != nil:
		c.logger.Warn("could not follow a turn that was already running",
			"turn", turnID, "session", sessionID, "error", err.Error())
	case result.Err != nil:
		c.logger.Warn("a turn that outlived its gateway failed",
			"turn", turnID, "session", sessionID, "error", result.Err.Message)
	default:
		c.logger.Info("a turn that outlived its gateway settled",
			"turn", turnID, "session", sessionID, "cancelled", result.Cancelled)
	}
}

/* ------------------------------------------------------------- harness port */

// Capabilities reports what the harness advertised.
func (c *Client) Capabilities() harness.Capabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps
}

// State reports the child's lifecycle state, as last reported by the host.
func (c *Client) State() harness.State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// HeldSessions reports the sessions the host is still holding a handle for.
//
// This is the set the harness's own listing cannot show — it skips every session
// live in the process answering it — so the API merges it back into the list it
// serves. Order is by id so two calls that hold the same sessions produce the
// same slice.
func (c *Client) HeldSessions() []harness.SessionInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]harness.SessionInfo, 0, len(c.held))
	for _, info := range c.held {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// rememberHeld records a session the host has just attached.
func (c *Client) rememberHeld(info harness.SessionInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held == nil {
		c.held = map[string]harness.SessionInfo{}
	}
	c.held[info.ID] = info
}

// forgetHeld drops a session the host no longer holds.
func (c *Client) forgetHeld(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.held, sessionID)
}

// replaceHeld installs the host's own account of what it holds.
func (c *Client) replaceHeld(held []hostwire.SessionHeld) {
	next := make(map[string]harness.SessionInfo, len(held))
	for _, s := range held {
		next[s.Info.ID] = fromWireSessionInfo(s.Info)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held = next
}

// ListSessions asks the host for a page of the sessions it can see.
func (c *Client) ListSessions(ctx context.Context, workspace, cursor string) (harness.SessionPage, error) {
	var out hostwire.SessionPage
	err := c.call(ctx, hostwire.MethodSessionList,
		hostwire.ListSessionsParams{Workspace: workspace, Cursor: cursor}, &out)
	if err != nil {
		return harness.SessionPage{}, err
	}
	page := harness.SessionPage{NextCursor: out.NextCursor}
	for _, info := range out.Sessions {
		page.Sessions = append(page.Sessions, fromWireSessionInfo(info))
	}
	return page, nil
}

// NewSession creates a session in the given workspace.
func (c *Client) NewSession(ctx context.Context, workspace string) (harness.Session, error) {
	var out hostwire.Session
	if err := c.call(ctx, hostwire.MethodSessionNew,
		hostwire.NewSessionParams{Workspace: workspace}, &out); err != nil {
		return harness.Session{}, err
	}
	sess := fromWireSession(out)
	c.rememberHeld(sess.Info)
	return sess, nil
}

// ResumeSession attaches an existing session, taking DSH's single-writer lock
// in the host. It fails when another process already holds that lock, which is
// the normal case while the desktop has the session open.
func (c *Client) ResumeSession(ctx context.Context, sessionID, workspace string) (harness.Session, error) {
	var out hostwire.Session
	if err := c.call(ctx, hostwire.MethodSessionResume,
		hostwire.ResumeSessionParams{SessionID: sessionID, Workspace: workspace}, &out); err != nil {
		return harness.Session{}, err
	}
	sess := fromWireSession(out)
	c.rememberHeld(sess.Info)
	return sess, nil
}

// ReleaseSession detaches a session, leaving it resumable.
//
// This is the call a lease expiring makes, and on this side of the seam it costs
// nothing durable: the host keeps its handle — and therefore DSH's writer lock —
// so a release is bookkeeping rather than the synthetic end-of-session that
// closing writes into the log.
//
// A release that fails leaves the handle — and therefore the session — held, so
// the set of held sessions only shrinks when the host says it did.
func (c *Client) ReleaseSession(ctx context.Context, sessionID string) error {
	if err := c.call(ctx, hostwire.MethodSessionRelease, hostwire.SessionRef{SessionID: sessionID}, nil); err != nil {
		return err
	}
	c.forgetHeld(sessionID)
	return nil
}

// CloseSession ends a session for real. It is for deletion, not for detaching.
func (c *Client) CloseSession(ctx context.Context, sessionID string) error {
	if err := c.call(ctx, hostwire.MethodSessionClose, hostwire.SessionRef{SessionID: sessionID}, nil); err != nil {
		return err
	}
	c.forgetHeld(sessionID)
	return nil
}

// SetConfigOption changes model or reasoning effort for later turns.
func (c *Client) SetConfigOption(ctx context.Context, sessionID, optionID, valueID string) ([]harness.ConfigOption, error) {
	var out hostwire.SetConfigResult
	if err := c.call(ctx, hostwire.MethodSessionSetConfig, hostwire.SetConfigParams{
		SessionID: sessionID, OptionID: optionID, ValueID: valueID,
	}, &out); err != nil {
		return nil, err
	}
	options := make([]harness.ConfigOption, 0, len(out.Config))
	for _, o := range out.Config {
		options = append(options, fromWireConfigOption(o))
	}
	return options, nil
}

// Cancel asks the host to interrupt the turn in flight. It returns as soon as
// the request is accepted; the turn settles asynchronously.
func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	return c.call(ctx, hostwire.MethodSessionCancel, hostwire.SessionRef{SessionID: sessionID}, nil)
}

// Prompt submits a turn and waits for its outcome.
//
// It is two calls, not one, and that is the design rather than an accident. The
// submission is acknowledged as soon as the host accepts the prompt, so the
// prompt is owned by the host from that moment; the outcome then arrives as a
// notification, correlated by turn id. A gateway that dies between the two
// rejoins the turn with `turn.await` instead of losing it — which is the entire
// reason the agent is behind a socket.
func (c *Client) Prompt(ctx context.Context, sessionID string, blocks []harness.PromptBlock) (string, error) {
	wire := make([]hostwire.PromptBlock, 0, len(blocks))
	for _, b := range blocks {
		wire = append(wire, hostwire.PromptBlock{
			Type:     b.Type,
			Text:     b.Text,
			MIMEType: b.MIMEType,
			Data:     b.Data,
		})
	}

	var ack hostwire.TurnSubmitResult
	if err := c.call(ctx, hostwire.MethodTurnSubmit, hostwire.TurnSubmitParams{
		SessionID: sessionID,
		Blocks:    wire,
	}, &ack); err != nil {
		return "", err
	}

	ch := c.register(ack.TurnID)
	defer c.forget(ack.TurnID)

	select {
	case result := <-ch:
		return c.settle(result)
	case <-ctx.Done():
		// The caller gave up, and the turn did not: the host is still running it
		// and the outcome is still on its way. That is deliberate — a phone that
		// walks into a lift must not cancel the agent mid-edit — so the only
		// thing to report is that this *observer* stopped waiting.
		return "", ctx.Err()
	}

	// There is deliberately no `case <-c.stop` above. The turn is the host's
	// now, and this process losing its connection does not end it: the
	// supervisor redials, `rejoin` re-establishes what is true, and the outcome
	// arrives through this same channel when it does. Returning early on a
	// disconnect would report a *surviving* turn as a failure — precisely the
	// confusion this tier exists to remove. If the host itself restarted, the
	// rejoin path fails every pending turn with an explicit reason, so nothing
	// waits forever.
}

// settle turns a wire outcome into the harness port's two return values.
func (c *Client) settle(result hostwire.TurnResult) (string, error) {
	if result.Err != nil {
		// A cancelled turn is not a failure: the harness says it stopped, and
		// the scheduler above distinguishes the two by asking whether a human
		// asked for it.
		if result.Cancelled {
			return result.StopReason, nil
		}
		return result.StopReason, fromWireError(result.Err)
	}
	return result.StopReason, nil
}

/* ----------------------------------------------------------------- plumbing */

// call performs one request with the gateway's standard timeout.
func (c *Client) call(ctx context.Context, method string, params, result any) error {
	conn := c.current()
	if conn == nil {
		return ErrNotConnected
	}
	callCtx, cancel := context.WithTimeout(ctx, c.opts.CallTimeout)
	defer cancel()
	return conn.Call(callCtx, method, params, result)
}

func (c *Client) current() *hostwire.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn
}

func (c *Client) stopping() bool {
	select {
	case <-c.stop:
		return true
	default:
		return false
	}
}

func (c *Client) setState(state harness.State, detail string) {
	c.mu.Lock()
	changed := c.state != state || c.detail != detail
	c.state = state
	c.detail = detail
	c.mu.Unlock()
	if changed && c.opts.StateSink != nil {
		c.opts.StateSink.PublishState(state, detail)
	}
}

// decide relays one permission request to the gateway's own handler — the same
// broker that decided these when the adapter was in-process. The host never
// answers one itself.
func (c *Client) decide(ctx context.Context, req hostwire.PermissionRequest) (hostwire.PermissionDecisionParams, error) {
	if c.opts.Permissions == nil {
		return hostwire.PermissionDecisionParams{}, errx.New(errx.KindUnavailable,
			"approvals_unavailable", "this gateway has no approval handler configured")
	}
	decision, err := c.opts.Permissions.RequestPermission(ctx, harness.PermissionRequest{
		ID:          req.ID,
		SessionID:   req.SessionID,
		ToolCallID:  req.ToolCallID,
		Tool:        req.Tool,
		Input:       req.Input,
		Options:     wireOptions(req.Options),
		RequestedAt: req.RequestedAt,
		ExpiresAt:   req.ExpiresAt,
	})
	if err != nil {
		return hostwire.PermissionDecisionParams{}, err
	}
	return hostwire.PermissionDecisionParams{
		SessionID: req.SessionID,
		RequestID: req.ID,
		OptionID:  decision.OptionID,
	}, nil
}

func wireOptions(options []hostwire.PermissionOption) []harness.PermissionOption {
	out := make([]harness.PermissionOption, 0, len(options))
	for _, o := range options {
		out = append(out, harness.PermissionOption{ID: o.ID, Name: o.Name, Kind: o.Kind})
	}
	return out
}

func (c *Client) register(turnID string) chan hostwire.TurnResult {
	ch := make(chan hostwire.TurnResult, 1)
	c.mu.Lock()
	// A turn that settled before this process asked for it is delivered by the
	// host immediately, so replacing an entry is safe and a duplicate is
	// harmless: the newer waiter is the one that still cares.
	c.awaiting[turnID] = ch
	c.mu.Unlock()
	return ch
}

func (c *Client) forget(turnID string) {
	c.mu.Lock()
	delete(c.awaiting, turnID)
	c.mu.Unlock()
}

// deliver routes one outcome to whoever is waiting, and drops it when nobody is.
func (c *Client) deliver(result hostwire.TurnResult) {
	c.mu.Lock()
	ch := c.awaiting[result.TurnID]
	c.mu.Unlock()
	if ch == nil {
		c.logger.Debug("a turn settled with nobody waiting for it",
			"turn", result.TurnID, "session", result.SessionID)
		return
	}
	select {
	case ch <- result:
	default:
		// The channel holds one result and is read once. A second delivery for
		// the same turn — the notification racing the await — is a duplicate.
	}
}

// sendDecision answers a relayed approval. It is a command rather than a
// response, so it works whatever connection the request arrived on.
func (c *Client) sendDecision(ctx context.Context, d hostwire.PermissionDecisionParams) {
	conn := c.current()
	if conn == nil {
		c.logger.Warn("an approval was decided with no host connected",
			"approval", d.RequestID, "option", d.OptionID)
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, c.opts.CallTimeout)
	defer cancel()
	if err := conn.Call(callCtx, hostwire.MethodPermissionDecide, d, nil); err != nil {
		c.logger.Warn("could not deliver an approval decision",
			"approval", d.RequestID, "error", err.Error())
	}
}

// Snapshot reads the host's current state. It is what a gateway calls after a
// reconnect, and what a doctor command calls to answer "what is it holding".
func (c *Client) Snapshot(ctx context.Context) (hostwire.SnapshotResult, error) {
	conn := c.current()
	if conn == nil {
		return hostwire.SnapshotResult{}, ErrNotConnected
	}
	var snap hostwire.SnapshotResult
	err := conn.Call(ctx, hostwire.MethodSnapshot, nil, &snap)
	return snap, err
}

// Drain asks the host to stop accepting turns, which is what a deploy does
// before replacing it.
func (c *Client) Drain(ctx context.Context, reason string, force bool) (hostwire.StatusResult, error) {
	conn := c.current()
	if conn == nil {
		return hostwire.StatusResult{}, ErrNotConnected
	}
	var status hostwire.StatusResult
	err := conn.Call(ctx, hostwire.MethodDrain,
		hostwire.DrainParams{Reason: reason, Force: force}, &status)
	return status, err
}

/* -------------------------------------------------------------------- timing */

// epochFromClock mints the client's fencing token.
//
// It is nanoseconds since the epoch, so it grows monotonically across a fleet
// without coordination: a gateway started later always carries a higher token
// than one started earlier, which is the only property the host needs. Two
// gateways started in the same nanosecond would tie, and the second connection
// still wins because the host adopts by arrival order as well.
func epochFromClock(now func() time.Time) uint64 {
	return uint64(now().UnixNano())
}

// jitter spreads redials so a host restart does not summon every client at once.
// It returns a value in [d/2, d).
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1)) //nolint:gosec // jitter, not a secret
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max || next <= 0 {
		return max
	}
	return next
}

var _ harness.Harness = (*Client)(nil)
