// Package bridge translates harness activity into bus events.
//
// It is the only place that knows how a harness-level update is shaped for the
// wire. Keeping that mapping in one small, testable type means the adapter can
// stay close to ACP's vocabulary and the API can stay close to the client's,
// without either leaking into the other.
package bridge

import (
	"strings"
	"sync"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/config"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/toolresult"
)

// Bridge implements harness.UpdateSink and harness.StateSink.
//
// It must not block: Publish is called on the ACP reader goroutine, and a delay
// there stalls the agent. The bus it forwards to is non-blocking by contract.
type Bridge struct {
	bus *events.Bus

	// mu guards open.
	mu sync.Mutex

	// open remembers what a call was opened with, per call id.
	//
	// This is not a nicety. Verified against the live harness: the opening
	// `tool_call` update carries `title` (the tool name) and `rawInput`, and the
	// closing `tool_call_update` carries neither. Without this the completing
	// event would reach the phone with an empty tool name *and* no arguments —
	// a card with no way to say what finished, or what it had been doing.
	//
	// A completion removes its own entry, but a completion is not guaranteed: a
	// turn cancelled mid-call, or a child killed, leaves the opening frame with
	// nothing to close it. So the table is also capped, oldest first — see
	// maxOpenCalls — rather than trusting every call to end.
	open map[string]opened
	// order is the call ids in the order they were first seen, so the cap has an
	// oldest to drop without scanning the map.
	order []string

	// turns is what each session's open turn has accumulated: the words it would
	// be quoted by, and the work it did. The scheduler takes it when it settles
	// the turn (see TakeTurn), which is what makes a notification's content the
	// turn's own record rather than a consumer's impression of a stream — the
	// bridge is where those two facts exist, and the settlement is when they are
	// complete.
	turns map[string]*events.TurnRecord
	// turnOrder is the session ids in the order their records were first seen,
	// so the cap below has an oldest to drop.
	turnOrder []string

	// limits is the deployment's byte budget for a tool's arguments and result.
	// It is held here so that one payload shape is bounded in one place.
	limits config.Limits
}

// maxOpenCalls bounds the in-flight call table.
//
// It has to exist because the map holds raw tool arguments, and an edit's
// argument is a whole file: one leaked entry the size of the file it wrote is
// worse than the few hundred bytes a name and a command cost. The number is the
// notifier's, for the same correlation and the same reason.
const maxOpenCalls = 256

// maxTurnRecords bounds the open-turn table.
//
// A record is handed over when its turn settles, so entries live for one turn —
// but a settle is not guaranteed, and a record is not small: it holds the turn's
// closing message, which for a report is kilobytes. Child sessions stream
// through this table too and none of them is ever taken — a delegation's turn is
// not the gateway's to settle — so the bound has to clear the largest fan-out
// this gateway has seen, which is why it matches the per-session ledger it
// replaces. Eviction is by least recent activity rather than by arrival: see
// touchTurnLocked.
const maxTurnRecords = 512

// opened is what a call was announced with.
type opened struct {
	name  string
	input string
}

// New builds a Bridge.
func New(bus *events.Bus, limits config.Limits) *Bridge {
	return &Bridge{
		bus:    bus,
		open:   map[string]opened{},
		turns:  map[string]*events.TurnRecord{},
		limits: limits,
	}
}

// Publish implements harness.UpdateSink.
func (b *Bridge) Publish(u harness.Update) {
	switch u.Kind {
	case harness.UpdateMessage:
		b.bus.Publish(events.TypeSessionMessage, u.SessionID, events.MessageData{
			ID:   u.MessageID,
			Role: "assistant",
			Text: u.Text,
		})
		b.rememberClosing(u)

	case harness.UpdateThought:
		b.bus.Publish(events.TypeSessionThought, u.SessionID, map[string]any{
			"id":   u.MessageID,
			"text": u.Text,
		})

	case harness.UpdateTool:
		b.publishTool(u)

	case harness.UpdateUsage:
		if u.Usage != nil {
			b.bus.Publish(events.TypeUsage, u.SessionID, map[string]any{
				"used":     u.Usage.Used,
				"size":     u.Usage.Size,
				"fraction": u.Usage.Fraction(),
			})
		}

	case harness.UpdateConfig:
		b.bus.Publish(events.TypeSessionState, u.SessionID, map[string]any{
			"config": configView(u.Config),
		})
	}
}

// publishTool emits a tool lifecycle event.
func (b *Bridge) publishTool(u harness.Update) {
	if u.Tool == nil {
		return
	}
	tool := u.Tool

	status := toolStatusOf(tool.Status)
	phase := events.ToolStarted
	if status != events.ToolInProgress {
		phase = events.ToolEnded
	}

	// Resolve the name and the arguments, preferring what the adapter reported
	// and falling back to what the opening frame recorded.
	name, input := tool.Title, tool.Input
	b.mu.Lock()
	known, tracked := b.open[tool.ID]
	switch {
	case phase == events.ToolStarted:
		if name == "" {
			name = known.name
		}
		if input == "" {
			input = known.input
		}
		b.trackOpenLocked(tool.ID, opened{name: name, input: input})
	case tracked:
		// A completion: fill in whatever it left out, then stop tracking.
		if name == "" {
			name = known.name
		}
		if input == "" {
			input = known.input
		}
		b.forgetOpenLocked(tool.ID)
	}
	b.mu.Unlock()

	b.bus.Publish(events.TypeSessionTool, u.SessionID, events.NewToolData(events.ToolFacts{
		Phase:   phase,
		CallID:  tool.ID,
		Tool:    name,
		Status:  status,
		Input:   input,
		Output:  tool.Output,
		IsError: tool.IsError,
		Facts:   tool.Result.Facts(),
	}, b.limits))

	if phase == events.ToolEnded {
		// The turn's own count of what it did. The name is the one resolved
		// above, because the closing update carries neither a title nor the
		// arguments — see `open` — and a call counted under an empty name would
		// be a call that is not a delegation and not an edit.
		b.countCall(u.SessionID, name, status == events.ToolFailed || toolresult.Failed(tool.IsError, tool.Result.Facts()))
	}
}

// rememberClosing keeps the last thing the turn said.
//
// Only the last, and only when it said something: a step that merely called
// tools commits a message with no text, and treating that as the turn's
// sign-off would erase an answer an earlier step had already written.
func (b *Bridge) rememberClosing(u harness.Update) {
	if strings.TrimSpace(u.Text) == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recordLocked(u.SessionID).Closing = &events.Closing{Text: u.Text, Model: u.Model}
}

// countCall folds one settled call into the session's open turn.
func (b *Bridge) countCall(sessionID, tool string, failed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recordLocked(sessionID).Work.Count(tool, failed)
}

// TakeTurn hands over what a session's open turn accumulated, and leaves nothing
// behind for the next one to inherit.
//
// The scheduler calls it as it settles a turn it drove, which is the one moment
// the record is complete: the prompt has answered, so the harness has committed
// everything the turn will ever commit. Reading is a handover rather than a
// copy because the two ends mean different things by "the turn's record" — this
// side means the turn in flight, the settlement means the turn that just ended —
// and a record left behind would be quoted as the closing words of whatever the
// session did next.
func (b *Bridge) TakeTurn(sessionID string) events.TurnRecord {
	b.mu.Lock()
	defer b.mu.Unlock()

	record, ok := b.turns[sessionID]
	if !ok {
		return events.TurnRecord{}
	}
	delete(b.turns, sessionID)
	for index, id := range b.turnOrder {
		if id == sessionID {
			b.turnOrder = append(b.turnOrder[:index], b.turnOrder[index+1:]...)
			break
		}
	}
	return *record
}

// recordLocked returns the session's open-turn record, creating it within the
// cap. The caller holds the lock.
func (b *Bridge) recordLocked(sessionID string) *events.TurnRecord {
	if record, ok := b.turns[sessionID]; ok {
		b.touchTurnLocked(sessionID)
		return record
	}
	record := &events.TurnRecord{}
	b.turns[sessionID] = record
	b.turnOrder = append(b.turnOrder, sessionID)
	for len(b.turnOrder) > maxTurnRecords {
		oldest := b.turnOrder[0]
		b.turnOrder = b.turnOrder[1:]
		delete(b.turns, oldest)
	}
	return record
}

// touchTurnLocked moves a session to the newest end of the eviction order.
//
// The order is by activity rather than by arrival, because the sessions in this
// table are not all the same kind. A turn the gateway drives is written to
// continuously — a call settles every few seconds — while a delegated child can
// finish and then say nothing for the rest of its parent's turn. Evicting on
// arrival would let a workflow that fans out to enough children drop the record
// of the one turn whose settlement is going to read it; evicting on activity
// drops the children that have stopped talking, which is what they are.
func (b *Bridge) touchTurnLocked(sessionID string) {
	for index, id := range b.turnOrder {
		if id != sessionID {
			continue
		}
		if index == len(b.turnOrder)-1 {
			return
		}
		b.turnOrder = append(b.turnOrder[:index], b.turnOrder[index+1:]...)
		b.turnOrder = append(b.turnOrder, sessionID)
		return
	}
}

// toolStatusOf maps a harness lifecycle word onto the wire vocabulary.
//
// The default matters more than the cases. A status this build has never heard
// of is read as "still running", which keeps the card on screen and honest; the
// alternative — and what this used to do — was to call it completed, which
// closes a call that has not finished and hides the output that is still coming.
func toolStatusOf(status harness.ToolStatus) events.ToolStatus {
	switch status {
	case harness.ToolCompleted:
		return events.ToolCompleted
	case harness.ToolFailed:
		return events.ToolFailed
	case harness.ToolInProgress:
		return events.ToolInProgress
	default:
		return events.ToolInProgress
	}
}

// PublishState implements harness.StateSink.
func (b *Bridge) PublishState(state harness.State, detail string) {
	b.bus.Publish(events.TypeHarnessState, "", events.HarnessState{
		State:  string(state),
		Detail: detail,
	})
}

// configView converts config options into their wire shape.
func configView(options []harness.ConfigOption) []map[string]any {
	out := make([]map[string]any, 0, len(options))
	for _, o := range options {
		values := make([]map[string]any, 0, len(o.Options))
		for _, v := range o.Options {
			values = append(values, map[string]any{
				"id":          v.ID,
				"name":        v.Name,
				"description": v.Description,
				"group":       v.Group,
				"label":       v.Label(),
			})
		}
		out = append(out, map[string]any{
			"id":      o.ID,
			"name":    o.Name,
			"current": o.Current,
			"options": values,
		})
	}
	return out
}

// trackOpenLocked records what a call was announced with, dropping the oldest
// entries once the table is at its cap. The caller holds b.mu.
func (b *Bridge) trackOpenLocked(callID string, o opened) {
	if _, exists := b.open[callID]; !exists {
		b.order = append(b.order, callID)
	}
	b.open[callID] = o

	for len(b.order) > maxOpenCalls {
		oldest := b.order[0]
		b.order = b.order[1:]
		delete(b.open, oldest)
	}
}

// forgetOpenLocked stops tracking a call that has ended. The caller holds b.mu.
//
// The id is dropped from the order as well as from the map, so the cap counts
// calls that are actually still open rather than every call the bridge has ever
// seen.
func (b *Bridge) forgetOpenLocked(callID string) {
	delete(b.open, callID)
	for index, id := range b.order {
		if id == callID {
			b.order = append(b.order[:index], b.order[index+1:]...)
			return
		}
	}
}
