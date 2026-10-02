# deploy/

Everything needed to put `dsh-gateway` on the public internet behind TLS the
phone trusts, with the gateway itself never leaving loopback.

```
phone ──HTTPS:8443──> VPS ──Caddy :8443/:8080──> frps 127.0.0.1:18787
                                                 ║ frp tunnel (TLS + encryption + token)
                                                 ▼
                         Mac ──frpc──> 127.0.0.1:8787 ──> dsh-gateway ──> dsh --profile acp
```

8443 and 8080 are the defaults (`--https-port`/`--http-port`); in that default
mode nothing listens on 80 or 443. Caddy issues from its own CA
(`--tls internal`), because many providers block the low ports until the domain
has an ICP filing; the phone installs that root once, from the same file served
in two places: `https://<domain>:8443/ca.crt` (primary — TLS keeps the request
away from a provider's plain-HTTP filter, at the cost of one warning to click
through) and `http://<domain>:8080/ca.crt` (no warning, but interceptable). With
`--tls acme` Caddy gets a publicly trusted certificate instead — that mode needs
`--https-port 443 --http-port 80` and needs nothing installed on the phone.

| Path | What it is |
|---|---|
| [`vps/install.sh`](vps/install.sh) | Idempotent root installer for the VPS: Caddy + frps binaries (pinned, checksum-verified), configs, systemd units, ufw. Prints the frp token. |
| [`vps/frps.toml.tmpl`](vps/frps.toml.tmpl) | frps config rendered to `/etc/frp/frps.toml`. Contains the load-bearing `proxyBindAddr = "127.0.0.1"`. |
| [`vps/Caddyfile.tmpl`](vps/Caddyfile.tmpl) | Caddyfile rendered to `/etc/caddy/Caddyfile`: the internal CA by default (or ACME), the CA root published on the HTTPS and HTTP ports, HTTP→HTTPS redirect, HSTS, streaming reverse proxy. |
| [`mac/install.sh`](mac/install.sh) | Idempotent macOS installer: builds the gateway, installs frpc, writes config + launchd agents, starts both. Prints the pairing steps. |
| [`mac/config.yaml.tmpl`](mac/config.yaml.tmpl) | Gateway config rendered (once, never overwritten) to `~/.dsh-gateway/config.yaml`. Mirrors `internal/config` field for field. |
| [`mac/frpc.toml.tmpl`](mac/frpc.toml.tmpl) | frpc config rendered to `~/.dsh-gateway/frpc.toml` (0600). |

## Order

1. **VPS**, as root:

   ```sh
   sudo bash deploy/vps/install.sh --domain 203-0-113-9.sslip.io
   ```

   `--domain` is required. With no domain of your own, sslip.io resolves
   `<ip-with-dashes>.sslip.io` to this host, so `203.0.113.9` becomes
   `203-0-113-9.sslip.io`. It ends by printing the frp token. Keep it for step 2.

2. **Mac**, as yourself (never with `sudo`):

   ```sh
   FRP_TOKEN=<token from step 1> bash deploy/mac/install.sh \
     --server <vps address> \
     --public-url https://<domain>:8443 \
     --workspaces "$HOME/code"
   ```

   All three are required, and deliberately so: `--server` is where your tunnel
   token goes, `--public-url` is the name the phone will trust, and
   `--workspaces` is what on this machine the phone may reach. None of them has a
   default, because a default would be a decision about your infrastructure made
   by whoever wrote this file. `--workspaces` will not accept your home directory
   itself — name the directories you actually work in.

3. **Phone trust**: in the default `--tls internal` mode, open
   `https://<domain>:8443/ca.crt` on the phone, accept the one-time certificate
   warning, and install the root (iOS needs the install *and* the trust toggle).
   It is a trust-on-first-use fetch either way, so take it from a network you
   trust. Two alternatives, if plain HTTP is not filtered where you are:

   ```
   http://<domain>:8080/ca.crt          # no warning, but interceptable
   ssh root@<vps> \
     'cat /var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt' \
     > dsh-gateway-ca.crt
   ```

   Skip this under `--tls acme`, and remove the root when you retire the
   deployment.

4. **Pair the phone**: open the public URL
   (`https://<domain>:8443` by default), then run `dsh-gateway pair` on the Mac
   and enter (or scan) the code.

Re-running either script is safe: the frp token is reused rather than
regenerated, config files are rewritten only when their content changes, and
`~/.dsh-gateway/config.yaml` is never touched once it exists.

Full walkthrough, updates, log locations, token rotation, device revocation and
troubleshooting: [`../docs/deployment.md`](../docs/deployment.md).
Threat model and limitations: [`../docs/security.md`](../docs/security.md).
