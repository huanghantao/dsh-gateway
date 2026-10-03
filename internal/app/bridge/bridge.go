// Package bridge translates harness activity into bus events.
//
// It is the only place that knows how a harness-level update is shaped for the
// wire. Keeping that mapping in one small, testable type means the adapter can
// stay close to ACP's vocabulary and the API can stay close to the client's,
// without either leaking into the other.
package bridge

import (
	"sync"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/config"
	"github.com/huanghantao/dsh-gateway/internal/harness"
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
	// Entries live only as long as the call does: a completion removes its own,
	// so the map is bounded by how many calls are in flight at once.
	open map[string]opened

	// limits is the deployment's byte budget for a tool's arguments and result.
	// It is held here so that one payload shape is bounded in one place.
	limits config.Limits
}

// opened is what a call was announced with.
type opened struct {
	name  string
	input string
}

// New builds a Bridge.
func New(bus *events.Bus, limits config.Limits) *Bridge {
	return &Bridge{bus: bus, open: map[string]opened{}, limits: limits}
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
		b.open[tool.ID] = opened{name: name, input: input}
	case tracked:
		// A completion: fill in whatever it left out, then stop tracking. Leaving
		// entries behind would grow without bound over a long-lived gateway.
		if name == "" {
			name = known.name
		}
		if input == "" {
			input = known.input
		}
		delete(b.open, tool.ID)
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
