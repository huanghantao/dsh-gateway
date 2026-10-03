// Package acp adapts the DeepSeek Harness ACP stdio server to the gateway's
// harness port.
//
// The wire protocol is Agent Client Protocol v1 carried as newline-delimited
// JSON-RPC 2.0 over the child's stdin and stdout. DSH advertises ACP v1 and adds
// no private methods, so this adapter depends on a published specification rather
// than on DSH internals. The field shapes in codec.go were taken from a live
// handshake against `dsh --profile acp`, not from documentation alone, and the
// integration test re-derives them so a drift is caught rather than guessed at.
//
// Design notes:
//
//   - One `dsh --profile acp` process serves every session. ACP multiplexes by
//     session id, so a per-session process would multiply memory and startup cost
//     for no benefit.
//   - A single writer goroutine owns stdin. Framing is newline-delimited, so two
//     concurrent writers could interleave a half-written line and desynchronise
//     the stream permanently. Serialising writes removes that class of bug
//     entirely rather than relying on a mutex being held correctly at each call
//     site.
//   - The reader goroutine never blocks on anything but the pipe. Updates go to a
//     sink that is contractually non-blocking; approvals block a *separate*
//     goroutine so that waiting for a human cannot stall unrelated sessions.
package acp

import (
	"encoding/json"
	"fmt"

	"github.com/huanghantao/dsh-gateway/internal/harness"
)

// protocolVersion is the only ACP version DSH speaks. DSH ignores the client's
// requested version and always answers 1, so the adapter asserts rather than
// negotiates.
const protocolVersion = 1

// maxFrameBytes bounds one JSON-RPC line. A committed assistant message can be
// large, but stdout is protocol-only, so anything beyond a few megabytes means
// the stream is desynchronised and continuing would be worse than failing.
const maxFrameBytes = 16 << 20

// rpcMessage is the JSON-RPC 2.0 envelope. Exactly one of Result or Error is set
// on a response; Method is set on a request or notification.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// isResponse reports whether the message answers a request we sent.
func (m rpcMessage) isResponse() bool { return m.Method == "" && len(m.ID) > 0 }

// isRequest reports whether the message expects an answer from us.
func (m rpcMessage) isRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// isNotification reports whether the message is fire-and-forget.
func (m rpcMessage) isNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// rpcError is a JSON-RPC error object. Code 0 means the field was absent.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message)
}

// methodNotFound is the JSON-RPC code for an unimplemented method. The adapter
// uses it to turn "DSH does not support this" into a clear capability error
// instead of a generic failure.
const methodNotFound = -32601

// --- initialize -------------------------------------------------------------

type initializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities clientCapabilities `json:"clientCapabilities"`
	ClientInfo         clientInfo         `json:"clientInfo"`
}

// clientCapabilities advertises what this client can do. The gateway implements
// neither filesystem access nor terminal hosting, and saying so is not optional:
// a client that under-reports is never asked, which is exactly the behaviour we
// want, since the gateway has no business reading the agent's files.
type clientCapabilities struct {
	FS struct {
		ReadTextFile  bool `json:"readTextFile"`
		WriteTextFile bool `json:"writeTextFile"`
	} `json:"fs"`
	Terminal bool `json:"terminal"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion   int   `json:"protocolVersion"`
	AuthMethods       []any `json:"authMethods"`
	AgentCapabilities struct {
		PromptCapabilities struct {
			Image           bool `json:"image"`
			Audio           bool `json:"audio"`
			EmbeddedContext bool `json:"embeddedContext"`
		} `json:"promptCapabilities"`
		SessionCapabilities struct {
			Close  json.RawMessage `json:"close"`
			List   json.RawMessage `json:"list"`
			Resume json.RawMessage `json:"resume"`
		} `json:"sessionCapabilities"`
	} `json:"agentCapabilities"`
	AgentInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"agentInfo"`
}

// --- session/new, session/resume, session/list ------------------------------

type newSessionParams struct {
	Cwd        string `json:"cwd"`
	McpServers []any  `json:"mcpServers"`
}

type resumeSessionParams struct {
	SessionID  string `json:"sessionId"`
	Cwd        string `json:"cwd"`
	McpServers []any  `json:"mcpServers"`
}

type listSessionsParams struct {
	// Cwd, when set, filters to one workspace. Omitted means every workspace.
	Cwd string `json:"cwd,omitempty"`
	// Cursor continues a previous page. The value is opaque: DSH encodes the
	// last row's position in it and assigns it no client-visible meaning.
	Cursor string `json:"cursor,omitempty"`
}

type listSessionsResult struct {
	Sessions []sessionSummary `json:"sessions"`
	// NextCursor is absent or empty on the last page.
	NextCursor string `json:"nextCursor"`
}

// sessionSummary is what `session/list` reports. Verified against the live
// server: DSH returns only an id and a cwd — there is no title and no timestamp.
// Titles therefore come from the session log projector, not from here.
type sessionSummary struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

// sessionResult is the shape returned by session/new and session/resume.
type sessionResult struct {
	SessionID     string         `json:"sessionId"`
	ConfigOptions []configOption `json:"configOptions"`
}

// configOption mirrors ACP's SessionConfigOption. `options` is either a flat list
// or a list of groups, so both shapes are modelled and flattened by values().
type configOption struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Category     string            `json:"category"`
	Type         string            `json:"type"`
	CurrentValue string            `json:"currentValue"`
	Options      []configOptionRow `json:"options"`
}

// configOptionRow is either a selectable value (Value set) or a group (Options
// set). ACP nests them one level, and DSH uses groups for the model list.
type configOptionRow struct {
	Value       string            `json:"value"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Group       string            `json:"group"`
	Options     []configOptionRow `json:"options"`
}

// values flattens the nested option tree into selectable values, prefixing each
// with its group name so the phone can render "DeepSeek · V4 Pro" without
// needing to understand ACP's grouping.
func (c configOption) values() []harness.ConfigOptionValue {
	var out []harness.ConfigOptionValue
	var walk func(rows []configOptionRow, group string)
	walk = func(rows []configOptionRow, group string) {
		for _, row := range rows {
			if len(row.Options) > 0 {
				g := row.Name
				if g == "" {
					g = row.Group
				}
				walk(row.Options, g)
				continue
			}
			if row.Value == "" {
				continue
			}
			out = append(out, harness.ConfigOptionValue{
				ID:          row.Value,
				Name:        row.Name,
				Description: row.Description,
				Group:       group,
			})
		}
	}
	walk(c.Options, "")
	return out
}

// --- session/set_config_option ----------------------------------------------

type setConfigOptionParams struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

type setConfigOptionResult struct {
	ConfigOptions []configOption `json:"configOptions"`
}

// --- session/prompt, session/cancel -----------------------------------------

type promptParams struct {
	SessionID string          `json:"sessionId"`
	Prompt    []promptContent `json:"prompt"`
}

// promptContent is one prompt block. Text and image are both emitted; an image
// block is refused in convertPrompt when the harness did not advertise support,
// which is why the type carries both sets of fields rather than two shapes.
type promptContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

type promptResult struct {
	StopReason string `json:"stopReason"`
}

type cancelParams struct {
	SessionID string `json:"sessionId"`
}

type closeSessionParams struct {
	SessionID string `json:"sessionId"`
}

// --- session/update ---------------------------------------------------------

type sessionUpdateParams struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// updateEnvelope peeks at the discriminator before decoding the variant.
type updateEnvelope struct {
	SessionUpdate string `json:"sessionUpdate"`
}

type chunkUpdate struct {
	MessageID string `json:"messageId"`
	Content   struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type toolCallUpdate struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	RawInput   json.RawMessage `json:"rawInput"`
	Content    []toolContent   `json:"content"`
	RawOutput  json.RawMessage `json:"rawOutput"`
}

// toolContent is one element of a tool_call_update's content array. Verified
// shape: [{"type":"content","content":{"type":"text","text":"…"}}].
type toolContent struct {
	Type    string `json:"type"`
	Content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// text concatenates the textual parts of a tool result.
func (t toolCallUpdate) text() string {
	var out string
	for _, c := range t.Content {
		if c.Content.Type == "text" {
			out += c.Content.Text
		}
	}
	return out
}

// usageUpdate is ACP's context-usage notification. Verified shape:
// {"used":10377,"size":1000000} — this is context occupancy, not per-step token
// accounting. Per-message token counts come from the session log projector.
type usageUpdate struct {
	Used int `json:"used"`
	Size int `json:"size"`
}

type configOptionUpdate struct {
	ConfigOptions []configOption `json:"configOptions"`
}

// --- session/request_permission ---------------------------------------------

type requestPermissionParams struct {
	SessionID string `json:"sessionId"`
	ToolCall  struct {
		ToolCallID string          `json:"toolCallId"`
		Title      string          `json:"title"`
		Kind       string          `json:"kind"`
		Status     string          `json:"status"`
		RawInput   json.RawMessage `json:"rawInput"`
	} `json:"toolCall"`
	Options []permissionOption `json:"options"`
}

type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// permissionResult is the answer to a permission request. ACP models the outcome
// as a tagged union; only the "selected" and "cancelled" arms are used here.
type permissionResult struct {
	Outcome permissionOutcome `json:"outcome"`
}

type permissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// decodeFrame parses one newline-delimited JSON-RPC frame.
func decodeFrame(line []byte) (rpcMessage, error) {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return rpcMessage{}, fmt.Errorf("acp: decode frame: %w", err)
	}
	if msg.JSONRPC != "2.0" {
		return rpcMessage{}, fmt.Errorf("acp: frame is not jsonrpc 2.0 (got %q)", msg.JSONRPC)
	}
	return msg, nil
}

// idKey renders an id for use as a map key. Raw JSON is compared verbatim, which
// is exactly the equality JSON-RPC requires.
func idKey(id json.RawMessage) string {
	if len(id) == 0 {
		return ""
	}
	return string(id)
}
