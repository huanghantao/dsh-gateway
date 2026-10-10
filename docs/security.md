# Security model and threat model

For a technical reader who wants to know exactly what this deployment exposes,
what stands between the internet and a shell on their Mac, and where the honest
gaps are.

Read [deployment.md](deployment.md) for the walkthrough and [api.md](api.md) for
the client contract. Everything below describes the shipped defaults; where this
document and the code disagree, the code wins.

---

## 1. What the gateway actually is

`dsh-gateway` puts a DeepSeek Harness installation — an AI coding agent that runs
commands, edits files and drives tools on your machine — behind an HTTP API that
a phone browser can use.

The API is not the dangerous part. The dangerous part is that the process on the
other side of it will execute what the agent decides to execute. So the gateway
is not "a web app with an agent feature": it is a **remote control for a shell**,
and it is designed accordingly:

* It has no listener on any public interface, ever (§3).
* It has exactly one credential type — a device token — and no user accounts,
  no password reset, and no administrative endpoint that mints credentials.
* It never approves a tool call that no human authorised: a decision is required,
  and a missing decision is a rejection. A decision may name a *scope* — this
  tool, in this session, for a bounded time — which is a change from earlier
  versions and is set out in §6.1.
* Its blast radius is bounded by an allowlist of workspace roots, an approval
  prompt for anything the sandbox does not permit, and a device record you can
  revoke in one request.
* It writes to your files only if you turn that on, and then only to undo a
  change the session recorded (§1.1).

What it is *not*: a multi-tenant service, a sandbox for untrusted users, or a
safe way to give someone else access to your machine. Every paired device has
your full authority, minus what the agent's sandbox and approvals withhold. There
is no per-device least privilege to grant.

### 1.1 The one path that writes

Everything else in this document assumes the gateway is read-only over your
workspace, and until now that was unconditionally true: it read DSH's session
logs, and it never opened one of your files for writing.

One feature changes that, and it is off by default. `changes.revert.enabled`
lets an operator undo the file changes a session recorded, which means the
gateway can write inside the configured workspace roots. Three things bound it:

* **It is absent, not disabled.** With the setting off, the gateway constructs no
  reverter at all and `POST /sessions/{id}/revert` answers `503
  revert_disabled`. There is no reachable code path that writes.
* **It refuses rather than guesses.** An undo reverses a change only when the
  text the change recorded is present in the file *exactly once*. A file that has
  moved on since — because you edited it, or another tool did — is refused with a
  reason, not fuzzy-matched. A whole-file write is refused outright, because the
  log does not record what the file held before it. The only files it will touch
  are ones inside the allowlist, with symlinks resolved first so a link inside the
  workspace cannot point out of it.
* **It refuses while the agent is working.** A turn in flight, or another process
  driving the session, is a `409`: undoing files underneath a running agent
  produces a tree that matches neither what the agent wrote nor what it held
  before.

Every undone file is recorded in the audit log as `workspace.reverted`, with the
paths and the device that asked. That event is the only audit record describing a
change *this gateway made* to a workspace rather than one it observed, which
makes it the first place to look when a file is not what someone left it as.

Reading what a session changed needs none of this: `GET
/sessions/{id}/changes` is a projection of the session log and never touches the
workspace.

---

## 2. Trust boundaries

```
┌────────┐  ①HTTPS (internal CA)  ┌───────┐  ②loopback  ┌──────┐
│ phone  │ ─────────────────────► │ Caddy │ ──────────► │ frps │
└────────┘                        └───────┘             └──┬───┘
                                                           │ ③frp tunnel
                                                           │ TLS + token
                                                           │ + payload encryption
                                                        ┌──▼───┐
                                                        │ frpc │
                                                        └──┬───┘
                                                           │ ④loopback
                                                        ┌──▼──────────┐
                                                        │ dsh-gateway │ ⑤spawns
                                                        └──┬──────────┘
                                                           │
                                                      ┌────▼─────┐
                                                      │ dsh --   │
                                                      │ profile  │
                                                      │ acp      │
                                                      └──────────┘
```

| # | Hop | Authenticated by | Confidentiality |
|---|---|---|---|
| ① | phone → Caddy | a certificate from Caddy's local CA (`tls internal`), installed on the phone once; not publicly trusted, and not validatable without that install | TLS 1.2/1.3 |
| ② | Caddy → frps | nothing — both are loopback on the same host, and neither is reachable from outside | none needed |
| ③ | frps ↔ frpc | tunnel token (HMAC-style token auth) | TLS (`transport.tls.force`, auto self-signed server cert) plus a second encryption layer keyed by the token (`transport.useEncryption`) |
| ④ | frpc → gateway | nothing — loopback on the Mac | none needed |
| ⑤ | gateway → `dsh` | nothing — a child process the gateway spawned itself | inherited |

Two credentials exist in the whole system:

* **The device token** — 256 bits from `crypto/rand`, presented by the phone as a
  cookie or `Authorization: Bearer` header, verified by the gateway. Only its
  SHA-256 is stored.
* **The frp token** — 256 bits of hex, shared by frps and frpc, verified by frps
  for the tunnel only.

Plus one short-lived secret: the **pairing code** (§5.3), and plus one signing
key that is not an API credential at all: **the private key of Caddy's local
CA**, which every phone that installed the root (§2.2) will trust to vouch for
any hostname.

They are deliberately independent: compromising the tunnel does not let you
impersonate a paired phone, and revoking a phone does not disturb the tunnel.

### 2.1 What each component trusts

* **Caddy** trusts the public internet to send it bytes and nothing else. It is
  the only public listener.
* **frps** trusts anyone who knows the token to register a proxy — but see §4 for
  what `allowPorts` and `proxyBindAddr` restrict that to.
* **The gateway** trusts requests from loopback peers (`auth.trustedProxies` is
  `127.0.0.1/32` and `::1/128` by default) to carry honest `X-Forwarded-For` and
  `X-Forwarded-Proto`. Because only frpc and Caddy can reach it, and both run on
  the same host, a client cannot spoof its own address. Every rate limit and
  lockout is keyed by that address, which is why widening `trustedProxies` is a
  meaningful weakening rather than a cosmetic one.
* **The phone** trusts the certificate chain for the hostname — which, here,
  terminates in the CA root it installed from the VPS — and the code it was
  given. Installing that root has its own consequences (§2.2), and that install
  is a trust-on-first-use step: the bootstrap fetch is either TLS to a
  certificate the phone cannot verify yet, or plain HTTP that is not
  authenticated at all, so whoever answers for the name at that moment can
  substitute their own root (§9.6).

### 2.2 The CA the phone installs, and what trusting it means

Because such a provider blocks 80 and 443, no publicly trusted certificate is
available for the hostname: http-01 needs port 80, tls-alpn-01 needs port 443,
and dns-01 needs a DNS provider API that sslip.io does not offer. Caddy therefore
serves the site from its **own local CA** (`tls internal`), and each phone
installs the root once (deployment.md §5.1). Caddy serves that root from one file
in two places: over TLS at `https://<domain>:8443/ca.crt`, and in the clear at
`http://<domain>:8080/ca.crt`. The HTTPS one is the primary route, because a
provider of that kind intercepts plain HTTP by the `Host` header and answers a
hostname without an ICP filing with a 403 block page (`Server: ADM/2.1.1`). The
honest consequences:

* **The CA can vouch for anything.** A root installed on a phone is trusted for
  *every* hostname, not just this one. An attacker who holds the CA private key —
  which is to say, anyone who has the VPS — can present a valid certificate for
  any site that phone visits. That is what a MITM-capable root is, and it is why
  the install is a deliberate, one-device, revocable action rather than a
  convenience.
* **So the private key is the thing to protect.** It lives with Caddy's data
  directory on the VPS (`/var/lib/caddy/.local/share/caddy/pki`, owned by the
  `caddy` user). Anyone who can read it, back it up, or copy it off the host can
  impersonate any hostname to every phone that trusts the root. It belongs in no
  backup you would not also put the frp token in, and on no machine but the VPS.
  Compromise of the VPS itself is therefore a compromise of the phones' trust —
  §11 covers what to do about that.
* **The root itself is public**, so serving it is correct: it is not a secret, and
  a device that does not trust it yet cannot fetch it over a connection it does
  trust. That is also why the plain-HTTP publication is not the leak it looks
  like. It is not, however, dependable on a filtering provider: the filter can
  answer that URL with a block page instead of the certificate, which is why TLS
  is the primary route for the same public file.
* **The bootstrap fetch is trust-on-first-use either way.** Because `ca.crt` must
  be fetched before trust exists, the normal path asks the user to click through
  a browser warning and accept a certificate the phone cannot verify yet; the
  plain-HTTP path skips the warning but authenticates nothing at all. The
  security question is the same in both cases — is the user looking at the real
  server, or at an interceptor? — and the answer is "whatever the network says
  this name is". An attacker who can answer for the name, or control the
  sslip.io record (§9.6), can hand the phone *their* root, and from then on be a
  complete man in the middle for every hostname. Serving it over TLS does not fix
  that; it moves the same trust-on-first-use problem from an unauthenticated
  channel to an unverified one, and the only real fix is to take the network out
  of the fetch. Do that by installing the CA from a network you trust, or by
  copying the root out of band over SSH (deployment.md §5.1); if you use the
  plain-HTTP URL, do it before the first HTTPS visit, because HSTS rewrites that
  fetch afterwards (deployment.md §5.1, §8.6). Remove the profile when the
  deployment is retired (deployment.md §7.9).
* **The alternative is a domain you control with a DNS-01 challenge.** With a
  DNS provider API in Caddy's configuration, a publicly trusted certificate for
  your own name needs neither port 80/443 nor any install on the phone. This
  deployment does not do that because sslip.io offers no DNS API and using a real
  domain means owning DNS; `--tls acme` is the middle option, for providers that
  do not block 80/443.

---

## 3. Why the gateway binds loopback only

The gateway speaks plain HTTP and has no certificate of its own. If it listened
on a public interface, every prompt, transcript and device token would cross the
internet in clear text, and the loopback-only assumption that makes the forwarded
headers trustworthy would be false.

So the check is a startup invariant, not a default:

```
listen: "0.0.0.0:8787" is not a loopback address; the gateway must sit behind the
frp tunnel and a TLS terminator, never on a public interface
```

`internal/config.Validate` parses the host out of `listen` and requires
`ip.IsLoopback()`. A hostname like `localhost:8787` is rejected too, because the
check is on the parsed IP — the operator must write the literal loopback address.
This is the same class of guard as the `workspaces` requirement: a configuration
that would weaken an invariant is refused at startup, with all problems reported
at once, rather than honoured and discovered later.

The consequence is honest and worth stating: **the only way in is the tunnel.**
There is no "temporarily expose it for debugging" switch, by design. If you need
LAN access, put the gateway on loopback and reach the *tunnel* over the LAN, or
run WireGuard/Tailscale and keep the loopback bind.

---

## 4. Why frps's proxy port is not public

By default frps binds every proxy's remote port on `bindAddr` — `0.0.0.0` —
which would publish port 18787, and therefore the gateway, directly to the
internet. That would bypass Caddy entirely: no certificate, no HSTS, no
HTTP→HTTPS redirect, and the plaintext-HTTP origin exposed. Anyone scanning port
18787 would find a gateway with no TLS whose `Secure`-cookie logic depends on the
`X-Forwarded-Proto` header that no longer exists.

So `frps.toml` sets:

```toml
proxyBindAddr = "127.0.0.1"
```

Verified against frp v0.71.0: this key exists (`ServerCommonConfig.ProxyBindAddr`,
`pkg/config/v1/server.go`) and is honoured — a live test with `bindAddr =
"0.0.0.0"` showed frps listening on `*:7000` while the proxy port bound
`127.0.0.1:18787` only. The template carries a note explaining the fallback if a
future frp removes the key (bind publicly and add an explicit
`ufw deny 18787/tcp`), and frp's strict TOML parsing means a removed key is a loud
startup failure rather than a silent exposure.

Two supporting controls sit on the same server:

* `allowPorts = [{ single = 18787 }]` — even with a valid token, a client cannot
  bind any other remote port. Without it, a leaked token could be used to
  impersonate other services on the VPS or to hold ports hostage.
* `transport.tls.force = true` — plaintext clients are refused, so the token can
  never cross the wire unencrypted.

`ufw` opens the SSH port(s), the two Caddy ports (8080 and 8443 by default, or
whatever `--http-port`/`--https-port` selected), and 7000, and denies everything
else inbound. Note that 7000 *is* open: the Mac dials in from a residential
address, so there is no stable source to allow. It is protected by TLS and the
token, and `FRP_CONTROL_ALLOW_FROM=<cidr>` narrows it when you do have a fixed
address.

---

## 5. Credentials, and what a leak costs

### 5.1 The frp token

**Where it lives:** `/etc/frp/frps.toml` (0640 `root:frp`) and
`~/.dsh-gateway/frpc.toml` (0600). Generated once with `openssl rand -hex 32`;
the installer never regenerates it, because doing so silently breaks the tunnel.

**What a leak enables.** Anyone who can reach port 7000 and knows the token can
log into frps as a client. With `allowPorts` limited to 18787 and
`proxyBindAddr` on loopback, they cannot publish a new public port on the VPS.
What they *can* do is the serious part: **if the legitimate Mac is offline**, they
can claim remote port 18787, and Caddy will then forward every phone request to
*their* process. That is a full phishing position on the ingress path — serve a
fake pairing screen, harvest a code or a device token — and a full read of
request bodies sent by the phone.

What a leak does **not** give them: decryption of past traffic (`useEncryption`
keys the payload layer with the token, and the TLS session keys are separate),
access to the Mac (the tunnel only carries what frpc forwards, and frpc is the
legitimate side), or the device tokens already stored on phones.

**Rotate it** (deployment.md §7.7):

```sh
sudo openssl rand -hex 32                          # new token
sudo $EDITOR /etc/frp/frps.toml                    # auth.token = "<new>"
sudo systemctl restart frps
FRP_TOKEN=<new> bash deploy/mac/install.sh         # rewrites frpc.toml, restarts frpc
```

**Detection.** frps logs every client login with its source address
(`journalctl -u frps | grep "client login info"`); a login you cannot account for
is the signal. Because a token holder only wins the race while your Mac is
disconnected, a hijack also shows up as Caddy 502s on a Mac that is up.

### 5.2 A device token

**Where it lives:** on the phone (cookie `dsh_gw_session`, or a bearer token), and
as a SHA-256 hash in `~/.dsh-gateway/devices.json` (0600). The token itself is
never stored, so stealing the state file yields no usable credential. Verified
per-request comparison is constant-time.

**What a leak enables.** Everything the operator can do through the API, until it
is revoked or expires (30 days by default): list workspaces, open sessions in
those directories, send prompts, read transcripts and pending approvals, and —
this is the important one — **answer approval prompts**. Approvals protect you
from the *agent*; they do not protect you from someone who already holds your
credential, because the gateway's principal model is a device, not a scoped user.

**Revoke it** — the fast path is the app's device list
(`DELETE /api/v1/devices/{id}`, effective immediately, including for the calling
device). If the phone is lost and it was the only device, the local procedure is
in deployment.md §7.6: stop the gateway, set `"revoked": true` on that entry in
`devices.json` (or delete the file to drop every device), start it again.

**Detection.** `~/.dsh-gateway/audit.jsonl` records `auth.failed`
(unauthenticated or revoked credentials), `session.opened`, `prompt.sent` and
`approval.decided` with device id and client IP. A session you do not recognise,
or a prompt you did not send, is the signal. `lastSeen` in `devices.json` and the
app's device list are the cheaper daily check.

### 5.3 The pairing code

**What it is:** eight characters over a 31-symbol alphabet, ~39.6 bits, derived
as `HMAC-SHA256(pairing.key, time window)` — never stored, never transmitted by
the gateway, and printed only by `dsh-gateway pair` on the Mac. The previous
window is accepted, so a code is valid for between 10 and 20 minutes.

That entropy is only adequate because of the controls around it: the code is
short-lived, every attempt is rate limited with escalating lockout (5 failures,
then 30s doubling to 1h), redemption is audited, and the failure response is the
same whether the code was wrong or merely expired.

**What a leak enables.** Anyone who has the code *within its window* and can reach
the public URL can enrol a device — which then holds a 30-day token with your
full authority. Treat the code as a secret for its ten minutes; do not paste it
into a chat "for later".

**Detection and response.** `device.paired` in the audit log carries the device
name and client IP; a pairing you did not perform means the code leaked. Revoke
that device (§5.2), and if you are unsure how it leaked, rotate the pairing
secret — stop the gateway, delete `~/.dsh-gateway/pairing.key`, start it again
(the gateway creates a fresh one; all outstanding codes become invalid, and
existing devices stay valid, which is usually what you want).

### 5.4 What is *not* a credential here

* The gateway's session cookie is `HttpOnly`, `Secure` (whenever `publicURL` is
  HTTPS), and `SameSite=Strict`, and every mutating request is additionally
  checked against `Origin` — so a cross-site page cannot ride a paired browser's
  cookie.
* `X-Forwarded-For` is only believed from loopback, so a client cannot forge an
  address to dodge the lockout or pin someone else's audit trail.
* There is no administrative HTTP endpoint that mints credentials, no password
  reset, and no "re-print the code" route. Code printing is a local file read by
  a local process. That removes an entire attack surface, which is the reason it
  was designed that way.
* The CA private key (§2.2) is not an API credential either, but it is a signing
  key: a leak does not let anyone call the API, and it does let them impersonate
  any hostname to a phone that installed the root. Its handling is covered in
  §2.2 and §11.

---

## 6. Every tool is authorised by a human, once per scope, and fails closed

When the agent wants to run something the sandbox does not already permit, DSH
emits an approval request. The gateway publishes `approval.requested` and the
agent blocks until a human answers:

* `session.approvalTimeout` (5 minutes by default) bounds the wait, and **on
  expiry the request is rejected**. There is no "allow on timeout" path, and no
  code path that approves without a decision from a human.
* Deciding an unknown, expired or already-decided approval returns
  `409 approval_closed`.
* `approval.decided` — with the option chosen — is the most important record in
  the audit log, because it is the trace of a human authorising a command on
  their machine.

### 6.1 Scoped decisions, and what they change

DSH offers exactly two choices: allow once, and reject once. The gateway adds two
of its own, and this is the one place where the security posture of an earlier
version changed rather than being extended — so it is worth being precise about.

A turn that edits six files and runs a dozen commands asks a dozen times, each
with a five-minute clock, and one missed prompt is a refusal that stops the
agent. On a desktop that is an annoyance; on a phone it is the difference between
delegating work and babysitting it. So a decision may now carry a **scope**:

| Option | What it authorises |
|---|---|
| `allow-once` | this invocation (DSH's own) |
| `reject-once` | this invocation (DSH's own) |
| `allow-session-tool` | this tool, in this session, until the grant expires |
| `allow-exact` | this tool with exactly these arguments, in this session, until the grant expires |

The property that is preserved is the one that matters: **no tool runs that a
human did not authorise.** What changed is that the authorisation can name a
scope. A grant is bounded on three axes and none of them is optional — one
session, one tool (and for an exact grant, one identical argument string), and
`session.approvalGrantTTL` (30 minutes by default).

* Grants live in memory only. A gateway restart clears them, and a rule that
  outlived the process would be one nobody remembers agreeing to.
* A grant dies with its session: releasing the lease revokes every grant made in
  it, because an authorisation for work that is finished must not apply to
  whatever runs in that session next.
* Matching is on the exact argument bytes the harness reported. A grant that
  matched "the same command with different whitespace" would be one whose scope
  the operator cannot predict from what they were shown.
* `GET /approvals/grants` lists what is in force and `DELETE
  /approvals/grants/{id}` withdraws it. Every automated yes is announced as
  `approval.granted`, so the transcript shows that a tool ran without anyone
  answering *this* prompt, and the audit log records both ends.
* **`session.approvalGrantTTL: 0` disables scoped grants entirely.** The
  synthesised options are then not offered at all, and every invocation needs its
  own answer — the behaviour every earlier version had.

Residual risk, stated plainly: a tool-scoped grant authorises *any* invocation of
that tool in that session, including one nobody has seen. That is the trade, and
it is why the exact-argument scope exists beside it and why the lifetime is
short. An operator who wants the narrower posture should set
`session.approvalGrantTTL: 0`, or use `allow-exact` rather than
`allow-session-tool`. The app shows the remaining lifetime of every live grant
and offers to withdraw it.

The gateway also sets the child's sandbox mode explicitly
(`dsh.sandboxMode`, default `workspace-write`) rather than letting it inherit the
operator's shell environment. This matters more than it looks: DSH derives its
approval policy from that value, and `danger-full-access` becomes "never ask" —
so an exported `DSH_PERMISSION_MODE=danger-full-access` in a shell profile would
silently turn every approval prompt into an automatic yes, leaving the approval
UI apparently working while never asking anything. The gateway warns at startup
if that mode is configured.

Residual risk, stated plainly: an attacker holding a device token is
indistinguishable from you, so they can approve — and can create a scoped grant,
which is a standing authorisation. Approvals bound the *agent's* autonomy and
provide the audit trail; they are not a second factor.

### 6.4 Questions, and the one credential that is not a device

The agent can also stop and ask a question — a choice, a confirmation, something
it needs before it can continue. That capability does not exist on the ACP
surface, so the gateway supplies it: a small plugin of its own, mounted into the
harness child with a `--patch` overlay the gateway generates, which forwards each
question to the gateway over loopback
([ADR 9](adr/0009-answer-agent-questions-through-an-installed-plugin.md)).

Three properties of that path are security-relevant, and each is deliberate:

* **The bridge route takes a per-process bearer token, not a device credential.**
  `/internal/questions` is the only route a client cannot reach with a cookie or
  a paired-device token. The token is 256 bits minted at startup, written to
  `<stateDir>/dsh/ask-answerer.endpoint.json` with mode 0600, and compared in
  constant time. An empty token never authenticates anything, so switching the
  capability off cannot degrade into "anything with an empty Authorization header
  is trusted".
* **The file is the capability's whole authority.** It is readable only by the
  operator's own account, and the plugin that reads it runs inside the harness
  child, which the operator started. An attacker who can read it is already the
  operator.
* **A question is not a permission.** Answering one supplies information to the
  model; it authorises nothing. Everything the agent then does still goes through
  §6, so the worst an answered question can cause is a better-informed agent —
  which is why a question may expire unanswered without failing closed, while an
  approval may not.

Residual risk, stated plainly: the harness child can ask the phone anything, and
the answer goes into the model's context. The model is the one that chose the
question, so a prompt-injected model can use this to talk the operator into
typing something into the conversation — the same exposure as the model asking in
prose, with the difference that the card looks like the product asking rather
than the model. The gateway shows the session it came from for that reason.

---

## 7. Rate limiting and lockout

Two in-memory controls, keyed by client address (as resolved through the trusted
proxy chain):

| Control | Default | Purpose |
|---|---|---|
| Token bucket | `auth.requestsPerSecond: 8`, `auth.burst: 24` | ordinary traffic: a stolen token cannot be used to hammer the gateway, and a stuck phone cannot flood the agent |
| Escalating lockout | 5 failures, `lockoutBase: 30s` doubling to `maxLockout: 1h` | pairing-code guessing |

Both live in one process's memory. The consequences are honest ones: **restarting
the gateway clears every counter**, and a distributed attacker with many source
addresses gets a bucket per address. Since a pairing code is only redeemable
within a ~10–20 minute window and the lockout doubles per failure, restarting the
process does not realistically help an attacker guess 39.6 bits — but the
counters are a brake, not a wall. The wall is the code's short life plus the fact
that every attempt is visible in the audit log.

The event stream has its own back-pressure rule: a subscriber that cannot keep up
loses the oldest events and receives `resync` rather than slowing the agent down.
The DSH reader is never blocked by a slow phone.

---

## 8. What the audit log records

`~/.dsh-gateway/audit.jsonl`, mode 0600, one JSON object per line, rotated at
8 MiB with a single `.1` generation (an audit log that fills a disk is a worse
failure than a short history, on a personal machine).

| Event | When |
|---|---|
| `device.paired` | a pairing code was redeemed and a device enrolled |
| `device.pair_failed` | a pairing attempt was rejected |
| `device.revoked` | a device was disabled |
| `auth.failed` | a credential was missing, malformed, expired or revoked |
| `session.opened` / `session.released` | a session lease took or released DSH's single-writer lock |
| `prompt.sent` / `prompt.cancelled` | work admitted or interrupted |
| `approval.decided` | a human allowed or rejected a tool call |
| `approval.grant_revoked` | a standing authorisation was withdrawn |
| `workspace.reverted` | an undo wrote to the operator's files |
| `harness.restarted` / `harness.failed` | the DSH child's lifecycle |

Each line carries `time`, `event`, `requestId`, `deviceId`, `clientIp`, and
event-specific `fields`. `requestId` also appears in the access log, so an audit
line can be correlated with a request without duplicating request detail.

**What it does not contain.** No prompt text: `prompt.sent` records the device,
the turn id, the block count and whether the prompt was admitted as running or
queued — and nothing else. No tool arguments: `approval.decided` records the
tool's *name*, the byte length of its arguments and
a SHA-256 of them, so a decision can be tied to the call it authorised and two
identical approvals can be recognised as identical — but the arguments themselves
(the body of a `Write`, the text of an `Edit`, the command line of a `Bash`) stay
out of the file. That is deliberate: the log is rotated rather than deleted, and
it is not cleared when the session it belongs to is, so anything written here
should be assumed to outlive the conversation.

Limits: it is a diagnostic aid, not a compliance archive. A failed write is
logged and the request proceeds (refusing a legitimate approval because the disk
is full would be worse than a gap in the log), it keeps one `.1` generation, and
the fingerprints it does store are not reversible but are also not anonymised —
a guessable command can be confirmed against its digest.

---

## 9. Known limitations

Ranked by how much they should change your behaviour, not by how easy they are to
fix.

### 9.1 The desktop GUI reverse proxy exposes an agent behind a single cookie

**Severity: high, but off by default.** With `desktopUI.enabled: true`, the
gateway reverse-proxies DSH's stock browser GUI at `/`. That GUI is a full agent
console — terminal, file browser, plugin management — and it authenticates only
with its own session cookie. It inherits the gateway's authentication, so an
unpaired visitor cannot reach it, but:

* Its own authorization model was built for a loopback-only server, not for
  public exposure, and the gateway is inserting itself as the auth layer for a
  surface it does not fully model.
* Getting there requires several deliberate decisions (forwarding `Host`
  untouched, not rewriting `Origin`, owning `/` rather than a subpath) that exist
  because DSH derives trust and cookie names from the request authority. Each is
  load-bearing, and a future DSH change could break the fence in a way that
  fails *open* rather than closed.
* Anything the GUI can do, an attacker with the cookie can do — including running
  commands the GUI's own paths allow.

**Mitigation:** leave it off (the default). Turn it on only for a specific task,
and turn it off again. If you need the GUI remotely more than occasionally, the
better answer is a VPN (WireGuard/Tailscale) to the loopback port, where DSH's
own threat model still holds.

### 9.2 DSH's GUI session cookie lacks `Secure` (upstream limitation)

**Severity: medium, mitigated here to low.** DSH's shipped server sets its GUI
session cookie without the `Secure` attribute, because it was written for
`http://127.0.0.1`. A cookie without `Secure` may be sent over plain HTTP.

This deployment's mitigation is that **there is no plaintext path to the cookie**:
the only public listeners are Caddy's HTTPS port (8443 by default) and its HTTP
port (8080), which serves nothing but the CA root — every other request on it is
a permanent redirect to the HTTPS name — and HSTS
(`max-age=31536000; includeSubDomains`) is sent on every HTTPS response. The
gateway's *own* cookie does set `Secure` whenever `publicURL` is HTTPS.

Residual risk: on the very first visit, before any HSTS policy is cached, a
network attacker who can intercept the HTTP request could… redirect it (they
cannot serve a plain-HTTP response for the name and have it trusted for the
cookie, because the cookie belongs to the HTTPS origin). HSTS is not preloaded,
so a determined attacker on the path on the first visit is the remaining window.
If that is in your threat model, do not use the desktop GUI proxy at all (§9.1),
or use a VPN.

### 9.3 DeepSeek account sign-in does not work behind a reverse proxy

**Severity: low (a feature is unavailable), not a security hole.** Upstream
`loginOrigin()` accepts exactly `http://localhost`, `http://127.0.0.1` or
`http://[::1]`, with an explicit port and no path, query or credentials. An
`https://203-0-113-9.sslip.io` origin cannot satisfy that check, so account
sign-in in the GUI fails when the GUI is reached through the proxy — by design,
not by misconfiguration.

Do account sign-in on the desktop, where DSH is on loopback. The mobile API and
the agent loop do not depend on this path.

### 9.4 The ACP seam cannot attach to a session another process is driving

**Severity: low (a usability constraint), by design.** DSH enforces one live
writer per session with a kernel file lock. The gateway's DSH child therefore
cannot attach to a session the desktop already has open, and vice versa.

That is why session **leases** exist rather than hidden retries: attaching is
explicit (`POST /sessions/{id}/lease`), `GET /sessions/{id}` reports `leased` so
the client can explain why the desktop refuses to open a session, and the lease
is released by an explicit `DELETE` or after `session.idleTimeout` (5 minutes)
with nothing watching and no prompt in flight — not by a client disconnecting,
and never for a lease that was pinned. Because the lock is a kernel lock held by
the `dsh` child, it dies with that child, so killing the agent host cannot brick
a session; killing the gateway on its own does not release it, because the child
belongs to the host.

### 9.5 Mobile history depends on DSH's versioned on-disk log format

**Severity: low (degrades readably), and it degrades rather than corrupts.**
Transcripts are projected from DSH's session log files, whose schema is versioned;
this build understands version 4. After DSH writes a newer format, the projector
refuses to interpret it and the API answers `200` with `"unsupported": true` and
no items, so the client shows "history unavailable" instead of a misparsed
conversation or a 500.

The failure is confined to the read path: live turns, approvals and prompts come
over ACP, not from the log. Fix it by updating the gateway to a build that knows
the newer format.

### 9.6 sslip.io is a third-party wildcard DNS service

**Severity: medium, and it is inherent to not owning a domain.** The hostname
`203-0-113-9.sslip.io` resolves to the VPS only because sslip.io's wildcard
says so. Whoever controls that DNS answer controls where the name points.

What it no longer buys them is a browser-trusted certificate for it: the phone
trusts Caddy's local CA, not the WebPKI, so pointing the name elsewhere and
presenting a publicly trusted certificate changes nothing — the phone rejects
it. The attack that does work is at install time. The bootstrap fetch happens
before the device trusts anything (§2.2), and it is trust-on-first-use on both
routes: an attacker who controls the sslip.io answer can serve *their own* root
at `https://203-0-113-9.sslip.io:8443/ca.crt` with a certificate the phone
cannot yet verify — the user is being asked to click through exactly that warning
— or at `http://203-0-113-9.sslip.io:8080/ca.crt`, where there is nothing to
verify at all. From then on they are a complete man in the middle for every
hostname that phone visits — strictly worse than the old WebPKI case, where the
damage was confined to this one name. The provider's ICP filter blocking the
plain-HTTP route today is not authentication; it is someone else's policy, and it
is why TLS is the primary route rather than the fallback.

HSTS does not help: it defends against a protocol downgrade, not against a
wrong-but-installed root, and it never applied to the HTTPS CA URL anyway.

**Mitigation:** install the CA from a network you trust, or copy the root out of
band over SSH so that no server on the path can answer for it at all
(deployment.md §5.1), and compare the subject of a freshly fetched `ca.crt` with
the one the server is serving if you ever doubt it. The structural fix is
unchanged: use a domain you control. Point an A record at the VPS, run the
installer with `--domain your.name`, and — if that domain has a DNS API — use a
DNS-01 challenge so the certificate is publicly trusted and no CA is installed on
the phone at all. `sslip.io` is a convenience for the zero-configuration case,
and the right call for it is "fine to start, worth replacing".

### 9.7 frpc does not authenticate frps

**Severity: low–medium depending on your network path.** `frpc` enables TLS but,
without a `trustedCaFile`, it sets `InsecureSkipVerify` — so it encrypts the
tunnel without verifying *who* is on the other end. An attacker who can intercept
the Mac→VPS connection and terminate TLS could impersonate frps, capture the
token from the login, and then use it as in §5.1.

**Mitigation:** pin the server certificate. Generate a certificate for the VPS,
put `transport.tls.certFile`/`keyFile` in `frps.toml`, copy the CA to the Mac and
set `transport.tls.trustedCaFile` in `frpc.toml`; frpc then verifies the server
and stops sending its token to impostors. The deployment does not do this by
default because it requires managing a certificate whose only consumer is one
client. Note also that `useEncryption` still protects the payload layer with the
token, so this is about token capture, not bulk traffic capture.

### 9.8 Caddy's admin API listens on loopback:2019

**Severity: low, local only.** Caddy's default admin endpoint is an
unauthenticated HTTP API on `127.0.0.1:2019` that can rewrite the running
configuration. Anything that can run code on the VPS as any user can therefore
reconfigure TLS ingress — but anything that can run code on the VPS can also read
`/etc/frp/frps.toml`, so this changes little in practice. To close it, add
`admin off` to the Caddyfile's global options block and apply config changes with
a restart instead of `caddy reload`.

### 9.9 Smaller items, stated for completeness

* **No per-device scoping.** Every device has full authority; devices exist to be
  revocable, not to be limited.
* **In-memory rate-limit state resets on restart** (§7).
* **`maxPromptBytes` (256 KiB) and `maxConcurrentTurns` (4)** bound abuse of the
  prompt path, but there is no cost control: a paired device can spend your model
  quota until you revoke it. An image block is bounded by `maxBodyBytes` (8 MiB)
  or by `maxImageBytes` when that is set.
* **The change screen under-reports, and says so.** It projects `edit` and
  `write` from the session log, so a file changed by any other means — a `bash`
  command running `sed -i`, an editor the agent opened — is not listed, and
  `GET /sessions/{id}/changes` names its source (`tool-calls`) rather than
  implying it knows everything. This is a completeness limit, not a disclosure
  one: the projection never reads a file it reports on.
* **`session.promptQueueDepth` (4)** bounds how much work one session can have
  waiting. A queued prompt is not a promise: `POST /sessions/{id}/cancel` drops
  the queue along with the turn, which is what "stop" means.
* **The audit log is one generation deep** (§8).
* **`open` workspaces are exactly the configured roots.** The client cannot name
  a path, but *within* an allowed root the agent can do anything the sandbox and
  approvals permit — so choose roots you would be comfortable letting an agent
  modify, and keep `dsh.sandboxMode` at its default.
* **The gateway trusts its own child.** A compromised `dsh` binary or a malicious
  plugin is outside this threat model; the gateway authenticates the phone, not
  the agent.

---

## 10. Hardening checklist

Ordered by value per unit of effort:

1. Keep `desktopUI.enabled: false` unless you are actively using it (§9.1).
2. Use your own domain instead of `sslip.io`; with a DNS API you get a publicly
   trusted certificate and no CA install at all (§9.6).
3. In the default `--tls internal` mode, install the CA only on the phones that
   need it, and treat the fetch as trust-on-first-use: take it from a network you
   trust, or copy the root out of band (deployment.md §5.1), and remove the
   profile when the deployment is retired (deployment.md §7.9).
4. Keep `dsh.sandboxMode: workspace-write`; never `danger-full-access` (§6).
5. Decide about scoped approvals deliberately. `session.approvalGrantTTL: 0` is
   the narrowest posture and needs no thought; leaving the default means a
   decision can cover a whole session's use of one tool for half an hour, which
   is what makes a long turn workable from a phone (§6.1). Leave
   `changes.revert.enabled: false` unless you want the gateway to be able to
   write to your files at all (§1.1).
6. Set `FRP_CONTROL_ALLOW_FROM=<your-ip>/32` if your Mac has a stable address
   (deployment.md §3).
7. Pin the frps certificate and set `transport.tls.trustedCaFile` on the Mac
   (§9.7).
8. Lower `auth.sessionTTL` from 30 days if the phone is shared.
9. Review `~/.dsh-gateway/devices.json` and the app's device list monthly; revoke
   what you do not recognise. The settings screen also lists the standing
   approvals in force, which is the other thing worth a look.
10. Read `audit.jsonl` after anything unusual, and rotate the frp token if
    `journalctl -u frps | grep "client login info"` shows a stranger.
11. Keep both sides updated: the phone's PWA, the gateway binary, and DSH itself
    (§9.5).

---

## 11. If you think you have been compromised

Do these in order; each is independent, so partial completion still helps.

1. **Cut the ingress.** `sudo systemctl stop caddy` on the VPS (or
   `launchctl bootout gui/$(id -u)/dev.dsh-gateway.frpc` on the Mac). A stopped
   gateway is not reachable, and the DSH session lock is a kernel lock, so it dies
   with the child that holds it.
2. **Stop the agent.** `launchctl bootout gui/$(id -u)/dev.dsh-gateway.agent-host`
   **and** `launchctl bootout gui/$(id -u)/dev.dsh-gateway.gateway` on the Mac.
   The agent host is the job that holds the `dsh` child, so booting out the
   gateway alone leaves the agent — and whatever tools it is running — alive.
3. **Revoke every device.** Delete `~/.dsh-gateway/devices.json` (all devices) or
   mark entries `"revoked": true` (deployment.md §7.6). Then rebuild your
   confidence in the phones before re-pairing.
4. **Rotate both secrets.** New frp token (§5.1), and delete
   `~/.dsh-gateway/pairing.key` so all outstanding pairing codes die.
5. **Treat the CA as compromised if the VPS was.** The CA private key lives in
   `/var/lib/caddy`, so an attacker who had the VPS can sign a certificate for
   any hostname that every phone trusting the root will accept (§2.2). Remove the
   CA profile from those phones, and do not re-install the same root on a rebuilt
   host: a fresh Caddy data directory means a fresh CA, and the new root is what
   gets installed. If only the Mac was affected, the CA is untouched.
6. **Read the record.** `jq -c . ~/.dsh-gateway/audit.jsonl` and
   `journalctl -u frps -u caddy --since '-7 days'`, looking for
   `device.paired`, `auth.failed`, `prompt.sent` and `approval.decided` you
   cannot account for.
7. **Assume the agent's reach was the attacker's reach.** Review what the
   approved commands and the workspace roots allowed: git remotes, CI
   credentials, cloud CLI sessions, SSH keys, anything in those directories.
8. **Rotate what the agent or a device could have read** that matters more than
   this deployment: model API keys, cloud credentials, and anything in the
   workspace roots.
9. Only then rebuild: re-run the installers with fresh secrets, and re-pair the
   phones one at a time.
