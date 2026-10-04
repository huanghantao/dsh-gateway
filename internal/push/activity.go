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
// notifier's policy: the strings are a pure function of the frames the gateway
// already publishes, which is what makes them testable without a bus.

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

// Digest is what one turn accumulated while it ran.
//
// It is counted from `session.tool` frames, which the gateway publishes for both
// of its producers — the ACP bridge for a session this process drives, and the
// log watcher for one the desktop drives — so the summary is available whichever
// end is running the turn.
//
// Nothing here is a guess: a count is incremented when the corresponding frame
// says the call settled. The one number that is deliberately absent is "how many
// lines changed" — the tool arguments carry it, but only a parse the change
// screen already does properly would extract it, and a second, weaker parse here
// would eventually disagree with that screen.
type Digest struct {
	// Calls is every tool call that settled during the turn.
	Calls int
	// Failed is how many of those the harness reported as failed.
	Failed int
	// Delegations is how many tool calls were handed to a child agent. It is
	// counted for the turn's own summary — "3 delegations" says how much of the
	// work was handed out — and it is the only trace a delegation leaves in a
	// notification, since the child's own finish is not announced.
	Delegations int
	// Edits is how many calls changed a file. Only the tools whose arguments
	// record a whole-file mutation are counted, so `bash` running `sed -i` is
	// not: see sessionlog.fileTools, whose list this mirrors on purpose.
	Edits int
}

// Summary renders a digest as the middle line of a notification.
//
// Empty when nothing happened, because "0 tool calls" is worse than saying
// nothing: a turn that was pure conversation has no activity to report, and the
// notification falls back to naming the session or the outcome.
func (d Digest) Summary() string {
	parts := make([]string, 0, 4)
	if d.Calls > 0 {
		parts = append(parts, plural(d.Calls, "tool call", "tool calls"))
	}
	if d.Edits > 0 {
		parts = append(parts, plural(d.Edits, "file changed", "files changed"))
	}
	if d.Delegations > 0 {
		parts = append(parts, plural(d.Delegations, "delegation", "delegations"))
	}
	if d.Failed > 0 {
		parts = append(parts, plural(d.Failed, "failure", "failures"))
	}
	return strings.Join(parts, " · ")
}

// add folds one settled call into the digest.
func (d Digest) add(call callFact) Digest {
	d.Calls++
	if call.failed {
		d.Failed++
	}
	if call.delegation {
		d.Delegations++
	}
	if fileTool(call.tool) {
		d.Edits++
	}
	return d
}

// fileTool names the tools whose arguments record a file mutation this build
// counts. It mirrors sessionlog's own list; the two exist in different packages
// because the projection and the notifier are separate readers of the same fact,
// and a tool added to one without the other shows up as a test that disagrees.
func fileTool(tool string) bool {
	switch tool {
	case "edit", "write":
		return true
	default:
		return false
	}
}

// delegationTool names the tools that run a child agent.
//
// `workflow` is included because it is the same event from the operator's point
// of view — work handed to agents that are not the main one — even though it may
// fan out to several children internally. The gateway sees one call, so it
// counts one delegation; counting the fan-out would mean inventing children
// whose names and results this process never receives.
func delegationTool(tool string) bool {
	switch tool {
	case "subagent", "subagent_fork", "workflow":
		return true
	default:
		return false
	}
}

// callFact is the part of a settled tool frame the digest cares about.
type callFact struct {
	tool       string
	failed     bool
	delegation bool
}

// settledFact reads one settled call down to what the digest and the delegation
// branch need, with the call's name already resolved by the caller.
//
// A call is failed when the harness said so, when the result carried a structured
// error, or when a command exited non-zero. All three are read rather than only
// the first: DSH reports a non-zero exit as a status, not an error, so a summary
// built from `failed` alone would call a broken build a success.
func settledFact(tool string, data events.ToolData) callFact {
	failed := data.Status == events.ToolFailed || data.IsError
	if code := data.ExitCode; code != nil && *code != 0 {
		failed = true
	}
	return callFact{
		tool:       tool,
		failed:     failed,
		delegation: delegationTool(tool),
	}
}

// normaliseTool strips the decoration a producer may have added around a tool
// name — "mcp__server__subagent", "bash: subagent" — so the vocabulary matches.
func normaliseTool(tool string) string {
	tool = strings.ToLower(strings.TrimSpace(tool))
	if index := strings.LastIndex(tool, "__"); index >= 0 {
		tool = tool[index+2:]
	}
	if index := strings.LastIndex(tool, ":"); index >= 0 {
		tool = strings.TrimSpace(tool[index+1:])
	}
	return tool
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

// keepShape shortens text to a rune budget without touching its whitespace.
//
// It exists because clip was once used on a model's answer, and an answer is
// markdown: its line breaks *are* its structure. Collapsing them turned a
// heading, a table and a list into one paragraph of punctuation — the card
// arrived reading "## 结论| 步骤 | 结果 ||---|---|| 构建 |…", which the platform
// then rendered as exactly that, because markdown needs the breaks to tell a
// table from a sentence. The two functions sit one line apart so that the next
// person picks the right one.
func keepShape(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return strings.TrimSpace(string([]rune(text)[:limit])) + "…"
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
