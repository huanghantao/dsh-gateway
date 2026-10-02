# 2. Project the session log read-only for history

Date: 2026-09-30
Status: Accepted

## Context

The ACP session listing returns an id and a working directory and nothing else —
no title, no timestamps, no messages. `session/resume` explicitly restores the
agent's log "without replaying old updates".

A phone opening a conversation started at the desk would therefore show a blank
screen. That is the primary use case, so it had to be solved.

## Decision

Read DSH's own persisted session log directly, read-only, and fold it into a
transcript.

`$DSH_HOME/sessions/<workspace>/<session-id>/session.v4.jsonl.zstd` is a
concatenation of zstd frames, one per flush, each holding newline-delimited
versioned event records.

## Rationale

- It is the only source of that history. Nothing exposes it over ACP.
- The alternative — an in-process DSH plugin that serves history over HTTP —
  would add a TypeScript component and an npm publish step to a project whose
  deployment story is "one Go binary".
- Reading is safe by construction. The gateway never takes the write lock and
  never appends, so no format surprise can corrupt a session.

## Consequences

This is the one place the gateway depends on a format DSH does not publish as a
contract, so the coupling is quarantined in `internal/sessionlog`:

- Access is strictly read-only.
- The log header carries `version`. An unknown version yields
  `ErrUnsupportedFormat`, which the API renders as an empty transcript flagged
  `unsupported: true` rather than an error, so the app degrades to "open this on
  your desktop".
- Unknown event types are skipped, not rejected, so a new DSH event kind does not
  blank the transcript.
- Session ids arrive from HTTP paths, so they are validated against a strict
  character set and the resolved path is confirmed to be inside the sessions root
  before anything is opened.

Only `source.kind == "user"` messages become user bubbles. DSH records its own
injections — a runtime-context snapshot and the installed skill catalog — as
user-role messages in the same log, and the catalog alone runs to tens of
kilobytes.
