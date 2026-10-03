# Deploying dsh-gateway

A complete operator walkthrough: from an empty Ubuntu VPS and a Mac with DSH
installed, to a phone driving that DSH over HTTPS.

Deployment automation lives in [`deploy/`](../deploy/README.md); this document is
the narrative. The security reasoning behind the shape of it is in
[security.md](security.md); the client contract is in [api.md](api.md).

---

## 1. What you are building

```
  PHONE ──HTTPS:8443──> VPS  (Ubuntu 24.04, 203.0.113.9)
                        │
                        ├─ Caddy  :8443 / :8080   certificate from Caddy's own CA
                        │    ├─ /ca.crt on :8443  the CA root, over TLS (primary)
                        │    ├─ /ca.crt on :8080  the same file, in the clear
                        │    └─ reverse_proxy 127.0.0.1:18787   (loopback)
                        │
                        └─ frps   :7000 control, :18787 bound to LOOPBACK ONLY
                        ═══════════ frp tunnel (TLS + token + payload encryption)
                        │
  HOME MAC ─────────────┘
      frpc (launchd) ──> 127.0.0.1:8787
      dsh-gateway (launchd, loopback only)      the tier a redeploy replaces
        └─ connects over a unix socket to
      dsh-agent-host (launchd, its own job)     the tier that holds the work
        └─ spawns: dsh --profile acp
```

| Port | Where | Who reaches it | Why it is open |
|---|---|---|---|
| 8443/tcp | VPS | the internet | Caddy, HTTPS |
| 8080/tcp | VPS | the internet | Caddy serves the CA root at `/ca.crt` here too; every other request is a permanent redirect to :8443. Plain HTTP with a domain in the `Host` header can be intercepted by the provider, so the HTTPS copy on 8443 is the dependable one (§5.1) |
| 7000/tcp | VPS | the internet, in practice only your Mac | frpc dials in here; protected by TLS and the tunnel token |
| 18787/tcp | VPS **loopback only** | Caddy on the same host | the tunnel's exit; `proxyBindAddr` keeps it off every public interface |
| 22/tcp | VPS | you | SSH |
| 8787/tcp | Mac **loopback only** | frpc on the same Mac | the gateway itself |
| 3080/tcp | Mac loopback | the gateway, if the desktop GUI proxy is enabled | optional, off by default |
| `~/.dsh-gateway/agent-host.sock` | Mac filesystem | the gateway, as the same user | the agent host; mode 0600 inside a 0700 directory, so no other account can reach it |

There are two long-lived processes on the Mac, not one. `dsh-gateway` serves the
phone; `dsh-agent-host` holds the DeepSeek Harness child and the turns it is
running. They are separate launchd jobs on purpose, and the reason is worth
knowing before you edit a plist: launchd kills every process in a job's process
group when that job dies, so a host started by the gateway would be killed by
exactly the redeploy it exists to survive. The gateway can be replaced while a
turn keeps running because the process holding the turn is not the one being
replaced.

There is no way to run the agent inside the gateway, and that is deliberate. It
used to be an option (`dsh.mode: in-process`); it was removed because a gateway
that runs the agent itself is a gateway whose redeploy ends whatever turn is in
flight — the precise failure the second process exists to prevent. `dsh.mode` is
still accepted so that a config file which names the old value produces a
sentence explaining this rather than a silent change in behaviour.

Both Caddy ports are defaults, not constants: `--https-port` and `--http-port`
change them. Nothing listens on 80 or 443. The shipped pair exists because some
providers — mainland Chinese clouds most notably — block inbound 80 and 443, and
the usual alternate HTTP ports (2053, 2083, 2087, 2096, 8880, 9443) with them,
until the domain has an ICP filing. Whether *your* host carries 8080 and 8443 is
something to verify rather than assume; [`--tls acme`](#3-step-1--the-vps) with
`--https-port 443 --http-port 80` is the simpler path on a provider that does not
block them.

The gateway is the only component that can run commands on your Mac, so it never
has a public listener. Everything public is either a TLS terminator (Caddy) or a
token-authenticated tunnel endpoint (frps).

The public hostname is `203-0-113-9.sslip.io`: [sslip.io](https://sslip.io) is
a wildcard DNS service that resolves any name containing an IP address back to
that address, so `203-0-113-9.sslip.io` → `203.0.113.9` with no DNS
configuration at all.

The certificate for that name is issued by **Caddy's own CA** (`tls internal`),
not by a public one, because the blocked low ports leave no ACME challenge to
run: http-01 needs port 80, tls-alpn-01 needs port 443, and dns-01 needs a DNS
provider API that sslip.io does not offer. Caddy mints a local root and serves it
from one file in two places — `https://203-0-113-9.sslip.io:8443/ca.crt`
primarily, and `http://203-0-113-9.sslip.io:8080/ca.crt` as a convenience —
and each phone installs it once (§5.1). That is a real trade — one manual trust
step per device instead of a publicly trusted chain — and it is the price of both
a hostname you do not own and a provider that blocks the low ports. §3 has the
alternative.

---

## 2. Prerequisites

**VPS**

* Ubuntu 24.04, root (or `sudo`). The script refuses to run as anything else.
* The two Caddy ports free — 8080 and 8443 by default, nothing else serving HTTP
  on them. Check with `ss -ltnp | grep -E ':8080|:8443'`. (Nothing here uses 80
  or 443; see §1 for why.)
* Outbound HTTPS, to fetch the frp and Caddy releases from GitHub or Caddy's apt
  repository.
* ~50 MB of disk for the two static binaries.
* **Your provider's firewall must allow the two Caddy ports (8080 and 8443 by
  default) and 7000.** `ufw` runs *inside* the VM; a cloud security group or an
  upstream firewall is a separate layer and is a common reason a tunnel never
  connects or a port answers nothing (see
  [Troubleshooting](#81-the-tunnel-never-connects-and-ufw-is-not-the-only-firewall)).

**Mac**

* macOS (Apple Silicon or Intel — the installer picks the right frpc build).
* Go 1.25 or newer: `go env GOVERSION` should print `go1.25.x` or later.
* Node 22 or newer with npm. The gateway embeds its web app, so the installer
  compiles `web/src` before `go build`; `npm run build` is a single `tsc` pass
  with one devDependency. Any machine that can run DSH already has Node, because
  DSH itself is installed with `npm i -g @deepseek-ai/dsh`.
* DSH installed and the ACP profile working:

  ```sh
  dsh --profile acp --help
  ```

  This must succeed. It is the exact command the gateway launches as a child
  process, so if it fails here it will fail under launchd too.
* A checkout of this repository (the installer builds the gateway from source):

  ```sh
  git clone https://github.com/huanghantao/dsh-gateway && cd dsh-gateway
  ```
* `~/.dsh` shared with your desktop install — that is the default, and it is what
  makes the phone and the desktop see the same sessions.

**Phone**

A modern browser, plus — in the default `--tls internal` mode — the CA root
installed once (§5.1). Until that is done the phone will refuse the HTTPS
origin; `--tls acme` is the way around it, on a provider that allows it.

---

## 3. Step 1 — the VPS

```sh
sudo bash deploy/vps/install.sh
```

Options: `--domain NAME` (default `203-0-113-9.sslip.io`), `--https-port N`
(default `8443`), `--http-port N` (default `8080`), `--tls internal|acme`
(default `internal`), and `--force` (reinstall the pinned binaries even if the
versions already match). `sudo bash deploy/vps/install.sh --help` lists
everything.

`--tls internal` is the default, and the mode the rest of this document assumes:
Caddy serves the name with a certificate from its own CA, and serves that CA root
for the phone to install from the same file on both ports — over TLS on the HTTPS
port, and in the clear on the HTTP port (§5.1). It is the only mode that works on
a provider which blocks 80 and 443.

`--tls acme` is the alternative: Caddy gets a publicly trusted certificate for
the hostname instead, there is no internal CA and neither `/ca.crt` route exists,
and nothing has to be installed on the phone. It needs the ACME challenge ports,
so the usual invocation is `--tls acme --https-port 443 --http-port 80`; the script
warns loudly if `acme` is selected while the ports are not 80/443. On *this*
provider that warning is the whole story: until the domain has an ICP filing,
`acme` cannot work here at all — the route to a publicly trusted certificate
would be a domain you control with a DNS-01 challenge (which needs a DNS
provider module and API credentials in the Caddyfile, not a flag).

What it does, in order:

1. Installs `curl`, `openssl`, `tar` and `ufw` if missing.
2. Downloads **frp 0.71.0** and **Caddy 2.11.4**, verifies them, and installs
   them to `/usr/local/bin`. frp comes from its GitHub release, checked against
   the published SHA-256 sums. Caddy is preferred from the vendor's signed apt
   repository — faster than GitHub on some hosts, and signature-verified rather
   than digest-verified — with the release tarball (checked against Caddy's
   SHA-512 sums) as the fallback. The distro `caddy` package is deliberately not
   installed: it ships a competing systemd unit.
3. Creates the system users `frp` and `caddy` and writes:
   * `/etc/frp/frps.toml` (0640 `root:frp`) with a token generated once by
     `openssl rand -hex 32`,
   * `/etc/caddy/Caddyfile` (0644), keeping the previous version as
     `/etc/caddy/Caddyfile.bak` whenever the rendered content changes,
   * `/etc/systemd/system/{frps,caddy}.service`.
4. Validates both configs with the real binaries (`frps verify`, `caddy
   validate` — the latter as the `caddy` user, in its own data directory)
   *before* installing them, then `systemctl enable --now`s both.
5. Sets `ufw` to deny inbound by default and allow: the SSH port(s) it detects
   (including the port your current session is on if you are connected over
   SSH — so a custom SSH port cannot lock you out), the two Caddy ports
   (`--http-port` and `--https-port`, 8080 and 8443 by default), and 7000. It
   does not open 80 or 443.

It ends by printing the **frp token**, the public URL, and — in the default
`internal` mode — the steps for teaching a phone to trust the CA. Copy the
token; you need it in step 2.

```
  FRP TOKEN (paste into the Mac installer; it is not regenerated):
      8f3c…64 hex characters…
```

Re-running the script is safe: the token is read back out of the existing
`/etc/frp/frps.toml` and reused, config files are only rewritten when their
content actually changes (the previous Caddyfile is kept as
`/etc/caddy/Caddyfile.bak`), and services are only restarted when something
changed.

> **Why 7000 is open.** frpc dials the control port from a residential address,
> so there is no stable source address to allow. What protects it is TLS plus a
> 256-bit token, and `allowPorts` in `frps.toml`, which stops a stolen token from
> binding any port other than 18787. If your Mac has a static address you can
> narrow it:
> `sudo FRP_CONTROL_ALLOW_FROM=198.51.100.7/32 bash deploy/vps/install.sh`.

---

## 4. Step 2 — the Mac

Run this as yourself. **Never with `sudo`**: the installer writes to `$HOME`, and
a `sudo` run would put DSH state somewhere else and print you a pairing code the
running gateway will reject.

```sh
FRP_TOKEN=<the token from step 1> bash deploy/mac/install.sh \
  --server <your VPS address> \
  --public-url https://<your domain>:8443 \
  --workspaces "$HOME/code"
```

All three flags are required. None of them is defaulted, and that is a deliberate
choice rather than an omission: each names something about *your* setup — where
your tunnel token is sent, which hostname the phone will trust, and what on this
machine the phone may reach — and a default for any of them would be this
project's author answering a question about your infrastructure. `--workspaces`
in particular will not accept your home directory itself, because that would hand
the agent `~/.ssh` and `~/.dsh`.

Useful variations:

```sh
# More than one workspace root (space- or comma-separated):
FRP_TOKEN=… bash deploy/mac/install.sh --server … --public-url … \
  --workspaces "$HOME/code $HOME/notes"

# Install the binaries somewhere else (default ~/.local/bin):
FRP_TOKEN=… bash deploy/mac/install.sh --server … --public-url … \
  --workspaces "$HOME/code" --install-dir /usr/local/bin
```

What it does:

1. Checks Go ≥ 1.25 and runs `dsh --profile acp --help` — proving the ACP
   profile resolves before anything is installed.
2. Builds `./cmd/dsh-gateway` and installs it (plus frpc 0.71.0, checksum
   verified) into `~/.local/bin`.
3. Renders `~/.dsh-gateway/frpc.toml` (0600) from the template, keeping the
   token it finds there if none was supplied, and validates it with
   `frpc verify`.
4. Renders `~/.dsh-gateway/config.yaml` **only if it does not exist**. It is
   never overwritten afterwards: it is your file.
5. Writes `~/Library/LaunchAgents/dev.dsh-gateway.{frpc,gateway}.plist` with
   `RunAtLoad`, `KeepAlive`, logs under `~/Library/Logs/dsh-gateway/`, and a
   `PATH` that it derives from wherever `node`, `npm` and `dsh` actually live —
   launchd gives a job a minimal `PATH`, so a node/nvm-installed `dsh` is
   invisible without this.
6. Loads both jobs (`launchctl bootout` then `bootstrap`, falling back to
   `launchctl load` on older macOS) and then probes
   `http://127.0.0.1:8787/healthz` to confirm the gateway actually came up.

Expect a summary ending in a "Pair a phone" section; that is step 4.

**Check it locally before involving the internet:**

```sh
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8787/healthz   # 200
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8787/readyz    # 200 once the DSH child is up
launchctl print gui/$(id -u)/dev.dsh-gateway.gateway | head -20
tail -f ~/Library/Logs/dsh-gateway/gateway.log
```

`/healthz` answers as soon as the process is serving. `/readyz` answers `503`
until the `dsh` child has completed its ACP handshake — that can take a few
seconds, and stays `503` if DSH cannot start (see
[§8.4](#84-dsh---profile-acp-not-found-under-launchd-path-problem)).

---

## 5. Step 3 — trust the CA, then verify the public path

### 5.1 Install the CA root on the phone

In the default `--tls internal` mode the certificate is not signed by a public
authority, so nothing on the phone will trust
`https://203-0-113-9.sslip.io:8443` until the root is installed. The root is
public information — it is the thing that lets everything else be verified — and
Caddy serves it from one file,
`/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt`, in two
places:

| URL | What it costs |
|---|---|
| `https://203-0-113-9.sslip.io:8443/ca.crt` | **The primary route.** TLS keeps the request away from the provider's Host-header filter (§8.7), so this is the one that keeps working. It costs one certificate warning the phone has to accept: the certificate cannot be trusted before the root that signs it has been installed. |
| `http://203-0-113-9.sslip.io:8080/ca.crt` | No warning to bypass, but plain HTTP with a *domain* in the `Host` header can be intercepted by the provider. On this host it currently answers with an ICP block page instead of the root (§8.7). |

Serving the root at all is correct rather than a leak: a root is public by
definition, and a device that does not trust it yet cannot fetch it over a
connection it already trusts. The HTTPS copy does not remove that problem, it
moves it — the phone is asked to click through a warning for a certificate it
cannot verify yet, which is trust-on-first-use, and is reasoned about in
[security.md](security.md#96-sslipio-is-a-third-party-wildcard-dns-service).
Both routes serve the identical file and answer `200`, 627 bytes,
`Content-Type: application/x-x509-ca-cert`, `Content-Disposition: attachment`;
only the transport differs.

**iOS Safari** — the install and the trust toggle are two separate steps, and
iOS ignores an installed-but-untrusted root:

1. Open `https://203-0-113-9.sslip.io:8443/ca.crt`. Safari shows "This
   Connection Is Not Private".
2. Show Details → visit this website (the device passcode is asked for). The
   profile downloads.
3. Settings → General → VPN & Device Management → install it.
4. Settings → General → About → Certificate Trust Settings → enable full trust
   for it.

**Android Chrome:**

1. Open the same URL. Chrome shows "Your connection is not private".
2. Advanced → Proceed to `203-0-113-9.sslip.io` (unsafe). The file downloads.
3. Settings → Security → Encryption & credentials → Install a certificate → CA
   certificate → pick the downloaded file.

Then revisit `https://203-0-113-9.sslip.io:8443`: no warning should appear.

**If the HTTPS URL is not usable**, the same root is available three other ways,
in order of convenience:

1. **Plain HTTP, domain form** — `http://<domain>:8080/ca.crt`.
   This needs no warning bypass, and on a host whose provider does not filter
   plain HTTP it is the more comfortable way in. On a provider that *does* filter
   — some mainland Chinese clouds, most notably — it is not dependable: the
   filter intercepts plain HTTP by the `Host` header and answers any hostname
   without an ICP filing with a block page. Such a response looks like this:

   ```sh
   $ curl -sS -D- http://<domain>:8080/ca.crt
   HTTP/1.1 403 Forbidden
   Content-Type: text/html; charset=utf-8
   Server: ADM/2.1.1
   ```

   The body is an ICP notice, and the filter is not user-agent dependent — curl's
   default UA, a desktop Chrome UA and an iPhone Safari UA all get the same 403.
   TLS is opaque to it, which is why everything on `:8443` is unaffected. It can
   also switch on without warning on a host where it was previously working, so
   do not build a deployment around this route.
2. **Plain HTTP, IP form** — `http://<ip>:8080/ca.crt` (the dashes of the
   sslip.io name become dots). The filter keys on the `Host` header being a
   *domain*, not on the address, so on a filtered host this is often the route
   that still works. A successful fetch answers `200` with
   `Content-Type: application/x-x509-ca-cert`,
   `Content-Disposition: attachment`, and a subject of
   `CN = Caddy Local Authority - <year> ECC Root`. It rests on an implementation
   detail of a filter this deployment does not control, so treat it as a
   convenience that can stop working without notice. It is plain HTTP, which is
   why the name check that rejects the bare IP over TLS (§5.2) does not apply.
3. **Out of band, no web fetch at all** — copy the file over SSH, transfer it to
   the phone (AirDrop, email, a file-sharing app) and install it from Settings
   with the same steps as above:

   ```sh
   ssh root@203.0.113.9 \
     'cat /var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt' \
     > dsh-gateway-ca.crt
   ```

   Nothing on the network can answer for that fetch, so it is the one path with
   no trust-on-first-use window at all. It is also the most manual.

Remove the CA from the phone when you stop using the deployment (§7.9).

> **HSTS only bites the plain-HTTP URL.** Every HTTPS response carries
> `Strict-Transport-Security: max-age=31536000; includeSubDomains`, and HSTS
> upgrade rules ignore the port: a browser profile that has once loaded the app
> over HTTPS rewrites a later `http://…:8080/ca.crt` to
> `https://…:8080/ca.crt` — where Caddy speaks plain HTTP — and the fetch fails.
> The HTTPS CA URL above is already HTTPS and is not affected. If you do want the
> plain-HTTP one, fetch it before the first HTTPS visit, from a browser profile
> that has not seen the header, or clear the HSTS state for this host (§8.6).

Under `--tls acme` there is no internal CA, neither `/ca.crt` route exists, and
this section does not apply.

### 5.2 Verify the public path

From anywhere (your phone's browser is a fine place to start, but `curl` is
easier to read). No third party is involved in issuing the leaf certificate —
Caddy's local CA signs it on first use — so there is no ACME round trip, no rate
limit, and no first-request wait.

```sh
# 1. Bootstrap one copy of the root. The HTTPS URL is the primary route, and -k
#    is correct exactly once: the certificate it presents is signed by the root
#    it is handing you, so there is nothing to verify it against yet.
curl -skS -o ca.crt -w '%{http_code}\n' \
  https://203-0-113-9.sslip.io:8443/ca.crt                     # 200
# On a host whose provider does not filter plain HTTP, this is the same file:
#   curl -sS -o ca.crt http://203.0.113.9:8080/ca.crt

# 2. Now verify that route properly, against the root it serves:
curl -sS --cacert ca.crt -o /dev/null -w '%{http_code}\n' \
  https://203-0-113-9.sslip.io:8443/ca.crt                     # 200

# 3. Without --cacert the same fetch fails, and is supposed to:
curl -sS -o /dev/null https://203-0-113-9.sslip.io:8443/ca.crt
# curl: (60) SSL certificate problem: unable to get local issuer certificate
# The bootstrap paradox, not a misconfiguration: the certificate on that URL is
# signed by the very root the URL hands you. A browser that has not installed
# the root is in exactly that position, and clicks through the warning (§5.1).

curl -sS --cacert ca.crt https://203-0-113-9.sslip.io:8443/healthz; echo
# {"status":"ok","uptime":"…","version":"dev"}

curl -sS --cacert ca.crt https://203-0-113-9.sslip.io:8443/readyz; echo
# {"droppedEvents":0,"harness":"ready","status":"ready"}   once the DSH child is up
```

`--insecure`/`-k` is only a shortcut for a shell that has not installed the CA —
and on the CA URL itself the one legitimate use of it, because that fetch is what
creates the trust `--cacert` would check. `--cacert ca.crt` actually checks the
chain, and fails loudly if it is wrong.

Also worth checking:

```sh
# The app shell. "/" redirects to "/m/", where the mobile app lives.
curl -sSI --cacert ca.crt https://203-0-113-9.sslip.io:8443/ | head -3   # 307 → /m/
curl -sS -o /dev/null -w '%{http_code}\n' --cacert ca.crt \
  https://203-0-113-9.sslip.io:8443/m/                        # 200, text/html, <title>dsh-gateway</title>

# :8080 serves the CA and redirects everything else. Ask by bare IP: a domain
# in the Host header can be answered by the provider's filter instead (§8.7).
curl -sSI http://203.0.113.9:8080/healthz | head -3           # 301 → https://…:8443/healthz
```

For a deeper check of the real app — QR pairing, session list, WebSocket
handshake, session create, prompt accepted, live events, transcript — the
browser script runs against any base URL, including this one:

```sh
node scripts/e2e-browser.mjs --base https://203-0-113-9.sslip.io:8443 --insecure
```

`--insecure` there tells Chrome to accept the certificate without verifying it,
which is what a browser that has not installed the CA needs. It changes nothing
about the rest of the path.

Do **not** test by IP (`https://203.0.113.9:8443`): the certificate is issued
for the hostname, so the IP is a name mismatch. That is expected, not a fault.

---

## 6. Step 4 — pair a phone

1. Open `https://203-0-113-9.sslip.io:8443` on the phone. It redirects to
   `/m/`, the mobile app. If the phone has not installed the CA yet, do §5.1
   first — otherwise this is a certificate warning, not a pairing problem.
2. On the Mac, print a pairing code and QR:

   ```sh
   dsh-gateway pair                      # QR + link + code
   dsh-gateway pair -json                # {"code":…,"url":…,"expiresAt":…}
   dsh-gateway pair -config ~/.dsh-gateway/config.yaml   # if you moved the config
   ```

   Output looks like:

   ```
     Pairing code:  K7M2QPX4
     Expires:       14:32:05 (9m58s from now)
     Link:          https://203-0-113-9.sslip.io:8443/m/#/pair?code=K7M2QPX4
     …QR code…
   ```

   The link is derived from `publicURL` in `~/.dsh-gateway/config.yaml`, so it
   already carries the port.

3. Scan the QR, or open the link, or type the 8-character code into the app.

How the code behaves — worth knowing before you debug it:

* It is **derived**, not stored: both the running gateway and the `pair` command
  compute it from `~/.dsh-gateway/pairing.key` with HMAC-SHA256 over the current
  time window. There is no endpoint that mints credentials, which is why `pair`
  must run as the same user that owns the state directory.
* It rotates every `auth.pairingTTL` (10 minutes by default), and the *previous*
  window is also accepted, so a code printed just before a boundary still works.
* The alphabet omits `0/O`, `1/I/L` and `U` so it can be read off a screen.
* Five failed attempts lock that client out (30s, doubling to an hour); each
  success clears the history.
* Every redemption enrols a device and is written to the audit log. Treat the
  code as a secret for its ten minutes: anyone who has it and can reach the URL
  can enrol a device.

Once paired, a device token is stored on the phone and a session cookie
(`dsh_gw_session`, `HttpOnly`, `Secure`, `SameSite=Strict`) is set. Devices last
`auth.sessionTTL` (30 days by default) and appear in the app's settings, where
you can revoke them (§7.6).

---

## 7. Day-2 operations

### 7.1 Status

```sh
# VPS
systemctl status frps caddy --no-pager
sudo ufw status numbered
ss -ltnp | grep -E ':8080|:8443|:7000|:18787'   # 18787 must show 127.0.0.1, never 0.0.0.0

# Mac
launchctl print gui/$(id -u)/dev.dsh-gateway.gateway | head -20
launchctl print gui/$(id -u)/dev.dsh-gateway.frpc    | head -20
curl -sS http://127.0.0.1:8787/healthz; echo
curl -sS -o /dev/null -w 'readyz %{http_code}\n' http://127.0.0.1:8787/readyz
dsh-gateway version
```

### 7.2 Logs

| What | Where |
|---|---|
| frps, Caddy, the VPS systemd units | `journalctl -u frps -u caddy -f` (add `-n 100 --no-pager` to read rather than follow) |
| gateway process | `~/Library/Logs/dsh-gateway/gateway.log` (everything the gateway says; it logs to stderr, so `gateway.stdout.log` is normally empty) |
| frpc | `~/Library/Logs/dsh-gateway/frpc.log` (`frpc.err.log`) |
| launchd state | `launchctl print gui/$(id -u)/dev.dsh-gateway.gateway` — includes the last exit status |
| security events | `~/.dsh-gateway/audit.jsonl` (see below) |

The audit log is one JSON object per line, rotated at 8 MiB with a single `.1`
generation:

```sh
tail -f ~/.dsh-gateway/audit.jsonl | jq -c .
jq -c 'select(.event=="device.paired" or .event=="device.revoked")' ~/.dsh-gateway/audit.jsonl
jq -c 'select(.event=="approval.decided")' ~/.dsh-gateway/audit.jsonl   # who authorised what
```

Recorded events: `device.paired`, `device.pair_failed`, `device.revoked`,
`auth.failed`, `session.opened`, `session.released`, `prompt.sent`,
`prompt.cancelled`, `approval.decided`, `harness.restarted`, `harness.failed`.
Each line carries `time`, `event`, `requestId`, `deviceId`, `clientIp` and
event-specific `fields`.

### 7.3 Change gateway configuration

```sh
$EDITOR ~/.dsh-gateway/config.yaml     # every key and default is documented inline
launchctl kickstart -k gui/$(id -u)/dev.dsh-gateway.gateway
```

The installer will never overwrite that file, so your edits survive every future
re-run. Configuration is validated at startup, and a bad value is refused with a
message naming the file — the process exits rather than running with a weakened
setting.

### 7.4 Update the gateway

```sh
cd /path/to/dsh-gateway
git pull
FRP_TOKEN=… bash deploy/mac/install.sh      # token optional; it reuses frpc.toml
```

The installer rebuilds, reinstalls, and reloads both launchd jobs (bootout +
bootstrap), so the new binary takes effect. `dsh-gateway version` prints the git
description the build was made from (falling back to `dev` when the checkout has
no tags), which is what you want in a bug report.

### 7.5 Update frp or Caddy

Both versions are pinned in variables at the top of `deploy/vps/install.sh` and
`deploy/mac/install.sh`. Bump them, then re-run the installer on each side:

```sh
# VPS
sudo bash deploy/vps/install.sh --force    # --force re-downloads even if the
                                           # version check would have skipped it
# Mac
FRP_TOKEN=… bash deploy/mac/install.sh
```

Re-run `frps verify -c /etc/frp/frps.toml` and `frpc verify -c
~/.dsh-gateway/frpc.toml` afterwards, and read the note in the frps template
about `proxyBindAddr`: frp parses TOML strictly, and if a future release renames
that key the binary refuses to start rather than silently binding the proxy port
publicly. That failure mode is deliberate.

### 7.6 Revoke a device

**Preferred: from the app.** Settings → devices → revoke. That is
`DELETE /api/v1/devices/{id}` and takes effect immediately, including for the
device doing the revoking (it is allowed, and useful when you are about to hand
the phone to someone else).

**Fallback, if the phone is lost and it was your only device** (there is no
administrative HTTP endpoint by design, so this is a local file edit):

```sh
# 1. Stop the gateway first: it rewrites the store on change, so an edit made
#    while it runs can be overwritten.
launchctl bootout gui/$(id -u)/dev.dsh-gateway.gateway

# 2. Mark the device revoked (find it by name or lastSeen).
$EDITOR ~/.dsh-gateway/devices.json
#    "devices": [ { "id": "dev_…", "name": "iPhone 15", "revoked": true, … } ]

# 3. Start it again.
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.dsh-gateway.gateway.plist
```

Deleting `devices.json` instead revokes **every** device at once (you are then
unpaired everywhere and need `dsh-gateway pair` again). Verify with:

```sh
jq -c '.devices[] | {id, name, revoked, lastSeen}' ~/.dsh-gateway/devices.json
jq -c 'select(.event=="auth.failed")' ~/.dsh-gateway/audit.jsonl | tail
```

A revoked device keeps getting `401` with `unknown_credential` — the same error
as a token that never existed, deliberately, so a stolen device cannot tell
whether it was revoked or merely wrong.

### 7.7 Rotate the frp token

The token authenticates the tunnel. Rotating it is a two-sided change: the tunnel
is down between the two steps, so do it when you can reach both machines.

```sh
# 1. VPS: write the new token and restart frps. install.sh will NOT do this for
#    you — it deliberately never regenerates a token, because doing so silently
#    would break a working tunnel.
sudo openssl rand -hex 32                       # copy the output
sudo $EDITOR /etc/frp/frps.toml                 # auth.token = "<new>"
sudo systemctl restart frps

# 2. Mac: hand the new token to the installer. It notices the change and rewrites
#    frpc.toml, then restarts frpc.
FRP_TOKEN=<new token> bash deploy/mac/install.sh

# 3. Confirm the tunnel is back.
tail -5 ~/Library/Logs/dsh-gateway/frpc.log      # "login to server success"
curl -skS -o /dev/null -w '%{http_code}\n' https://203-0-113-9.sslip.io:8443/healthz
```

Rotate after any suspicion that `/etc/frp/frps.toml` or
`~/.dsh-gateway/frpc.toml` was read by someone else, and rotate the *device*
tokens too if the same suspicion covers the Mac (§7.6). Rotating the token does
not invalidate paired devices: they authenticate to the gateway, not to the
tunnel.

### 7.8 Move to a new IP or domain

The name is derived from the address, so both change together:
`203-0-113-9.sslip.io` → `203-0-113-7.sslip.io` for `203.0.113.7`.

```sh
# VPS (from the box's new address)
sudo bash deploy/vps/install.sh --domain 203-0-113-7.sslip.io

# Mac — --public-url must carry the HTTPS port
FRP_TOKEN=… bash deploy/mac/install.sh --server 203.0.113.7 \
                                        --public-url https://203-0-113-7.sslip.io:8443
```

`frps.toml` and `frpc.toml` themselves do not mention the domain — only
`publicURL` in `config.yaml`, the Caddyfile, and the launchd job do. Because the
installer will not overwrite `config.yaml`, edit `publicURL` there by hand if you
changed it directly.

**The launchd job carries the same value as `DSH_GATEWAY_PUBLIC_URL`, and the
environment wins over the file.** A job left over from an earlier address
therefore keeps using it however correct `config.yaml` is — the gateway now warns
at startup when the two disagree, naming both. Fixing it means either re-running
the Mac installer (it rewrites the job) or changing both by hand:

```sh
$EDITOR ~/.dsh-gateway/config.yaml
plutil -replace EnvironmentVariables.DSH_GATEWAY_PUBLIC_URL \
       -string "https://203-0-113-7.sslip.io:8443" \
       ~/Library/LaunchAgents/dev.dsh-gateway.gateway.plist
launchctl bootout   gui/$(id -u)/dev.dsh-gateway.gateway
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.dsh-gateway.gateway.plist
```

The certificate does not need re-issuing from anywhere: `tls internal` means
Caddy signs a leaf for the new name with the same local CA, so nothing waits on
an ACME challenge and the root already installed on the phone still verifies the
new name — you do not re-install the CA. The old leaf certificate is simply
abandoned.

### 7.9 Remove it all

Remove the CA from every phone you installed it on (§5.1). On iOS that is
Settings → General → VPN & Device Management → delete the profile (plus the
trust toggle under Certificate Trust Settings); on Android it is Settings →
Security → Encryption & credentials → Trusted credentials → User → remove the
certificate.

```sh
# Mac
launchctl bootout gui/$(id -u)/dev.dsh-gateway.gateway
launchctl bootout gui/$(id -u)/dev.dsh-gateway.frpc
rm ~/Library/LaunchAgents/dev.dsh-gateway.*.plist
rm -rf ~/.dsh-gateway ~/Library/Logs/dsh-gateway
rm ~/.local/bin/dsh-gateway ~/.local/bin/frpc

# VPS
sudo systemctl disable --now frps caddy
sudo rm /etc/systemd/system/{frps,caddy}.service
sudo rm -rf /etc/frp /etc/caddy /var/lib/caddy   # /var/lib/caddy holds the CA private key
sudo rm /usr/local/bin/frps /usr/local/bin/caddy
sudo systemctl daemon-reload
sudo ufw delete allow 7000/tcp                   # and the two Caddy ports
```

Deleting `~/.dsh-gateway` destroys the pairing secret and the device list; DSH's
own sessions in `~/.dsh` are untouched. Deleting `/var/lib/caddy` destroys the CA
private key along with the certificate it signed — which is the right thing to do
on retirement, and the reason a phone that still trusts that root should have it
removed.

---

### 7.10 Notifications on a phone that cannot reach Google

Web Push on Android is Google's push service (FCM) and nothing else: the browser
will not use another. If the settings screen says *"Registration failed - push
service error"*, the phone's network cannot reach FCM (common on mainland
carrier networks) or the device has no Google Play services at all. Nothing on
this end can change that — so configure a chat channel instead:

```yaml
push:
  webhooks:
    - kind: feishu
      url: "https://open.feishu.cn/open-apis/bot/v2/hook/…"
```

A Feishu group bot needs no public callback: the gateway only makes an outbound
HTTPS request, and the card it posts carries an *Open the session* button built
from `publicURL`. Add the bot to the group (Group settings → Bots → Add bot →
Custom bot), copy its webhook address, and press **Send a test notification** in
the app's settings — it posts to the chat channel as well as to any browser that
subscribed, and reports each one separately.

iOS is unaffected: Safari's Web Push goes through APNs, which is reachable, so an
iPhone or iPad gets notifications without any of this.

## 8. Troubleshooting

**Start here.** Before working through the specific cases below, run:

```sh
dsh-gateway doctor
```

It walks the whole path — configuration, state directory, the local gateway, the
harness child, the frp control port, and the public HTTPS ingress — and reports
the first thing that is wrong with the specific change to make. Most of the
sections below exist because a particular failure was not obvious; the doctor was
written from those failures, so it usually names yours directly.

The one it exists for: a port behind a NAT can complete a TCP handshake and then
forward nothing. `nc -z` reports it **open**, `curl` returns `000`, and nothing
ever arrives. The doctor distinguishes that from a genuinely open port by
checking whether any bytes actually move.

Work from the inside out: Mac local → tunnel → VPS loopback → public HTTPS. Most
problems are one layer, and each layer has one command that identifies it.

```sh
curl -sS -o /dev/null -w 'mac    %{http_code}\n' http://127.0.0.1:8787/healthz
tail -3 ~/Library/Logs/dsh-gateway/frpc.log       # tunnel login + proxy start
sudo ss -ltnp | grep 18787                        # must be 127.0.0.1:18787
curl -skS -o /dev/null -w 'vps    %{http_code}\n' https://203-0-113-9.sslip.io:8443/healthz
```

### 8.1 The tunnel never connects, and ufw is not the only firewall

`frpc.log` repeats `try to connect to server...` and never logs
`login to server success`.

1. Is frps listening? On the VPS: `sudo ss -ltnp | grep 7000`.
   If not, `journalctl -u frps -n 50 --no-pager`.
2. Is the port reachable *from outside*? `nc -vz 203.0.113.9 7000` from the
   Mac. If frps is listening but nothing can connect, the block is either ufw
   (`sudo ufw status verbose`) **or your VPS provider's own firewall / security
   group**, which ufw cannot see or open. Cloud firewalls are the most common
   cause at this step; open 8080, 8443 and 7000 there (or whichever ports you
   gave `--http-port`/`--https-port`).

   **`nc` can lie, so do not stop at it.** On a NAT'd cloud host the port may be
   mapped such that the TCP handshake completes while no data ever flows —
   `nc -z` reports "open" and nothing arrives. Confirm with a request that
   actually carries bytes:

   ```sh
   curl -sS -o /dev/null -w '%{http_code}\n' --max-time 20 \
     http://203.0.113.9:8080/healthz
   ```

   `000` means blocked regardless of what `nc` said; a `301` means Caddy
   answered and the port is genuinely reachable. Ask by bare IP, as above: with
   `-H 'Host: 203-0-113-9.sslip.io'` the provider's plain-HTTP filter answers
   `403` itself, which looks like a dead port but is not (§8.7). This exact trap
   cost real time on the first deployment of this repository: ports 22 and 7000
   were reachable
   while 80 and 443 were not, so the tunnel came up perfectly and the
   certificate could never be issued. That turned out not to be a quirk but the
   provider's policy — 80 and 443 stay blocked until the domain has an ICP
   filing — which is why this deployment moved to 8443/8080. Compare
   `ip -4 addr show` against the public address — if the public IP appears on no
   interface, it is an EIP in front of the instance and **its** port mapping, not
   the instance's firewall, is what you must change.

   In `--tls acme` mode the same block shows up in Caddy's log as
   `Timeout during connect (likely firewall problem)` for both `http-01` and
   `tls-alpn-01`, which is the signature of a network-level block rather than a
   configuration fault: Caddy is listening correctly and simply never hears from
   the validator. In the default `tls internal` mode there is no validator to
   hear from, and the symptom is simply that nothing answers on the port.
3. Is the address right? `grep serverAddr ~/.dsh-gateway/frpc.toml`. A changed
   VPS IP needs `--server` on a re-run (§7.8).
4. Token mismatch shows up differently — see the next entry.

### 8.2 frp "port already used" / "port unavailable"

The message frp actually prints (in `journalctl -u frps` and `frpc.log`) is:

```
[dsh-gateway] start error: port unavailable          # client side
new proxy [dsh-gateway] type [tcp] error: port unavailable   # server side
```

It means frps could not bind 18787 on the VPS. Causes, in order of likelihood:

* **Another frpc is already connected and holding 18787** — for example the same
  Mac after a `launchctl` restart that left an old frpc process behind, or a
  second Mac. Check `sudo ss -ltnp | grep 18787` on the VPS and
  `pgrep -fl frpc` on the Mac. Only one client may claim a given remote port.
* **Something else on the VPS is using 18787.** `sudo ss -ltnp | grep 18787`
  names it. (Nothing in this deployment should: Caddy talks to the tunnel, it
  does not listen on it.)
* The port is outside `allowPorts` in `/etc/frp/frps.toml`. That produces a
  different, clearer error (`port not allowed`), but check it if you changed the
  proxy's `remotePort`.

If a stale frpc is the cause, `launchctl kickstart -k gui/$(id -u)/dev.dsh-gateway.frpc`
replaces it cleanly.

### 8.3 Caddy returns 502

`502` comes from Caddy, which means it accepted the request and then failed to
get a usable response from `127.0.0.1:18787`. The gateway never saw the request.

```sh
sudo ss -ltnp | grep 18787          # no output → no tunnel client is connected
tail -20 ~/Library/Logs/dsh-gateway/frpc.log
journalctl -u frps -n 30 --no-pager
```

* **Nothing listening on 18787**: frps has no registered proxy, so the Mac's
  frpc is down or failing to log in. Fix the tunnel first (§8.1, §8.2).
* **Listening, but 502 persists**: the tunnel exists and the far end is refusing
  connections — i.e. the gateway on the Mac is not listening on 8787. Check
  `curl http://127.0.0.1:8787/healthz` on the Mac and
  `tail -30 ~/Library/Logs/dsh-gateway/gateway.log`.
* **Everything is up but 502 only for some requests**: check `journalctl -u
  caddy` for `dial tcp 127.0.0.1:18787: connect: connection refused` versus a
  timeout — a timeout points at the tunnel's health, a refusal at the listener.

### 8.4 `dsh --profile acp` not found under launchd (PATH problem)

Symptom: the gateway starts, `/healthz` is `200`, but `/readyz` stays `503` and
`gateway.log` shows the harness failing to exec `dsh`, or the harness restarting
in a loop (`harness.restarted` in the audit log).

This is a `PATH` problem, not a DSH problem: launchd starts jobs with a minimal
`PATH` (`/usr/bin:/bin:/usr/sbin:/sbin`) and no shell profile, so a `dsh`
installed by npm — especially under nvm, where the bin directory contains a
version number — is invisible.

```sh
# What the job was actually given:
launchctl print gui/$(id -u)/dev.dsh-gateway.gateway | grep -A4 'environment'
# What a shell sees, for comparison:
command -v dsh
```

The installer derives that `PATH` from wherever `node`, `npm`, `dsh` and
`$NVM_BIN` live at install time, so the usual fix is simply to re-run it from a
shell where `command -v dsh` works:

```sh
FRP_TOKEN=… bash deploy/mac/install.sh
```

If `dsh` lives somewhere unusual, put the absolute path in the config instead —
that removes `PATH` from the equation entirely:

```yaml
dsh:
  binary: "/Users/you/.nvm/versions/node/v22.23.2/bin/dsh"
```

then `launchctl kickstart -k gui/$(id -u)/dev.dsh-gateway.gateway`. (Note that
an nvm path changes when you upgrade node; the installer's derived `PATH` is
normally the better answer, and `binary: "dsh"` is the default.)

### 8.5 The phone does not trust the certificate

Symptoms: the phone shows a certificate warning for
`https://203-0-113-9.sslip.io:8443`, or `curl` reports
`SSL certificate problem: unable to get local issuer certificate` because it
does not have the CA.

In the default `--tls internal` mode this is expected until the CA root is
installed on the device (§5.1), and Caddy's log will not show an ACME error
because no ACME exchange happens. Diagnose:

```sh
dig +short 203-0-113-9.sslip.io          # must be 203.0.113.9
sudo ss -ltnp | grep -E ':8080|:8443'       # Caddy must hold both

# Fetch the root. Over HTTPS, -k is correct exactly once: the certificate that
# URL presents is signed by the root it hands you (the bootstrap paradox, §5.2).
curl -skS -o ca.crt -w '%{http_code}\n' https://203-0-113-9.sslip.io:8443/ca.crt  # 200
# The same file over plain HTTP, on a host whose provider does not filter it:
#   curl -sS -o ca.crt http://203.0.113.9:8080/ca.crt

curl -sS --cacert ca.crt -o /dev/null -w '%{http_code}\n' \
  https://203-0-113-9.sslip.io:8443/healthz                                       # 200
journalctl -u caddy -n 50 --no-pager
```

If the root never downloaded in the first place, that is a different failure:
§8.7 for the provider's block page, §8.6 for HSTS on the plain-HTTP URL.

Causes, in order:

1. **The CA root was never installed, or was installed but not trusted.** On iOS
   those are two separate steps and skipping the second leaves the root inert —
   that is the most common cause by far. Re-do §5.1.
2. **The root on the phone is not the root the server is using.** That happens
   after installing a root from a previous deployment (or a previous CA on this
   host) or after `/var/lib/caddy` was deleted and Caddy created a new one.
   Compare the subject of the freshly downloaded `ca.crt` with what the phone
   holds, and re-install.
3. **The name does not resolve to this host.** sslip.io derives the address from
   the name; if the VPS IP changed, the name is now wrong. Re-run with
   `--domain <new-name>` (§7.8).
4. **The request went to the wrong port.** `https://…:8080` is plain HTTP, so a
   TLS handshake there fails; `https://…` with no port goes to 443, where
   nothing is listening in this deployment.
5. **You tested by IP.** `https://203.0.113.9:8443` can never match a
   certificate issued for a hostname. Use the name.

If you selected `--tls acme` instead, the failure looks different: Caddy's log
shows `acme: error` / `challenge failed`, and the usual cause is that the ACME
challenge ports are not reachable — http-01 needs 80, tls-alpn-01 needs 443
(§8.1). A rate limit also applies to a public CA (five failed validations per
hostname per hour), and the global options block in the Caddyfile can be pointed
at a staging directory while testing; a staging certificate is *not* trusted by
phones, so expect a warning while it is in use.

### 8.6 `http://…:8080/ca.crt` stops working after an HTTPS visit

Symptom: the plain-HTTP CA download URL that worked before now fails, and the
browser reports a TLS error or a connection reset against port 8080 — while
`curl` on the same URL still returns `200`. (If the provider's filter is active
too, `curl` returns its block page instead, and that is §8.7.)

The Caddyfile sends `Strict-Transport-Security: max-age=31536000;
includeSubDomains` on every HTTPS response, and HSTS upgrade rules ignore the
port. Once a browser profile has seen that header, it rewrites
`http://203-0-113-9.sslip.io:8080/ca.crt` to
`https://203-0-113-9.sslip.io:8080/ca.crt` — and Caddy speaks plain HTTP on
8080, so the handshake dies. Nothing is wrong with Caddy or the port. The HTTPS
CA URL (§5.1) is not affected: it is already HTTPS, so there is no upgrade to
attempt and HSTS has nothing to rewrite.

Fix, in order of convenience:

* Use the HTTPS CA URL — `https://203-0-113-9.sslip.io:8443/ca.crt` — which
  this document prefers anyway, or
* fetch the CA from a browser that has not loaded the app over HTTPS (a private
  window may still share the HSTS cache on some platforms, so a second browser
  is the safer bet), or
* clear the HSTS state for `203-0-113-9.sslip.io` and retry, or
* install the CA before the first HTTPS visit next time — over the HTTPS URL
  (§5.1).

### 8.7 The CA download is blocked (403) or the browser will not open it

Symptom: the CA download returns no certificate. If it is the plain-HTTP URL
(`http://<domain>:8080/ca.crt`), a provider that filters unfiled hostnames is
intercepting plain HTTP by the `Host` header. Such a response looks like this:

```sh
$ curl -sS -D- http://<domain>:8080/ca.crt
HTTP/1.1 403 Forbidden
Content-Type: text/html; charset=utf-8
Server: ADM/2.1.1
```

The body is an ICP notice. It is not user-agent dependent — curl's default UA, a
desktop Chrome UA and an iPhone Safari UA all get the same 403 — and it does not
touch TLS, so `https://…:8443` (and with it the whole app) is unaffected. On a
filtered host it can also switch on *after* the deployment has been working, which
is why the HTTPS route is the primary one rather than a fallback.

Fix, in order:

1. **Use the HTTPS CA URL** — `https://203-0-113-9.sslip.io:8443/ca.crt`
   (§5.1). A TLS session carries the name where the filter cannot read it, so
   this route keeps working; it costs the one-time warning the phone has to click
   through.
2. **Use the IP form** — `http://203.0.113.9:8080/ca.crt`. The filter keys on
   a domain in the `Host` header, so the bare IP is passed through today. A
   convenience, not a guarantee.
3. **Copy the root out of band** —
   `ssh root@203.0.113.9 'cat /var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt' > dsh-gateway-ca.crt`,
   then transfer the file to the phone and install it from Settings. No web fetch
   is involved at all.

If it is instead the *HTTPS* URL that will not open — no way past the certificate
warning, or a management policy that blocks the interstitial — fall back to the
plain-HTTP copy, the IP form, or the out-of-band file. A browser refusing the
*plain-HTTP* URL is neither of these; that is HSTS (§8.6).

### 8.8 The phone shows "history unavailable"

Opening a session's transcript answers `200` with `"unsupported": true` and no
items, and the app says history is unavailable. Sessions themselves still work.

The transcript is a **read-only projection of DSH's on-disk session log**, and
that log is versioned. This build understands version 4
(`internal/sessionlog.SupportedVersion`). After DSH writes a newer format, the
projector refuses to guess rather than showing you a misparsed conversation —
that refusal is the `unsupported` flag.

```sh
dsh --version                                     # what DSH is now
dsh-gateway version                               # what the gateway was built from
head -c 200 "$HOME/.dsh/sessions/"*/"$SESSION_ID"*.jsonl 2>/dev/null | jq -c '{version}'
tail -20 ~/Library/Logs/dsh-gateway/gateway.log | grep -i transcript
```

Fix: update the gateway to a build that knows the current format —

```sh
cd /path/to/dsh-gateway && git pull
FRP_TOKEN=… bash deploy/mac/install.sh
```

Until then, read the session on the desktop GUI, where DSH reads its own log
natively. Note that this is a *read* path only: live turns, approvals and
prompts are unaffected, because they come over ACP rather than from the log.

### 8.9 The gateway refuses to start, complaining about loopback

```
listen: "0.0.0.0:8787" is not a loopback address; the gateway must sit behind
the frp tunnel and a TLS terminator, never on a public interface
```

This is the guard rail working. The gateway is the component that can run
commands on your Mac; it has no TLS of its own, so binding it publicly would put
a command executor on the internet in clear text. `internal/config` refuses any
`listen` value that is not a loopback IP.

Fix the address, do not weaken the check:

```sh
grep -n 'listen:' ~/.dsh-gateway/config.yaml      # must be 127.0.0.1:8787 or [::1]:…
# An address that looks like a hostname ("localhost:8787") is also rejected:
# the check is on the parsed IP, so use the literal loopback address.
launchctl kickstart -k gui/$(id -u)/dev.dsh-gateway.gateway
```

If you genuinely need the gateway on another interface (a LAN, a VPN), do not
edit this: keep it on loopback and reach it through the tunnel. Tailscale or
WireGuard plus the loopback bind is the safe version of that request.

### 8.10 The pairing code is rejected

`401 invalid_pairing_code` — the code is wrong or its window has closed. The code
rotates every 10 minutes; run `dsh-gateway pair` again. The previous window is
accepted, so a code that is a few seconds "old" is fine, but one from an hour ago
is not.

`429 pairing_locked` — too many failed attempts from your client address. The
first lockout is 30 seconds and doubles per further failure up to an hour; a
successful pairing clears it. Wait, or pair from a different network.

`dsh-gateway pair` itself failing means the *Mac* is misconfigured, not the
phone: it validates the same configuration the gateway does, and it reads
`~/.dsh-gateway/pairing.key`, so it must run as same user (no `sudo`) and with
the same `-config`/`-state-dir` the service uses.

### 8.11 Paired, but every request is 401

The device token is expired or revoked, or the cookie was not kept.

```sh
jq -c '.devices[] | {name, expiresAt, revoked, lastSeen}' ~/.dsh-gateway/devices.json
jq -c 'select(.event=="auth.failed")' ~/.dsh-gateway/audit.jsonl | tail -5
```

Sessions last `auth.sessionTTL` (30 days). If `expiresAt` has passed or
`revoked` is `true`, pair the phone again. If the phone was using a private
browsing window, the cookie is gone when the window closes — pair again, or use a
normal window.

### 8.12 Desktop GUI proxy enabled but unusable

Only relevant if you set `desktopUI.enabled: true` (it is **off** by default).
With it on, DSH's own GUI is proxied at `/` — and DSH's sign-in flow will not
work through it: upstream `loginOrigin()` accepts only `http://localhost`,
`http://127.0.0.1` or `http://[::1]` with a port, so an `https://…sslip.io` origin
is rejected by design. See [security.md](security.md#93-deepseek-account-sign-in-does-not-work-behind-a-reverse-proxy)
for why, and use the desktop locally for anything that needs account sign-in.

---

## 9. Reference

### 9.1 Files

| Path | Side | Notes |
|---|---|---|
| `/usr/local/bin/{frps,caddy}` | VPS | pinned, checksum-verified |
| `/etc/frp/frps.toml` | VPS | 0640 `root:frp`; contains the tunnel token |
| `/etc/caddy/Caddyfile` | VPS | 0644; the previous version is kept as `Caddyfile.bak` |
| `/var/lib/caddy/.local/share/caddy/pki/authorities/local/` | VPS | Caddy's local CA: `root.crt` is the file both `/ca.crt` routes serve, and the CA private key lives here too — keep the directory private |
| `/etc/systemd/system/{frps,caddy}.service` | VPS | `User=frp` / `User=caddy` |
| `~/.local/bin/{dsh-gateway,frpc}` | Mac | override with `--install-dir` |
| `~/.dsh-gateway/config.yaml` | Mac | 0600, written once, yours thereafter |
| `~/.dsh-gateway/frpc.toml` | Mac | 0600, contains the tunnel token |
| `~/.dsh-gateway/devices.json` | Mac | 0600, paired devices (token *hashes* only) |
| `~/.dsh-gateway/models.json` | Mac | 0600, the model catalog last observed from DSH — a cache the next session overwrites, safe to delete |
| `~/.dsh-gateway/audit.jsonl` | Mac | 0600, append-only security log |
| `~/.dsh-gateway/pairing.key` | Mac | 0600, the pairing secret |
| `~/Library/LaunchAgents/dev.dsh-gateway.*.plist` | Mac | launchd jobs |
| `~/Library/Logs/dsh-gateway/` | Mac | gateway/frpc stdout+stderr |

### 9.2 Commands worth knowing

| Command | Purpose |
|---|---|
| `dsh-gateway run -config ~/.dsh-gateway/config.yaml` | what launchd runs; `run` is the default command |
| `dsh-gateway run -h` | every flag: `-listen`, `-public-url`, `-state-dir`, `-workspace`, `-expose-desktop-ui`, … |
| `dsh-gateway pair` | print code + link + QR |
| `dsh-gateway pair -json` | machine-readable pairing output |
| `dsh-gateway version` | build stamp |
| `frpc verify -c ~/.dsh-gateway/frpc.toml` | validate the client config |
| `sudo frps verify -c /etc/frp/frps.toml` | validate the server config |
| `sudo caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile` | validate the Caddyfile |
| `sudo caddy reload --config /etc/caddy/Caddyfile` | apply Caddyfile changes without dropping connections |
| `launchctl kickstart -k gui/$(id -u)/dev.dsh-gateway.gateway` | restart the gateway |
| `launchctl bootout gui/$(id -u)/dev.dsh-gateway.frpc` | stop the tunnel client |

### 9.3 Configuration knobs that change behaviour

| Key | Default | Effect |
|---|---|---|
| `publicURL` | from `--public-url`: `https://203-0-113-9.sslip.io:8443` | must be the exact public HTTPS URL, port included: the pairing link, `Secure` cookies and the `Origin` check all derive from it |
| `auth.sessionTTL` | `720h` | how long a phone stays paired |
| `auth.pairingTTL` | `10m` | pairing-code window (max 1h) |
| `auth.trustedProxies` | loopback | who may set `X-Forwarded-For`; widening this lets clients spoof their address and evade the lockout |
| `session.idleTimeout` | `5m` | how long a session stays attached to the phone before the desktop can open it again |
| `session.defaultModel` | empty | the model a *new* session gets when the app sends none. Empty leaves the choice to the harness, which is the only honest default: the right value names a provider route that exists on your machine, not on someone else's. To pin one, copy the opaque ACP id from `GET /models` or from the app's own picker and quote it in YAML; a stale one costs a log warning, not a failed session |
| `session.defaultReasoningEffort` | empty | same for reasoning effort (`off` / `low` / `high` / `max`) |
| `session.approvalTimeout` | `5m` | how long an approval waits before it is **rejected** |
| `session.approvalGrantTTL` | `30m` | how long a *scoped* approval lasts — "allow this tool in this session" — after which it stops applying. Bounded on three axes: one session, one tool, and this lifetime; it also dies with its session. `0` disables scoped approvals entirely, leaving only allow-once and reject-once. Read [security.md §6.1](security.md) before turning it on |
| `session.promptQueueDepth` | `4` | how many prompts may wait behind a running turn in one session. A follow-up typed while the agent works is queued and runs when the turn finishes; `0` refuses it with `409`, which is what earlier versions did |
| `limits.maxBodyBytes` | `8388608` | the request body ceiling. 8 MiB rather than the 1 MiB a text-only prompt needed, because a prompt may now carry a photograph and base64 inflates one by a third |
| `limits.maxImageBytes` | `0` | a per-image cap, decoded. `0` means no bound tighter than `maxBodyBytes`, which is the real ceiling either way |
| `limits.toolInputBytes` | `65536` | how much of one tool call's arguments is sent to a phone. Long string values are clipped rather than the document being cut, so the JSON stays parseable and the client can still say what the call was; an item that was trimmed says so with `inputTruncated` |
| `limits.toolOutputBytes` | `24576` | how much of one tool result is sent. The beginning and the end are kept — where a failure starts and where the exit status is — with an elision between them, and the item reports `outputTruncated`. Raise it if you read long results on a tablet, lower it on a metered connection: a card scrolls inside 60dvh whatever you choose |
| `changes.revert.enabled` | `false` | whether the gateway may write to your files to undo a session's recorded changes. **This is the only feature here that writes**, and with it off no reverter is constructed at all — the path is absent, not disabled. `POST /sessions/{id}/revert` then answers `503 revert_disabled`. Reading what changed needs none of it |
| `dsh.sandboxMode` | `workspace-write` | `danger-full-access` silently disables approval prompts entirely; the gateway warns at startup if you set it |
| `workspaces` | required | the only directories the phone can open. There is no default, and your home directory itself is refused: this list is the boundary between a paired phone and everything you own. Matching is exact, so a subdirectory needs its own entry, and `~/.dsh/storages/workspace.json` is the desktop's own list of them — mirror it by hand to offer the phone the same projects |
| `push.enabled` | `true` | Web Push notifications: approvals waiting, approvals that **expired**, turns that finished after running longer than the threshold, turns that **failed** at any length, and the agent process giving up. Subscriptions are dropped when a device is revoked |
| `push.subject` | `mailto:dsh-gateway@localhost` | the contact a push service can reach you at. RFC 8292 allows a `mailto:` or `https:` URI and nothing else, and the shape is validated at startup — a bare address is rejected there rather than by Apple with a bare 403. The shipped value is a placeholder that reaches nobody: it is accepted, but the gateway warns at every start until you replace it |
| `push.turnThreshold` | `2m` | how long a turn must run before finishing is worth a notification |
| `push.includeSessionName` | `false` | whether a notification body may carry the session's title (or the first line of the prompt that opened it). Off because a notification is read on a lock screen and mirrored to any webhook — see [PRIVACY.md](../PRIVACY.md) |
| `push.includeTaskNames` | `true` | whether a notification may name a delegated task by the description the model wrote for it. On because without it every subagent in a session settles as an identical "Subagent finished"; see [PRIVACY.md](../PRIVACY.md) |
| `push.webhooks` | none | chat channels that receive the same notifications. **This is the delivery path that works on a phone which cannot reach Google's push service** — Android Web Push is FCM and nothing else, so a phone without Google Play services, or on a network that cannot route to it, can never receive a Web Push however correct this end is. Empty by default: a webhook posts in clear to a third party, which is your decision to make and not this project's |
| `curation.testTitlePatterns` | none | extra regexes, ORed with the built-in set, that recognise a title only an automated run would produce. The shipped patterns are the portable ones — a marker belonging to one person's test harness has no business in the source |
| `curation.tempRoots` | platform temp dir | directories triage treats as scratch work |
| `receipt.pricing` | none | unit prices (per million tokens) used to estimate a session's cost. Without an entry the receipt shows tokens, tools and duration but no money — the gateway will not invent a price, and the authoritative numbers carry an account discount it cannot know |
| `transcript.enabled` | `true` | read-only history projection |
| `transcript.follow` | `true` | stream what other DSH processes append to a session log, so a desktop session is live on the phone. Sessions this gateway drives are excluded rather than duplicated |
| `transcript.followInterval` | `1s` | how often those logs are re-read; raise it on a slow machine, lower it for a snappier feed |
| `desktopUI.enabled` | `false` | exposes DSH's own GUI at `/`; read the threat model first |
