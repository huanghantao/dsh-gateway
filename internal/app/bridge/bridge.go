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
	"github.com/huanghantao/dsh-gateway/internal/harness"
)

// Bridge implements harness.UpdateSink and harness.StateSink.
//
// It must not block: Publish is called on the ACP reader goroutine, and a delay
// there stalls the agent. The bus it forwards to is non-blocking by contract.
type Bridge struct {
	bus *events.Bus

	// tools remembers the tool name per call id.
	//
	// This is not a nicety. Verified against the live harness: the opening
	// `tool_call` update carries `title` (the tool name) and the closing
	// `tool_call_update` does not. Without this map the completing event would
	// reach the phone with an empty tool name, and the UI could not say what
	// finished.
	mu    sync.Mutex
	tools map[string]string
}

// New builds a Bridge.
func New(bus *events.Bus) *Bridge {
	return &Bridge{bus: bus, tools: map[string]string{}}
}

// Publish implements harness.UpdateSink.
func (b *Bridge) Publish(u harness.Update) {
	switch u.Kind {
	case harness.UpdateMessage:
		b.bus.Publish(events.TypeSessionMessage, u.SessionID, map[string]any{
			"id":   u.MessageID,
			"role": "assistant",
			"text": u.Text,
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

	// Resolve the tool name, preferring what the adapter reported and falling
	// back to what the opening event recorded.
	name := tool.Title
	if name != "" {
		if tool.Status == harness.ToolInProgress {
			b.mu.Lock()
			b.tools[tool.ID] = name
			b.mu.Unlock()
		}
	} else {
		b.mu.Lock()
		name = b.tools[tool.ID]
		b.mu.Unlock()
	}

	phase := "start"
	switch tool.Status {
	case harness.ToolCompleted, harness.ToolFailed:
		phase = "end"
		b.mu.Lock()
		// The call is over, so stop tracking it. Leaving entries behind would
		// grow without bound over a long-lived gateway.
		delete(b.tools, tool.ID)
		b.mu.Unlock()
	case harness.ToolInProgress:
		// The opening event. phase already says "start".
	default:
		// A status the harness added later. Publishing it as a start is the safe
		// reading: the client shows the tool as running, which is true of any
		// non-terminal status.
	}

	b.bus.Publish(events.TypeSessionTool, u.SessionID, map[string]any{
		"phase":   phase,
		"callId":  tool.ID,
		"tool":    name,
		"status":  string(tool.Status),
		"input":   tool.Input,
		"output":  tool.Output,
		"isError": tool.IsError,
	})
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
