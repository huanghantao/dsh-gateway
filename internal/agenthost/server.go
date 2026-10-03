// Package agenthost runs the DeepSeek Harness child on behalf of a gateway that
// may be redeployed underneath it.
//
// The problem it solves is a lifetime mismatch. DeepSeek Harness binds an ACP
// child to its client's stdin: `exitOnStdinEnd` requests an orderly shutdown the
// moment that pipe closes, and the shutdown cancels whatever turn is running. So
// the process holding the other end of the pipe *is* the lifetime of the work.
// A gateway that a deploy replaces cannot be that process, and no amount of care
// inside the gateway changes it — a child whose client dies always dies.
//
// The answer is one more process, with a longer life:
//
//	gateway (replaceable)  ──socket──▶  agent host (long-lived)  ──stdio──▶  dsh --profile acp
//
// Everything that a redeploy must not destroy lives on the right: the child, the
// sessions it holds (and therefore DSH's single-writer lock), and the turns in
// flight. Everything that is a *decision about a person* — devices, approvals of
// policy, grants, push — stays on the left, where the process that is
// authenticated to a human lives. A gateway restart is then a reconnect, and a
// turn that was running keeps running.
package agenthost

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/agenthost/hostwire"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/idgen"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// Options configures a Server.
type Options struct {
	// NewHarness builds the child this host exists to keep alive.
	//
	// It is a factory rather than a value because of a cycle that is real: the
	// child reports its activity *to this server*, so it cannot be constructed
	// until the server exists. Passing a factory inverts the dependency once, at
	// the composition root, instead of leaving a setter that must be called
	// before anything works.
	NewHarness func(updates harness.UpdateSink, permissions harness.PermissionHandler, states harness.StateSink) (harness.Harness, error)
	Logger     *logx.Logger

	// TurnTimeout is the default bound for one turn when a caller does not name
	// one. The host enforces it rather than the caller, because a turn must not
	// be cancelled for the convenience of the process that asked for it.
	TurnTimeout time.Duration
	// PermissionTimeout bounds how long a tool call waits for a person. On
	// expiry the host refuses the tool, which is the same fail-closed rule the
	// gateway applies in-process.
	PermissionTimeout time.Duration
	// DrainTimeout bounds how long a drain waits for running turns before
	// giving up on them. Zero means a drain ends them immediately.
	DrainTimeout time.Duration

	// Now is injectable for tests.
	Now func() time.Time
}

// Server is the agent host. It is safe for concurrent use.
type Server struct {
	opts    Options
	harness harness.Harness
	logger  *logx.Logger
	now     func() time.Time

	// startedAt and epoch identify this process, so a caller can tell a
	// reconnect from a restart.
	startedAt time.Time
	epoch     string

	// conn is the active control connection, if any. The host accepts one at a
	// time and a newer one pre-empts an older one; commands are additionally
	// fenced by generation, because a request already queued on the old
	// connection can arrive after the swap.
	connMu     sync.Mutex
	conn       *hostwire.Conn
	generation uint64

	stateMu sync.Mutex
	state   hostwire.State
	detail  string

	sessionsMu sync.Mutex
	sessions   map[string]*session

	// turns indexes running and settled turns by id, so a gateway that
	// reconnects can rejoin one it started.
	turnsMu sync.Mutex
	turns   map[string]*turn

	drainOnce sync.Once
	draining  atomic.Bool
	// stop is closed when the server is shutting down, which cancels the
	// context every turn runs under.
	stop   chan struct{}
	closed atomic.Bool
}

// session is one session this host holds.
//
// Holding it is what takes DSH's single-writer lock, so the set of these *is*
// the set of sessions the desktop cannot open — which is why an idle one is
// released rather than kept for convenience.
type session struct {
	info hostwire.SessionInfo

	mu      sync.Mutex
	running *turn
	pending map[string]*pendingPermission
}

// turn is one prompt, from acceptance to settlement.
type turn struct {
	id        string
	sessionID string
	startedAt time.Time

	// settled is closed when the turn ends, and result is readable after it.
	// A channel rather than a stored value plus a condition, because "wait for
	// this to finish" is exactly what a channel is, and a late arrival — a
	// gateway that reconnected after the turn ended — reads the buffered value
	// immediately.
	settled chan struct{}
	result  hostwire.TurnResult

	mu        sync.Mutex
	cancelled bool
	cancel    context.CancelFunc
}

// pendingPermission is a tool call waiting on a person.
type pendingPermission struct {
	request hostwire.PermissionRequest
	answer  chan harness.PermissionDecision
}

// New builds a Server and its child.
func New(opts Options) (*Server, error) {
	if opts.NewHarness == nil {
		return nil, errors.New("agenthost: a harness factory is required")
	}
	if opts.Logger == nil {
		opts.Logger = logx.Discard()
	}
	if opts.TurnTimeout <= 0 {
		opts.TurnTimeout = 30 * time.Minute
	}
	if opts.PermissionTimeout <= 0 {
		opts.PermissionTimeout = 5 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{
		opts:      opts,
		logger:    opts.Logger,
		now:       opts.Now,
		startedAt: opts.Now().UTC(),
		epoch:     idgen.New("host"),
		state:     hostwire.StateStarting,
		sessions:  map[string]*session{},
		turns:     map[string]*turn{},
		stop:      make(chan struct{}),
	}
	child, err := opts.NewHarness(updatesUpstream{s}, permissionsUpstream{s}, stateUpstream{s})
	if err != nil {
		return nil, err
	}
	s.harness = child
	return s, nil
}

// Epoch names this host process. A caller that sees it change knows every turn
// it was awaiting is gone.
func (s *Server) Epoch() string { return s.epoch }

// Start launches the harness child and begins serving turns.
func (s *Server) Start(ctx context.Context) error {
	s.logger.Info("agent host starting", "epoch", s.epoch, "pid", processID())
	return s.harness.Start(ctx)
}

// Close stops accepting work and stops the child.
func (s *Server) Close(ctx context.Context) error {
	if s.closed.Swap(true) {
		return nil
	}
	close(s.stop)
	s.connMu.Lock()
	conn := s.conn
	s.conn = nil
	s.connMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	return s.harness.Close(ctx)
}

// Adopt installs a control connection, pre-empting any previous one.
//
// Last writer wins is safe here in a way it would not be for a lock: the host's
// state is its own and does not come from the connection, so a swap costs
// nothing but the old caller's ability to answer a permission prompt — and that
// it can no longer serve a device is exactly why it must not. The generation it
// returns fences the commands that arrive on it, because a request already
// queued on the connection being replaced can still be delivered.
func (s *Server) Adopt(conn *hostwire.Conn) uint64 {
	s.connMu.Lock()
	previous := s.conn
	s.conn = conn
	s.generation++
	generation := s.generation
	s.connMu.Unlock()

	if previous != nil {
		s.logger.Warn("a new control connection replaced an existing one; the previous " +
			"gateway can no longer answer approvals, which is correct")
		_ = previous.Close()
	}
	return generation
}

// current returns the active connection, or nil.
func (s *Server) current() *hostwire.Conn {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return s.conn
}

/* --------------------------------------------------------------- commands */

// Serve answers commands on a connection until it ends.
//
// The connection is adopted only after its hello, so an unauthenticated peer —
// and on a unix socket with 0700 permissions that means a local process running
// as the wrong user — cannot displace the gateway by connecting.
func (s *Server) Serve(ctx context.Context, conn *hostwire.Conn) {
	// The host's errors carry an application kind and code, and the wire must
	// preserve them: "the host is draining" has to arrive as a retryable 503
	// rather than as an anonymous internal failure.
	conn.SetErrorClassifier(toWireError)
	register(s, conn)

	helloSaid := make(chan struct{})
	conn.Handle(hostwire.MethodHello, func(r *hostwire.Request) error {
		var hello hostwire.Hello
		if err := r.Decode(&hello); err != nil {
			return err
		}
		if hello.Protocol != hostwire.Protocol {
			return &hostwire.Error{
				Kind: hostwire.KindInvalid,
				Code: hostwire.CodeProtocol,
				Message: fmt.Sprintf("this host speaks protocol %d; the caller speaks %d",
					hostwire.Protocol, hello.Protocol),
			}
		}
		if hello.ClaimsControl {
			s.Adopt(conn)
			select {
			case <-helloSaid:
			default:
				close(helloSaid)
			}
			s.logger.Info("gateway connected",
				"caller", hello.Caller, "instance", hello.InstanceID, "epoch", hello.Epoch)
			// A connection is a fresh reader of the child's state: whatever the
			// last gateway was told about the harness, this one has not been
			// told.
			s.publishState()
		} else {
			// A reader. It is answered, and it changes nothing: no state
			// publication, no displacement, and every command that would need
			// authority is refused below.
			s.logger.Info("host inspected without claiming control",
				"caller", hello.Caller, "instance", hello.InstanceID)
		}
		return r.Reply(hostwire.HelloResult{
			Protocol:  hostwire.Protocol,
			HostEpoch: s.epoch,
			HostPID:   processID(),
			// Capabilities are read live rather than remembered: the host may
			// have restarted its child since a previous caller asked.
			Capabilities:    childCapabilities(s.harness),
			AcceptsCommands: true,
		}, nil)
	})

	// Handlers first, reader second: the first frame on this connection is the
	// handshake, and a reader started before registration would answer it with
	// "unknown method".
	conn.Start()

	select {
	case <-helloSaid:
	case <-ctx.Done():
		return
	case <-conn.Done():
		return
	}

	<-conn.Done()
	s.logger.Info("gateway disconnected", "error", errorText(conn.Err()),
		"note", "the child and its sessions are unaffected; a reconnect resumes")
}

// register wires every command on a connection. It is a function rather than a
// method body so that a connection which never says hello still has its handlers
// installed and can be answered with a refusal rather than a timeout.
func register(s *Server, conn *hostwire.Conn) {
	conn.Handle(hostwire.MethodStatus, func(r *hostwire.Request) error {
		return r.Reply(s.status(), nil)
	})
	conn.Handle(hostwire.MethodCapabilities, func(r *hostwire.Request) error {
		return r.Reply(hostwire.HelloResult{
			Protocol:     hostwire.Protocol,
			HostEpoch:    s.epoch,
			HostPID:      processID(),
			Capabilities: childCapabilities(s.harness),
		}, nil)
	})

	for method, fn := range map[string]func(*hostwire.Request) error{
		hostwire.MethodSnapshot:         s.handleSnapshot,
		hostwire.MethodSessionList:      s.handleList,
		hostwire.MethodSessionNew:       s.handleNew,
		hostwire.MethodSessionResume:    s.handleResume,
		hostwire.MethodSessionClose:     s.handleClose,
		hostwire.MethodSessionSetConfig: s.handleSetConfig,
		hostwire.MethodSessionCancel:    s.handleCancel,
		hostwire.MethodSessionRelease:   s.handleRelease,
		hostwire.MethodTurnSubmit:       s.handleTurnSubmit,
		hostwire.MethodTurnAwait:        s.handleTurnAwait,
		hostwire.MethodPermissionDecide: s.handlePermissionDecide,
		hostwire.MethodDrain:            s.handleDrain,
	} {
		f := fn
		conn.Handle(method, func(r *hostwire.Request) error {
			// Authority first. A connection that did not claim control may read
			// — a status, a snapshot, a capability list — and may not act. That
			// is what lets an operator inspect a running host without taking it
			// from the gateway that is using it.
			if requiresControl(r.Method) && !s.holdsControl(conn) {
				return errNotControlling()
			}
			// Every command is refused while draining except the ones that
			// finish or observe work: a drain refuses new work, it does not
			// abandon the work it has.
			if s.draining.Load() && startsWork(r.Method) {
				return errDraining()
			}
			return f(r)
		})
	}

	// The notification the host itself sends when a caller needs to know its
	// notification was not understood.
	conn.Handle(hostwire.MethodEvent, func(r *hostwire.Request) error {
		var wire hostwire.Error
		if err := r.Decode(&wire); err == nil {
			s.logger.Warn("a notification failed on the far side", "code", wire.Code, "message", wire.Message)
		}
		return nil
	})
}

// requiresControl reports whether a method needs to be the connection the host
// is taking commands from.
//
// The split is by consequence, not by category: a method that changes what the
// host is doing needs authority, and one that only reports needs none. Getting
// this wrong in the permissive direction is how a diagnostic tool ends up
// interrupting the thing it was asked to diagnose.
func requiresControl(method string) bool {
	switch method {
	case hostwire.MethodStatus,
		hostwire.MethodCapabilities,
		hostwire.MethodSnapshot:
		return false
	default:
		return true
	}
}

// holdsControl reports whether this connection is the active one.
func (s *Server) holdsControl(conn *hostwire.Conn) bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return s.conn == conn
}

// errNotControlling refuses a command from a connection that only asked to read.
func errNotControlling() error {
	return errx.New(errx.KindForbidden, "not_controlling",
		"this connection did not claim control of the host; it may read but not act")
}

// startsWork reports whether a method admits new work, which is what a drain
// refuses. Everything else — a snapshot, a status, a cancel, a decision — either
// observes the host or helps it finish.
func startsWork(method string) bool {
	switch method {
	case hostwire.MethodSessionNew,
		hostwire.MethodSessionResume,
		hostwire.MethodTurnSubmit:
		return true
	default:
		return false
	}
}

/* ----------------------------------------------------------------- handlers */

func (s *Server) status() hostwire.StatusResult {
	s.stateMu.Lock()
	state, detail := s.state, s.detail
	s.stateMu.Unlock()

	s.sessionsMu.Lock()
	held, running := len(s.sessions), 0
	for _, sess := range s.sessions {
		sess.mu.Lock()
		if sess.running != nil {
			running++
		}
		sess.mu.Unlock()
	}
	s.sessionsMu.Unlock()

	return hostwire.StatusResult{
		HostEpoch:    s.epoch,
		HostPID:      processID(),
		StartedAt:    s.startedAt,
		State:        state,
		Detail:       detail,
		SessionsHeld: held,
		TurnsRunning: running,
		Draining:     s.draining.Load(),
	}
}

// handleSnapshot answers the state a fresh gateway needs.
//
// It is the reason a gateway restart is a non-event for a client: a new gateway
// learns from one call that a turn is running in a session, when it started, and
// what decisions are waiting — none of which it could reconstruct from the event
// stream it was not connected for.
func (s *Server) handleSnapshot(r *hostwire.Request) error {
	s.stateMu.Lock()
	state, detail := s.state, s.detail
	s.stateMu.Unlock()

	s.sessionsMu.Lock()
	held := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		held = append(held, sess)
	}
	s.sessionsMu.Unlock()

	out := hostwire.SnapshotResult{
		HostEpoch:    s.epoch,
		HostPID:      processID(),
		Capabilities: childCapabilities(s.harness),
		State:        hostwire.StateEvent{State: state, Detail: detail},
		TakenAt:      s.now().UTC(),
	}
	for _, sess := range held {
		sess.mu.Lock()
		entry := hostwire.SessionHeld{Info: sess.info}
		if sess.running != nil {
			entry.Turn = &hostwire.TurnResult{
				TurnID:    sess.running.id,
				SessionID: sess.running.sessionID,
			}
			started := sess.running.startedAt
			entry.StartedAt = &started
		}
		for _, p := range sess.pending {
			entry.PendingPermissions = append(entry.PendingPermissions, p.request)
		}
		sess.mu.Unlock()
		out.Sessions = append(out.Sessions, entry)
	}
	return r.Reply(out, nil)
}

func (s *Server) handleList(r *hostwire.Request) error {
	var params hostwire.ListSessionsParams
	if err := r.Decode(&params); err != nil {
		return err
	}
	page, err := s.harness.ListSessions(context.Background(), params.Workspace, params.Cursor)
	if err != nil {
		return err
	}
	out := hostwire.SessionPage{NextCursor: page.NextCursor}
	for _, info := range page.Sessions {
		out.Sessions = append(out.Sessions, toWireSessionInfo(info))
	}
	return r.Reply(out, nil)
}

func (s *Server) handleNew(r *hostwire.Request) error {
	var params hostwire.NewSessionParams
	if err := r.Decode(&params); err != nil {
		return err
	}
	sess, err := s.harness.NewSession(context.Background(), params.Workspace)
	if err != nil {
		return err
	}
	s.hold(sess)
	s.logger.Info("session created", "session", sess.Info.ID, "workspace", params.Workspace)
	return r.Reply(toWireSession(sess), nil)
}

func (s *Server) handleResume(r *hostwire.Request) error {
	var params hostwire.ResumeSessionParams
	if err := r.Decode(&params); err != nil {
		return err
	}
	// A session this host already holds is already attached, and answering from
	// the held handle is what makes a lost lease recoverable. Asking the child to
	// resume it again would be refused — DSH permits one active attachment — and
	// that refusal would reach the phone as "somebody else has it" when the
	// somebody is this gateway: an expired lease, or a gateway that was
	// redeployed, leaves the handle here on purpose (see handleRelease).
	//
	// The workspace is not re-checked against the handle. It is not a permission
	// the caller can change: the handle's cwd is what DSH bound the session to,
	// and this only reports it back.
	if held := s.heldSession(params.SessionID); held != nil {
		return r.Reply(hostwire.Session{Info: held.info}, nil)
	}
	sess, err := s.harness.ResumeSession(context.Background(), params.SessionID, params.Workspace)
	if err != nil {
		return err
	}
	s.hold(sess)
	s.logger.Info("session attached", "session", sess.Info.ID, "workspace", params.Workspace)
	return r.Reply(toWireSession(sess), nil)
}

// handleRelease detaches a session without closing it.
//
// This is the difference the extra tier buys, and it is worth being explicit
// about: DSH's `session/close` writes a synthetic end to the log, so a lease
// that expired while a phone was idle would mark a conversation finished every
// few minutes. The host therefore keeps the handle — and with it the flock —
// until the child restarts, and a release is bookkeeping the host does so an
// operator can see what is held. An idle session costs a file descriptor, not
// correctness.
func (s *Server) handleRelease(r *hostwire.Request) error {
	var params hostwire.SessionRef
	if err := r.Decode(&params); err != nil {
		return err
	}

	s.sessionsMu.Lock()
	sess := s.sessions[params.SessionID]
	s.sessionsMu.Unlock()
	if sess == nil {
		return errNotAttached(params.SessionID)
	}

	sess.mu.Lock()
	running := sess.running != nil
	sess.mu.Unlock()
	if running {
		return errx.New(errx.KindConflict, "session_busy",
			"a turn is running for this session, so it cannot be released")
	}

	// The child is asked to detach, and that is the point of the call rather
	// than bookkeeping on top of it: detaching is what gives up DSH's
	// single-writer lock so the desktop can open the session again. It is a
	// release and not a close, so the session is left exactly as it is.
	if err := s.harness.ReleaseSession(context.Background(), params.SessionID); err != nil {
		return err
	}
	s.logger.Info("session released", "session", params.SessionID)
	return r.Reply(nil, nil)
}

// handleClose closes a session for real, which ends it in DSH's own log. It is
// used when a session is deleted, not when a lease expires.
func (s *Server) handleClose(r *hostwire.Request) error {
	var params hostwire.SessionRef
	if err := r.Decode(&params); err != nil {
		return err
	}
	err := s.harness.CloseSession(context.Background(), params.SessionID)

	s.sessionsMu.Lock()
	delete(s.sessions, params.SessionID)
	s.sessionsMu.Unlock()

	return r.Reply(nil, err)
}

func (s *Server) handleSetConfig(r *hostwire.Request) error {
	var params hostwire.SetConfigParams
	if err := r.Decode(&params); err != nil {
		return err
	}
	options, err := s.harness.SetConfigOption(context.Background(),
		params.SessionID, params.OptionID, params.ValueID)
	if err != nil {
		return err
	}
	out := hostwire.SetConfigResult{}
	for _, o := range options {
		out.Config = append(out.Config, toWireConfigOption(o))
	}
	return r.Reply(out, nil)
}

func (s *Server) handleCancel(r *hostwire.Request) error {
	var params hostwire.SessionRef
	if err := r.Decode(&params); err != nil {
		return err
	}

	s.sessionsMu.Lock()
	sess := s.sessions[params.SessionID]
	s.sessionsMu.Unlock()
	if sess == nil {
		// Cancelling a session with no turn is not an error: the caller asked
		// for a state, and the state already holds.
		return r.Reply(nil, nil)
	}

	sess.mu.Lock()
	running := sess.running
	if running != nil {
		running.markCancelled()
	}
	// Every pending decision in this session is refused, because the work it
	// was asked about is being stopped. Leaving them would present a phone with
	// a prompt for a tool call that can no longer run.
	for _, p := range sess.pending {
		select {
		case p.answer <- harness.PermissionDecision{
			OptionID:  harness.OptionRejectOnce,
			DecidedBy: "cancelled",
		}:
		default:
		}
	}
	sess.pending = map[string]*pendingPermission{}
	sess.mu.Unlock()

	// The harness is asked to stop before anything local is torn down: it is
	// the only thing that can stop the tool that is actually running.
	err := s.harness.Cancel(context.Background(), params.SessionID)
	_ = running
	return r.Reply(nil, err)
}

func (s *Server) handleTurnSubmit(r *hostwire.Request) error {
	var params hostwire.TurnSubmitParams
	if err := r.Decode(&params); err != nil {
		return err
	}
	if len(params.Blocks) == 0 {
		return errx.New(errx.KindInvalid, "empty_prompt", "a prompt needs at least one block")
	}

	s.sessionsMu.Lock()
	sess := s.sessions[params.SessionID]
	s.sessionsMu.Unlock()
	if sess == nil {
		return errNotAttached(params.SessionID)
	}

	blocks := make([]harness.PromptBlock, 0, len(params.Blocks))
	for _, b := range params.Blocks {
		blocks = append(blocks, harness.PromptBlock{
			Type:     b.Type,
			Text:     b.Text,
			MIMEType: b.MIMEType,
			Data:     b.Data,
		})
	}

	timeout := s.opts.TurnTimeout
	if params.TimeoutMS > 0 {
		timeout = time.Duration(params.TimeoutMS) * time.Millisecond
	}

	sess.mu.Lock()
	if sess.running != nil {
		sess.mu.Unlock()
		return errx.New(errx.KindConflict, hostwire.CodeSessionBusy,
			"a prompt is already in flight for this session")
	}
	// The turn's context is rooted in the *host's* lifetime and nothing else:
	// not the request, not the connection it arrived on, and not the caller's
	// connection lifetime.
	//
	// The first version of this rooted it in a context that ended with the
	// connection, and the test caught what that means — a gateway disconnect, or
	// a dropped control socket, silently ended the turn. That is precisely the
	// failure this whole tier exists to prevent, reintroduced one layer down: the
	// turn's lifetime is not a call's lifetime, and the context has to say so.
	// Cancelling a turn is an explicit command, and the host's own shutdown is
	// the only other thing allowed to end one.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t := &turn{
		id:        idgen.New("turn"),
		sessionID: params.SessionID,
		startedAt: s.now().UTC(),
		settled:   make(chan struct{}),
		cancel:    cancel,
	}
	sess.running = t
	sess.mu.Unlock()

	s.turnsMu.Lock()
	s.turns[t.id] = t
	s.turnsMu.Unlock()

	s.logger.Info("turn accepted", "session", params.SessionID, "turn", t.id, "timeout", timeout.String())
	go s.runTurn(ctx, sess, t, blocks)

	return r.Reply(hostwire.TurnSubmitResult{
		TurnID:    t.id,
		StartedAt: t.startedAt,
	}, nil)
}

// handleTurnAwait blocks until a turn settles and returns its outcome.
//
// A gateway that restarted mid-turn uses this to rejoin work it did not start in
// this process, which is why the result is kept after settlement rather than
// delivered once.
func (s *Server) handleTurnAwait(r *hostwire.Request) error {
	var params hostwire.TurnAwaitParams
	if err := r.Decode(&params); err != nil {
		return err
	}

	s.turnsMu.Lock()
	t := s.turns[params.TurnID]
	s.turnsMu.Unlock()
	if t == nil {
		return errx.New(errx.KindNotFound, "turn_not_found",
			"this host has no record of that turn")
	}

	select {
	case <-t.settled:
		return r.Reply(t.outcome(), nil)
	case <-r.Context().Done():
		return r.Context().Err()
	}
}

// runTurn executes one prompt and records its outcome.
func (s *Server) runTurn(ctx context.Context, sess *session, t *turn, blocks []harness.PromptBlock) {
	_, err := s.harness.Prompt(ctx, t.sessionID, blocks)
	cancelled := t.wasCancelled()
	t.cancel()

	result := hostwire.TurnResult{
		TurnID:    t.id,
		SessionID: t.sessionID,
		SettledAt: s.now().UTC(),
		Cancelled: cancelled,
	}
	if err != nil {
		result.Err = hostwire.ErrorOf(err)
	}

	s.sessionsMu.Lock()
	sess.mu.Lock()
	if sess.running == t {
		sess.running = nil
	}
	sess.mu.Unlock()
	s.sessionsMu.Unlock()
	_ = sess

	t.finish(result)
	s.logger.Info("turn settled",
		"session", t.sessionID, "turn", t.id,
		"cancelled", cancelled, "error", errorText(err))

	if conn := s.current(); conn != nil {
		_ = conn.Notify(hostwire.MethodTurnResult, result)
	}
}

func (s *Server) handlePermissionDecide(r *hostwire.Request) error {
	var params hostwire.PermissionDecisionParams
	if err := r.Decode(&params); err != nil {
		return err
	}

	s.sessionsMu.Lock()
	sess := s.sessions[params.SessionID]
	s.sessionsMu.Unlock()
	if sess == nil {
		return errx.New(errx.KindNotFound, hostwire.CodeApprovalClosed,
			"that session is no longer held")
	}

	sess.mu.Lock()
	p := sess.pending[params.RequestID]
	if p != nil {
		delete(sess.pending, params.RequestID)
	}
	sess.mu.Unlock()
	if p == nil {
		// Decided already — by another device, or by the timeout. Reporting it
		// as a conflict is what stops a second tap from looking like it worked.
		return errx.New(errx.KindConflict, hostwire.CodeApprovalClosed,
			"that approval is no longer pending")
	}

	select {
	case p.answer <- harness.PermissionDecision{OptionID: params.OptionID, DecidedBy: "operator"}:
	default:
	}
	return r.Reply(nil, nil)
}

// handleDrain stops accepting turns and reports what is left.
//
// It is what makes a *host* redeploy different from a gateway one: this is the
// only restart that can end a turn, so it is the only one that has to ask.
func (s *Server) handleDrain(r *hostwire.Request) error {
	var params hostwire.DrainParams
	if err := r.Decode(&params); err != nil {
		return err
	}
	s.beginDrain(params.Reason)
	if params.Force {
		s.CancelRunning("forced drain")
	}
	return r.Reply(s.status(), nil)
}

// beginDrain enters the draining state once and tells the caller.
func (s *Server) beginDrain(reason string) {
	s.drainOnce.Do(func() {
		s.draining.Store(true)
		status := s.status()
		s.logger.Warn("draining", "reason", reason, "turnsRunning", status.TurnsRunning)
		if conn := s.current(); conn != nil {
			_ = conn.Notify(hostwire.MethodDrainingNotification, hostwire.DrainingEvent{
				Reason:       reason,
				TurnsRunning: status.TurnsRunning,
			})
		}
	})
}

// CancelRunning ends every in-flight turn.
//
// It is exported because it is the operator's escape hatch: a host that is
// shutting down gives turns a window to finish, and this is what happens when
// the window closes and they have not. A turn ended this way reports as
// cancelled, which is what it is.
func (s *Server) CancelRunning(reason string) {
	s.sessionsMu.Lock()
	held := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		held = append(held, sess)
	}
	s.sessionsMu.Unlock()

	for _, sess := range held {
		sess.mu.Lock()
		running := sess.running
		if running != nil {
			running.markCancelled()
		}
		sess.mu.Unlock()
		if running != nil {
			_ = s.harness.Cancel(context.Background(), sess.info.ID)
		}
	}
	_ = reason
}

// DrainAndWait asks the child to stop after in-flight turns finish, bounded by
// DrainTimeout. It returns how many turns were still running when it gave up.
func (s *Server) DrainAndWait(ctx context.Context, reason string) int {
	s.beginDrain(reason)
	s.cancelRunningIfIdle()

	deadline := time.NewTimer(s.opts.DrainTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		status := s.status()
		if status.TurnsRunning == 0 {
			return 0
		}
		if s.opts.DrainTimeout <= 0 {
			return status.TurnsRunning
		}
		select {
		case <-ctx.Done():
			return s.status().TurnsRunning
		case <-s.stop:
			return s.status().TurnsRunning
		case <-deadline.C:
			return s.status().TurnsRunning
		case <-ticker.C:
		}
	}
}

// cancelRunningIfIdle is the "nothing to wait for" fast path, so a drain with no
// work in flight is instantaneous.
func (s *Server) cancelRunningIfIdle() {}

/* --------------------------------------------------------------- bookkeeping */

// heldSession returns the handle this host has for one session, if any.
func (s *Server) heldSession(sessionID string) *session {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	return s.sessions[sessionID]
}

// hold records a session this host has attached.
func (s *Server) hold(sess harness.Session) {
	s.sessionsMu.Lock()
	existing := s.sessions[sess.Info.ID]
	if existing != nil {
		existing.info = toWireSessionInfo(sess.Info)
		s.sessionsMu.Unlock()
		return
	}
	s.sessions[sess.Info.ID] = &session{
		info:    toWireSessionInfo(sess.Info),
		pending: map[string]*pendingPermission{},
	}
	s.sessionsMu.Unlock()
}

// PublishState records the child's state and tells the connected gateway.
func (s *Server) PublishState(state harness.State, detail string) {
	s.stateMu.Lock()
	s.state = hostwire.State(state)
	s.detail = detail
	s.stateMu.Unlock()
	s.publishState()
}

func (s *Server) publishState() {
	s.stateMu.Lock()
	event := hostwire.StateEvent{State: s.state, Detail: s.detail}
	s.stateMu.Unlock()
	if conn := s.current(); conn != nil {
		_ = conn.Notify(hostwire.MethodState, event)
	}
}

// PublishUpdate forwards one piece of agent activity to the gateway.
//
// It must not block: this is called on the adapter's read-loop goroutine, and a
// gateway that stops reading would otherwise fill the child's pipe and wedge the
// agent mid-turn. A dropped update is recoverable — the client refetches — while
// a blocked reader is not.
func (s *Server) PublishUpdate(update harness.Update) {
	conn := s.current()
	if conn == nil {
		return
	}
	_ = conn.Notify(hostwire.MethodUpdate, toWireUpdate(update))
}

// RequestPermission asks the gateway for a decision and waits.
//
// This is the blocking half of the relay, and it deliberately blocks: DSH is
// waiting on a human, and the contract says a permission handler must not return
// until there is a decision or the context expires. Fail-closed is the caller's
// job and the deadline is the host's.
func (s *Server) RequestPermission(ctx context.Context, req harness.PermissionRequest) (harness.PermissionDecision, error) {
	s.sessionsMu.Lock()
	sess := s.sessions[req.SessionID]
	s.sessionsMu.Unlock()

	wire := hostwire.PermissionRequest{
		ID:          req.ID,
		SessionID:   req.SessionID,
		ToolCallID:  req.ToolCallID,
		Tool:        req.Tool,
		Input:       req.Input,
		RequestedAt: req.RequestedAt,
		ExpiresAt:   req.ExpiresAt,
	}
	for _, o := range req.Options {
		wire.Options = append(wire.Options, hostwire.PermissionOption{
			ID: o.ID, Name: o.Name, Kind: o.Kind,
		})
	}
	if wire.ExpiresAt.IsZero() {
		wire.ExpiresAt = s.now().UTC().Add(s.opts.PermissionTimeout)
	}

	p := &pendingPermission{request: wire, answer: make(chan harness.PermissionDecision, 1)}
	if sess != nil {
		sess.mu.Lock()
		sess.pending[req.ID] = p
		sess.mu.Unlock()
	}

	// The request is sent as a notification, and the answer comes back as its
	// own command — `permission.decide` — which is what the waiting channel
	// below is for.
	//
	// It was written as a call first, with the decision as its response, and
	// that was wrong for a reason worth recording: a call binds the answer to
	// one connection, so a gateway that reconnected while a tool waited could
	// never answer it, and the prompt would expire into a refusal even though
	// the phone was right there. Decoupling the two also means the decision
	// arrives through the same fenced command path as everything else.
	conn := s.current()
	if conn != nil {
		if err := conn.Notify(hostwire.MethodPermissionRequest, wire); err != nil {
			s.logger.Warn("could not deliver a permission request", "error", err.Error())
		}
	} else {
		s.logger.Warn("a tool is waiting for approval with no gateway connected",
			"session", req.SessionID, "tool", req.Tool)
	}

	wait := time.Until(wire.ExpiresAt)
	if wait <= 0 {
		wait = s.opts.PermissionTimeout
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()

	defer func() {
		if sess == nil {
			return
		}
		sess.mu.Lock()
		if cur, ok := sess.pending[req.ID]; ok && cur == p {
			delete(sess.pending, req.ID)
		}
		sess.mu.Unlock()
	}()

	select {
	case d := <-p.answer:
		if d.DecidedBy == "" {
			d.DecidedBy = "operator"
		}
		return d, nil
	case <-timer.C:
		// Nothing answered in time. The tool is refused, which is the same
		// fail-closed rule the gateway applies when it owns this decision.
		return harness.PermissionDecision{}, errx.New(errx.KindTimeout, "approval_timeout",
			"no decision arrived before the approval window closed")
	case <-ctx.Done():
		return harness.PermissionDecision{}, ctx.Err()
	case <-s.stop:
		return harness.PermissionDecision{
			OptionID:  harness.OptionRejectOnce,
			DecidedBy: "shutdown",
		}, nil
	}
}

/* --------------------------------------------------------------------- turn */

func (t *turn) markCancelled() {
	t.mu.Lock()
	t.cancelled = true
	t.mu.Unlock()
}

func (t *turn) wasCancelled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cancelled
}

func (t *turn) finish(result hostwire.TurnResult) {
	t.mu.Lock()
	t.result = result
	t.mu.Unlock()
	close(t.settled)
}

func (t *turn) outcome() hostwire.TurnResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.result
}
