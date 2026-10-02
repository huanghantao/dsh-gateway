# Security policy

This project is a remote control for a shell. It is designed to be the only thing
exposed to the internet in a setup where an AI agent can read and write files on
someone's machine, so a vulnerability here is not a bug in a web app — it is a
way into a computer. Reports are welcome and taken seriously.

## Reporting a vulnerability

**Use GitHub's private vulnerability reporting**: go to the
[Security tab](https://github.com/huanghantao/dsh-gateway/security) →
*Report a vulnerability*. That opens a private advisory only the maintainer can
see, and it is the preferred route because it keeps the discussion, the patch and
the credit in one place.

If you cannot use that, open a normal issue that says only *"I have a security
report, please contact me"* — with no technical detail — and a private channel
will be arranged.

Please do **not** open a public issue containing an exploit, a proof of concept,
or the specifics of a bypass. This deployment model puts a working exploit in
front of every operator running the project.

### What to include

- The version (`dsh-gateway version`) and the DSH version (`dsh --version`).
- How the gateway is deployed: TLS mode (`internal` or `acme`), whether
  `desktopUI.enabled` is on, whether any push webhook is configured.
- The smallest reproduction you have. `dsh-gateway doctor` output is useful.
- What an attacker gains — "reads another session", "runs a command",
  "enrols a device" — rather than only which check failed.

### What to expect

This is a single-maintainer project with no funding and no on-call. The honest
commitment is:

| Stage | Target |
|---|---|
| Acknowledgement | within 7 days |
| Initial assessment (is it a vulnerability, how bad) | within 14 days |
| Fix for a confirmed high-severity issue | as soon as is practical, and released with an advisory |
| Credit | in the advisory and the release notes, unless you prefer otherwise |

There is no bug bounty. If a report is declined, you will be told why.

## Supported versions

The project has no releases yet, so there is exactly one supported version:
**the tip of `main`**. Once there are tags, this table will name them.

The desktop side is macOS-only (`deploy/mac/install.sh`). The VPS side supports
Ubuntu 24.04. Neither is a security boundary — they are the platforms the
installers have been exercised on.

## Scope

**In scope** — anything in this repository:

- Authentication or authorisation bypass: reaching the API or the proxied
  desktop GUI without a paired device.
- Device-token or pairing-code weaknesses: prediction, replay, a way to use one
  after revocation or expiry, or a path that leaks either into a log.
- The approval surface failing open: a tool call that runs without a human, or
  one that keeps running after the timeout that should have refused it.
- Sandbox or workspace-allowlist escape: reaching a path outside a configured
  workspace root, or a session the operator did not open.
- Injection through the proxy, the session-log parser, or the API — including
  path traversal in session ids and zip-bomb-style inputs to the zstd decoder.
- Secret exposure: state files readable by another local user, secrets in logs,
  or anything written world-readable.
- The web app's CSP and its XSS surface, including Markdown rendering of model
  output.

**Out of scope** — real, but documented and deliberate. Each of these is
explained in [`docs/security.md`](docs/security.md), which is the threat model
this policy assumes:

- **`desktopUI.enabled: true` exposes the full DSH GUI** behind a single cookie,
  with whatever authority that GUI has. Off by default; §9.1 explains the risk.
- **frpc does not authenticate frps** (§9.7), and the tunnel token is a bearer
  credential (§5.1).
- **Caddy's local CA, once installed on a phone, can vouch for any hostname**
  (§2.2). That is what a MITM-capable root is, and the install is a deliberate
  act.
- **sslip.io is a third-party DNS service** (§9.6); whoever controls the answer
  controls where the name points at bootstrap time.
- **A device token has full authority.** There is no per-device least privilege
  and no read-only mode; revoking the device is the control.
- **The desktop machine is trusted.** An attacker who can already run code as
  your user does not need this gateway.
- **DSH itself.** Vulnerabilities in DeepSeek Harness, in the model provider, or
  in the ACP profile belong upstream.
- Findings that require an already-compromised VPS, an already-paired device, or
  physical access to an unlocked machine — unless the finding is that the gateway
  makes one of those materially worse.

## If you are not sure

Report it anyway. A false positive costs a little time; the alternative costs
someone their machine. If it turns out to be a documented trade-off, the reply
will point at the section that documents it — and if that section is wrong or
unclear, that is itself worth fixing.
