# Gateway API v1

The mobile PWA and any future native app speak this contract. It is the only
interface clients depend on; the ACP adapter, the session-log projector, and the
desktop proxy behind it are implementation detail and may be replaced.

Base path: `/api/v1`. All bodies are JSON unless stated otherwise.

## Conventions

**Errors.** Every failure is an RFC 9457 problem document with
`Content-Type: application/problem+json`:

```json
{
  "type": "urn:dsh-gateway:problem:invalid_pairing_code",
  "title": "Unauthorized",
  "status": 401,
  "detail": "the pairing code is not valid or has expired",
  "instance": "req_1f0c…",
  "code": "invalid_pairing_code",
  "retryable": false
}
```

`code` is the stable handle for client logic; `detail` is for humans and may be
reworded. `retryable` says whether retrying the identical request could succeed.

**Authentication.** After pairing, the gateway sets an `HttpOnly`, `Secure`,
`SameSite=Strict` cookie. Native apps may instead send
`Authorization: Bearer <device token>` — the same value the pairing response
returns once. Every endpoint below requires one or the other, except these, which
are deliberately reachable before pairing so that a deployment can be checked
from outside:

| Public endpoint | Aliases |
|---|---|
| `POST /pair` | — |
| `GET /healthz` | `GET /api/v1/healthz` |
| `GET /readyz` | `GET /api/v1/readyz` |

Both spellings of the health endpoints are served on purpose, not by accident: the
contract's base path is `/api/v1`, and an operator or a load balancer will try
both. Neither reveals anything — `/readyz` distinguishes "the harness is up" from
"the gateway is up", which an unauthenticated caller can already infer from
whether a pairing attempt is answered.

**Mutating requests** must additionally present an `Origin` header equal to the
request `Host`, or be rejected `403`. This is the CSRF control.

**Timestamps** are RFC 3339 with milliseconds, always UTC.

**Identifiers** are opaque strings. Do not parse them.

## Pairing

### `POST /pair`

Unauthenticated. Exchanges a one-time pairing code for a device token.

```json
{ "code": "K7M2QPX4", "deviceName": "iPhone 15" }
```

Response `201`:

```json
{
  "device": { "id": "dev_9a…", "name": "iPhone 15", "createdAt": "…", "expiresAt": "…" },
  "token": "3f9c…"
}
```

`token` is returned exactly once and is never recoverable. The response also sets
the session cookie, so a browser can ignore the field.

Errors: `401 invalid_pairing_code`, `429 pairing_locked`.

## Devices

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/me` | The calling device's principal. |
| `GET` | `/devices` | Every enrolled device. |
| `DELETE` | `/devices/{id}` | Revoke a device. Idempotent. |

Revoking the calling device's own id is allowed and takes effect immediately.

## Notifications

Web Push, so a phone that is not looking still learns the two things it cannot
afford to miss: an approval that expires, and a turn that ran longer than
`push.turnThreshold`.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/push/key` | The VAPID public key to subscribe with, and whether push is on. |
| `POST` | `/push/subscribe` | Record this device's subscription. |
| `POST` | `/push/unsubscribe` | Forget one endpoint. |
| `POST` | `/push/test` | Send one notification to this device now, and to every chat channel. |

```json
{ "endpoint": "https://fcm.googleapis.com/…", "p256dh": "…", "auth": "…" }
```

`GET /push/key` also reports `channels` — the chat channels the operator
configured, by service name — because on many phones Web Push cannot work at all:
Android's is Google's push service and nothing else. A chat channel is delivered
by the gateway itself (one HTTPS POST to the bot's incoming webhook), so it works
wherever the phone can receive that chat app's messages. `POST /push/test`
answers `{"sent": n, "channels": m}`: `sent` is browsers, `channels` is chat
channels, and either being non-zero means the test went somewhere.

The subscription is bound to the calling device, never to a device named in the
body: a phone must not be able to register itself as another phone, or revoking
one would not stop it. Revoking a device drops its subscriptions in the same
request. The payload is encrypted to the subscription (RFC 8291, `aes128gcm`)
and authorised with a VAPID token (RFC 8292), so the push service relays bytes it
cannot read. `POST /push/test` answers `409 push_not_subscribed` when this device
has nothing registered, which is what the settings screen turns into "turn
notifications on first".

## Workspaces and models

### `GET /workspaces`

The allowlisted workspace roots. The client picks one when opening a new session;
it may never supply an arbitrary path.

```json
{ "workspaces": [ { "path": "/Users/me/code/api", "name": "api", "exists": true } ] }
```

### `GET /models`

The harness's option catalog, passed through in ACP's shape rather than
flattened into named lists: an array of options, each with the values it accepts.
A client that wants a model picker reads the option whose `id` is `model`; a
reasoning-effort picker reads `reasoning_effort`. `current` is the value the
harness is using now, and a value's `id` is opaque — it is the exact string
`POST /sessions` and `PATCH /sessions/{id}` expect back.

```json
{
  "config": [
    {
      "id": "model",
      "name": "Model",
      "current": "[\"deepseek-official\",\"deepseek-v4-flash\"]",
      "options": [
        {
          "id": "[\"deepseek-official\",\"deepseek-v4-flash\"]",
          "name": "DeepSeek-V4-Flash",
          "label": "DeepSeek · DeepSeek-V4-Flash",
          "group": "DeepSeek",
          "description": ""
        }
      ]
    },
    {
      "id": "reasoning_effort",
      "name": "Reasoning effort",
      "current": "",
      "options": [
        { "id": "off", "name": "Off", "label": "Off", "group": "", "description": "" },
        { "id": "high", "name": "High", "label": "High", "group": "", "description": "" }
      ]
    }
  ],
  "defaults": {
    "model": "[\"command-code\",\"deepseek/deepseek-v4.1-flash\"]",
    "reasoningEffort": "max"
  }
}
```

`label` is the presentation string — `Group · Name` when the harness groups its
values, otherwise the name — and it is what the pickers show. The catalog is
empty until the harness has been asked for a session: ACP reveals it only when
one is created or resumed, so this response is a cache of the last observation
rather than a directory the gateway can query on demand. The last observation is
also remembered in `<stateDir>/models.json`, because otherwise every restart
answered with an empty catalog until someone opened a session, and the model
picker had nothing to offer — not even the configured default. A remembered
catalog can name a model the harness no longer offers; the next session that
attaches replaces it, and one that cannot be read is ignored rather than being
treated as fatal.

`defaults` is the gateway's own answer to "what does `POST /sessions` do when the
client omits these", from `session.defaultModel` and
`session.defaultReasoningEffort`; either is `""` when the operator has not
configured one, in which case the harness decides. It is reported separately
from `config` on purpose: a configured id can name a value the harness no longer
offers, and a client should be able to tell that apart from "there is no
default". The app preselects these values when they are in the catalog, and
leaves the picker on "Gateway default" when they are not.

## Sessions

A session maps one-to-one onto a DSH session. `leased` is the important field: a
session is only attached to the gateway's DSH process while it is leased, and
while leased the desktop cannot open it. See *Session leases* below.

```json
{
  "id": "session-4ef56f1f-…",
  "title": "Add retry to the uploader",
  "workspace": "/Users/me/code/api",
  "createdAt": "…",
  "updatedAt": "…",
  "leased": true,
  "busy": false,
  "pinned": false,
  "model": "[\"command-code\",\"deepseek/deepseek-v4.1-flash\"]",
  "reasoningEffort": "high"
}
```

`model` and `reasoningEffort` are value ids from `GET /models` — the strings
`PATCH /sessions/{id}` takes back — and either is omitted when the gateway has
nothing to report. It knows both while it holds the session, and it knows what
the session log recorded otherwise: the model that the last request ran on, and
the reasoning effort it ran at. That second source spells the model as the
harness's own id (`deepseek/deepseek-v4.1-flash`) rather than as the compound
value id above, because the log records the route's parts; a client that wants a
display name should match it against the catalog instead of assuming one shape.
An absent `model` means the gateway does not know it, never that the session has
none.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/sessions?workspace=&cursor=&limit=` | Persisted sessions, newest first. |
| `POST` | `/sessions` | Create a session. |
| `GET` | `/sessions/{id}` | One session's metadata. |
| `POST` | `/sessions/{id}/lease` | Attach the session; body `{}`. Idempotent. |
| `DELETE` | `/sessions/{id}/lease` | Detach and release the DSH write lock. |
| `GET` | `/sessions/{id}/transcript?before=&limit=` | Read-only history projection. |
| `GET` | `/sessions/triage` | Sessions the gateway suggests archiving, each with a reason. |
| `POST` | `/sessions/curate` | Archive, unarchive, pin or unpin a set of sessions. |
| `GET` | `/sessions/search?q=` | Free-text search inside session *bodies*. |
| `GET` | `/sessions/{id}/receipt` | What the session did: tokens, tools, files, duration. |
| `DELETE` | `/sessions/{id}` | Move a session to the trash. |
| `GET` | `/trash` | What is in the trash, newest first. |
| `POST` | `/trash/restore` | Put a trashed session back where it came from. |
| `POST` | `/sessions/{id}/prompt` | Send a prompt. |
| `POST` | `/sessions/{id}/cancel` | Interrupt the in-flight turn. |
| `PATCH` | `/sessions/{id}` | Change `model` or `reasoningEffort`. |

### `POST /sessions`

```json
{ "workspace": "/Users/me/code/api", "model": "…", "reasoningEffort": "high" }
```

Query parameters:

- `workspace` — only sessions under one allowlisted root.
- `state` — `active` (default), `archived`, or `all`. Archived sessions are out
  of the default list; nothing is ever deleted by archiving.
- `q` — free text over the title, the preview, the workspace and the id. It is
  matched by the gateway, not the client, because a client holds one page of a
  list that may be hundreds deep.
- `cursor` / `limit` — paging. The gateway scans as far as it needs to fill a page
  of *visible* sessions, so a request never comes back empty because everything
  on that page was archived.

Every session carries `archived`, `archivedOnDesk` and `pinned`. `archivedOnDesk`
means the desktop's own store is the reason — the phone shows it and can undo it
locally, but the desktop keeps its list.

`workspace` must be one of `GET /workspaces`. Response `201` is a session object,
already leased.

`model` and `reasoningEffort` are optional, and their values are the opaque ids
from `GET /models`. When one is omitted the gateway applies its own configured
default (`session.defaultModel` / `session.defaultReasoningEffort`, reported as
`defaults` by `GET /models`) before the first prompt; when that is unset too, the
harness's own default stands. An explicit value always wins, and an id the
harness rejects is a warning in the log, not a failed request: the session exists
and is usable either way.

### `GET /sessions/triage`

What a tidy-up would put away, and why, before it does anything:

```json
{
  "summary": { "scanned": 251, "archived": 3, "candidates": 131, "test": 74, "temp": 50, "draft": 7 },
  "candidates": [
    { "id": "session-…", "title": "Reply with exactly: probe-ok", "workspace": "/Users/me/work",
      "messages": 5, "verdict": "test", "reason": "the title reads like an automated run" }
  ]
}
```

The rules are structural — a title an automated run would write, a workspace in a
scratch directory, a session that was created and never used — and they need the
session's history to be readable before they will judge its content, so a gateway
with the transcript projection switched off proposes nothing rather than
everything. Length is the guard rail: a session in a scratch directory is left
alone past eight messages, a test-titled one past twelve.

### `POST /sessions/curate`

```json
{ "ids": ["session-…", "session-…"], "archived": true }
```

Exactly one of `archived` or `pinned` per request, and the answer is the state of
every id asked about, so a client can update its rows or offer an undo without a
second round trip. `archived: false` undoes an archive, including one the desktop
made. One request for a set rather than one per session, because the gesture that
produces it is "these 131".

### `DELETE /sessions/{id}`

Deletes nothing. The session's directory — log, lock and all — is moved into the
gateway's own trash, out of every listing and restorable byte for byte for 30
days, after which a purging sweep removes it. DSH has no delete of its own, and a
phone button that destroys a conversation is not something to build on a good
day. A session a live process is writing to is refused with `409 session_running`
rather than moved out from under the agent.

```json
{ "deleted": true, "id": "session-…", "deletedAt": "2026-10-01T02:11:04Z" }
```

### `GET /trash`, `POST /trash/restore`

```json
{ "trash": [ { "id": "session-…", "title": "把发布说明整理成文档", "deletedAt": "2026-10-01T02:11:04Z" } ] }
```

Titles are read from the trashed logs themselves — the same parser that renders a
live transcript — so a list of ids does not become a list nobody can act on.
`POST /trash/restore` takes `{ "id": "session-…" }` and answers `409
session_exists` if a session with that id is in the store again (the desktop may
have resumed it); overwriting a live session with an older copy is worse than
refusing.

### `POST /sessions/{id}/prompt`

```json
{ "blocks": [ { "type": "text", "text": "why is the upload failing?" } ] }
```

Response `202`:

```json
{ "turnId": "turn_…" }
```

The turn's progress arrives on the event stream. At most one prompt per session
may be in flight; a second returns `409 prompt_in_flight`.

Prompt text is limited to `limits.maxPromptBytes`.

### `GET /sessions/{id}/transcript`

A read-only projection of the DSH session log, so a phone can show history for a
session it has never attached to.

```json
{
  "items": [
    { "id": "…", "seq": 8, "time": "…", "role": "user", "text": "…" },
    { "id": "…", "seq": 15, "time": "…", "role": "assistant", "text": "…",
      "thinking": "…", "model": "deepseek/deepseek-v4.1-flash",
      "usage": { "inputTokens": 5195, "outputTokens": 2 } },
    { "id": "…", "seq": 38, "time": "…", "role": "tool", "tool": "bash",
      "input": "{\"command\":\"pwd\"}", "output": "/private/tmp", "isError": false },
    { "id": "…", "seq": 4, "time": "…", "role": "notice", "text": "turn started" }
  ],
  "nextBefore": 4,
  "truncated": false
}
```

`items` is oldest-first. `nextBefore` pages backwards; **`0` means there is nothing
older**, and a client that keeps requesting with `before=0` will be handed the newest
page again.

Only messages the operator actually sent appear with `role: "user"`. DeepSeek
Harness records its own injections — a runtime-context snapshot and the installed
skill catalog — as user-role messages in the same log, and those are filtered out.
The catalog alone runs to tens of kilobytes and would bury a short exchange. When the on-disk format is
newer than this build understands, the endpoint answers `200` with
`"unsupported": true` and no items rather than failing — the client shows
"history unavailable, open the desktop GUI" instead of an error.

### `GET /sessions/search?q=`

Free-text search **inside** session bodies, which the list's `q` does not cover:
that one matches metadata only.

```json
{
  "matches": [
    { "sessionId": "session-…", "title": "构建缓存问题", "workspace": "/Users/me/code/api",
      "seq": 2, "role": "user", "tool": "",
      "snippet": "…构建缓存一直不生效，怎么办？…", "time": "…" }
  ],
  "scanned": 2,
  "truncated": false
}
```

It is a bounded scan, not an index: `internal/sessionlog/search.go` reads the logs
in memory, matches case-insensitively, and caps both the sessions scanned and the
matches returned. An index would have to be built, persisted, invalidated when the
desktop writes to a log this process is not watching, and would then sit on disk
as a second copy of every conversation — which is why there is not one, and why a
search on a large history is slower than a client-side filter would be. The
trade-off is deliberate.

Search is restricted to allowlisted workspaces, like every other read. `truncated`
says the caps were hit and there are more matches than were returned.

### `GET /sessions/{id}/receipt`

What a session actually did, derived from its own log — the "what did this cost me"
panel.

```json
{
  "rows": { "Messages": 47, "Tokens": "128.4k in / 9.2k out", "Tools": 23, "Files": 9, "Duration": "47m" },
  "tools": [ { "name": "bash", "count": 14 }, { "name": "edit", "count": 5 } ],
  "files": ["internal/config/config.go", "…"],
  "usage": { "inputTokens": 128400, "outputTokens": 9200, "cacheReadTokens": 96100 },
  "cost": { "amount": 0.42, "currency": "CNY" },
  "pricing": "configured"
}
```

`cost` is present **only** when the operator configured `receipt.pricing` for the
model the session ran on; the gateway will not invent a price, and it does not
fetch one, because the authoritative numbers carry an account's discount and a
price that changed under an old session would rewrite history. `pricing` reports
which of those two situations applies, so a client can say "no price configured"
rather than showing a bare `0`.

DSH's own log does not record reasoning tokens separately from output tokens, so
they are reported together.

## Approvals

When DSH asks permission to run a tool, the gateway publishes an
`approval.requested` event and blocks the agent. The decision must come from a
human; if none arrives within `session.approvalTimeout` the request is **rejected**.
Nothing is ever auto-approved.

```json
{
  "id": "apr_…",
  "sessionId": "session-…",
  "toolCallId": "call_…",
  "tool": "bash",
  "input": "{\"command\":\"rm -rf build\"}",
  "requestedAt": "…",
  "expiresAt": "…",
  "options": [ { "id": "allow-once", "name": "Allow once" },
               { "id": "reject-once", "name": "Reject" } ]
}
```

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/approvals` | Pending approvals across all sessions. |
| `POST` | `/approvals/{id}` | Decide: `{ "optionId": "allow-once" }`. |

Deciding an unknown, expired, or already-decided approval returns `409
approval_closed`.

## Event stream

### `GET /events` (WebSocket)

One multiplexed stream for every session. Query parameters:

- `since` — replay events after this sequence number. Use the last `seq` seen.
- `session` — restrict to one session id. Repeatable; omit for all.

Server frames:

```json
{ "seq": 42, "time": "…", "type": "session.update", "sessionId": "session-…", "data": { … } }
```

| `type` | Meaning |
|---|---|
| `hello` | First frame. `data` = `{ "deviceId": …, "replayFrom": … }`. |
| `session.state` | Session metadata changed (leased, busy, title, model). |
| `session.message` | A committed message. `data = { id, role: "user" \| "assistant", text }`. `id` matches the transcript item id for the same message, so a client folding live events into fetched history can dedupe exactly rather than by comparing text. `role: "user"` means the prompt was typed somewhere else — the desktop, a headless run — and this client is watching it. |
| `session.thought` | A committed reasoning block, shown collapsed. `data = { id, text }`. |
| `session.tool` | Tool lifecycle. `data.phase` = `start` \| `end`, plus `callId`, `tool`, `status`, `input`, `output`, `isError`. |
| `usage.update` | Context occupancy, **not** per-step token counts. `data = { used, size, fraction }` where `size` is the model's context window. Per-message token accounting is on transcript items. |
| `approval.requested` | See above. |
| `approval.resolved` | `data = { id, optionId, decidedBy }`. |
| `turn.state` | `data = { turnId, state: "running" \| "completed" \| "cancelled" \| "failed", stopReason }`. |
| `harness.state` | The DSH child process: `data = { state: "starting" \| "ready" \| "restarting" \| "failed", detail? }`. |
| `resync` | Events were dropped for this subscriber. Refetch transcripts and session metadata. |

Client frames:

```json
{ "type": "subscribe",   "sessionId": "session-…" }
{ "type": "unsubscribe", "sessionId": "session-…" }
{ "type": "ping" }
```

`seq` is globally monotonic across the gateway's lifetime, and a reconnect with
`since` replays from the in-memory ring buffer. A filtered connection therefore
sees gaps in `seq`; that is expected, and the value to resume from is always the
last `seq` actually received.

Two things publish here, and which one applies depends on who is running the
session:

- **Sessions this gateway drives** (opened or prompted from the phone) report
  their activity over ACP, as it commits.
- **Sessions another DSH process runs** (the desktop GUI, `dsh headless`, a
  terminal) publish nothing over ACP, so the gateway follows their log instead
  (`transcript.follow`) and emits the same event types from it. A session the
  gateway holds is explicitly excluded, so nothing is reported twice.

Both routes produce the same row ids as `GET /sessions/{id}/transcript`, which is
what lets a client interleave them without duplicating a message.

If `since` is older than the buffer, the server sends `resync` first. A `resync`
frame carries `seq: 0` because it has no position in the stream — do not bookmark
it as a resume point.

A subscriber that cannot keep up loses the oldest events and receives `resync`
rather than slowing the agent down. The DSH reader is never blocked by a slow
phone.

## Session leases

DSH enforces **one live writer per session** with a kernel file lock. A session
attached to the gateway's DSH process therefore cannot be opened on the desktop,
and vice versa.

`pinned` keeps a lease alive indefinitely; without it, a lease is reclaimed after
`session.idleTimeout` even if a phone still has the conversation open.

The gateway makes this explicit rather than hiding it:

- Attaching happens on `POST /sessions/{id}/lease`, and implicitly when creating a
  session or sending a prompt.
- The lease is released by `DELETE /sessions/{id}/lease`, when the last event
  subscriber disconnects, or after `session.idleTimeout` with no subscriber and no
  prompt in flight.
- `GET /sessions/{id}` reports `leased`, so a client can explain *why* the desktop
  may refuse to open the session.

## Health

These are mounted at the site root, not under `/api/v1`.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/healthz` | none | Liveness. Always `200` while the process serves. |
| `GET` | `/readyz` | none | Readiness. `503` until the DSH child completes its handshake. Also reports `droppedEvents`, which climbing means a client is falling behind. |
