# 8. Merge back the sessions the harness still holds

Date: 2026-10-03
Status: Accepted

## Context

An operator's session disappeared from the phone while its agent was still
working. The mechanism is three facts that are each correct on their own:

- **DSH's `session/list` skips every session that is live in the process
  answering it.** `dsh-acp/lib/index.js` filters a row out when
  `sessions.has(id) || activating.has(id) || ctx.sessions.get(id) !== undefined
  || origin === "subagent" || parentSession !== undefined`. The child the
  gateway drives is that process.
- **The agent host holds a session for the life of its child.** `session.release`
  cannot detach one: DSH offers exactly one lever, `session/close`, and closing
  writes a synthetic end into the session's own log. A release is therefore
  bookkeeping, and the flock stays where it is (ADR 7's third tier, and
  `Server.handleRelease`).
- **The gateway compensates by merging back the sessions it leases.** That covers
  a session only while a phone is attached to it. Once the lease is gone — an
  idle timeout, a prompt that outlived the gateway's 30-minute patience, a
  redeploy — nothing merges the row back, and the harness cannot report it
  either.

The result is a session that is invisible on the phone while its agent keeps
working, and unreachable rather than merely unlisted: the list is where an id
comes from, so an operator cannot search their way back to it either.

## Decision

One change per link in that chain.

1. `agenthost.Client` keeps the set of sessions the host holds. It is replaced
   wholesale from the host's snapshot on every connection — including the first
   one, which is what `Client.Start` now rejoins on rather than only reconnecting
   — and maintained by the calls that attach and drop a handle. It is offered to
   the API as `Held`.
2. `GET /sessions` merges those rows in alongside the leased ones, subject to the
   same filters as the page they join: archived state, search text, and the
   requested workspace.
3. `session.resume` for a session the host already holds answers from the held
   handle instead of asking the child again, so a session whose lease was lost
   can be attached — and prompted — again.

## Rationale

The harness's listing is authoritative about which sessions exist; the gateway is
authoritative about what it is holding. The merge is not a workaround for a bug
in DSH — it is the gateway completing a list that only it can complete.

The alternative, treating "released" as "gone", is what produced the report. No
client can tell a session the harness still holds from one that was deleted, and
the cost of guessing wrong is a conversation the operator cannot get back to.

Answering a resume from the held handle is the same reasoning one step further
in. DSH refuses a second attachment (`session is already active`), and that
refusal would reach the phone as "somebody else has this session" when the
somebody is this gateway's own host. The handle is right there; the question was
already answered.

## Consequences

- A session whose lease was released comes back into the list and stays until the
  child restarts. That is honest rather than tidy: the desktop cannot open it
  either, because DSH's lock is still held, so the row is the only way back to
  it.
- "Released" no longer means "the desktop can open this now". It means "this
  gateway is no longer driving it". ADR 3 says the release path frees DSH's lock;
  the agent-host tier made that false, and this record corrects it.
- The held set is per connection: a gateway restart repopulates it from the
  snapshot, and a host restart empties it — after which DSH's own listing carries
  those sessions again, unchanged.
- Re-attaching does not cancel the turn the gateway abandoned. DSH refuses a
  second prompt while the first is in flight (`a prompt is already in flight for
  this session`), which is a legible error rather than a silent second turn.
  Making an abandoned turn visible and followable from the phone — a turn ticket
  whose life is the host's rather than the gateway's — is the follow-up this
  record does not cover.
