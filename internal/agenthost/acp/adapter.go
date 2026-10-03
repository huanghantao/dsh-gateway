// Package acp implements the harness port over an ACP stdio child.
//
// It lives under the agent host because that is the only thing that uses it: the
// gateway drives the agent across a socket and never spawns a child itself, so
// the package's home is a statement about the architecture rather than a filing
// decision. There is no way for the gateway to run the agent in-process, because
// it does not contain the code that could.
package acp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/toolresult"
)

// Options configures the adapter.
type Options struct {
	// Binary is the dsh executable; resolved on PATH when not absolute.
	Binary string
	// Profile is the ACP profile name. The shipped default is "acp".
	Profile string
	// Home is DSH_HOME. It must match the desktop install so that the phone and
	// the desktop share one session store.
	Home string
	// SandboxMode maps to DSH_PERMISSION_MODE. It is the harness's sandbox
	// preset, from which DSH derives the approval policy: "danger-full-access"
	// means no approval prompts at all, anything else means prompts are asked.
	SandboxMode string
	// ExtraEnv is appended to the child environment as KEY=VALUE. It is how an
	// operator supplies a provider credential without writing it to disk.
	ExtraEnv []string
	// WorkingDir is the child's initial working directory. Sessions carry their
	// own absolute cwd, so this is cosmetic.
	WorkingDir string

	StartTimeout      time.Duration
	StopTimeout       time.Duration
	RestartBackoff    time.Duration
	MaxRestartBackoff time.Duration
	// ApprovalTimeout bounds how long a permission prompt waits for a human.
	ApprovalTimeout time.Duration

	Logger      *logx.Logger
	Updates     harness.UpdateSink
	Permissions harness.PermissionHandler
	States      harness.StateSink
}

// Adapter implements harness.Harness over an ACP stdio child process.
type Adapter struct {
	opts   Options
	logger *logx.Logger

	mu       sync.RWMutex
	conn     *conn
	proc     *process
	state    harness.State
	caps     harness.Capabilities
	sessions map[string]harness.Session

	// toolCalls remembers the details of in-flight tool calls, keyed by call id.
	//
	// This exists because of a gap in the protocol as DSH implements it: the
	// `session/request_permission` request carries only a toolCallId, with no
	// title and no rawInput. Without this correlation the gateway would ask the
	// operator to authorise something it could not name or display, and an
	// approval prompt that shows nothing is worse than useless — it trains the
	// operator to tap "allow" without reading.
	//
	// Populated from the `tool_call` update, which does carry both. In a live
	// trace the update arrives before the permission request, so the lookup hit
	// is reliable rather than lucky.
	toolCalls   map[string]harness.ToolCall
	toolCallIDs []string

	stopping sync.Once
	stopCh   chan struct{}
	closed   chan struct{}
}

// New validates options and returns an adapter. Start must be called before any
// session method.
func New(opts Options) (*Adapter, error) {
	if opts.Binary == "" {
		return nil, errors.New("acp: Binary is required")
	}
	if opts.Profile == "" {
		return nil, errors.New("acp: Profile is required")
	}
	if opts.Logger == nil {
		return nil, errors.New("acp: Logger is required")
	}
	if opts.Updates == nil {
		return nil, errors.New("acp: Updates is required")
	}
	if opts.ApprovalTimeout <= 0 {
		opts.ApprovalTimeout = 5 * time.Minute
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = 90 * time.Second
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = 15 * time.Second
	}
	if opts.RestartBackoff <= 0 {
		opts.RestartBackoff = time.Second
	}
	if opts.MaxRestartBackoff < opts.RestartBackoff {
		opts.MaxRestartBackoff = time.Minute
	}
	return &Adapter{
		opts:      opts,
		logger:    opts.Logger,
		state:     harness.StateStarting,
		sessions:  map[string]harness.Session{},
		toolCalls: map[string]harness.ToolCall{},
		stopCh:    make(chan struct{}),
		closed:    make(chan struct{}),
	}, nil
}

// Start launches the child and begins supervising it.
//
// A failed first launch is not returned as an error: the supervisor keeps
// retrying with backoff, and readiness is reported through State and /readyz.
// This matters because the gateway and the harness are started independently —
// a launchd unit or a login shell may well bring the gateway up before `dsh` is
// resolvable, and giving up then would leave a dead service that a restart
// happens to fix.
func (a *Adapter) Start(ctx context.Context) error {
	if err := a.spawn(ctx); err != nil {
		a.logger.Error("harness did not start; the supervisor will keep retrying", "error", err.Error())
		a.setState(harness.StateRestarting, err.Error())
	}
	// The supervisor lives as long as the process does, so it is rooted at
	// Background rather than at whatever launched it.
	go a.supervise() //nolint:gosec,contextcheck // process-lifetime goroutine, not request-scoped
	return nil
}

// Capabilities returns what the connected harness advertised.
func (a *Adapter) Capabilities() harness.Capabilities {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.caps
}

// State implements harness.Harness.
func (a *Adapter) State() harness.State {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.state
}

// Close stops supervision and shuts the child down.
func (a *Adapter) Close(ctx context.Context) error {
	a.stopping.Do(func() { close(a.stopCh) })

	a.mu.Lock()
	proc := a.proc
	a.proc = nil
	conn := a.conn
	a.conn = nil
	a.sessions = map[string]harness.Session{}
	a.mu.Unlock()

	if conn != nil {
		// Closing the connection first makes every in-flight call fail fast
		// instead of waiting for the child to exit.
		conn.close(errors.New("acp: adapter is shutting down"))
	}

	a.setState(harness.StateStopped, "")

	if proc == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- proc.Stop(a.opts.StopTimeout) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		proc.Kill()
		return ctx.Err()
	}
}

// spawn starts a child and completes the ACP handshake against it.
func (a *Adapter) spawn(ctx context.Context) error {
	a.setState(harness.StateStarting, "")

	proc, err := startProcess(processConfig{
		Binary: a.opts.Binary,
		Args:   []string{"--profile", a.opts.Profile},
		Env:    a.childEnv(),
		Dir:    a.opts.WorkingDir,
	}, a.logger)
	if err != nil {
		return errx.Wrap(err, errx.KindUnavailable, "harness_unavailable", "could not start the harness process")
	}

	c := newConn(proc.stdout, proc.stdin, a.logger)
	c.onNotification = a.handleNotification
	c.onRequest = a.handleRequest
	// The connection's reader owns no request context: it is driven by a child
	// process, and the request handlers it dispatches (approvals) receive their
	// own bounded context rather than one inherited from a caller that does not
	// exist.
	c.start() //nolint:contextcheck // the reader is not part of any request

	initCtx, cancel := context.WithTimeout(ctx, a.opts.StartTimeout)
	defer cancel()

	var result initializeResult
	err = c.Call(initCtx, "initialize", initializeParams{
		ProtocolVersion: protocolVersion,
		ClientInfo:      clientInfo{Name: "dsh-gateway", Version: "1"},
	}, &result)
	if err != nil {
		// Never leave a half-initialised child running.
		_ = proc.Stop(a.opts.StopTimeout)
		return errx.Wrap(err, errx.KindUnavailable, "harness_handshake_failed",
			"the harness process did not complete the ACP handshake")
	}

	if result.ProtocolVersion != protocolVersion {
		_ = proc.Stop(a.opts.StopTimeout)
		return errx.New(errx.KindUnavailable, "harness_protocol_mismatch",
			fmt.Sprintf("the harness speaks ACP version %d, this gateway implements %d",
				result.ProtocolVersion, protocolVersion))
	}

	caps := harness.Capabilities{
		ProtocolVersion: result.ProtocolVersion,
		AgentName:       result.AgentInfo.Name,
		AgentVersion:    result.AgentInfo.Version,
		CanList:         len(result.AgentCapabilities.SessionCapabilities.List) > 0,
		CanResume:       len(result.AgentCapabilities.SessionCapabilities.Resume) > 0,
		CanClose:        len(result.AgentCapabilities.SessionCapabilities.Close) > 0,
		CanPromptImages: result.AgentCapabilities.PromptCapabilities.Image,
	}

	a.mu.Lock()
	a.proc = proc
	a.conn = c
	a.caps = caps
	a.state = harness.StateReady
	a.mu.Unlock()

	a.logger.Info("harness ready",
		"agent", caps.AgentName,
		"protocol", caps.ProtocolVersion,
		"canList", caps.CanList,
		"canResume", caps.CanResume,
		"images", caps.CanPromptImages,
	)
	if a.opts.States != nil {
		a.opts.States.PublishState(harness.StateReady, "")
	}
	return nil
}

// supervise restarts the child after an unexpected exit, with exponential
// backoff and jitter.
//
// A restart is not transparent: every session the previous child held loses its
// write lock and its in-memory agent state. The adapter therefore clears its
// session table and announces the transition, so clients can tell the operator
// that their conversation was interrupted rather than silently appearing to
// hang.
func (a *Adapter) supervise() {
	backoff := a.opts.RestartBackoff

	for {
		a.mu.RLock()
		c := a.conn
		a.mu.RUnlock()

		if c != nil {
			// Wait for the live connection to end, or for shutdown.
			select {
			case <-a.stopCh:
				return
			case <-c.Done():
			}
			select {
			case <-a.stopCh:
				return
			default:
			}

			cause := c.Err()
			a.logger.Error("harness exited", "error", errorString(cause), "backoff", backoff.String())
			a.dropSessions()
			a.setState(harness.StateRestarting, errorString(cause))
		}

		// Either the child just died or it never started; in both cases back off
		// before trying again so a persistently broken install does not spin.
		select {
		case <-a.stopCh:
			return
		case <-time.After(jitter(backoff)):
		}

		ctx, cancel := context.WithTimeout(context.Background(), a.opts.StartTimeout)
		err := a.spawn(ctx)
		cancel()
		if err != nil {
			a.logger.Error("harness restart failed", "error", err.Error())
			a.setState(harness.StateRestarting, err.Error())
			backoff = nextBackoff(backoff, a.opts.MaxRestartBackoff)
			continue
		}
		backoff = a.opts.RestartBackoff
	}
}

// nextBackoff doubles the delay, capped at max.
func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max || next <= 0 {
		return max
	}
	return next
}

// jitter spreads retries so that a crashing harness is not restarted in lockstep
// with anything else, and so logs stay readable. It returns a value in
// [d/2, d).
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	//nolint:gosec // jitter is not a security control; math/rand is the right tool
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// dropSessions forgets every attached session, because the child that held them
// is gone.
func (a *Adapter) dropSessions() {
	a.mu.Lock()
	a.sessions = map[string]harness.Session{}
	// The child that issued these calls is gone, so a remembered call can never
	// be correlated with anything again.
	a.toolCalls = map[string]harness.ToolCall{}
	a.toolCallIDs = nil
	a.mu.Unlock()
}

func (a *Adapter) setState(state harness.State, detail string) {
	a.mu.Lock()
	a.state = state
	a.mu.Unlock()
	if a.opts.States != nil {
		a.opts.States.PublishState(state, detail)
	}
}

// childEnv builds the child's environment.
//
// DSH reads DSH_HOME to find the session store and credentials, and
// DSH_PERMISSION_MODE to decide whether approvals are interactive. An explicit
// mode is always set: leaving it unset would let a stray value in the gateway's
// own environment silently disable approval prompts.
func (a *Adapter) childEnv() []string {
	env := os.Environ()
	env = append(env, "DSH_HOME="+a.opts.Home)

	mode := a.opts.SandboxMode
	if mode == "" {
		// The harness's own default. Chosen over leaving the variable unset so a
		// `danger-full-access` exported in the operator's shell cannot leak in and
		// silently disable every approval prompt.
		mode = "workspace-write"
	}
	env = append(env, "DSH_PERMISSION_MODE="+mode)
	env = append(env, a.opts.ExtraEnv...)
	return env
}

// currentConn returns the live connection, or ErrNotReady.
func (a *Adapter) currentConn() (*conn, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.state != harness.StateReady || a.conn == nil {
		return nil, harness.ErrNotReady
	}
	return a.conn, nil
}

// --- Harness implementation -------------------------------------------------

// ListSessions implements harness.Harness.
//
// DSH pages this call and reports only an id and a cwd per row. Titles and
// timestamps come from the session log projector, which the API layer merges in.
func (a *Adapter) ListSessions(ctx context.Context, workspace, cursor string) (harness.SessionPage, error) {
	c, err := a.currentConn()
	if err != nil {
		return harness.SessionPage{}, err
	}
	var result listSessionsResult
	if err := c.Call(ctx, "session/list", listSessionsParams{Cwd: workspace, Cursor: cursor}, &result); err != nil {
		return harness.SessionPage{}, err
	}
	page := harness.SessionPage{
		Sessions:   make([]harness.SessionInfo, 0, len(result.Sessions)),
		NextCursor: result.NextCursor,
	}
	for _, s := range result.Sessions {
		page.Sessions = append(page.Sessions, harness.SessionInfo{ID: s.SessionID, Workspace: s.Cwd})
	}
	return page, nil
}

// NewSession implements harness.Harness.
func (a *Adapter) NewSession(ctx context.Context, workspace string) (harness.Session, error) {
	c, err := a.currentConn()
	if err != nil {
		return harness.Session{}, err
	}
	if workspace == "" {
		return harness.Session{}, errx.New(errx.KindInvalid, "workspace_required",
			"a workspace is required to create a session")
	}

	var result sessionResult
	if err := c.Call(ctx, "session/new", newSessionParams{Cwd: workspace, McpServers: []any{}}, &result); err != nil {
		return harness.Session{}, err
	}
	return a.remember(harness.Session{
		Info:   harness.SessionInfo{ID: result.SessionID, Workspace: workspace},
		Config: convertOptions(result.ConfigOptions),
	}), nil
}

// ResumeSession implements harness.Harness.
//
// This is the operation DSH's single-writer lock governs. When the desktop has
// the session open, resume fails; the error is returned as a conflict so the
// phone can say "open on your desktop" instead of showing a generic fault.
func (a *Adapter) ResumeSession(ctx context.Context, sessionID, workspace string) (harness.Session, error) {
	c, err := a.currentConn()
	if err != nil {
		return harness.Session{}, err
	}
	if workspace == "" {
		return harness.Session{}, errx.New(errx.KindInvalid, "workspace_required",
			"a workspace is required to resume a session")
	}

	// Use a fresh context: cancellation here would leave the child holding a
	// write lock with no way for the gateway to know, so it is better to let the
	// call finish and report the real outcome.
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()

	var result sessionResult
	if err := c.Call(callCtx, "session/resume", resumeSessionParams{
		SessionID:  sessionID,
		Cwd:        workspace,
		McpServers: []any{},
	}, &result); err != nil {
		return harness.Session{}, errx.Wrap(err, errx.KindConflict, "session_unavailable",
			"the session could not be attached; it may already be open in another DeepSeek Harness process")
	}

	info := harness.SessionInfo{ID: sessionID, Workspace: workspace}
	return a.remember(harness.Session{Info: info, Config: convertOptions(result.ConfigOptions)}), nil
}

// CloseSession implements harness.Harness. It releases DSH's write lock, which is
// what lets the desktop open the session again.
// ReleaseSession detaches a session, which this adapter cannot do.
//
// DSH offers exactly one lever for a session an ACP client holds — `session/close`
// — and closing is not detaching: it writes a synthetic end to the session's own
// log. The first version of this method called it anyway, which meant a lease
// expiring while a phone was idle left a visible "this conversation is over"
// boundary behind it, every few minutes.
//
// So it refuses. The agent host never calls it (it keeps its handle, which is
// what a detach means there), and an implementation that cannot honour a
// contract should say so rather than approximate it: a caller seeing this error
// knows the session is still held, while a caller seeing `nil` would believe it
// had been let go.
func (a *Adapter) ReleaseSession(context.Context, string) error {
	return errx.New(errx.KindUnavailable, "release_unsupported",
		"this adapter holds its sessions for the life of the process and cannot "+
			"detach one; the agent host does not call this")
}

func (a *Adapter) CloseSession(ctx context.Context, sessionID string) error {
	c, err := a.currentConn()
	if err != nil {
		// The child is gone, so the lock is already released.
		a.forget(sessionID)
		return nil
	}

	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	callErr := c.Call(callCtx, "session/close", closeSessionParams{SessionID: sessionID}, nil)
	a.forget(sessionID)

	if callErr != nil {
		// Closing an already-closed session is not a failure worth surfacing.
		var e *errx.Error
		if errors.As(callErr, &e) && e.Kind == errx.KindInvalid {
			return nil
		}
		return callErr
	}
	return nil
}

// SetConfigOption implements harness.Harness.
func (a *Adapter) SetConfigOption(ctx context.Context, sessionID, optionID, valueID string) ([]harness.ConfigOption, error) {
	c, err := a.currentConn()
	if err != nil {
		return nil, err
	}
	var result setConfigOptionResult
	if err := c.Call(ctx, "session/set_config_option", setConfigOptionParams{
		SessionID: sessionID,
		ConfigID:  optionID,
		Value:     valueID,
	}, &result); err != nil {
		return nil, err
	}
	opts := convertOptions(result.ConfigOptions)
	a.updateConfig(sessionID, opts)
	return opts, nil
}

// Prompt implements harness.Harness.
//
// It blocks until the turn settles. Callers must pass a context that is *not*
// tied to an HTTP request: a phone that locks its screen must not cancel the
// agent mid-turn.
func (a *Adapter) Prompt(ctx context.Context, sessionID string, blocks []harness.PromptBlock) (string, error) {
	c, err := a.currentConn()
	if err != nil {
		return "", err
	}
	content, err := convertPrompt(blocks, a.Capabilities().CanPromptImages)
	if err != nil {
		return "", err
	}

	var result promptResult
	if err := c.Call(ctx, "session/prompt", promptParams{
		SessionID: sessionID,
		Prompt:    content,
	}, &result); err != nil {
		return "", err
	}
	return result.StopReason, nil
}

// Cancel implements harness.Harness. ACP models cancellation as a notification,
// so it returns as soon as the frame is queued; the turn settles asynchronously.
func (a *Adapter) Cancel(ctx context.Context, sessionID string) error {
	c, err := a.currentConn()
	if err != nil {
		return err
	}
	return c.Notify(ctx, "session/cancel", cancelParams{SessionID: sessionID})
}

// --- session bookkeeping ----------------------------------------------------

func (a *Adapter) remember(s harness.Session) harness.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[s.Info.ID] = s
	return s
}

func (a *Adapter) forget(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, sessionID)
}

// Attached reports whether the adapter currently holds a session's write lock.
func (a *Adapter) Attached(sessionID string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.sessions[sessionID]
	return ok
}

// maxRememberedToolCalls bounds the correlation store. A long-lived gateway sees
// thousands of tool calls; keeping only recent ones is what stops the map from
// growing without limit while still covering the window in which a permission
// request can arrive.
const maxRememberedToolCalls = 128

// rememberToolCall records a tool call for later correlation with a permission
// request.
func (a *Adapter) rememberToolCall(call harness.ToolCall) {
	if call.ID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if existing, ok := a.toolCalls[call.ID]; ok {
		// A completion update carries no title in DSH's implementation, so an
		// empty field must not erase what the opening update taught us.
		if call.Title == "" {
			call.Title = existing.Title
		}
		if call.Input == "" {
			call.Input = existing.Input
		}
	} else {
		a.toolCallIDs = append(a.toolCallIDs, call.ID)
	}
	a.toolCalls[call.ID] = call

	for len(a.toolCallIDs) > maxRememberedToolCalls {
		oldest := a.toolCallIDs[0]
		a.toolCallIDs = a.toolCallIDs[1:]
		delete(a.toolCalls, oldest)
	}
}

// lookupToolCall returns what is known about a tool call.
func (a *Adapter) lookupToolCall(callID string) (harness.ToolCall, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	c, ok := a.toolCalls[callID]
	return c, ok
}

func (a *Adapter) updateConfig(sessionID string, opts []harness.ConfigOption) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.sessions[sessionID]; ok {
		s.Config = opts
		a.sessions[sessionID] = s
	}
}

// --- server-initiated messages ----------------------------------------------

// handleNotification runs on the connection's reader goroutine and therefore must
// not block. Everything it does is a decode plus a non-blocking publish.
func (a *Adapter) handleNotification(method string, params json.RawMessage) {
	if method != "session/update" {
		a.logger.Debug("acp notification ignored", "method", method)
		return
	}

	var p sessionUpdateParams
	if err := json.Unmarshal(params, &p); err != nil {
		a.logger.Warn("acp session/update rejected", "error", err.Error())
		return
	}

	var envelope updateEnvelope
	if err := json.Unmarshal(p.Update, &envelope); err != nil {
		a.logger.Warn("acp update envelope rejected", "error", err.Error())
		return
	}

	now := time.Now()
	switch envelope.SessionUpdate {
	case "agent_message_chunk":
		var u chunkUpdate
		if json.Unmarshal(p.Update, &u) == nil {
			a.publish(harness.Update{
				SessionID: p.SessionID, Kind: harness.UpdateMessage, Time: now,
				// The harness assigns a durable id to every committed message and
				// repeats it here, which is what a client uses to avoid rendering
				// the same message twice when live events overlap fetched history.
				MessageID: u.MessageID,
				Text:      u.Content.Text,
			})
		}

	case "agent_thought_chunk":
		var u chunkUpdate
		if json.Unmarshal(p.Update, &u) == nil {
			a.publish(harness.Update{
				SessionID: p.SessionID, Kind: harness.UpdateThought, Time: now,
				MessageID: u.MessageID,
				Text:      u.Content.Text,
			})
		}

	case "tool_call":
		var u toolCallUpdate
		if json.Unmarshal(p.Update, &u) == nil {
			a.rememberToolCall(harness.ToolCall{
				ID:     u.ToolCallID,
				Title:  u.Title,
				Status: harness.ToolInProgress,
				Input:  rawToDisplay(u.RawInput),
			})
			a.publish(harness.Update{
				SessionID: p.SessionID, Kind: harness.UpdateTool, Time: now,
				Tool: &harness.ToolCall{
					ID:     u.ToolCallID,
					Title:  u.Title,
					Status: harness.ToolInProgress,
					Input:  rawToDisplay(u.RawInput),
				},
			})
		}

	case "tool_call_update":
		var u toolCallUpdate
		if json.Unmarshal(p.Update, &u) == nil {
			status := harness.ToolCompleted
			if u.Status == string(harness.ToolFailed) {
				status = harness.ToolFailed
			}
			// The completion update carries neither a title nor rawInput —
			// verified against DSH's own ACP projection, which sends the call id,
			// the status and the content blocks and nothing else. Publishing what
			// that decodes to would have the phone erase the arguments it
			// describes the call with, so the opening update's copy is carried
			// forward from the correlation store.
			input, title := rawToDisplay(u.RawInput), u.Title
			if known, ok := a.lookupToolCall(u.ToolCallID); ok {
				if input == "" {
					input = known.Input
				}
				if title == "" {
					title = known.Title
				}
			}
			output := u.text()
			a.rememberToolCall(harness.ToolCall{
				ID:     u.ToolCallID,
				Title:  title,
				Status: status,
				Input:  input,
			})
			a.publish(harness.Update{
				SessionID: p.SessionID, Kind: harness.UpdateTool, Time: now,
				Tool: &harness.ToolCall{
					ID:      u.ToolCallID,
					Title:   title,
					Status:  status,
					Input:   input,
					Output:  output,
					IsError: status == harness.ToolFailed,
					// What the result itself says: the exit status, the harness's
					// stop markers, and whether it truncated its own output.
					Result: toolresult.Observe(output),
				},
			})
		}

	case "usage_update":
		var u usageUpdate
		if json.Unmarshal(p.Update, &u) == nil {
			a.publish(harness.Update{
				SessionID: p.SessionID, Kind: harness.UpdateUsage, Time: now,
				// ACP reports context occupancy, not per-step token accounting.
				Usage: &harness.Usage{Used: u.Used, Size: u.Size},
			})
		}

	case "config_option_update":
		var u configOptionUpdate
		if json.Unmarshal(p.Update, &u) == nil {
			opts := convertOptions(u.ConfigOptions)
			a.updateConfig(p.SessionID, opts)
			a.publish(harness.Update{SessionID: p.SessionID, Kind: harness.UpdateConfig, Time: now, Config: opts})
		}

	case "available_commands_update", "current_mode_update", "plan":
		// Advertised by ACP but not emitted by DSH. Ignoring them explicitly
		// documents that the omission is deliberate.

	default:
		a.logger.Debug("acp update variant ignored", "variant", envelope.SessionUpdate)
	}
}

// handleRequest answers server-initiated requests. The connection runs it on its
// own goroutine precisely so that this may block for minutes.
func (a *Adapter) handleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if method != "session/request_permission" {
		return nil, errx.New(errx.KindUnavailable, "unsupported_request",
			"the gateway does not implement "+method)
	}

	var p requestPermissionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, errx.Wrap(err, errx.KindInvalid, "invalid_permission_request", "unparseable permission request")
	}

	// DSH sends only the call id here, so recover the tool name and arguments
	// from the update that announced the call. Prefer whatever the request did
	// carry, in case a future harness fills them in.
	known, _ := a.lookupToolCall(p.ToolCall.ToolCallID)
	tool := p.ToolCall.Title
	if tool == "" {
		tool = known.Title
	}
	input := rawToDisplay(p.ToolCall.RawInput)
	if input == "" {
		input = known.Input
	}
	if tool == "" && input == "" {
		// Better to say so plainly than to show an empty sheet the operator
		// cannot evaluate.
		a.logger.Warn("approval request has no correlated tool call; the prompt will "+
			"not describe what is being authorised", "call_id", p.ToolCall.ToolCallID)
	}

	now := time.Now()
	req := harness.PermissionRequest{
		ID:          "apr_" + p.ToolCall.ToolCallID,
		SessionID:   p.SessionID,
		ToolCallID:  p.ToolCall.ToolCallID,
		Tool:        tool,
		Input:       input,
		RequestedAt: now,
		ExpiresAt:   now.Add(a.opts.ApprovalTimeout),
	}
	for _, o := range p.Options {
		req.Options = append(req.Options, harness.PermissionOption{ID: o.OptionID, Name: o.Name, Kind: o.Kind})
	}

	decision, err := a.askHuman(ctx, req)
	if err != nil {
		// Fail closed. A human did not answer in time; the tool must not run. The
		// agent is told "reject" rather than "cancel" so it can adapt and explain
		// instead of the whole turn being torn down.
		a.logger.Warn("approval not answered; rejecting",
			"session", req.SessionID, "tool", req.Tool, "reason", err.Error())
		return rejectResult(req), nil
	}
	return permissionResult{Outcome: permissionOutcome{Outcome: "selected", OptionID: decision.OptionID}}, nil
}

// askHuman blocks for a decision, bounded by the approval timeout.
func (a *Adapter) askHuman(ctx context.Context, req harness.PermissionRequest) (harness.PermissionDecision, error) {
	if a.opts.Permissions == nil {
		return harness.PermissionDecision{}, errors.New("acp: no permission handler is configured")
	}
	ctx, cancel := context.WithTimeout(ctx, a.opts.ApprovalTimeout)
	defer cancel()
	return a.opts.Permissions.RequestPermission(ctx, req)
}

// rejectResult answers with the request's own reject option, falling back to the
// canonical id when the harness offered none.
func rejectResult(req harness.PermissionRequest) permissionResult {
	for _, o := range req.Options {
		if o.Kind == "reject_once" || o.ID == harness.OptionRejectOnce {
			return permissionResult{Outcome: permissionOutcome{Outcome: "selected", OptionID: o.ID}}
		}
	}
	return permissionResult{Outcome: permissionOutcome{Outcome: "cancelled"}}
}

func (a *Adapter) publish(u harness.Update) {
	a.opts.Updates.Publish(u)
}

// --- conversions ------------------------------------------------------------

func convertOptions(in []configOption) []harness.ConfigOption {
	if len(in) == 0 {
		return nil
	}
	out := make([]harness.ConfigOption, 0, len(in))
	for _, c := range in {
		out = append(out, harness.ConfigOption{
			ID:      c.ID,
			Name:    c.Name,
			Current: c.CurrentValue,
			Options: c.values(),
		})
	}
	return out
}

// convertPrompt maps gateway prompt blocks onto ACP content. An image block is
// rejected rather than silently dropped when the harness did not advertise image
// support: a user who attaches a screenshot must be told it will not be seen.
func convertPrompt(blocks []harness.PromptBlock, imagesAllowed bool) ([]promptContent, error) {
	if len(blocks) == 0 {
		return nil, errx.New(errx.KindInvalid, "empty_prompt", "a prompt must contain at least one block")
	}
	out := make([]promptContent, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "", "text":
			if b.Text == "" {
				continue
			}
			out = append(out, promptContent{Type: "text", Text: b.Text})
		case "image":
			if !imagesAllowed {
				return nil, errx.New(errx.KindInvalid, "images_unsupported",
					"this harness is not configured to accept image prompts")
			}
			if len(b.Data) == 0 {
				return nil, errx.New(errx.KindInvalid, "invalid_image", "an image block requires data")
			}
			out = append(out, promptContent{
				Type:     "image",
				MimeType: b.MIMEType,
				Data:     base64.StdEncoding.EncodeToString(b.Data),
			})
		default:
			return nil, errx.New(errx.KindInvalid, "unsupported_block",
				"unsupported prompt block type "+b.Type)
		}
	}
	if len(out) == 0 {
		return nil, errx.New(errx.KindInvalid, "empty_prompt", "a prompt must contain at least one non-empty block")
	}
	return out, nil
}

// rawToDisplay renders a raw JSON value for display.
//
// ACP sends rawInput as a JSON *object*, while the API contract exposes it as a
// string, so it is re-serialised compactly: the phone shows the operator the
// exact arguments that would run, unaltered in content and easy to read in a
// narrow column.
func rawToDisplay(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if !json.Valid(raw) {
		// Not JSON at all. Showing something odd beats showing nothing.
		return strings.TrimSpace(string(raw))
	}
	var buf bytes.Buffer
	// Compact preserves the original values and key order-independence while
	// removing the whitespace that would waste screen width.
	if err := json.Compact(&buf, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return buf.String()
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// LookPath resolves the dsh binary, so a configuration mistake surfaces at
// startup with a clear message instead of at the first prompt.
func LookPath(binary string) (string, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", fmt.Errorf("acp: %q is not on PATH: %w", binary, err)
	}
	return path, nil
}
