# dsh-gateway

Drive the DeepSeek Harness agent running on your desktop from your phone, over
the public internet.

Your desktop agent can read your files and run commands. This gateway is the only
thing you expose to the internet, and it is built so that the answer to "what if
someone finds this URL?" is "nothing happens".

```
PHONE ──HTTPS──▶ VPS: Caddy (TLS) ──▶ frps ─┐
                                            │  encrypted tunnel
HOME MAC:  frpc ◀───────────────────────────┘
             │
             ▼  127.0.0.1 only
        dsh-gateway ──▶ /m/            mobile PWA
                    ──▶ /api/v1/*      versioned API (REST + WebSocket)
                    ──▶ ACP stdio ──▶ dsh --profile acp
                    ──▶ read-only  ──▶ ~/.dsh/sessions/*.jsonl.zstd
                    ──▶ undo only  ──▶ your files   (off unless enabled)
```

## Why it is shaped this way

Three properties of DeepSeek Harness dictated the design, and each was
established by reading and probing a real installation rather than assumed:

1. **`dsh web` binds loopback only and carries no TLS or authentication of its
   own.** So the gateway has to share the machine, and it has to own the entire
   security surface.
2. **The ACP profile (`dsh --profile acp`) is the only documented, versioned,
   wire-level seam for a third-party client.** It speaks ACP v1 over
   newline-delimited JSON-RPC on stdio — a published specification, not DSH
   internals — and explicitly adds no private methods. That is why the gateway
   can drive the agent without reverse-engineering anything, and why a DSH upgrade
   is unlikely to break it.
3. **DSH enforces one live writer per session with a file lock.** A session
   attached to the gateway cannot be opened on the desktop. That is a real
   limitation, and the gateway surfaces it rather than hiding it — see
   [session leases](#session-leases) below.

## What you get

- **A mobile web app** at `/m/`, installable to your home screen, for the loop
  you actually use on a phone: browse sessions, read history, send prompts
  (photographs and screenshots included), watch tools run, approve or refuse
  them, queue a follow-up while the agent works, stop a turn, switch model and
  reasoning effort.
- **Approvals that mean something, at a granularity you choose.** Every tool that
  needs authorisation is shown with what it will actually do — a real change for
  an edit, a warning for a command that looks destructive — and anything
  unanswered within the timeout is **refused**. A decision can name a scope
  ("this tool, in this session, for the next half hour") so that a long turn does
  not need a tap per tool call; every such authorisation is listed, bounded and
  withdrawable, and `session.approvalGrantTTL: 0` removes the option entirely.
- **What the session changed, and a way to put it back.** A change screen built
  from the agent's own record of its edits: which files, how many lines, and the
  changes themselves. Undo is available when you turn it on, exact-match only,
  and never while the agent is running.
- **A follow-up is not lost.** Prompts typed while a turn runs are queued rather
  than refused, and a turn that has been going a while shows how long and what it
  is doing now.
- **Your desktop sessions.** The gateway reads DSH's own session store, so a
  conversation you started at your desk shows up on your phone with its history
  and title.
- **An optional escape hatch.** Set `desktopUI.enabled` to also reach the full
  DSH browser GUI through the same authenticated origin, for the things a phone
  app has no business doing — the terminal, the file browser, plugin management.

## What this is not for

Worth reading before the quick start, because the security story above is only
true within these limits:

- **Not a multi-tenant service.** Every paired device has full authority: it can
  drive any session, approve any tool call, and reach anything in the workspace
  allowlist. There is no per-device least privilege and no read-only mode;
  revoking a device is the only control.
- **Not a sandbox for untrusted users.** Do not give a paired phone to someone
  you would not give a shell on the desktop machine.
- **Not a way to expose your desktop to the internet safely on someone else's
  behalf.** The gateway binds loopback and refuses anything else, but the
  deployment around it — a VPS, a tunnel, a CA root on a phone — is a real
  security decision, and [`docs/security.md`](docs/security.md) is the document
  that explains what you are taking on.

## Quick start

You need a checkout, and the desktop side is **macOS only** (it installs a
launchd job). The VPS side targets Ubuntu 24.04.

```sh
git clone https://github.com/huanghantao/dsh-gateway
cd dsh-gateway
```

Prerequisites beyond that: a machine running DeepSeek Harness, a public Linux
server, a Go 1.25+ toolchain, Node 22+ with npm, and a hostname. Node is needed
because the gateway embeds its web app, which is compiled during the build — the
same Node that DSH itself is installed with. If you have no domain,
`<ip-with-dashes>.sslip.io` resolves to your server's IP.

**What this was built against.** DeepSeek Harness **0.1.7-rc.2**, speaking ACP
**v1** — the version the adapter asserts rather than negotiates, because DSH
ignores the client's proposal and v1 is the only version it speaks. The
transcript projection is separately gated on the `session.v4` on-disk log format
and degrades to "history unavailable, open this on your desktop" when it meets a
newer one. A DSH upgrade is therefore most likely to show up first as either a
refused ACP handshake or missing history, not as a crash. Neither number is
pinned anywhere in the code, because nothing in the code can enforce it — if you
are on a different version and something is wrong, that is the first thing to
mention in an [issue](.github/ISSUE_TEMPLATE/bug_report.yml).

`go install github.com/huanghantao/dsh-gateway/cmd/dsh-gateway@latest` **cannot
work**: the frontend is compiled into the binary and is not committed, so the
module proxy has nothing to embed. Build from a checkout — that is what the
installer does.

Caddy serves HTTPS on **8443**, because many providers block inbound 80 and 443
until the domain has an ICP filing. On those hosts there is no publicly trusted
certificate to be had — every ACME challenge that does not need a DNS API needs
port 80 or 443 — so Caddy issues from its own CA and serves that root from the
same file in two places: over TLS at `https://<domain>:8443/ca.crt` (the primary
route, and one warning to click through) and in the clear at
`http://<domain>:8080/ca.crt` (no warning, but a provider that filters plain HTTP
by hostname can intercept it). That is what the commands below assume. If your
provider does not block 80/443, `--tls acme --https-port 443 --http-port 80` gets
a publicly trusted certificate and needs nothing installed on the phone.

```sh
# 1. On the server. --domain is required; the name is yours to choose.
sudo ./deploy/vps/install.sh --domain 203-0-113-10.sslip.io
#    defaults: HTTPS on 8443, Caddy's own CA, the CA root served on 8443 (TLS)
#    and, in the clear, on 8080

# 2. On the desktop machine (copy the frp token the installer printed).
#    All three flags are required: where the tunnel goes, what name the phone
#    will trust, and what on this machine the phone may reach.
./deploy/mac/install.sh \
  --server 203.0.113.10 \
  --public-url https://203-0-113-10.sslip.io:8443 \
  --workspaces "$HOME/code" \
  --token <token>

# 3. Teach the phone to trust this deployment (default --tls internal mode).
#    Open https://203-0-113-10.sslip.io:8443/ca.crt and accept the one-time
#    certificate warning: the root it serves is the one that would verify it.
#    iOS:     install the profile, then enable full trust under Certificate
#             Trust Settings. Both steps are required.
#    Android: Settings → Security → Encryption & credentials → Install a
#             certificate → CA certificate.
#    Alternatives, if plain HTTP is not filtered where you are:
#      http://203-0-113-10.sslip.io:8080/ca.crt (no warning), or out of band:
#      ssh root@203.0.113.10 \
#        'cat /var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt' \
#        > dsh-gateway-ca.crt
#    Skip this step entirely with --tls acme, and remove the CA when you retire
#    the deployment.

# 4. Check the whole path, then pair your phone
dsh-gateway doctor        # names the first thing that is wrong, and how to fix it
dsh-gateway pair          # prints a link and a QR code
```

Scan the code, and the app opens with the pairing code prefilled.

See [docs/deployment.md](docs/deployment.md) for the full walkthrough,
[docs/security.md](docs/security.md) for the threat model, and
[docs/api.md](docs/api.md) for the API contract.

## The ACP profile needs your provider

The `acp` profile ships configured for the built-in `deepseek-official` route,
which needs `DEEPSEEK_API_KEY`. If your desktop runs against a different
provider, the phone will fail every prompt with:

```
no API key for provider route "deepseek-official"
```

Point the ACP profile at the same provider your `web` profile uses by editing
`~/.dsh/profiles/acp/cordis.patch.yml`. The row that matters is `acp` itself —
overriding `agent-default-model` alone has no effect, because a different row
decides the fallback selection for sessions created over ACP:

```yaml
- id: acp
  name: "@deepseek-ai/dsh-acp"
  config:
    provider: your-provider
    model: your-model
```

Alternatively, put the credential in the gateway's environment:

```yaml
dsh:
  extraEnv:
    - DEEPSEEK_API_KEY=sk-…
```

## Session leases

DSH allows one live writer per session, enforced with a kernel file lock. A
session the gateway has attached therefore cannot be opened on your desktop, and
vice versa. Rather than letting that surface as a confusing error, the gateway
makes it a first-class concept:

- Attaching happens when you open a conversation, send a prompt, or explicitly
  request a lease.
- The lease is released when the last client goes away, after
  `session.idleTimeout` (default 5 minutes) with nothing happening.
- A turn in flight pins the lease; nothing releases a session mid-turn. The
  scheduler answers that question, so "mid-turn" has exactly one definition.
- Releasing a session drops any standing approval grants made in it: an
  authorisation for work that is finished must not apply to whatever runs there
  next.
- `GET /sessions/{id}` reports `leased`, so the app can tell you *why* your
  desktop is refusing to open something. It also reports the turn in flight and
  anything queued behind it.

## Design

Hexagonal, with the composition root as the only place that knows how the pieces
fit together. The dependencies point inward:

| Layer | Packages | Knows about |
|---|---|---|
| Ports | `internal/harness`, `internal/authn` | nothing but the standard library |
| Adapters | `internal/harness/acp`, `internal/authn/devicetoken`, `internal/sessionlog`, `internal/edge` | one external system each |
| Application | `internal/app/{events,lease,turns,approvals,bridge}` | the ports |
| Driving adapter | `internal/httpapi/v1`, `web` | the application |
| Composition | `cmd/dsh-gateway` | everything, and nothing else does |

Nothing above the adapter layer mentions ACP, stdio, or JSON-RPC. Swapping the
ACP adapter for an in-process bridge — which would enable mirroring a live
desktop session — is a change in one package.

Two of those packages own a decision worth knowing about before you change them.
`internal/app/turns` is the only thing that knows whether a turn is running:
the lease asks it rather than keeping a flag of its own, because two copies of
that answer is one copy too many, and the copy that used to live in the prompt
handler could disagree with reality under a double tap. `internal/workspace` is
the only thing that opens one of your files for writing, and a deployment that
leaves undo off constructs no instance of it, so the write path is absent rather
than disabled.

Four runtime dependencies, each earning its place:

| Package | Why |
|---|---|
| `coder/websocket` | context-aware, minimal, one concurrent writer by design |
| `klauspost/compress` | pure-Go zstd, needed to read DSH's session logs |
| `yaml.v3` | configuration |
| `skip2/go-qrcode` | the pairing QR code |

## Development

```sh
make check     # gofmt + tsc, go vet, and the full test suite
make lint      # golangci-lint v2 (the tree is lint-clean; CI fails on findings)
make build     # builds web/dist then the binary
make run       # runs against ./.dev-state
make pair      # prints a pairing code for the dev instance
make test-llm  # opt-in: drives a real model turn (costs money)
```

If a deployment is not working, `dsh-gateway doctor` is the fastest way to find
out why. See [docs/deployment.md](docs/deployment.md#8-troubleshooting).

`make build` compiles the frontend first because the Go binary embeds it. That
ordering is enforced rather than documented: `go build` fails outright if
`web/dist` is absent, which is a better failure than a binary that serves a blank
page. `web/dist` is generated, not committed, so every path that builds the
gateway builds it first — `deploy/mac/install.sh` included. A fresh clone
therefore needs Node and npm on `PATH`, which a working `dsh` install (itself an
npm package) already implies. The frontend has one devDependency and no bundler,
so that step is a `tsc` run over `web/src`.

The test suite has three tiers:

- **Unit** — run everywhere, no external dependencies.
- **Integration** — drive a real `dsh --profile acp` child. Skipped automatically
  when `dsh` is not installed.
- **Opt-in** — a real model turn, gated behind `DSH_GATEWAY_TEST_LLM=1`.
- **Browser** — `make e2e` drives the real app in real Chrome against a running
  gateway: it scans a pairing link, pairs, opens the event stream, creates a
  session, and runs a real turn.

The integration tier exists because the ACP field shapes this code depends on
were themselves established by probing a live server. A documentation-only test
would keep passing while the wire format drifted out from under it.

The browser tier exists for the same reason applied to the other boundary. There
is **no frontend unit test suite** — `tsc` proves the types line up and nothing
proves the app works — and the Go tests call the API directly, so neither crosses
where the two meet. Every bug found there so far lived exactly on that seam: a
`/me` response whose field names did not match the device list; an event stream
that stayed paused after pairing because the phone's first request had been a
401; and a "copy session id" that copied nothing on any plain-HTTP origin, because
an optional chain short-circuited the whole statement. All three were invisible to
every other kind of test and obvious within a minute of driving a real browser.

The browser tier is **not** part of CI: it needs a running gateway, which needs a
`dsh` install and a model credential, and neither belongs in a workflow that runs
on fork pull requests. CI checks that it parses; running it is on you.

## Known limitations

These are real, and each is a deliberate trade rather than an oversight.

1. **The gateway cannot mirror a session the desktop is actively driving.** DSH's
   one-live-writer lock makes that impossible over ACP. You get the desktop's
   *idle* sessions, with full history, and can take them over — but not watch a
   turn running in another process. The optional desktop-GUI proxy is the way to
   see a live session; a proper fix needs an in-process DSH plugin, which the
   port design leaves room for.
2. **No token-level streaming.** ACP delivers committed message blocks, not
   partial deltas, so text appears a paragraph at a time with tool activity
   filling the gaps. The transcript is unaffected.
3. **History depends on DSH's on-disk log format**, which is versioned but not a
   published contract. The gateway version-gates it and degrades to "history
   unavailable, open this on your desktop" rather than failing. All format
   knowledge is confined to `internal/sessionlog`.
4. **DeepSeek-account sign-in does not work through the desktop-GUI proxy.**
   Upstream restricts the OAuth redirect to loopback origins. Everything else in
   the GUI works.
5. **`sslip.io` is a third-party DNS service.** It is convenient, and swapping to
   a domain you control is a one-line configuration change.
6. **The baseline authenticator is a paired device token.** TOTP and WebAuthn are
   not implemented; `internal/authn` is a port, so they are additional adapters
   rather than a redesign.

## Contributing, security, privacy

- [CONTRIBUTING.md](CONTRIBUTING.md) — the build-order trap, what CI runs that
  `make check` does not, and the commit-message style this history is written in.
- [SECURITY.md](SECURITY.md) — how to report a vulnerability privately, and what
  is deliberately out of scope because [`docs/security.md`](docs/security.md)
  already documents it as a trade-off.
- [PRIVACY.md](PRIVACY.md) — every third party a deployment can involve, what
  the audit log does and does not record, and how long a "deleted" session
  actually stays on disk.

## Licence

MIT. See [LICENSE](LICENSE); third-party notices are in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
