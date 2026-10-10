package push

import (
	"strings"
	"unicode/utf8"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
)

// The activity model: who did something, and what came of it.
//
// A notification used to be an outcome and nothing else — every turn ended with
// "The agent finished", whichever turn it was and whatever it had done. That is
// one sentence too few for a reader who is not looking at the app, because the
// two questions a lock screen has to answer are "which of my agents is this?"
// and "what actually happened?". Neither is answerable from the outcome alone.
//
// So a notification is stated as three facts, in the order they matter:
//
//	actor    the main agent, or the gateway itself
//	outcome  completed / failed / cancelled
//	summary  what the work amounted to ("12 tool calls · 3 files changed")
//
// A delegated child agent is deliberately absent from that vocabulary. It has an
// actor of its own in the app — a settlement is a row in the conversation, and a
// reader scrolling history has to know which agent it belongs to — but it is not
// something a notification is ever about: a child settles inside its parent's
// turn, and the parent is what the reader is waiting for. The delegation still
// shows up here, counted in the summary of the turn that made it.
//
// This file builds those three. It is deliberately free of I/O and of the
// notifier's policy: the strings are a pure function of what a settlement
// states, which is what makes them testable without a bus.

// ActorKind is who a notification is about.
//
// The distinction exists because not everything that settles is an agent: the
// gateway itself reports an approval nobody answered and a harness that gave up,
// and a reader who cannot tell those from the agent's own work reads them as
// something the agent did.
type ActorKind string

const (
	// ActorMain is the session's own agent: the one a prompt is sent to.
	ActorMain ActorKind = "main"
	// ActorSystem is the gateway itself — a stopped harness, a refused
	// approval. Nothing an agent did.
	ActorSystem ActorKind = "system"
)

// Actor names who a notification is about.
type Actor struct {
	Kind ActorKind `json:"kind"`
}

// Label is the actor as a notification prints it: "Main agent", "Gateway".
func (a Actor) Label() string {
	switch a.Kind {
	case ActorSystem:
		return "Gateway"
	default:
		return "Main agent"
	}
}

// isZero reports whether an actor was never set, in which case a message keeps
// the older, actor-free shape rather than claiming to be the main agent.
func (a Actor) isZero() bool { return a.Kind == "" }

// workSummary renders what a turn did, as the middle line of a notification.
//
//	12 tool calls · 3 files changed · 1 delegation · 1 failure
//
// Empty when the turn did nothing countable, because "0 tool calls" is worse
// than saying nothing: a turn that was pure conversation has no activity to
// report, and the notification falls back to naming the session or the outcome.
//
// The counts are the settlement's own — see events.Work — and that is the point
// of them living there rather than here. They used to be folded from the
// `session.tool` frames this process happened to see, which made them a count of
// the observer: a gateway that attached fourteen minutes into a thirty-eight
// minute turn reported 105 calls for a turn that made 300, with a duration
// measured from the same moment and the same error in it.
func workSummary(work events.Work) string {
	parts := make([]string, 0, 4)
	if work.Calls > 0 {
		parts = append(parts, plural(work.Calls, "tool call", "tool calls"))
	}
	if work.Edits > 0 {
		parts = append(parts, plural(work.Edits, "file changed", "files changed"))
	}
	if work.Delegations > 0 {
		parts = append(parts, plural(work.Delegations, "delegation", "delegations"))
	}
	if work.Failed > 0 {
		parts = append(parts, plural(work.Failed, "failure", "failures"))
	}
	return strings.Join(parts, " · ")
}

// clip shortens a label to a rune budget without cutting a multi-byte character
// in half, and marks that it did.
//
// It collapses whitespace first, because everything it is used on — a session's
// name, a one-line label — is a *title*, and a title with a newline in it draws
// as two lines in a field that has room for one. Use keepShape for text whose
// line breaks mean something.
func clip(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

// plural renders a count with its noun, so "1 failure" is not "1 failures".
func plural(count int, one, many string) string {
	if count == 1 {
		return "1 " + one
	}
	return itoa(count) + " " + many
}

// itoa is a small integer formatter, kept here rather than pulling strconv in
// for the four call sites above.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}

// activityTitle states who did what, in the order a notification is read.
//
//	completed · Main agent · Checkout flow
//	failed · Main agent
//	Approval expired
//
// The outcome leads, because it is the one word that decides whether the reader
// has to do anything; the actor follows, because "the agent or the gateway?" is
// the question it answers — an approval nobody answered is not something an
// agent did. The session name — the operator's own words, and only present when
// the deployment opted in — closes the line, naming which conversation the work
// belonged to. An actor that was never set is omitted rather than guessed at:
// the line then keeps its older, actor-free shape instead of claiming to be the
// main agent.
func activityTitle(state string, actor Actor, session string) string {
	parts := make([]string, 0, 3)
	if outcome := outcomeWord(state); outcome != "" {
		parts = append(parts, outcome)
	}
	if !actor.isZero() {
		parts = append(parts, actor.Label())
	}
	if session != "" {
		parts = append(parts, session)
	}
	return strings.Join(parts, " · ")
}

// outcomeWord names a settled turn in one word.
//
// Lower case, because the word is the vocabulary the rest of the system uses —
// `completed` is what the wire says and what a client switches on — and a title
// is assembled from it rather than paraphrasing it.
func outcomeWord(state string) string {
	switch state {
	case "failed":
		return "failed"
	case "cancelled":
		return "stopped"
	case "completed":
		return "completed"
	default:
		return ""
	}
}

// activityBody is the rest of what a notification knows, in one line.
//
// The pieces are ordered by how much they add: what the work accumulated, then
// the harness's error. A reader who only sees the first fragment still learns
// something the title did not say.
func activityBody(summary, detail string) string {
	parts := make([]string, 0, 2)
	if summary != "" {
		parts = append(parts, summary)
	}
	if detail != "" {
		parts = append(parts, truncate(detail))
	}
	return strings.Join(parts, " · ")
}
