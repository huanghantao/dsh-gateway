// Tool frames: the shape of `session.tool`, in one place.
//
// Two producers publish tool lifecycle on the bus — the ACP bridge, for a session
// this gateway is driving, and the log watcher, for one the desktop is driving —
// and a client cannot tell them apart, which is the point. Keeping the payload
// type here rather than building a `map[string]any` at each producer is what
// makes that true by construction: there is one vocabulary, one set of byte
// budgets, and no way for the two routes to drift apart.

package events

import (
	"github.com/huanghantao/dsh-gateway/internal/config"
	"github.com/huanghantao/dsh-gateway/internal/toolresult"
)

// ToolPhase is where a call stands: it has started, or it has settled.
type ToolPhase string

const (
	// ToolStarted is the opening frame. It carries the arguments, because they
	// are what a reader needs in order to know what is about to run.
	ToolStarted ToolPhase = "start"
	// ToolEnded is the closing frame. It carries the result.
	ToolEnded ToolPhase = "end"
)

// ToolStatus is where a call stands in the wire vocabulary.
//
// It is a closed set on purpose. The two producers reach it from different
// places — the ACP bridge from the harness's own status, the watcher from a
// recorded result — and a client has to be able to read either without knowing
// which one it got. A word outside this set is a bug in a producer, not a new
// state for a client to guess at.
type ToolStatus string

const (
	// ToolInProgress means the call has been issued and has not settled.
	ToolInProgress ToolStatus = "in_progress"
	// ToolCompleted means the call settled. It does **not** mean the command
	// succeeded: DSH reports a non-zero exit as a status, not an error, and the
	// exit code travels in Facts for a client that cares.
	ToolCompleted ToolStatus = "completed"
	// ToolFailed means the harness reported the call itself as failed — a spawn
	// error, an abort, a refusal.
	ToolFailed ToolStatus = "failed"
)

// ToolData is the payload of a session.tool frame.
type ToolData struct {
	Phase  ToolPhase `json:"phase"`
	CallID string    `json:"callId"`
	Tool   string    `json:"tool"`
	// Status is the lifecycle word, from the set above. A client does not have
	// to derive it from the frame's shape, and the two producers cannot disagree
	// about it without failing to compile.
	Status ToolStatus `json:"status"`
	// Input is the raw JSON arguments, bounded. The gateway does not otherwise
	// parse it: the phone shows what will actually run.
	Input string `json:"input,omitempty"`
	// Output is the tool's textual result, bounded.
	Output string `json:"output,omitempty"`
	// IsError is the harness saying the call itself failed — a spawn error, an
	// abort. A command that merely exited non-zero is *not* an error here; that
	// is in Facts.
	IsError bool `json:"isError"`

	// Facts and Trim are the shared halves of the vocabulary: what the result
	// said about how it ended, and which payload the deployment bounded.
	toolresult.Facts
	toolresult.Trim
}

// ToolFacts is what a producer knows about a call, before it is shaped for the
// wire. Fields that a producer cannot know are left at their zero value and are
// omitted from the frame rather than sent as a guess.
type ToolFacts struct {
	Phase   ToolPhase
	CallID  string
	Tool    string
	Status  ToolStatus
	Input   string
	Output  string
	IsError bool
	Facts   toolresult.Facts
}

// NewToolData bounds a call's payloads for the deployment and shapes the frame.
//
// The bounds are applied here, at the edge, and never to the projection itself:
// the transcript reader folds the same full text to build diffs for the change
// screen, and a bounded copy there would silently truncate a diff.
func NewToolData(facts ToolFacts, limits config.Limits) ToolData {
	input, output, trim := toolresult.Bound(facts.Input, facts.Output, limits.ToolInputBytes, limits.ToolOutputBytes)
	return ToolData{
		Phase:   facts.Phase,
		CallID:  facts.CallID,
		Tool:    facts.Tool,
		Status:  facts.Status,
		Input:   input,
		Output:  output,
		IsError: facts.IsError,
		Facts:   facts.Facts,
		Trim:    trim,
	}
}
