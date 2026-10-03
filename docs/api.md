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

`GET /me` returns the calling device plus what this deployment offers, so a
client does not have to probe and fail:

```json
{
  "id": "dev_…", "name": "iPhone", "createdAt": "…", "expiresAt": "…",
  "limits": { "maxPromptBytes": 262144, "maxBodyBytes": 8388608,
              "maxImageBytes": 8388608, "transcriptPage": 200 },
  "features": { "transcript": true, "desktopUI": false, "imagePrompts": true,
                "approvalTimeoutSecs": 300, "sessionIdleTimeoutSec": 300,
                "approvalGrantTTLSecs": 1800, "promptQueueDepth": 4,
                "revertEnabled": false }
}
```

`approvalGrantTTLSecs: 0` means scoped approvals are off and a client must not
offer the choices that would create one; `promptQueueDepth: 0` means a mid-turn
prompt is refused rather than queued; `revertEnabled` says whether this
deployment will write to a workspace at all. Every optional surface defaults to
off, so a client that assumed a capability because a field was missing would be
offering a control that fails.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/me` | The calling device's principal, and the deployment's limits and features. |
| `GET` | `/devices` | Every enrolled device. |
| `DELETE` | `/devices/{id}` | Revoke a device. Idempotent. |

Revoking the calling device's own id is allowed and takes effect immediately.

## Notifications

Web Push, so a phone that is not looking still learns what it cannot afford to
miss: an approval waiting on a human, an approval that **expired** and was
therefore refused, a turn that finished after running longer than
`push.turnThreshold`, a turn that **failed** at any length, and the agent process
giving up. An expiry is the one worth calling out: the operator who missed the
first notification would otherwise never learn that the tool did not run.

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
| `POST` | `/sessions/{id}/prompt` | Send a prompt; queue it if a turn is running. |
| `POST` | `/sessions/{id}/cancel` | Interrupt the in-flight turn **and** drop the queue. |
| `DELETE` | `/sessions/{id}/queue/{turnId}` | Drop one waiting prompt. |
| `GET` | `/sessions/{id}/changes` | What the session changed, from its own log. |
| `POST` | `/sessions/{id}/revert` | Undo recorded changes. Off by default. |
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
{ "blocks": [
    { "type": "text",  "text": "why is the upload failing?" },
    { "type": "image", "mimeType": "image/png", "data": "iVBORw0KGgo…" }
] }
```

Response `202`:

```json
{ "turnId": "turn_…", "state": "running",
  "queuedAt": "2026-10-03T09:12:00.000Z", "startedAt": "2026-10-03T09:12:00.000Z" }
```

The answer is the whole ticket, not a bare id: `state` is `running` or `queued`,
and a queued ticket carries its 1-based `position`. A client that wants to know
whether its follow-up is waiting or working does not need a second request.

**A prompt that arrives while a turn is running is queued, not refused.** It runs
when the current turn settles, and the gateway republishes tickets whenever the
queue moves. `session.promptQueueDepth` bounds the queue (default 4); a fuller
queue is `409 queue_full`, and a depth of `0` restores the strict behaviour, where
a mid-turn prompt is `409 prompt_in_flight`.

Images are the second block type. `data` is standard base64, padded or not, and
the decoded size is checked against `limits.maxImageBytes` — or against
`limits.maxBodyBytes` when that is `0`. Accepted types are `image/png`,
`image/jpeg`, `image/webp` and `image/gif`; anything else is
`400 unsupported_image_type`, and `GET /me` reports `features.imagePrompts`
so a client can avoid offering the control at all.

Prompt text is limited to `limits.maxPromptBytes`.

### `POST /sessions/{id}/cancel`

```json
{ "cancelled": true, "dropped": 2 }
```

Interrupts the running turn **and discards everything queued behind it**: "stop"
on a phone means stop, not stop this and immediately begin the next thing. The
answer reports what actually happened, so cancelling a session with nothing
running is a no-op rather than a claim to have stopped something.

### `DELETE /sessions/{id}/queue/{turnId}`

Removes one prompt waiting behind the running turn, without stopping the turn.
Answers the session's queue, so a client can update its rows without a second
request. `404 no_such_turn` when that prompt has already started or been dropped.

### `GET /sessions/{id}/changes`

What the session changed, projected from its own log.

```json
{
  "files": [
    { "path": "/Users/me/code/api/internal/cache/store.go",
      "display": "internal/cache/store.go",
      "added": 3, "deleted": 1, "edits": 2, "writes": 0,
      "binary": false, "truncated": false,
      "revertible": true, "reason": "",
      "hunks": [
        { "seq": 38, "callId": "call_…", "tool": "edit",
          "lines": [" func (s *Store) Get(k string) (V, bool) {", "-    return s.m[k]", "+    v, ok := s.m[k]", "+    return v, ok"],
          "added": 2, "deleted": 1, "wholeFile": false, "truncated": false }
      ] }
  ],
  "summary": { "files": 1, "total": 1, "added": 3, "deleted": 1, "edits": 2,
               "source": "tool-calls", "truncated": false },
  "revertEnabled": false
}
```

**This is a projection, not a workspace diff.** It is built from the file tools'
own recorded arguments — `edit` and `write` — so it is complete for what those
tools did and silent about everything else. A `bash` command that ran `sed -i`
changed a file; this does not claim to know that, and `summary.source` says so.
The gateway never reads the files it reports on, which is the property the rest
of the design rests on.

There are no line numbers on a hunk: the log records *what* an edit replaced, not
where in the file it landed, and inventing them would send a reader to the wrong
place. `lines` are prefixed with `+`, `-` or a space.

`deleted` counts what the log recorded, so a file overwritten whole reports its
additions and no deletions rather than a guess.

`revertible` and `revertEnabled` are two facts, not one. Per file, `revertible`
is the projection's own verdict: the log records enough to reverse this file.
Top-level, `revertEnabled` says whether this deployment will act on that at all.
**A client must require both before offering undo** — they are separate so that a
read-only gateway does not report every file as irreversible, which is what a
single ANDed field did: `revertible: false` with an empty `reason` told a client
nothing about which of the two conditions held. Whenever `revertible` is false,
`reason` says why.

A session with no readable log answers with an empty projection rather than an
error; a log from a newer DSH answers `200` with `"unsupported": true`, the same
degradation the transcript uses.

### `POST /sessions/{id}/revert`

```json
{ "paths": ["/Users/me/code/api/internal/cache/store.go"] }
```

Undoes recorded changes. `paths` restricts it; omitting it undoes every
reversible file.

```json
{ "files": [ { "path": "…", "display": "internal/cache/store.go",
               "status": "reverted", "replacements": 2, "reason": "" } ],
  "reverted": 1, "refused": 0, "skipped": 0 }
```

`503 revert_disabled` unless the operator set `changes.revert.enabled`. This is
the **only endpoint in the gateway that writes to your files**, and everything
about it is conservative:

* `409 session_busy` while a turn is running here, and `409
  session_running_elsewhere` while another process drives the session. Undoing
  files underneath a running agent produces a tree that matches neither what the
  agent wrote nor what it held before.
* A change is reversed only when the text it replaced is present in the file
  **exactly once**. A file that has moved on is `refused` with a reason, never
  fuzzy-matched.
* A whole-file write is refused: the log does not record what the file held
  before it. So is a change that deleted text, and for the same reason — there is
  no position to put it back at.
* Only files inside the configured workspace roots are touched, with symlinks
  resolved first, so a link inside the workspace cannot point out of it.

The answer is per file, and there is no transaction: a half-undone tree with a
report naming which files came back is more useful than a rollback that hides
what it touched. Every file undone is recorded in the audit log as
`workspace.reverted`.

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

A message that carried images reports how many on the item:
`"attachments": 2`. The image itself is deliberately not projected — a transcript
that carried every image anyone ever attached would be tens of megabytes for a
phone to scroll, and the log remains the place to look one up in full. What
matters for reading history is that a prompt was not only its words; a prompt
that was *only* an image appears with an empty `text` and a non-zero
`attachments`.

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
      "seq": 2, "role": "user", "tool": "", "field": "text",
      "snippet": "…构建缓存一直不生效，怎么办？…", "time": "…" }
  ],
  "scanned": 2,
  "truncated": false
}
```

`field` names which part of the row matched — `text`, `tool`, `input` or
`output` — and it is there because those four mean different things to a reader.
A hit in `output` is the answer to "which session printed this stack trace", and
a result list that did not distinguish it from something the operator typed would
send them looking in the wrong place. Tool output is searched deliberately: the
call that produced a result says nothing about what came back, so it is the only
place that question can be answered. The parts are tried in that order, so a
phrase appearing both in a prompt and in some tool's output is reported as the
prompt.

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
`approval.requested` event and blocks the agent. The decision comes from a human;
if none arrives within `session.approvalTimeout` the request is **rejected**.
Nothing is approved that no human authorised.

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
               { "id": "reject-once", "name": "Reject" },
               { "id": "allow-session-tool", "name": "Allow bash in this session", "grant": true },
               { "id": "allow-exact", "name": "Allow this exact call", "grant": true } ]
}
```

The first two are DSH's own. The last two are **synthesised by the gateway** and
present only when `session.approvalGrantTTL` is greater than zero: choosing one
records a standing authorisation — scoped to this session, this tool, and for
`allow-exact` these exact argument bytes — and answers DSH with `allow-once`,
which is the only affirmative option it has. The lifetime is bounded by
`session.approvalGrantTTL`, and §6.1 of [security.md](security.md) sets out what
this changes about the threat model. `session.approvalGrantTTL: 0` removes the
options entirely.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/approvals` | Pending approvals across all sessions. |
| `POST` | `/approvals/{id}` | Decide: `{ "optionId": "allow-once" }`. |
| `GET` | `/approvals/grants` | Standing authorisations currently in force. |
| `DELETE` | `/approvals/grants/{id}` | Withdraw one; the next match asks again. |

Deciding an unknown, expired, or already-decided approval returns `409
approval_closed`. Choosing an option the request was not shown with returns `400
unknown_option` and leaves the request open, so a malformed client cannot resolve
it by accident.

A grant is listed as `{ id, sessionId, tool, scope, summary, createdAt,
expiresAt, uses }`, where `scope` is `tool` or `exact`. Grants live in memory,
die with their session, and are never written down — a rule that outlived a
restart would be one nobody remembers agreeing to.

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
| `approval.resolved` | `data = { id, optionId, decidedBy, tool, sessionId, grantId }`. `decidedBy` is `operator` when a person answered, and `timeout` or `shutdown` when the tool was refused because nobody did — a client that rendered those the same way would tell the operator their agent stopped for a reason it did not. |
| `approval.granted` | A standing grant answered a request, so no prompt was shown. `data = { grant, tool, input }`. It is separate from `approval.resolved` because the two say different things: one is "you decided", this is "a decision you made earlier applied here". |
| `turn.state` | `data = { turnId, state: "queued" \| "running" \| "completed" \| "cancelled" \| "failed", position?, queueDepth?, queuedAt?, startedAt?, stopReason?, detail? }`. `startedAt` is sent on every state including the settled ones, because a phone that reconnects mid-turn needs the start rather than the duration so far — the event that announced it may be long past the replay window. A queued ticket is republished whenever the queue moves. |
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

`turn.state` is published by both routes too, and they fill in different amounts
of it: the watcher cannot know when a turn it did not start began, so `startedAt`
is absent on a turn the desk is driving. Everything else in the payload is
bounded by what the log records.

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
