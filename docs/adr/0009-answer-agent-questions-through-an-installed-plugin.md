# 9. Answer the agent's questions through a plugin the gateway installs

Date: 2026-10-10
Status: Accepted

## Context

DeepSeek Harness lets the model stop mid-turn and ask the operator something:
`ask_user_question`, whose card carries numbered choices, an optional free-text
answer, several questions in one request, and a `(Recommended)` convention for
the option the model prefers. It is the same shape of stop as an approval — the
agent cannot proceed until a person answers — but it carries content rather than
permission.

The gateway could not offer it, for three reasons that are each independently
verified against a live install:

- **The ACP surface has no questions in it.** `dsh-acp` contributes exactly one
  human-facing waterfall, `approval/request`, which it projects as
  `session/request_permission`. There is no elicitation: the string appears
  nowhere in the installation, `README.md` in that package lists elicitation
  among the surfaces it deliberately omits, and no `clientCapabilities` field is
  ever read. So there is nothing for the gateway to answer, however it asks.
- **The `acp` profile does not compose the tool.** `dsh-base` mounts the
  `user-questions` seam and nothing that consumes it; `dsh-acp-app` adds two
  rows. `@deepseek-ai/dsh-tool-ask-user` is mounted only inside the Web bundle's
  agent presets, and no preset is mounted on the ACP profile — the composed tree
  is 95 rows and none of them is that tool. A probe confirms the consequence in
  both directions: with the tool unmounted the model cannot ask at all, and with
  the tool mounted but no answerer it fails with
  `Error: no user-questions answerer accepted the request`.
- **The seam is a waterfall with no answerer.** `ctx.userQuestions.ask()`
  dispatches `user-questions/request` through Cordis, and an unscoped listener
  registered by any profile plugin is admitted (`dsh-scope`'s routing filter
  returns true for a listener with no scope tag). A listener that returns
  `{ answers: [...] }` settles the call; one that returns `next()` declines.

The last point is the whole opening: the model-facing tool is a package, the
answerer is a listener, and both can be mounted by a `--patch` overlay — the
documented profile extension mechanism the gateway already drives the child
through.

## Decision

The gateway ships the missing answerer as its own DSH plugin and mounts it, per
child, with an overlay it generates.

1. `internal/dshplugin` embeds a single-file Cordis plugin (`answerer.mjs`), no
   imports beyond Node's own builtins, which registers one
   `user-questions/request` listener. It is written into the gateway's state
   directory together with a patch overlay that mounts two rows: the harness's
   `ask_user_question` tool, and the plugin.
2. The agent host — the process that composes the child's command line — starts
   the child with `--patch <overlay>` and with
   `DSH_GATEWAY_QUESTIONS_ENDPOINT_FILE` in its environment. The gateway writes
   that file (0600, atomically) at startup: a loopback URL, a bearer token minted
   for this process, and the question window.
3. The plugin forwards each question to `POST /internal/questions` and returns
   the answer to the waiting tool call. It mints the request id once and reuses
   it across retries, so a redeploy mid-question re-parks the same card instead
   of asking a second time.
4. `internal/app/questions` parks questions, announces them as
   `question.requested`, and resolves them as `question.resolved`. `GET
   /questions` and `POST /questions/{id}` are the phone's half; the sheet itself
   is `web/src/views/question.ts`.
5. Silence is not a decision. An approval nobody answers is refused, because the
   safe reading of "no" is "do not run that tool". A question nobody answers is
   withdrawn — `answeredBy: "timeout"` — and the model is told plainly that
   nobody answered, which is the honest outcome for a request that only ever
   carried information.

## Rationale

**Why the harness's own tool rather than one of ours.** The alternative was an
MCP server declared in `session/new`, which is a published ACP field and needs no
DSH plugin at all. It works, and it was rejected for what the model sees: an MCP
tool arrives as `mcp__<server>__ask_user_question`, with a description and answer
shape the gateway invents, so the phone and the desktop would ask the model for
the same thing in two different dialects. Reusing the seam means one vocabulary —
including the `(Recommended)` convention and the `detail` field a plan review
travels in.

That last field is not hypothetical. `@deepseek-ai/dsh-plan-mode` registers
`exit_plan_mode`, and that tool asks through `ctx.userQuestions.ask()` with the
finished plan as `detail` and `intent: { kind: "plan-review", approve: … }`. The
plan-mode row is mounted on the `acp` profile, so a plan review raised there
reaches the phone through the same listener with no extra work: the card shows
the plan and the two options, and the answer comes back as the labels the model
named. The intent tag itself is not carried — a client that does not know a tag
is specified to render the generic option list — which is what the phone does.

**Why a generated overlay rather than a profile edit.** The overlay is written
from the binary, so the running plugin cannot drift from the gateway that wrote
it, nothing has to be placed by hand, and an operator's own
`~/.dsh/profiles/acp/cordis.patch.yml` is left alone. It also keeps the two
processes honest: the host that mounts the answerer and the gateway that answers
it derive their halves from one function of one configuration.

**Why the endpoint is a file, not an environment variable.** The token has to
change when the gateway restarts, and the child is built to outlive exactly that
event. A path in the environment is stable; the document behind it is re-read per
question, so a redeploy costs a retry rather than an outage. The file is 0600 and
the plugin and gateway run as the same user.

**Why the plugin must not serialise the request.** `request.agent` is a live
Cordis context and `request.agent.id` is the session id. Treating the context as
data throws `cannot get property "toJSON" without inject` from the context proxy
and fails the tool call at the one moment that matters. That was found by
running it, and `internal/dshplugin`'s test pins it.

## Consequences

- **The gateway now names a package shipped by DeepSeek Harness.** If
  `@deepseek-ai/dsh-tool-ask-user` is renamed or removed by an upgrade, the row
  fails to resolve and the child refuses to boot — the failure mode this project
  has otherwise avoided on purpose. `dsh.questions.enabled: false` starts the
  child without the overlay, and is the documented way back.
- **The capability is mounted per child, not per session.** Every session the
  gateway drives can ask; there is no way to turn it on for one conversation.
- **A question asked over the gateway is answered over the gateway.** The answer
  arrives on the phone, and the same session open on the desktop is a different
  process with its own answerer — which DSH's single-writer lock already makes
  mutually exclusive for a gateway-held session.
- **Subagents still cannot ask.** DSH rejects a delegated child's request with
  `DELEGATED_CALLER`, by design; the child must report the unresolved question in
  its result, and the gateway does not change that.
- **The question window is the gateway's, not DSH's.** The tool declares no
  deadline of its own, so `session.questionTimeout` (10 minutes by default) is
  what bounds a question, and the plugin waits slightly longer than that so the
  card is never still on screen after the model has been told it expired.
