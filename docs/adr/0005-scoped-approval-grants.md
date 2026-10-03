# 5. Authorise a scope once, instead of every invocation

Date: 2026-10-03
Status: Accepted

## Context

DSH offers exactly two choices on an approval prompt: allow once, and reject
once. The gateway relayed them verbatim and stated the resulting property
plainly — "there is deliberately no allow always and no auto-approve".

That property is right about the threat and wrong about the interface. A turn
that edits six files and runs a dozen commands asks a dozen times, each with a
five-minute clock, and a single missed prompt is a refusal that stops the agent
doing the thing it was asked to do. On a desktop that is a mild annoyance. On a
phone it is the difference between delegating work and babysitting it, and
babysitting is not what a remote control is for.

The failure is not that the operator wants less safety. It is that consent is
being collected at the wrong granularity: "this session may run read-only tools
for the next half hour" is a decision a person can genuinely make once, while
"allow this one `grep`" is a decision they can only rubber-stamp.

## Decision

Let a decision carry an optional scope, and remember it.

An approval view is offered the harness's own options plus two the gateway
synthesises:

| Option | Scope |
|---|---|
| `allow-once` | this invocation (DSH's own) |
| `reject-once` | this invocation (DSH's own) |
| `allow-session-tool` | this tool, in this session |
| `allow-exact` | this tool with exactly these arguments, in this session |

Choosing a grant records a rule in the broker and answers DSH with `allow-once`,
because that is the only affirmative option DSH has. A later request that matches
a live rule is answered from the rule without a human, and the broker publishes
`approval.granted` so the transcript shows what happened and no client can
mistake an automated yes for a person's.

A rule is bounded on three axes, and every one of them is required:

- **Time.** `session.approvalGrantTTL`, default 30 minutes, `0` disables grants
  entirely.
- **Scope.** A tool name, and for `allow-exact` a digest of the arguments.
- **Session.** A rule never applies to another session.

Rules are listed by `GET /approvals/grants` and revoked by
`DELETE /approvals/grants/{id}`. They die with the broker, so a gateway restart
clears them, and the app shows the remaining time next to every live rule.

## Rationale

The property that is preserved is the one that matters: **no tool runs that a
human did not authorise.** What changes is that the authorisation can name a
scope, which is what makes it a decision rather than a reflex.

Exact-argument grants are deliberately available alongside tool grants, because
the two cover different risks. "Allow every `read`" is safe and saves the most
taps. "Allow this one `rm -rf build`" is what an operator wants for a dangerous
command they have read carefully, and it must not generalise to the next one.

Grants are held in the broker rather than in the session log or on disk: the
authorisation is a property of this gateway's live supervision of this child
process, and a rule that outlived a restart would be a rule nobody remembers
agreeing to.

Refusals are unchanged. Expiry is still a refusal, shutdown is still a refusal,
and there is still no path that approves by default.

## Consequences

- `docs/security.md` and `README.md` must state the new posture: approval is
  per scope, not per invocation, and a grant is a real authorisation with a
  visible expiry rather than a setting that turns prompts off.
- The app must render three things it did not before: the grant options, the
  countdown on a live grant, and the fact that an approval was answered from one.
- An operator who wants the old behaviour sets `session.approvalGrantTTL: 0`, and
  the synthesised options are not offered at all.
