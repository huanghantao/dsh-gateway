package events

import "time"

// This file holds the payload schemas that travel on the bus.
//
// They live beside the event types because a type and its payload are one
// contract, and because the alternative — every publisher building its own
// `map[string]any` — is how a consumer ends up asserting a shape no producer
// emits. That is not hypothetical: the approval notifier read a map while the
// broker published a struct, so every approval notification silently lost the
// tool name and shared one tag with every other approval, and the test that
// covered it passed because it published the map the broker never did.
//
// A payload type here must stay free of any other package's types: the bus is a
// leaf, and every layer above it publishes onto it.

// TurnState is the payload of TypeTurnState.
//
// It is published by both the turn scheduler, for a turn this gateway is
// driving, and the session-log watcher, for one the desktop is driving. The two
// fill in different amounts of it — the watcher cannot know when a turn it did
// not start began — which is why every field beyond the id and the state is
// optional.
type TurnState struct {
	// TurnID identifies one turn within its session.
	TurnID string `json:"turnId"`
	// State is one of "queued", "running", "completed", "cancelled", "failed".
	State string `json:"state"`

	// Position is the 1-based place in the queue while State is "queued", and
	// omitted otherwise. It is republished whenever the queue moves, so a client
	// can show a follow-up creeping to the front without polling.
	Position int `json:"position,omitempty"`
	// QueueDepth is how many prompts wait behind this turn.
	QueueDepth int `json:"queueDepth,omitempty"`

	// QueuedAt is when the prompt was accepted, and StartedAt when the turn
	// began. StartedAt is what a client ticks an elapsed timer from, so it is
	// sent on every state including the settled ones: a phone that reconnects
	// mid-turn needs the start, not the duration so far.
	QueuedAt  *time.Time `json:"queuedAt,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`

	// StopReason is the harness's own verdict, for example "end_turn".
	StopReason string `json:"stopReason,omitempty"`
	// Detail explains a failure, and is empty otherwise.
	Detail string `json:"detail,omitempty"`
}

// HarnessState is the payload of TypeHarnessState.
type HarnessState struct {
	// State is one of "starting", "ready", "restarting", "failed", "stopped".
	State string `json:"state"`
	// Detail explains a non-ready state, and is empty otherwise.
	Detail string `json:"detail,omitempty"`
}

// SessionBusy is the payload of the `session.state` frame that answers whether a
// turn is running for a session.
//
// It is published by the turn scheduler, for a session this gateway drives, and
// by the log watcher, for one the desktop drives; the two never overlap, because
// the watcher is silent about a session the gateway owns. The frame exists
// because a client keeps its own copy of the answer — the composer reads it to
// decide whether Stop belongs on screen — so the producer that changes it has to
// say so. Without it, a phone that had already loaded a session sat on the
// `busy: true` it was born with: Stop stayed on screen after the turn it stopped
// had ended, and nothing but a reload could clear it.
type SessionBusy struct {
	Busy bool `json:"busy"`
}

// MessageData is the payload of TypeSessionMessage: one row of the conversation.
//
// Three producers fill it in, and they know different amounts: the ACP bridge
// sends the assistant's committed text as it arrives, the log watcher sends
// whole rows read back from a session the desk is driving, and the turn
// scheduler sends the echo of a prompt this gateway has just admitted. Fields
// beyond the role and the text are optional for that reason.
type MessageData struct {
	// ID is DSH's durable id once the session log has one. It is empty for a
	// prompt echo, which exists before the log records the prompt — so a client
	// that merges history by id must let the committed row replace the echo
	// rather than adding to it.
	ID string `json:"id,omitempty"`
	// Role is "user" or "assistant".
	Role string `json:"role"`
	// Text is the message body; a prompt that was only images has none.
	Text string `json:"text"`
	// Thinking is committed reasoning, which only the log path carries: the ACP
	// path sends it as its own `session.thought` frame.
	Thinking string `json:"thinking,omitempty"`
	// Model names the model that produced an assistant message.
	Model string `json:"model,omitempty"`
	// Usage is per-message token accounting, from the log alone.
	Usage *MessageUsage `json:"usage,omitempty"`
	// Attachments counts the non-text blocks the message carried, so that a
	// prompt which was only a screenshot does not render as an empty row.
	Attachments int `json:"attachments,omitempty"`
}

// MessageUsage is per-message token accounting on a committed message.
type MessageUsage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	TotalTokens  int `json:"totalTokens"`
}

// ApprovalDecision is the payload of TypeApprovalResolved.
//
// DecidedBy is the field that matters most to a reader: "operator" means a
// person answered, while "timeout" and "shutdown" mean the tool was refused
// because nobody did. A client that renders those two the same way is telling
// the operator their agent stopped for a reason it did not.
type ApprovalDecision struct {
	ID        string `json:"id"`
	OptionID  string `json:"optionId"`
	DecidedBy string `json:"decidedBy"`
	// Tool names what was decided, so a client can say what was refused without
	// looking the request up — which it cannot do once the request is gone.
	Tool string `json:"tool,omitempty"`
	// SessionID is redundant with the envelope's, and carried anyway because a
	// notification is composed from the payload alone.
	SessionID string `json:"sessionId,omitempty"`
	// GrantID is set when the decision came from a standing grant rather than
	// from a person answering this prompt.
	GrantID string `json:"grantId,omitempty"`
}
