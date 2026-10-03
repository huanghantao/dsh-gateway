// Package harness defines the gateway's driven port for talking to DeepSeek
// Harness.
//
// Everything above this package — the HTTP API, the session lease manager, the
// event broker — is written against these types and knows nothing about ACP,
// stdio, or JSON-RPC. Swapping the ACP adapter for an in-process bridge later is
// therefore a change in one place, not a rewrite.
//
// Two design choices are worth calling out:
//
//   - Updates and permission prompts arrive as callbacks, not channels. A channel
//     would force the adapter to decide what to do when its buffer fills, and the
//     only correct answer — never block the agent's reader — is a policy decision
//     that belongs to the broker above. Callbacks make that explicit.
//   - Permission prompts block. DSH is waiting on a human, so the adapter's call
//     must not return until a decision exists or the context expires. Fail-closed
//     is the caller's job, and the contract says so.
package harness

import (
	"context"
	"errors"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/toolresult"
)

// State is the lifecycle state of the DSH child process.
type State string

const (
	// StateStarting means the process is launching or completing its handshake.
	StateStarting State = "starting"
	// StateReady means the ACP handshake completed and the process accepts work.
	StateReady State = "ready"
	// StateRestarting means the process died and the supervisor is backing off.
	StateRestarting State = "restarting"
	// StateFailed means the supervisor gave up or was never able to start.
	StateFailed State = "failed"
	// StateStopped means shutdown was requested.
	StateStopped State = "stopped"
)

// Capabilities describes what the connected DSH advertised during initialize.
type Capabilities struct {
	// ProtocolVersion is the ACP protocol version DSH reported.
	ProtocolVersion int
	// AgentName and AgentVersion identify the harness build. Both are advisory:
	// DSH reports a constant version, so they are for logs, not for gating.
	AgentName    string
	AgentVersion string
	// CanList, CanResume, and CanClose mirror ACP's sessionCapabilities.
	CanList   bool
	CanResume bool
	CanClose  bool
	// CanPromptImages reports whether image prompt blocks are accepted.
	CanPromptImages bool
}

// SessionPage is one page of persisted sessions.
type SessionPage struct {
	Sessions []SessionInfo
	// NextCursor is empty when this is the last page, and opaque otherwise: the
	// gateway never interprets it, it only passes it back.
	NextCursor string
}

// SessionInfo is the durable identity and metadata of one DSH session.
type SessionInfo struct {
	ID string
	// Title is DSH's deterministic fallback title; it may be empty.
	//
	// The ACP session listing carries no title at all, so in practice this is
	// populated by the session-log projector rather than by the harness.
	Title string
	// Workspace is the absolute cwd the session is bound to.
	Workspace string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ConfigOption is one knob DSH advertises for a session, such as model or
// reasoning effort. The gateway does not interpret the ids; it relays them.
type ConfigOption struct {
	ID      string
	Name    string
	Current string
	Options []ConfigOptionValue
}

// ConfigOptionValue is one selectable value of a ConfigOption.
type ConfigOptionValue struct {
	ID          string
	Name        string
	Description string
	// Group is the provider or category the value belongs to, when the harness
	// nests its options. DSH groups the model list by provider.
	Group string
}

// Label renders the value for a picker: "Group · Name" when grouped, else Name,
// falling back to the raw id.
func (v ConfigOptionValue) Label() string {
	name := v.Name
	if name == "" {
		name = v.ID
	}
	if v.Group != "" {
		return v.Group + " · " + name
	}
	return name
}

// Session is an attached session and its current configuration.
type Session struct {
	Info   SessionInfo
	Config []ConfigOption
	// Modes is reserved: DSH does not implement ACP session modes today, so this
	// is always empty. It exists so that adding them is not a breaking change.
	Modes []ConfigOption
}

// Option returns the configuration option with the given id.
func (s Session) Option(id string) (ConfigOption, bool) {
	for _, c := range s.Config {
		if c.ID == id {
			return c, true
		}
	}
	return ConfigOption{}, false
}

// PromptBlock is one element of a prompt.
type PromptBlock struct {
	// Type is "text" today. Image support depends on DSH's attachment store
	// being configured and the selected model accepting images.
	Type string
	Text string
	// MIMEType and Data are set for image blocks.
	MIMEType string
	Data     []byte
}

// UpdateKind discriminates Update payloads.
type UpdateKind string

const (
	// UpdateMessage is a committed assistant message.
	UpdateMessage UpdateKind = "message"
	// UpdateThought is a committed reasoning block.
	UpdateThought UpdateKind = "thought"
	// UpdateTool is a tool-call lifecycle change.
	UpdateTool UpdateKind = "tool"
	// UpdateUsage is token accounting for a completed step.
	UpdateUsage UpdateKind = "usage"
	// UpdateConfig reports that a session's configuration changed.
	UpdateConfig UpdateKind = "config"
)

// ToolStatus is a tool call's lifecycle position.
type ToolStatus string

const (
	// ToolInProgress means the call has been issued and not yet settled.
	ToolInProgress ToolStatus = "in_progress"
	// ToolCompleted means the call returned successfully.
	ToolCompleted ToolStatus = "completed"
	// ToolFailed means the call returned an error.
	ToolFailed ToolStatus = "failed"
)

// ToolCall is a tool invocation observed on the update stream.
type ToolCall struct {
	// ID correlates the start and end updates, and the permission prompt.
	ID string
	// Title is the tool name, for example "bash".
	Title string
	// Status is the lifecycle position.
	Status ToolStatus
	// Input is the raw JSON arguments exactly as DSH reported them. The gateway
	// deliberately does not parse it: the phone shows what will actually run.
	Input string
	// Output is the tool's textual result, when settled.
	Output string
	// IsError reports whether the tool *failed* — a spawn error, an abort. It is
	// deliberately not set for a command that merely exited non-zero: DSH
	// reports those, and Result carries what it reported.
	IsError bool
	// Result is what the settled result says about how the call ended. The zero
	// value means the result said nothing, which is not the same as success.
	Result toolresult.Outcome
}

// Usage is context occupancy for a session, as ACP reports it.
//
// ACP's usage update carries how much of the model's context window is currently
// occupied — not per-step input/output token counts. Per-message token accounting
// is available from the session log projector, which reads the harness's own
// durable records.
type Usage struct {
	// Used is how many tokens of the context window are occupied.
	Used int
	// Size is the context window size. Zero when the harness did not report one.
	Size int
}

// Fraction returns the occupied proportion in [0,1], or 0 when Size is unknown.
func (u Usage) Fraction() float64 {
	if u.Size <= 0 {
		return 0
	}
	f := float64(u.Used) / float64(u.Size)
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// Update is one event from the agent.
//
// It is a flat struct with a discriminating Kind rather than an interface: the
// set of kinds is closed, the payloads are small, and a flat shape keeps the
// broker's fan-out allocation-free and trivially serialisable.
type Update struct {
	// SessionID identifies the session the update belongs to.
	SessionID string
	// Kind selects which of the payload fields below is meaningful.
	Kind UpdateKind
	// Time is when the gateway observed the update.
	Time time.Time

	// Kind == UpdateMessage or UpdateThought.
	Text string
	// MessageID is the harness's durable identifier for a committed message. It
	// matches the id the transcript projection reports for the same message, which
	// is what lets a client folding live events into fetched history dedupe
	// exactly rather than by comparing text.
	MessageID string
	// Model is the model that produced a message, when known.
	Model string

	// Kind == UpdateTool.
	Tool *ToolCall

	// Kind == UpdateUsage.
	Usage *Usage

	// Kind == UpdateConfig.
	Config []ConfigOption
}

// PermissionOption is one choice offered on an approval prompt.
type PermissionOption struct {
	ID   string
	Name string
	// Kind is the ACP option kind, for example "allow_once" or "reject_once".
	Kind string
}

// PermissionRequest is DSH asking a human to authorise a tool call.
type PermissionRequest struct {
	// ID is the gateway-generated identifier used to answer the request.
	ID string
	// SessionID and ToolCallID say what is being authorised.
	SessionID  string
	ToolCallID string
	// Tool is the tool name and Input its raw arguments, so the phone can show
	// exactly what would run.
	Tool  string
	Input string
	// Options is the set of choices. DSH offers allow-once and reject-once.
	Options []PermissionOption
	// RequestedAt and ExpiresAt bound the wait.
	RequestedAt time.Time
	ExpiresAt   time.Time
}

// PermissionDecision is a human's answer.
type PermissionDecision struct {
	// OptionID must be one of the ids from the request's Options.
	OptionID string
	// DecidedBy records which device answered, for the audit log.
	DecidedBy string
}

// PermissionOutcome values map onto ACP's outcome shapes.
const (
	// OptionAllowOnce authorises this single invocation.
	OptionAllowOnce = "allow-once"
	// OptionRejectOnce refuses this single invocation.
	OptionRejectOnce = "reject-once"
)

// UpdateSink receives agent updates. Implementations must not block: the call
// happens on the adapter's reader goroutine, and blocking it would stall the
// agent, fill the pipe, and eventually wedge DSH itself.
type UpdateSink interface {
	Publish(update Update)
}

// PermissionHandler answers approval prompts.
//
// RequestPermission blocks until a decision is available. Returning an error —
// including a context deadline — means the request is **rejected**; there is no
// path that approves by default.
type PermissionHandler interface {
	RequestPermission(ctx context.Context, req PermissionRequest) (PermissionDecision, error)
}

// StateSink receives child-process lifecycle transitions.
type StateSink interface {
	PublishState(state State, detail string)
}

// Harness drives one DSH process.
//
// Implementations must be safe for concurrent use. Methods that address a
// specific session return errx.KindNotFound when the session is not attached, and
// errx.KindConflict when the operation contradicts current state — for example a
// second prompt while one is in flight.
type Harness interface {
	// Start launches the harness and begins supervising it.
	//
	// A transient launch failure is not returned: the implementation retries with
	// backoff, and readiness is reported through State. Returning an error here
	// means the configuration is unusable, not that the harness is briefly down.
	Start(ctx context.Context) error
	// Capabilities returns what the connected harness advertised during its
	// handshake. It reports zero values until the harness is ready.
	Capabilities() Capabilities
	// Close shuts the harness down, waiting for it to exit or for ctx to expire.
	Close(ctx context.Context) error
	// State reports the current lifecycle state.
	State() State

	// ListSessions returns persisted resumable sessions, newest first, one page
	// at a time. An empty workspace lists every workspace; an empty cursor starts
	// at the newest.
	ListSessions(ctx context.Context, workspace, cursor string) (SessionPage, error)
	// NewSession creates a session bound to workspace.
	NewSession(ctx context.Context, workspace string) (Session, error)
	// ResumeSession attaches an existing session. It fails when another process
	// holds DSH's write lock, which is the normal case while the desktop GUI has
	// the session open.
	ResumeSession(ctx context.Context, sessionID, workspace string) (Session, error)
	// ReleaseSession detaches a session without ending it: the writer's lock is
	// given up and the session is left exactly as it is, resumable later.
	//
	// It is separate from CloseSession because the two are called for opposite
	// reasons, and conflating them was a real bug. "Nobody is watching this any
	// more" — a lease that expired while a phone was idle — must not mark a
	// conversation finished; only "this session is being deleted" should. An
	// implementation with one method has to choose which of the two to be wrong
	// about, and either choice is visible to the operator.
	ReleaseSession(ctx context.Context, sessionID string) error
	// CloseSession ends a session, durably. DSH records a synthetic end in the
	// session's own log, which is what makes this the deletion path rather than
	// the detach path.
	CloseSession(ctx context.Context, sessionID string) error

	// SetConfigOption changes model or reasoning effort.
	SetConfigOption(ctx context.Context, sessionID, optionID, valueID string) ([]ConfigOption, error)
	// Prompt submits a prompt and returns when the turn settles. The returned
	// stop reason is ACP's, for example "end_turn" or "cancelled".
	Prompt(ctx context.Context, sessionID string, blocks []PromptBlock) (stopReason string, err error)
	// Cancel interrupts the in-flight turn. Cancel is best-effort and returns
	// immediately; the turn settles asynchronously.
	Cancel(ctx context.Context, sessionID string) error
}

// Errors returned by Harness implementations.
var (
	// ErrNotReady means the child is not in StateReady.
	ErrNotReady = errors.New("harness: not ready")
	// ErrSessionNotAttached means the session is not leased in this process.
	ErrSessionNotAttached = errors.New("harness: session is not attached")
)
