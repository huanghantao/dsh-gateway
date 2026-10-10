package toolresult

import "strings"

// The tool vocabulary: which call is which, and how a settled one ended.
//
// It lives here, beside Facts, because these are the two halves of one question
// — "what did this call amount to?" — and because every reader of a tool call
// needs both. The projection counts a turn's file changes from it, the ACP
// bridge and the log watcher count the same turn's work from it, and a
// disagreement between any two of them is a number that differs depending on
// which end ran the turn: a card that says "105 tool calls" for a turn the app
// shows 300 in, or a change screen that misses an edit an MCP-namespaced tool
// made.
//
// It used to be three lists in three packages — a map in sessionlog, two
// switches in push — with a comment admitting that a tool added to one and not
// the others "shows up as a test that disagrees". One list is the only version
// of this that cannot drift.

// CallName strips the decoration a producer may have added around a tool name.
//
// The harness reports a tool as the model called it, and a call that arrived
// through an MCP server or a wrapper is named for the route rather than for the
// tool: `mcp__filesystem__edit`, `bash: subagent`. Classifying on the raw string
// would miss every one of them, and — worse — would classify them differently
// on the two producers, because only the ACP stream carries the decoration.
func CallName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if index := strings.LastIndex(name, "__"); index >= 0 {
		name = name[index+2:]
	}
	if index := strings.LastIndex(name, ":"); index >= 0 {
		name = strings.TrimSpace(name[index+1:])
	}
	return name
}

// FileTool reports whether a tool's arguments record a whole-file mutation this
// build understands.
//
// Anything else — a `bash` running `sed -i`, a `notebook_edit` with its own
// argument shape — is not guessed at: the change screen reads the arguments, and
// a call whose arguments do not carry the replacement cannot be projected into
// one.
func FileTool(name string) bool {
	switch CallName(name) {
	case "edit", "write":
		return true
	default:
		return false
	}
}

// DelegationTool reports whether a tool hands work to other agents.
//
// `workflow` is included because it is the same event from the operator's point
// of view — work handed to agents that are not the main one — even though it may
// fan out to several children internally. The gateway sees one call, so it
// counts one delegation; counting the fan-out would mean inventing children
// whose names and results this process never receives.
func DelegationTool(name string) bool {
	switch CallName(name) {
	case "subagent", "subagent_fork", "workflow":
		return true
	default:
		return false
	}
}

// Failed reports whether a settled call failed.
//
// Three signals, all read: the harness saying the call itself failed, and the
// result saying a command exited non-zero. The second is not redundant — DSH
// reports a non-zero exit as a status rather than an error, so a summary built
// from the first alone would call a broken build a success.
func Failed(isError bool, facts Facts) bool {
	if isError {
		return true
	}
	return facts.ExitCode != nil && *facts.ExitCode != 0
}
