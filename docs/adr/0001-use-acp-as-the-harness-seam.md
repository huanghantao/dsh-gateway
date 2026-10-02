# 1. Drive DeepSeek Harness over ACP stdio

Date: 2026-09-30
Status: Accepted

## Context

The gateway needs to drive a DeepSeek Harness agent: list sessions, create and
resume them, submit prompts, observe progress, answer approval prompts, cancel
turns, and change model and reasoning effort. It also needs to work from Go, and
to survive DSH upgrades.

Three seams were available.

1. **The web GUI's wire protocol.** `dsh web` exposes `/api` and a
   `/api/remote.mux` WebSocket. It is what the shipped browser client speaks, so
   it has the richest capability set — including the ability to follow a session
   another process is driving.
2. **The SDK profile.** `dsh --profile sdk` speaks a small JSON-RPC protocol.
3. **The ACP profile.** `dsh --profile acp` speaks Agent Client Protocol v1 over
   newline-delimited JSON-RPC on stdio.

## Decision

Use ACP.

## Rationale

- It is the only seam that is a *published specification*. ACP v1 is defined by
  the Agent Client Protocol project, and `dsh-acp-app` states that the profile
  "adds no private method, capability, `_meta`, environment variable, or transport
  field". The field shapes were confirmed against a live server.
- It has the capability set the product needs: `session/list`, `session/resume`,
  `session/close`, `session/prompt`, `session/cancel`,
  `session/set_config_option`, `session/update`, and `session/request_permission`.
- The web seam is generated Typert Remote plus a Gateway and Connection layer,
  documented for "business-package and assembly maintainers". A Go client would
  have to reimplement token and cookie authentication, the Host/Origin trust
  fence, WebSocket stream framing, generated invocation descriptors, multipart
  attachment envelopes, and `$events` generation lifecycles — with no versioned
  specification.
- The SDK seam is materially weaker: no cancel, no session close, no approvals,
  an unvalidated `0.0.1` handshake version, and an explicit pre-release stance
  with no compatibility promise.

## Consequences

- The gateway must spawn and supervise a child process; ACP is stdio-only.
- The gateway cannot attach to a session another process holds. DSH enforces one
  live writer per session with `flock(2)`, so this is a property of the harness,
  not of ACP. It is why session leases exist (ADR 0003).
- Progress is commit-grained. ACP updates derive from committed durable events, so
  there is no token-by-token streaming.
- `session/resume` does not replay history, which is why a separate read-only
  session-log projector exists (ADR 0002).
