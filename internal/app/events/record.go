package events

import "github.com/huanghantao/dsh-gateway/internal/toolresult"

// The turn's own record: what it said last, and what it did.
//
// A settlement used to be a *signal* — "this turn is over" — and a notification
// was reconstructed around it from whatever fragments of the turn had arrived in
// time. The notifier kept the last assistant message it happened to see while
// the turn was live and counted the tool frames that happened to arrive before
// it settled, which made both facts depend on publish order and on when this
// process attached. Neither is a property of the turn, and both broke the way
// that invites: DSH writes a turn's last message and its `turn/end` boundary
// milliseconds apart, so the one message a reader actually wants arrived one
// frame after the settlement that needed it and was dropped — the card then
// carried the narration from seven minutes earlier, or a bare "6 tool calls".
//
// The fix is ownership rather than a stricter order. Whoever owned the turn —
// the scheduler for one this gateway drives, the log watcher for one another DSH
// process is driving — states what the turn was, in the frame that settles it.
// A consumer has nothing left to reconstruct, so nothing about a notification
// depends on which producer won a race or on how much of the turn this process
// was awake for.
//
// The record travels on `turn.state` and is bounded by the message it quotes:
// the closing text is the turn's last committed message, whole, exactly as
// `session.message` carried it. A channel decides how much of it to print; a
// producer that truncated it would be a second budget answering "did I see all
// of it?".

// TurnRecord is what a turn's owner states about it, in the frame that settles
// it. Both halves are optional: a turn that ended on a tool call has no closing
// message, and one that only talked has no work.
type TurnRecord struct {
	// Closing is the model's own sign-off for the turn, quoted from the
	// transcript rather than summarised.
	Closing *Closing `json:"closing,omitempty"`
	// Work is what the turn did, counted by whoever was in a position to count
	// the whole of it.
	Work Work `json:"work,omitzero"`
}

// Closing is the last thing a turn said.
type Closing struct {
	Text string `json:"text"`
	// Model names the model that wrote it, when the producer knows. The ACP
	// stream does not name one on a committed message; a session log does.
	Model string `json:"model,omitempty"`
}

// Work is a turn's tool activity.
//
// It is counted by the turn's owner and not by its consumer, which is what makes
// the numbers the turn's own rather than the observer's: a gateway that attached
// fourteen minutes into a thirty-eight-minute turn used to report the 105 calls
// it had watched as though the turn had made 105. Where the owner cannot count —
// a harness that reports nothing — the record says nothing, and a notification
// falls back to what it does know rather than to a number it invented.
type Work struct {
	// Calls is every tool call that settled during the turn.
	Calls int `json:"calls,omitempty"`
	// Failed is how many of those failed, by the three signals toolresult.Failed
	// reads.
	Failed int `json:"failed,omitempty"`
	// Delegations is how many calls were handed to other agents. It is the only
	// trace a delegation leaves in a notification, since a child's own finish is
	// not announced.
	Delegations int `json:"delegations,omitempty"`
	// Edits is how many calls changed a file, by toolresult.FileTool.
	Edits int `json:"edits,omitempty"`
}

// Count folds one settled call into the work it belongs to.
//
// A call that is still in flight is not counted: a turn's own record describes
// work that happened, and a call whose result never arrived is not a fact the
// log or the update stream can state.
func (w *Work) Count(tool string, failed bool) {
	w.Calls++
	if failed {
		w.Failed++
	}
	if toolresult.DelegationTool(tool) {
		w.Delegations++
	}
	if toolresult.FileTool(tool) {
		w.Edits++
	}
}
