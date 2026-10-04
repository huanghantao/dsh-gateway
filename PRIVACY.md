# Privacy

This is a self-hosted tool that reads your AI conversations off your own disk and
puts them on your own phone. Nothing is sent to the project's author, and there
is no telemetry, analytics, crash reporting or update check anywhere in the code.
That is worth saying plainly because it is unusual, and because the interesting
question is not whether *this project* collects data — it does not — but which
*third parties* a deployment ends up involving.

This document lists every one of them. It describes the default configuration;
where a feature is off unless you turn it on, that is stated.

## What never leaves your machine

- **Your sessions.** The gateway reads DSH's session store on disk and serves it
  to paired devices over your own tunnel. No session content is uploaded
  anywhere. The transcript projection is read-only and is documented in
  [ADR 0002](docs/adr/0002-project-the-session-log-read-only.md).
- **Your files.** The agent reads and writes them under DSH's own sandbox; the
  gateway passes tool calls through and shows you their arguments so you can
  approve them. It reads your files only when you turn undo on, and even then
  only the files a session recorded a change to. A photograph you attach to a
  prompt is sent to the harness as part of that prompt, and the transcript
  projection keeps a *count* of attachments rather than the image itself.
- **What a session changed.** The change screen is a projection of the session
  log — the file tools' own recorded arguments — so it is answered without
  reading your workspace at all. See
  [ADR 0006](docs/adr/0006-project-changes-and-separate-undo.md).
- **Search queries.** Search is a bounded read-only scan over the same local
  files, with no index built or persisted ([`internal/sessionlog/search.go`](internal/sessionlog/search.go)).
  The query travels in a URL query string, which the access log deliberately does
  not record, and the shipped Caddy configuration has no `log` directive.
- **Device identity.** A paired device is a name you choose, a timestamp, and a
  SHA-256 of a 256-bit random token. The token itself is never stored, and the
  user agent is kept only so the device list can tell two phones apart.

## Third parties a deployment can involve

| Who | What they receive | Default | How to avoid it |
|---|---|---|---|
| **Your model provider** (DeepSeek, or whatever your `acp` profile points at) | Every prompt, tool call and file the agent reads | On — it is the product | Use a local model |
| **Your push service** (Apple, Google, Mozilla — whichever the browser names) | An **encrypted** payload, plus delivery metadata: endpoint, timing, frequency, size | On | `push.enabled: false` |
| **A chat webhook** (Feishu today) | The notification card in **plain text**: title, body, and a link containing your gateway URL and session id | **Off** — `push.webhooks` is empty | Leave it empty |
| **Your VPS provider** | The tunnel's traffic, which is TLS and not readable; connection metadata | On | Self-host |
| **sslip.io**, if you use it | The DNS lookup for your hostname | Only if you use an sslip.io domain | Use a domain you control |

### Push notifications

The payload is encrypted end to end to the browser's own key (ECDH P-256, HKDF,
AES-128-GCM, per RFC 8291), so the push service relays bytes it cannot read. This
is tested against the RFC's published vector — see
[`internal/push/encrypt.go`](internal/push/encrypt.go).

What the push service still learns is metadata: which endpoint, when you are
notified, how often, and how large the payload is. That is inherent to push and
no amount of work on this end changes it. The VAPID subject is configurable
(`push.subject`); set it to a real address you control, because some services
require a contact and the shipped placeholder is not one.

**What the notification says.** A notification is titled with the session's own
name, because a title is the one field a lock screen always draws and a
notification that cannot say *which* conversation it is about is one the reader
has to open to identify. For a session with no title the name falls back to the
first line of the prompt that opened it. The rest of the lock-screen text is
three facts — which agent settled (the session's own agent, or the gateway
itself), how it went, and what the work amounted to as a count ("12 tool calls ·
3 files changed · 2 delegations"). None of that is text you typed: it is either
the harness's own vocabulary or a count of tool calls.

A delegated child is not something a notification is about, so the one piece of
generated text that used to reach a lock screen — the task description the model
wrote for a child — no longer has a path there at all. It still appears in the
app, on the settlement row for the child it belongs to.

**What a lock screen never carries** is the model's answer. A settled turn's
closing message is the body of a *chat* card (see below); the push payload has
none of the room for it — the record is capped at 3000 bytes — and none of the
privacy, being read by whoever picks the phone up. The notifier does not even
hold the text unless a chat channel has asked for it.

Setting `push.includeSessionName: true` adds the session's title to the few
notification *bodies* that would otherwise say it — the approval cards. It does
not govern the title, which names the session either way.

### Chat webhooks

`push.webhooks` is empty by default, and that is a change from earlier revisions
that shipped a placeholder Feishu hook. A webhook posts the notification in
clear to whichever service it names —  the encryption above does not apply — so
enabling one is a decision to send that text to a third party. It is not a
decision this project should make on your behalf.

A card posted to a group is the one place model output leaves this machine. Its
body is the model's own closing message for the turn, printed as markdown, capped
at `maxAnswerChars` (4000 by default, with the head, the tail and an exact count
of what was dropped when an answer is longer). It is whatever the model wrote —
which is to say whatever the files, the tools and the web put in front of it —
and it is sent to a chat service that stores it and shows it to everyone in the
group. Set `includeAnswer: false` on the webhook to keep the one-line body
instead.

If you do enable one, remember the webhook URL is a capability: anyone holding it
can post into that chat. It is stored in `config.yaml` (0600) and never echoed
back to a client.

### Your VPS

The VPS terminates TLS, so it carries your traffic without being able to read it,
and it holds Caddy's local CA private key if you use `--tls internal`. That key
can impersonate any hostname to any phone that trusts the root, which is why
[`docs/security.md`](docs/security.md) §2.2 treats the VPS as trusted
infrastructure rather than a neutral relay.

frps logs client logins with source addresses to journald, so your home IP and
your connection times are recorded on the VPS.

## What is stored on your machine

| Where | What | Mode |
|---|---|---|
| `~/.dsh-gateway/devices.json` | Device names, SHA-256 token hashes, last-seen times | 0600 |
| `~/.dsh-gateway/pairing.key` | The secret every pairing code is derived from | 0600 |
| `~/.dsh-gateway/vapid.key` | The push signing key | 0600 |
| `~/.dsh-gateway/push.json` | Browser push endpoints and their keys | 0600 |
| `~/.dsh-gateway/audit.jsonl` | Security-relevant events, rotated at 8 MiB | 0600 |
| `~/.dsh-gateway/curation.json` | Session ids you archived or pinned — no content | 0600 |
| `~/Library/Logs/dsh-gateway/gateway.log` | Operational log | 0700 directory |

The state directory itself is created 0700, and `dsh-gateway doctor` warns if it
is not.

### The audit log

It records **no prompt text**. `prompt.sent` carries the device, the turn id, a
block count and whether the prompt was admitted as running or queued.
`workspace.reverted` — written only when undo is enabled — carries the device and
the paths it wrote to, because that is the record of a change this gateway made
to your files rather than one it observed. `approval.decided` carries the tool's name, the byte length of its
arguments and a SHA-256 of them — enough to tie a decision to the call it
authorised, not enough to be a second copy of your files.

It does carry your device id and client IP, and it is rotated rather than deleted.
It is not cleared when the session it refers to is deleted.

### Deleted sessions

Deleting from the phone **destroys nothing immediately**. The session's directory
is moved into `~/.dsh-gateway/trash/`, out of every listing, and is restorable
byte for byte for **30 days**, after which a sweep removes it with a plain
unlink. Two consequences worth knowing:

- A "deleted" conversation still exists on disk for 30 days, and `Purge` is an
  unlink rather than a secure erase.
- If your state directory is inside iCloud Drive, Dropbox or a Time Machine
  backup, the deleted session has been copied somewhere else and removing it here
  will not remove it there.

## What the phone stores

No transcript is cached. The service worker caches only the static app shell and
never intercepts `/api/`, so nothing the gateway serves is written to the browser
cache; history is read on demand, and the tab holds what it is showing in memory.

What does persist is the notification list, in `localStorage`, so that the
activity screen survives a reload:

- `dsh.activity.v1` — at most **200 rows**, none older than **30 days**. A row
  carries the session's *id* (never its name: that is resolved when the row is
  drawn), when it happened, which kind of event it was, who it was about, how it
  ended, a count such as "12 tool calls · 3 files changed", and the one sentence
  the notification showed.
- `dsh.activity.read.v1` — a single timestamp: how far you have acknowledged,
  which is all the unread badge is computed from.

Both are device-local and nothing in them is sent back to the gateway. **Three
fields can hold text the model wrote**, which is worth saying plainly rather than
calling the list "metadata": a delegated task's row carries the child's own
closing message (up to 400 characters of arbitrary model output) and is named by
the description the model wrote for that task — both read from the session's own
stream, and neither sent anywhere — and a failed turn's row carries the harness's
failure detail. No prompt you typed is stored, and no tool call's arguments are
kept: for a delegation, only that one-line description survives here.

Clearing site data removes both keys, and nothing else depends on them. Where
storage is unavailable — Safari in a private window, or a browser with storage
disabled — the list still works for as long as the tab lives and is simply not
kept.

The session credential is an HttpOnly cookie. The pairing response's bearer token
is discarded rather than stored. The one residue is that the pairing code sits in
the URL fragment after you pair, which stays in the phone's own history — a
fragment is never sent to a server, so this is local only.

## Children, and data you did not create

The gateway reads a DSH session store. If that store contains conversations with
or about other people — a transcript someone pasted in, a document the agent
read — then those are on your phone and in your audit-log fingerprints too. This
project has no redaction feature and cannot tell whose data is in a file. Treat
the deployment as covering everything DSH can reach: the workspace allowlist
decided at install time is the only boundary, and it is worth keeping narrow.
