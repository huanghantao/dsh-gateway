# 4. Schedule turns in an application service, not in the prompt handler

Date: 2026-10-03
Status: Accepted

## Context

The prompt handler owned the turn lifecycle: it checked that no turn was in
flight, marked the session busy, and started the turn on a detached goroutine
that cleared the flag when the prompt returned.

Two problems came out of that shape.

**The guard was not atomic.** "Is a turn running?" and "mark a turn running" were
two separate acquisitions of the lease mutex. Two requests that arrived together
— a double tap on a phone, or a retry after a flaky radio — could both observe
"idle" and both start a turn. DSH permits one prompt in flight, so the loser
surfaced as a failed turn with no explanation the operator could act on.

**A prompt that arrived mid-turn was refused.** `409 prompt_in_flight` is honest,
but it throws away the thing the operator typed. On a phone the follow-up is
usually typed *while* the agent works: "also run the tests", "and check the other
file". Refusing it means the thought is lost, or the operator stops a turn that
was doing useful work in order to re-state it.

The lifecycle also lived in the wrong layer. "One turn at a time per session"
and "what happens to a prompt that arrives while a turn runs" are policy, and
policy in a driving adapter is policy that a second adapter would have to
reimplement.

## Decision

Move turn lifecycle into an application service, `internal/app/turns`, and leave
the HTTP layer to translate.

The scheduler owns, per session, at most one running turn and a bounded FIFO of
queued ones. Admission is a single critical section, so a double submit is
decided by one `select`-free switch rather than by a check and a later set.

- `Submit` admits a prompt as *running* or *queued* and returns the ticket. A
  submit that would exceed `session.promptQueueDepth` is refused, and the
  refusal names the depth.
- A finished turn starts the next queued one without leaving an idle window in
  which a concurrent submit could start a second turn.
- `Cancel` interrupts the running turn and drops the queue. `Drop` removes one
  queued ticket. "Stop" on the phone means stop this, not stop this and then
  immediately start the next thing.
- The scheduler publishes the same `turn.state` event the handler used to, with
  the fields a reconnecting client needs: the turn id, its state, when it
  started, and how many are queued behind it.

The lease no longer stores a `busy` flag. It asks the scheduler, through a
`Busy func(sessionID string) bool` injected at the composition root, because two
copies of "is this session mid-turn" is one copy too many — and the copy in the
lease was the one that could disagree with reality. The idle reaper keeps its
existing guarantee for free: the scheduler reports a session with a running turn
as busy, and one with a merely queued prompt as idle, which is correct — a queued
prompt has not started, so the desktop may still take the session back.

## Rationale

The scheduler needs nothing but the harness port, the event bus, and a clock. It
does not know about HTTP, leases, or ACP. That is what makes it testable with a
fake harness and what makes the queue policy a thing a test can hold still.

Making the queue a property of the session rather than of a client connection is
the same decision the lease already made for liveness: a phone that locks its
screen mid-turn must not lose the prompt it already sent.

## Consequences

- `POST /sessions/{id}/prompt` answers `202` with the ticket, so a client can
  tell "running" from "queued, position 2" without a second request.
- `GET /sessions/{id}` reports the turn, so a client that reconnects mid-turn
  learns when it started rather than inferring it from the event stream, which it
  may have missed.
- A client that wants the old strict behaviour can read `state` and stop; nothing
  in the gateway refuses a follow-up any more, and `session.promptQueueDepth: 0`
  restores it for an operator who wants it.
