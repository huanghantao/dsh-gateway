// Package hostwire defines the protocol between the gateway and the agent host.
//
// Why there are two processes at all: DeepSeek Harness binds the lifetime of an
// ACP child to its client's stdin (`exitOnStdinEnd`), so the process holding the
// other end of that pipe *is* the lifetime of the turn it is running. A gateway
// that a deploy replaces cannot be that process. So the process that runs the
// agent is separate, long-lived, and does not participate in a redeploy; the
// gateway becomes a client of it, and a redeploy becomes a reconnect.
//
// The wire shape is line-delimited JSON, one message per line, in both
// directions:
//
//	gateway -> host   {"id":"3","method":"session.prompt","params":{…}}
//	host -> gateway   {"id":"3","result":{…}}   or   {"id":"3","error":{…}}
//	host -> gateway   {"event":"update","data":{…}}        (no id: a notification)
//
// It is deliberately the same shape as the ACP frames this system already
// speaks, for the same reason: newline-delimited JSON-RPC is a published
// pattern, debuggable with `nc`, and needs no schema compiler to add a method.
//
// Two design rules are load-bearing and are worth stating before the types:
//
//   - **A turn's life is not a call's life.** `turn.submit` returns as soon as
//     the host has accepted the prompt, and the outcome arrives later as a
//     `turn.result` event. If the outcome were the response to the call, a
//     gateway that died mid-turn would take the turn with it — which is the
//     whole problem this protocol exists to solve. A restarted gateway asks for
//     a snapshot, finds the turn still running, and awaits the same result.
//   - **Every command carries the caller's epoch**, and the host refuses one
//     that is older than the connection it arrived on. Mutual exclusion is not
//     a guarantee; the resource refusing the stale writer is.
package hostwire

import (
	"encoding/json"
	"time"
)

// Protocol is the version this package speaks. It is asserted, not negotiated:
// both ends are built from the same repository, and a mismatch is a deployment
// mistake that should fail loudly at connect time rather than be papered over.
const Protocol = 1

// Method and notification names. They are the wire vocabulary; the constants
// exist so a typo is a compile error rather than a timeout.
//
// Both kinds live in this one block because the frame tells them apart by slot,
// not by name: `method` carries a request (gateway to host) and `event` carries
// a notification (host to gateway). Each constant below says which it is.
const (
	// MethodHello identifies the caller and its epoch. It must be the first
	// command on a connection.
	MethodHello = "host.hello"
	// MethodStatus reports liveness and what the host is holding. It is what a
	// deploy script asks instead of guessing.
	MethodStatus = "host.status"
	// MethodSnapshot returns the state a fresh gateway needs: harness state,
	// sessions held, turns running, approvals pending.
	MethodSnapshot = "host.snapshot"

	// MethodSessionList enumerates the sessions in one workspace.
	MethodSessionList = "session.list"
	// MethodSessionNew creates a session in a workspace.
	MethodSessionNew = "session.new"
	// MethodSessionResume attaches an existing session, taking DSH's
	// single-writer lock.
	MethodSessionResume = "session.resume"
	// MethodSessionClose ends a session for real, which is what makes DSH write
	// an end to the session's own log.
	MethodSessionClose = "session.close"
	// MethodSessionSetConfig sets one of the harness's configuration options.
	MethodSessionSetConfig = "session.set_config_option"
	// MethodSessionCancel asks the harness to stop the turn in flight.
	MethodSessionCancel = "session.cancel"
	// MethodSessionRelease detaches a session without ending it. It is what a
	// lease expiring means, and it deliberately does not call DSH's
	// session/close, which writes an end to the session's own log.
	MethodSessionRelease = "session.release"
	// MethodTurnSubmit admits a prompt and answers with the turn id.
	MethodTurnSubmit = "turn.submit"
	// MethodTurnAwait waits for a turn to settle.
	MethodTurnAwait = "turn.await"
	// MethodPermissionDecide answers a permission request the host relayed.
	MethodPermissionDecide = "permission.decide"
	// MethodCapabilities answers the same reply as MethodHello. Nothing in this
	// repository calls it.
	MethodCapabilities = "host.capabilities"
	// MethodDrain asks the host to stop accepting work and wait for the turns in
	// flight.
	MethodDrain = "host.drain"

	// MethodEvent reports that a notification could not be handled. The
	// transport sends it back with the handler's error, because a notification
	// has no reply; the host logs it.
	MethodEvent = "event"
	// MethodUpdate is a notification carrying one harness update.
	MethodUpdate = "update"
	// MethodState is a notification carrying the harness's state.
	MethodState = "state"
	// MethodTurnResult is a notification carrying a settled turn's outcome.
	MethodTurnResult = "turn.result"
	// MethodPermissionRequest is a notification asking the gateway to put a
	// permission prompt in front of a person.
	MethodPermissionRequest = "permission.request"
	// MethodPermissionResolved is declared for the mirror direction; nothing in
	// this repository sends it.
	MethodPermissionResolved = "permission.resolved"
	// MethodDrainingNotification tells a client the host has begun draining.
	MethodDrainingNotification = "host.draining"
)

// Frame is one line of the protocol, in either direction.
//
// It is a single envelope for requests, responses and notifications rather than
// three shapes, because the discriminator is already unambiguous: an `id` and a
// `method` is a request, an `id` and no `method` is a response, and no `id` is a
// notification. One envelope means one decoder, one encoder, and one place to
// add a field.
type Frame struct {
	// ID correlates a request with its response. Empty on a notification.
	ID string `json:"id,omitempty"`
	// Method names the command. Empty on a response.
	Method string `json:"method,omitempty"`
	// Params is the command's argument, decoded per method.
	Params json.RawMessage `json:"params,omitempty"`
	// Result is a successful response's body.
	Result json.RawMessage `json:"result,omitempty"`
	// Error is a failed response. Its presence is what makes the response a
	// failure; a response carries exactly one of Result and Error.
	Error *Error `json:"error,omitempty"`
	// Event names a notification. Empty on everything else.
	Event string `json:"event,omitempty"`
	// Data is a notification's body.
	Data json.RawMessage `json:"data,omitempty"`
}

// Error is a failure crossing the process boundary.
//
// It carries a code and a kind rather than a Go error, because an error value
// cannot cross a socket and a string can only be matched by luck. The kinds
// mirror errx.Kind deliberately and are translated on arrival, so the HTTP layer
// above keeps answering with the same status codes it did when the harness was
// in-process; a client cannot tell where the seam is, which is the point.
type Error struct {
	// Kind is one of Kind*, mirroring errx.Kind.
	Kind string `json:"kind"`
	// Code is the stable, machine-readable code the contract names, for example
	// "session_not_attached" or "prompt_in_flight".
	Code string `json:"code"`
	// Message is for a human reading a log or a problem document.
	Message string `json:"message"`
	// Retryable is the sender's own verdict on whether the same call could
	// succeed later.
	Retryable bool `json:"retryable,omitempty"`
}

// Error kinds, mirroring internal/errx. They are duplicated as strings rather
// than imported because this package is the wire contract: a decoder that
// depended on the application's error package would make every future kind a
// protocol change.
const (
	KindInternal        = "internal"
	KindInvalid         = "invalid"
	KindNotFound        = "not_found"
	KindConflict        = "conflict"
	KindUnauthenticated = "unauthenticated"
	KindForbidden       = "forbidden"
	KindRateLimited     = "rate_limited"
	KindUnavailable     = "unavailable"
	KindTimeout         = "timeout"
)

// Stable codes the host uses for conditions the gateway acts on.
const (
	// CodeSessionNotAttached means the session is not held by this host, so the
	// caller must resume it first.
	CodeSessionNotAttached = "session_not_attached"
	// CodeSessionBusy means a prompt is already in flight for the session.
	CodeSessionBusy = "prompt_in_flight"
	// CodeHarnessNotReady means the child has not completed its handshake.
	CodeHarnessNotReady = "harness_not_ready"
	// CodeStaleEpoch means the command arrived on a connection that has been
	// superseded.
	CodeStaleEpoch = "stale_epoch"
	// CodeProtocol means the two ends disagree about the wire version.
	CodeProtocol = "protocol_mismatch"
	// CodeApprovalClosed means a decision arrived for a request that is no longer
	// pending — decided elsewhere, expired, or cancelled with the turn.
	CodeApprovalClosed = "approval_closed"
)

/* ------------------------------------------------------------ command bodies */

// Hello greets the host. It is the first command on a connection, and it is
// deliberately not an act of ownership.
type Hello struct {
	Protocol int `json:"protocol"`
	// Caller names the client for logs, for example "gateway" or "status".
	Caller string `json:"caller"`
	// InstanceID identifies this gateway process. It is what an operator greps
	// for when two gateways are racing to own the host.
	InstanceID string `json:"instanceId"`
	// Epoch is the caller's fencing token. A later connection always carries a
	// higher one; the host refuses commands carrying a lower one than the
	// connection they arrive on.
	Epoch uint64 `json:"epoch"`
	// ClaimsControl asks to become the connection the host will take commands
	// from, displacing whoever held that role.
	//
	// It is a field rather than an implication of connecting because the two are
	// genuinely different intentions, and conflating them was a defect: an
	// operator running `status`, or a deploy script asking whether a restart is
	// safe, would say hello — and take the role from the live gateway, which
	// then lost its connection and had to reconnect. A read-only observer must
	// be able to read without seizing anything.
	ClaimsControl bool `json:"claimsControl,omitempty"`
}

// HelloResult confirms the connection and names the host.
type HelloResult struct {
	Protocol int `json:"protocol"`
	// HostEpoch is minted per host process. A caller that sees it change knows
	// every turn it was awaiting is gone.
	HostEpoch string `json:"hostEpoch"`
	// HostPID is for an operator with `ps` open.
	HostPID int `json:"hostPid"`
	// Capabilities is what the harness advertised at its own handshake.
	Capabilities Capabilities `json:"capabilities"`
	// AcceptsCommands is set on the hello reply. Nothing in this repository reads
	// it: the host decides whether a connection may act by pointer identity, not
	// by this flag, so a caller that never claimed control is refused on an
	// acting command rather than told in advance.
	AcceptsCommands bool `json:"acceptsCommands"`
}

// Capabilities mirrors harness.Capabilities.
type Capabilities struct {
	ProtocolVersion int    `json:"protocolVersion"`
	AgentName       string `json:"agentName"`
	AgentVersion    string `json:"agentVersion"`
	CanList         bool   `json:"canList"`
	CanResume       bool   `json:"canResume"`
	CanClose        bool   `json:"canClose"`
	CanPromptImages bool   `json:"canPromptImages"`
}

// SessionInfo mirrors harness.SessionInfo.
type SessionInfo struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Workspace string    `json:"workspace"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Session mirrors harness.Session.
type Session struct {
	Info   SessionInfo    `json:"info"`
	Config []ConfigOption `json:"config,omitempty"`
}

// ConfigOption mirrors harness.ConfigOption.
type ConfigOption struct {
	ID      string              `json:"id"`
	Name    string              `json:"name"`
	Current string              `json:"current,omitempty"`
	Options []ConfigOptionValue `json:"options,omitempty"`
}

// ConfigOptionValue mirrors harness.ConfigOptionValue.
type ConfigOptionValue struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Group       string `json:"group,omitempty"`
}

// SessionPage mirrors harness.SessionPage.
type SessionPage struct {
	Sessions   []SessionInfo `json:"sessions,omitempty"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

// ListSessionsParams asks for a page of sessions.
type ListSessionsParams struct {
	Workspace string `json:"workspace,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
}

// NewSessionParams creates a session in a workspace.
type NewSessionParams struct {
	Workspace string `json:"workspace"`
}

// ResumeSessionParams attaches a session, taking DSH's single-writer lock.
type ResumeSessionParams struct {
	SessionID string `json:"sessionId"`
	Workspace string `json:"workspace"`
}

// SessionRef names one attached session.
type SessionRef struct {
	SessionID string `json:"sessionId"`
}

// SetConfigParams changes one option for later turns.
type SetConfigParams struct {
	SessionID string `json:"sessionId"`
	OptionID  string `json:"optionId"`
	ValueID   string `json:"valueId"`
}

// SetConfigResult reports the session's options after the change.
type SetConfigResult struct {
	Config []ConfigOption `json:"config,omitempty"`
}

// PromptBlock mirrors harness.PromptBlock. Data is base64 on the wire, which
// encoding/json does for a []byte without help.
type PromptBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	Data     []byte `json:"data,omitempty"`
}

// TurnSubmitParams asks the host to run one prompt.
type TurnSubmitParams struct {
	SessionID string        `json:"sessionId"`
	Blocks    []PromptBlock `json:"blocks"`
	// TimeoutMS bounds the turn. The host enforces it, not the caller: a turn
	// must not be cancelled because the process that asked for it went away.
	TimeoutMS int64 `json:"timeoutMs,omitempty"`
}

// TurnSubmitResult acknowledges acceptance. The outcome is not here — see the
// package comment.
type TurnSubmitResult struct {
	TurnID string `json:"turnId"`
	// StartedAt is when the host began the turn.
	StartedAt time.Time `json:"startedAt"`
}

// TurnAwaitParams asks for the outcome of a turn.
//
// It is how a gateway that restarted mid-turn rejoins one: the host already has
// the result, so this returns immediately if the turn has settled and blocks
// until it does if it has not.
type TurnAwaitParams struct {
	TurnID string `json:"turnId"`
}

// TurnResult is one turn's outcome.
type TurnResult struct {
	TurnID    string `json:"turnId"`
	SessionID string `json:"sessionId"`
	// StopReason is the harness's own verdict, for example "end_turn".
	StopReason string `json:"stopReason,omitempty"`
	// Err is set when the turn failed. A cancelled turn reports no error: the
	// harness says it stopped, and "cancelled" is a stop reason rather than a
	// fault.
	Err *Error `json:"err,omitempty"`
	// Cancelled reports that a human stopped it, which is what makes an
	// otherwise-ordinary stop report as cancelled rather than completed.
	Cancelled bool `json:"cancelled,omitempty"`
	// SettledAt is when it ended.
	SettledAt time.Time `json:"settledAt"`
}

// PermissionDecisionParams answers a pending request.
type PermissionDecisionParams struct {
	SessionID string `json:"sessionId"`
	RequestID string `json:"requestId"`
	OptionID  string `json:"optionId"`
}

// PermissionRequest is one tool call waiting on a person.
type PermissionRequest struct {
	ID          string             `json:"id"`
	SessionID   string             `json:"sessionId"`
	ToolCallID  string             `json:"toolCallId"`
	Tool        string             `json:"tool"`
	Input       string             `json:"input"`
	Options     []PermissionOption `json:"options"`
	RequestedAt time.Time          `json:"requestedAt"`
	ExpiresAt   time.Time          `json:"expiresAt"`
}

// PermissionOption is one choice on a request.
type PermissionOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

// Update is one piece of agent activity, mirroring harness.Update.
//
// The fields are flattened exactly as the in-process type is, and for the same
// reason: the set of kinds is closed, the payloads are small, and a flat shape
// keeps the fan-out allocation-free.
type Update struct {
	SessionID string    `json:"sessionId"`
	Kind      string    `json:"kind"`
	Time      time.Time `json:"time"`

	Text      string `json:"text,omitempty"`
	MessageID string `json:"messageId,omitempty"`
	Model     string `json:"model,omitempty"`

	Tool   *ToolCall      `json:"tool,omitempty"`
	Usage  *Usage         `json:"usage,omitempty"`
	Config []ConfigOption `json:"config,omitempty"`
}

// Update kinds, mirroring harness.UpdateKind.
const (
	UpdateMessage = "message"
	UpdateThought = "thought"
	UpdateTool    = "tool"
	UpdateUsage   = "usage"
	UpdateConfig  = "config"
)

// ToolCall mirrors harness.ToolCall.
type ToolCall struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Input   string `json:"input,omitempty"`
	Output  string `json:"output,omitempty"`
	IsError bool   `json:"isError,omitempty"`
	// Result carries the settled result's own verdict. It is an opaque object on
	// the wire so that a change to the outcome vocabulary is a change in one
	// place rather than in three.
	Result json.RawMessage `json:"result,omitempty"`
}

// Usage mirrors harness.Usage.
type Usage struct {
	Used int `json:"used"`
	Size int `json:"size,omitempty"`
}

// State is the harness child's lifecycle state.
type State string

// States, mirroring harness.State.
const (
	StateStarting   State = "starting"
	StateReady      State = "ready"
	StateRestarting State = "restarting"
	StateFailed     State = "failed"
	StateStopped    State = "stopped"
)

// StateEvent is the body of a `state` notification.
type StateEvent struct {
	State  State  `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// DrainingEvent is the body of a `host.draining` notification: the host is
// going away on purpose, which is the one restart that can end a turn.
type DrainingEvent struct {
	Reason string `json:"reason"`
	// TurnsRunning is how many turns the host is about to end. It is reported so
	// the operator, and the deploy script, can decide whether to wait.
	TurnsRunning int `json:"turnsRunning"`
}

// SessionHeld is one session the host is holding, inside a snapshot.
type SessionHeld struct {
	Info SessionInfo `json:"info"`
	// Turn is the turn running in it, if any. This is the field that makes a
	// gateway restart survivable: a fresh gateway learns that work is in flight
	// and rejoins it rather than reporting an idle session.
	Turn *TurnResult `json:"turn,omitempty"`
	// StartedAt is when that turn began, so a client can render an elapsed timer
	// from the start rather than from the reconnect.
	StartedAt *time.Time `json:"startedAt,omitempty"`
	// PendingPermissions are the decisions this session is waiting on.
	PendingPermissions []PermissionRequest `json:"pendingPermissions,omitempty"`
}

// SnapshotResult is the answer to MethodSnapshot.
type SnapshotResult struct {
	HostEpoch    string        `json:"hostEpoch"`
	HostPID      int           `json:"hostPid"`
	Capabilities Capabilities  `json:"capabilities"`
	State        StateEvent    `json:"state"`
	Sessions     []SessionHeld `json:"sessions,omitempty"`
	TakenAt      time.Time     `json:"takenAt"`
}

// StatusResult is the answer to MethodStatus: liveness an operator or a deploy
// script can read without owning the connection.
type StatusResult struct {
	HostEpoch    string    `json:"hostEpoch"`
	HostPID      int       `json:"hostPid"`
	StartedAt    time.Time `json:"startedAt"`
	Version      string    `json:"version,omitempty"`
	State        State     `json:"state"`
	Detail       string    `json:"detail,omitempty"`
	SessionsHeld int       `json:"sessionsHeld"`
	TurnsRunning int       `json:"turnsRunning"`
	// Draining reports whether the host is on its way out, so a redeploy can
	// tell "already going" from "needs asking".
	Draining bool `json:"draining"`
}

// DrainParams asks the host to stop accepting turns and report what is left.
type DrainParams struct {
	Reason string `json:"reason,omitempty"`
	// Force ends running turns immediately instead of waiting for them. It is
	// the operator's escape hatch for a wedged host, and it is deliberately
	// spelled out rather than implied by a timeout.
	Force bool `json:"force,omitempty"`
}
