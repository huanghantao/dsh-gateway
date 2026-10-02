#!/usr/bin/env bash
#
# install.sh — bring up the VPS half of the dsh-gateway deployment.
#
# What this builds, and why each piece exists:
#
#   phone ──HTTPS──> Caddy ──> frps 127.0.0.1:18787 ══tunnel══> frpc (Mac)
#
#   * Caddy terminates TLS and is the only process with a public listener. Which
#     ports, and where its certificate comes from, depend on the host: by
#     default it serves HTTPS on 8443 with a certificate from a CA it generates
#     itself, because some providers block inbound 80/443 (and the usual
#     alternates) until the domain has an ICP filing — which also rules out
#     every ACME challenge that sslip.io could satisfy. `--tls acme` switches to
#     a publicly trusted certificate on the challenge ports instead.
#   * frps accepts the tunnel from the Mac and binds the proxy port on
#     LOOPBACK only, so the gateway is never on the public internet.
#
# The script is idempotent: re-running it re-checks the binaries, rewrites a
# config only when the rendered content actually differs, and never regenerates
# the frp token (regenerating it would silently break a working tunnel until the
# Mac is updated to match).
#
# Usage:
#   sudo bash deploy/vps/install.sh
#   sudo bash deploy/vps/install.sh --domain 203-0-113-7.sslip.io
#   sudo bash deploy/vps/install.sh --force        # reinstall binaries
#   sudo bash deploy/vps/install.sh --help
#
# Environment:
#   FRP_CONTROL_ALLOW_FROM  optional source CIDR for the frp control port, e.g.
#                           "198.51.100.7/32". Narrowing this to the Mac's
#                           current address is stronger than leaving 7000 open
#                           to the world, at the cost of breaking when your ISP
#                           changes it. Unset (default) allows any source.

set -euo pipefail

# ---------------------------------------------------------------------------
# Pinned versions.
#
# Bump deliberately, not automatically:
#   FRP_VERSION   — check https://github.com/fatedier/frp/releases . If a new
#                   release renames the checksum asset, update
#                   FRP_CHECKSUMS_ASSET too; the download is verified against
#                   it, so a stale name fails loudly instead of silently.
#                   Re-run `frps verify -c /etc/frp/frps.toml` afterwards:
#                   frp parses TOML strictly and drops/renames keys between
#                   releases (the proxyBindAddr key this deployment depends on
#                   is documented in the template — re-read that note).
#   CADDY_VERSION — check https://github.com/caddyserver/caddy/releases .
#                   Caddy's checksums file is SHA-512, not SHA-256.
# ---------------------------------------------------------------------------
FRP_VERSION="0.71.0"
FRP_CHECKSUMS_ASSET="frp_sha256_checksums.txt"
CADDY_VERSION="2.11.4"
CADDY_CHECKSUMS_ASSET="caddy_${CADDY_VERSION}_checksums.txt"

readonly FRP_DOWNLOAD_BASE="https://github.com/fatedier/frp/releases/download"
readonly CADDY_DOWNLOAD_BASE="https://github.com/caddyserver/caddy/releases/download"

# Caddy's own apt repository. It is preferred over the release tarball for two
# reasons that both showed up on a real deployment:
#
#   1. It is fast where GitHub is not. On a China-hosted VPS the tarball came
#      down at roughly 34 KB/s and timed out repeatedly, while this repository
#      answered in under two seconds. A 15 MB binary over that link is a seven
#      minute download that a flaky uplink will not finish.
#   2. It is the vendor's signed channel. The .deb is verified by apt against a
#      GPG key fetched over TLS from the same vendor, which is at least as strong
#      as comparing a tarball against a digest fetched from the same host that
#      served the tarball — and unlike a digest, the signature cannot be swapped
#      by whoever controls that one host.
#
# The tarball path remains as a fallback for hosts with no apt.
readonly CADDY_APT_KEY_URL="https://dl.cloudsmith.io/public/caddy/stable/gpg.key"
readonly CADDY_APT_LIST_URL="https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt"
readonly CADDY_KEYRING="/usr/share/keyrings/caddy-stable-archive-keyring.gpg"
readonly CADDY_SOURCES="/etc/apt/sources.list.d/caddy-stable.list"

# Wait for the apt lock instead of failing the moment it is held.
#
# A fresh Ubuntu image runs unattended-upgrades, which takes the dpkg and apt
# locks for minutes. Without this, an otherwise perfect install fails
# intermittently with "could not get lock", and the Caddy step then falls back to
# a GitHub download that is unusably slow on some hosts.
readonly APT_LOCK_OPTS=(-o DPkg::Lock::Timeout=180)

# The public hostname Caddy serves. Required — there is no default, because a
# default would be a name whose DNS points at whoever wrote this file, and the
# phone is about to be told to trust a CA root served under it.
#
# sslip.io's wildcard DNS maps an address back to a name, so if you have no
# domain of your own, the name for this host is its public IP with the dots
# turned into dashes plus .sslip.io (203.0.113.9 -> 203-0-113-9.sslip.io).
DOMAIN=""

readonly FRP_CONTROL_PORT=7000

# Where Caddy serves HTTPS, and the plain-HTTP port that exists only to hand out
# the CA certificate.
#
# Not 443/80, because on some providers those are blocked outright until the
# domain has an ICP filing. 8443 and 8080 are the shipped choice for that case,
# but they are only a guess about your provider: pick any pair your host actually
# carries. See Caddyfile.tmpl for the full reasoning, including why a publicly
# trusted certificate is not obtainable without 80 or 443.
HTTPS_PORT=8443
HTTP_PORT=8080

# Where the certificate comes from: "internal" (Caddy signs with a CA it
# generates on this host; the phone installs that root once) or "acme" (a
# publicly trusted certificate, which needs the challenge ports — see --tls in
# the usage text). The default is the only mode that works on a provider which
# blocks 80 and 443.
TLS_MODE="internal"
readonly FRP_PROXY_PORT=18787
readonly BIN_DIR="/usr/local/bin"
readonly FRP_CONFIG="/etc/frp/frps.toml"
readonly CADDYFILE="/etc/caddy/Caddyfile"
readonly FRPS_UNIT="/etc/systemd/system/frps.service"
readonly CADDY_UNIT="/etc/systemd/system/caddy.service"

FORCE=0
WORK_DIR=""
# Set to 1 by install_config whenever a live file actually changed, so a re-run
# that changes nothing does not bounce the services (and therefore the tunnel).
CONFIG_CHANGED=0

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------
if [[ -t 2 ]]; then
	readonly C_INFO=$'\033[1;34m' C_WARN=$'\033[1;33m' C_ERR=$'\033[1;31m' C_OFF=$'\033[0m'
else
	readonly C_INFO="" C_WARN="" C_ERR="" C_OFF=""
fi

log() { printf '%s==>%s %s\n' "$C_INFO" "$C_OFF" "$*" >&2; }
warn() { printf '%swarning:%s %s\n' "$C_WARN" "$C_OFF" "$*" >&2; }
die() {
	printf '%serror:%s %s\n' "$C_ERR" "$C_OFF" "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
install.sh — set up Caddy + frps on the VPS side of dsh-gateway.

Usage:
  sudo bash install.sh [options]

Options:
  --domain NAME   Public hostname for the gateway. Required. If you have no
                  domain, sslip.io resolves <ip-with-dashes>.sslip.io to this
                  host, so 203.0.113.9 becomes 203-0-113-9.sslip.io.
  --https-port N  Port Caddy serves HTTPS on. Default: 8443. 443 is the natural
                  choice, but it is blocked on providers that require an ICP
                  filing; pick a port your provider actually carries.
  --http-port N   Plain-HTTP port. In the default --tls internal mode it serves
                  exactly one thing — the CA certificate a phone must install —
                  and redirects everything else to HTTPS. Default: 8080. With
                  --tls acme it is the port ACME's http-01 challenge arrives on,
                  so it should be 80.
  --tls MODE      Where the certificate comes from:

                    internal  Caddy signs with a CA it generates on this host.
                              The root is published at
                              http://DOMAIN:HTTP_PORT/ca.crt and must be
                              installed once on each phone. The default, and the
                              only mode that works when 80/443 are blocked:
                              every ACME challenge that does not need a DNS
                              provider API needs one of those two ports, and
                              sslip.io has no DNS API.

                    acme      Caddy obtains a publicly trusted certificate.
                              Needs the challenge ports to be reachable, so this
                              normally means --https-port 443 --http-port 80.
                              No phone-side installation is involved.

  --force         Re-download and reinstall the frps and caddy binaries even if
                  the pinned versions are already installed. Config files are
                  still only rewritten when their content changes.
  -h, --help      Show this help.

Environment:
  FRP_CONTROL_ALLOW_FROM   Optional source CIDR allowed to reach the frp
                           control port (default: any source).

What it touches:
  /usr/local/bin/{frps,caddy}
  /etc/frp/frps.toml            (0640 root:frp, holds the tunnel token)
  /etc/caddy/Caddyfile          (0644 root:root)
  /etc/systemd/system/{frps,caddy}.service
  ufw rules: allow SSH port(s), HTTP_PORT/tcp, HTTPS_PORT/tcp, and the frp
             control port. FRP_CONTROL_ALLOW_FROM may narrow that last one.

The frp token is printed at the end. It is generated once and reused forever
after; copy it into the Mac installer.
EOF
}

# ---------------------------------------------------------------------------
# Argument / environment checks
# ---------------------------------------------------------------------------
parse_args() {
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--domain)
			[[ $# -ge 2 ]] || die "--domain needs a value"
			DOMAIN="$2"
			shift 2
			;;
		--https-port)
			[[ $# -ge 2 ]] || die "--https-port needs a value"
			HTTPS_PORT="$2"
			shift 2
			;;
		--http-port)
			[[ $# -ge 2 ]] || die "--http-port needs a value"
			HTTP_PORT="$2"
			shift 2
			;;
		--tls)
			[[ $# -ge 2 ]] || die "--tls needs a value"
			TLS_MODE="$2"
			shift 2
			;;
		--force)
			FORCE=1
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*) die "unknown argument: $1 (try --help)" ;;
		esac
	done
	[[ -n $DOMAIN ]] || die "--domain must not be empty"
	if [[ $DOMAIN == *"/"* || $DOMAIN == *:* ]]; then
		die "--domain takes a bare hostname, got: $DOMAIN"
	fi

	# Ports have to be real ports before they reach ufw, Caddy or the summary.
	local port_var port_value
	for port_var in HTTPS_PORT HTTP_PORT; do
		port_value="${!port_var}"
		[[ $port_value =~ ^[0-9]+$ ]] || die "${port_var} takes a port number, got: $port_value"
		((port_value >= 1 && port_value <= 65535)) || die "${port_var} is not a port: $port_value"
	done
	[[ $HTTPS_PORT != "$HTTP_PORT" ]] ||
		die "the HTTPS and plain-HTTP ports must differ (both $HTTPS_PORT)"

	case "$TLS_MODE" in
	internal | acme) ;;
	*) die "--tls takes 'internal' or 'acme', got: $TLS_MODE" ;;
	esac

	# Worth saying out loud rather than discovering from a failed handshake:
	# without a DNS API there is no challenge that avoids 80 and 443.
	if [[ $TLS_MODE == acme && ($HTTPS_PORT != 443 || $HTTP_PORT != 80) ]]; then
		warn "--tls acme normally needs --https-port 443 --http-port 80 (tls-alpn-01
       and http-01 respectively); with $HTTPS_PORT/$HTTP_PORT certificate issuance
       will most likely fail. Use --tls internal if this provider blocks them."
	fi
}

require_root() {
	[[ ${EUID:-$(id -u)} -eq 0 ]] || die "must run as root, e.g.: sudo bash $0 --help"
}

require_supported_os() {
	[[ -r /etc/os-release ]] || die "/etc/os-release is missing; this script targets Ubuntu 24.04"
	# shellcheck source=/dev/null
	. /etc/os-release
	if [[ ${ID:-} != "ubuntu" ]]; then
		die "this script targets Ubuntu (24.04); found ID=${ID:-unknown}. The
       package and systemd assumptions below are Ubuntu's."
	fi
	if [[ ${VERSION_ID:-} != "24.04" ]]; then
		# Not fatal: the steps are the same on adjacent releases. Warn so that a
		# surprise later is not a mystery.
		warn "written and tested against Ubuntu 24.04; this is ${VERSION_ID:-unknown}. Continuing."
	fi
}

detect_arch() {
	local machine
	machine="$(uname -m)"
	case "$machine" in
	x86_64 | amd64) printf 'amd64' ;;
	aarch64 | arm64) printf 'arm64' ;;
	*) die "unsupported architecture: $machine (frp and Caddy ship amd64 and arm64)" ;;
	esac
}

# ---------------------------------------------------------------------------
# Packages and downloads
# ---------------------------------------------------------------------------
ensure_base_packages() {
	local missing=() cmd
	for cmd in curl openssl tar ufw; do
		command -v "$cmd" >/dev/null 2>&1 || missing+=("$cmd")
	done
	if [[ ${#missing[@]} -eq 0 ]]; then
		log "base packages already present"
		return 0
	fi

	log "installing base packages: ${missing[*]}"
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -qq "${APT_LOCK_OPTS[@]}"
	# openssl: token and key generation. curl/ca-certificates: release
	# downloads over HTTPS. tar: extracting them. ufw: the firewall rules.
	apt-get install -y -qq --no-install-recommends "${APT_LOCK_OPTS[@]}" \
		curl ca-certificates openssl tar ufw
}

# verify_sha ALGO CHECKSUM_FILE ASSET_NAME FILE
#
# Compares FILE against the digest published for ASSET_NAME. Downloading over
# HTTPS already authenticates the transport, but this catches a truncated file,
# a CDN/cache mix-up, or a swapped release asset before it becomes /usr/local/bin.
verify_sha() {
	local algo="$1" sumfile="$2" asset="$3" file="$4" want got
	# Field 2 is the asset name in both projects' checksum files.
	want="$(awk -v a="$asset" '$2 == a { print $1; exit }' "$sumfile")"
	[[ -n $want ]] || die "no checksum entry for $asset in $(basename "$sumfile")"
	case "$algo" in
	sha256) got="$(sha256sum "$file" | awk '{print $1}')" ;;
	sha512) got="$(sha512sum "$file" | awk '{print $1}')" ;;
	*) die "verify_sha: unsupported algorithm $algo" ;;
	esac
	[[ $got == "$want" ]] || die "checksum mismatch for $asset
       expected $want
       got      $got
       Refusing to install a binary that does not match its published digest."
	log "checksum ok: $asset"
}

download() {
	local url="$1" dest="$2"
	# -f: fail on HTTP errors instead of writing an error page.
	# -L: GitHub release downloads are redirects; without this, -O writes a
	#     zero-byte file (a trap worth knowing about).
	# --retry: a flaky VPS uplink should not require a re-run.
	curl -fsSL --retry 3 --retry-delay 2 --connect-timeout 15 -o "$dest" "$url" ||
		die "download failed: $url"
}

install_frp_binary() {
	local arch="$1"
	if [[ $FORCE -eq 0 && -x $BIN_DIR/frps ]]; then
		local current
		current="$("$BIN_DIR/frps" --version 2>/dev/null || true)"
		if [[ $current == "$FRP_VERSION" ]]; then
			log "frps $FRP_VERSION already installed"
			return 0
		fi
		log "frps $current installed; upgrading to $FRP_VERSION"
	fi

	local asset="frp_${FRP_VERSION}_linux_${arch}.tar.gz"
	log "downloading $asset"
	download "${FRP_DOWNLOAD_BASE}/v${FRP_VERSION}/${asset}" "$WORK_DIR/$asset"
	download "${FRP_DOWNLOAD_BASE}/v${FRP_VERSION}/${FRP_CHECKSUMS_ASSET}" "$WORK_DIR/$FRP_CHECKSUMS_ASSET"
	verify_sha sha256 "$WORK_DIR/$FRP_CHECKSUMS_ASSET" "$asset" "$WORK_DIR/$asset"

	tar -xzf "$WORK_DIR/$asset" -C "$WORK_DIR"
	# The tarball also contains frpc; only the server belongs on the VPS, and
	# not shipping it removes a tool an intruder could use to pivot out.
	install -m 0755 "$WORK_DIR/frp_${FRP_VERSION}_linux_${arch}/frps" "$BIN_DIR/frps"
	log "installed $BIN_DIR/frps $FRP_VERSION"
}

# install_caddy picks a channel and installs $BIN_DIR/caddy.
#
# Both channels put the binary in the same place so that the systemd unit this
# script writes is correct either way. The apt package is deliberately NOT
# installed: it ships its own unit, and having two units own one service is a
# worse problem than the one being solved here. Only the binary is extracted.
install_caddy_binary() {
	local arch="$1"
	if [[ $FORCE -eq 0 && -x $BIN_DIR/caddy ]]; then
		local current
		current="$("$BIN_DIR/caddy" version 2>/dev/null | awk '{print $1}' || true)"
		if [[ $current == "v$CADDY_VERSION" ]]; then
			log "caddy v$CADDY_VERSION already installed"
			return 0
		fi
		log "caddy ${current:-unknown} installed; upgrading to v$CADDY_VERSION"
	fi

	if install_caddy_from_apt; then
		return 0
	fi
	log "the Caddy apt repository was not usable; falling back to the release tarball"

	local asset="caddy_${CADDY_VERSION}_linux_${arch}.tar.gz"
	log "downloading $asset"
	download "${CADDY_DOWNLOAD_BASE}/v${CADDY_VERSION}/${asset}" "$WORK_DIR/$asset"
	download "${CADDY_DOWNLOAD_BASE}/v${CADDY_VERSION}/${CADDY_CHECKSUMS_ASSET}" "$WORK_DIR/$CADDY_CHECKSUMS_ASSET"
	# SHA-512: that is what Caddy publishes. Do not "fix" this to sha256.
	verify_sha sha512 "$WORK_DIR/$CADDY_CHECKSUMS_ASSET" "$asset" "$WORK_DIR/$asset"

	tar -xzf "$WORK_DIR/$asset" -C "$WORK_DIR" caddy
	install -m 0755 "$WORK_DIR/caddy" "$BIN_DIR/caddy"
	log "installed $BIN_DIR/caddy v$CADDY_VERSION"
}

# install_caddy_from_apt extracts the caddy binary from the vendor's signed apt
# package. It returns non-zero at the first sign of trouble so the caller can
# fall back; it never dies, because a slow or unreachable repository is a reason
# to use another channel, not a reason to stop.
install_caddy_from_apt() {
	command -v apt-get >/dev/null 2>&1 || return 1
	command -v dpkg-deb >/dev/null 2>&1 || return 1

	if ! curl -fsSL --retry 2 --connect-timeout 15 -o "$WORK_DIR/caddy.gpg.key" "$CADDY_APT_KEY_URL"; then
		log "caddy apt signing key could not be fetched from $CADDY_APT_KEY_URL"
		return 1
	fi
	if ! curl -fsSL --retry 2 --connect-timeout 15 -o "$WORK_DIR/caddy.deb.txt" "$CADDY_APT_LIST_URL"; then
		log "caddy apt source list could not be fetched from $CADDY_APT_LIST_URL"
		return 1
	fi

	local line
	line="$(grep -m1 '^deb ' "$WORK_DIR/caddy.deb.txt" || true)"
	if [[ -z $line ]]; then
		log "the caddy apt source list contained no 'deb' line"
		return 1
	fi

	# Use the published line verbatim and install the key wherever that line says
	# it lives.
	#
	# An earlier revision rewrote the signed-by path to a location of its own
	# choosing. That is how a malformed entry gets written: a bad substitution
	# produces a line apt cannot parse, and the failure looks like a broken
	# repository rather than a broken script. Trusting the vendor's line removes
	# the class of bug entirely — there is nothing left to get wrong.
	local keypath="$CADDY_KEYRING"
	if [[ $line =~ signed-by=([^]]+) ]]; then
		keypath="${BASH_REMATCH[1]}"
	fi
	install -d -m 0755 "$(dirname "$keypath")"
	if ! gpg --dearmor --yes -o "$keypath" "$WORK_DIR/caddy.gpg.key" 2>/dev/null; then
		log "the caddy apt signing key could not be installed to $keypath"
		return 1
	fi
	chmod 0644 "$keypath"

	printf '%s\n' "$line" >"$CADDY_SOURCES"

	# Capture the output rather than discarding it. A silent fallback to a slow
	# channel is impossible to diagnose after the fact, which is exactly what
	# happened on the first real deployment of this script.
	local aptout
	if ! aptout="$(apt-get update -qq "${APT_LOCK_OPTS[@]}" \
		-o Dir::Etc::sourcelist="$CADDY_SOURCES" \
		-o Dir::Etc::sourceparts="-" \
		-o APT::Get::List-Cleanup="0" 2>&1)"; then
		log "caddy apt repository could not be refreshed:"
		printf '%s\n' "$aptout" | sed 's/^/    /' >&2
		return 1
	fi

	# Download only. Installing the package would create a competing unit.
	rm -f "$WORK_DIR"/caddy_*.deb
	local dlout
	if ! dlout="$(cd "$WORK_DIR" && apt-get download -qq "${APT_LOCK_OPTS[@]}" caddy 2>&1)"; then
		log "caddy apt package could not be downloaded:"
		printf '%s\n' "$dlout" | sed 's/^/    /' >&2
		return 1
	fi

	local deb
	deb="$(find "$WORK_DIR" -maxdepth 1 -name 'caddy_*.deb' -print -quit)"
	[[ -n $deb ]] || return 1

	# Extract just the binary from the signature-verified package.
	if ! dpkg-deb --fsys-tarfile "$deb" | tar -xO ./usr/bin/caddy >"$WORK_DIR/caddy.from-deb" 2>/dev/null; then
		log "caddy could not be extracted from its package"
		return 1
	fi
	[[ -s "$WORK_DIR/caddy.from-deb" ]] || return 1

	install -m 0755 "$WORK_DIR/caddy.from-deb" "$BIN_DIR/caddy"
	log "installed $BIN_DIR/caddy $("$BIN_DIR/caddy" version 2>/dev/null | awk '{print $1}') from the signed apt repository"
	return 0
}

# ---------------------------------------------------------------------------
# Users
# ---------------------------------------------------------------------------
ensure_frp_user() {
	# The group is created explicitly rather than relying on --user-group, whose
	# behaviour depends on USERGROUPS_ENAB in /etc/login.defs. The config file's
	# 0640 root:frp mode is what lets the service (and only the service) read the
	# tunnel token, so the group's existence is load-bearing.
	getent group frp >/dev/null 2>&1 || groupadd --system frp

	if id -u frp >/dev/null 2>&1; then
		log "user frp already exists"
		# Idempotent, and it repairs the case where frp was created earlier with
		# a different primary group: systemd gives the service its supplementary
		# groups, so membership is what matters.
		usermod -aG frp frp
	else
		log "creating system user frp"
		# No home, no shell: frps needs neither. Running it as an unprivileged
		# user is cheap insurance, since the only privileged thing it does is
		# bind port 7000 (>1024, so no capability is required either).
		useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --gid frp frp
	fi
	# 0750 root:frp — frps must read its config (which contains the token) but
	# nothing else on the host should.
	install -d -m 0750 -o root -g frp /etc/frp
}

ensure_caddy_user() {
	getent group caddy >/dev/null 2>&1 || groupadd --system caddy

	if id -u caddy >/dev/null 2>&1; then
		log "user caddy already exists"
		usermod -aG caddy caddy
	else
		log "creating system user caddy"
		# --create-home because Caddy's data directory (its CA private key, or
		# the ACME account key, plus every certificate it holds) lives under
		# /var/lib/caddy.
		useradd --system --create-home --home-dir /var/lib/caddy --shell /usr/sbin/nologin --gid caddy caddy
	fi
	install -d -m 0755 -o root -g root /etc/caddy
	# 0700 caddy:caddy. This directory holds the private key of the CA that this
	# deployment's phone is told to trust (internal mode), or the ACME account
	# key that can request certificates for our domain (acme mode). Either one
	# is a credential; neither may be readable by anyone else.
	install -d -m 0700 -o caddy -g caddy /var/lib/caddy
	install -d -m 0700 -o caddy -g caddy /var/lib/caddy/.local
	install -d -m 0700 -o caddy -g caddy /var/lib/caddy/.local/share
	install -d -m 0700 -o caddy -g caddy /var/lib/caddy/.config
}

# ---------------------------------------------------------------------------
# Config rendering
# ---------------------------------------------------------------------------
# render_template TEMPLATE KEY=VALUE...
#
# Writes the template to a temp file with every __KEY__ token replaced and
# prints the temp file's path. Values are substituted literally by bash, so a
# value containing '/', '&' or '.' needs no escaping — sed would mangle the
# first two, which is exactly the class of bug this avoids.
#
# The caller installs the result only after validating it, so a bad render can
# never reach a live path.
render_template() {
	local tmpl="$1"
	shift
	[[ -r $tmpl ]] || die "template not found: $tmpl"

	local body
	body="$(cat "$tmpl")"
	local pair key value
	for pair in "$@"; do
		key="${pair%%=*}"
		value="${pair#*=}"
		body="${body//__${key}__/$value}"
	done

	local leftover
	leftover="$(printf '%s\n' "$body" | grep -oE '__[A-Z][A-Z0-9_]*__' | sort -u | tr '\n' ' ' || true)"
	[[ -z $leftover ]] || die "$(basename "$tmpl") has unresolved placeholders: ${leftover}
       Every __TOKEN__ in a template is substituted, including ones mentioned in
       comments, so keep placeholder-looking text out of comments."

	local out
	out="$(mktemp "$WORK_DIR/render.XXXXXX")"
	printf '%s\n' "$body" >"$out"
	printf '%s' "$out"
}

# install_config RENDERED_TMP DEST MODE OWNER_GROUP
#
# Atomically replaces DEST, but only when the content differs, and keeps the
# previous generation as DEST.bak.
#
# CONFIG_CHANGED is sticky and only ever set to 1 (never reset), so it answers
# "did any of the files this run render actually change?" — which is exactly the
# question that decides whether the services need a reload.
install_config() {
	local tmp="$1" dest="$2" mode="$3" owner="$4"

	# Create the replacement next to the destination so the final rename is
	# atomic: a reader (or a restarting service) never sees a half-written file.
	local staged
	staged="$(mktemp "$(dirname "$dest")/.$(basename "$dest").XXXXXX")"
	cat "$tmp" >"$staged"
	chmod "$mode" "$staged"
	chown "$owner" "$staged"

	if [[ -f $dest ]] && cmp -s "$staged" "$dest"; then
		rm -f "$staged"
		log "$dest unchanged"
		return 0
	fi

	if [[ -f $dest ]]; then
		cp -p "$dest" "$dest.bak"
		log "$dest changed; previous version saved as $dest.bak"
	else
		log "writing $dest"
	fi
	mv -f "$staged" "$dest"
	CONFIG_CHANGED=1
}

resolve_frp_token() {
	local existing="" token
	if [[ -f $FRP_CONFIG ]]; then
		# Pull the value out of the existing config rather than keeping a second
		# copy of the token anywhere: one source of truth, nothing to drift.
		existing="$(awk -F'"' '/^[[:space:]]*auth\.token[[:space:]]*=/{print $2; exit}' "$FRP_CONFIG" || true)"
	fi
	if [[ -n $existing ]]; then
		if [[ ! $existing =~ ^[0-9a-fA-F]{64}$ ]]; then
			warn "the existing token in $FRP_CONFIG is not 64 hex characters; reusing it anyway"
		fi
		printf '%s' "$existing"
		return 0
	fi
	token="$(openssl rand -hex 32)"
	log "generated a new 256-bit frp token (it will be reused on every later run)" >&2
	printf '%s' "$token"
}

write_frps_config() {
	local token="$1" tmp
	tmp="$(render_template "$SCRIPT_DIR/frps.toml.tmpl" "TOKEN=$token")"

	# Validate with the real binary before it can reach /etc: frp parses TOML in
	# strict mode, so a typo means "frps will not start", and finding that out
	# now beats finding it out from a dead tunnel.
	"$BIN_DIR/frps" verify -c "$tmp" >/dev/null ||
		die "frps rejected the rendered config; run: $BIN_DIR/frps verify -c $tmp"
	install_config "$tmp" "$FRP_CONFIG" 0640 root:frp
}

# caddyfile_render — render Caddyfile.tmpl for the current TLS mode and print the
# path of the result. Split out of write_caddyfile so that both variants can be
# rendered and inspected from a test harness, without a live VPS or a caddy
# binary, and so the two fragments that differ per mode live in exactly one
# place.
caddyfile_render() {
	# The template fragments that differ per TLS mode.
	local tls_directive ca_route ca_handle
	if [[ $TLS_MODE == internal ]]; then
		# Caddy provisions its own CA on first use, signs the leaf with it, and
		# keeps the root at a path both copies below serve.
		tls_directive="	# Caddy's own CA, generated on this host on first use. The root is
	# published for the phone to install, on this port and on the plain-HTTP
	# one; see the two /ca.crt blocks in this file.
	tls internal"
		# This is the copy the documentation points a phone at first. It is the
		# same file as the plain-HTTP one, but the request arrives inside TLS,
		# which matters: a provider that filters unfiled hostnames does it by
		# reading the Host header of plain HTTP requests, and answers those with
		# an ICP block page. It cannot read this one. The cost is one certificate
		# warning for the phone to accept, which is the bootstrap paradox — the
		# certificate cannot be verified before the root that signs it is
		# installed.
		ca_route="	# The CA root, over HTTPS. Use this copy when the plain-HTTP one is
	# intercepted; see the note on the HTTP site below.
	handle /ca.crt {
		root * /var/lib/caddy/.local/share/caddy/pki/authorities/local
		rewrite * /root.crt
		file_server
		header Content-Type application/x-x509-ca-cert
		header Content-Disposition attachment
	}
"
		ca_handle="	# The same root, over plain HTTP: no certificate warning to accept, but
	# no TLS to hide the request from a provider's hostname filter either, so
	# this copy can come back as an ICP block page. Prefer the HTTPS one.
	handle /ca.crt {
		root * /var/lib/caddy/.local/share/caddy/pki/authorities/local
		rewrite * /root.crt
		file_server
		header Content-Type application/x-x509-ca-cert
		header Content-Disposition attachment
	}"
	else
		# No `tls` directive: the site address is a hostname, so Caddy obtains and
		# renews a publicly trusted certificate for it automatically.
		tls_directive="	# Automatic HTTPS. The site address is a hostname, so Caddy obtains and
	# renews a publicly trusted certificate for it over ACME."
		# Nothing to serve: there is no private CA for a phone to install, so
		# neither site gets a /ca.crt handler, and the ACME challenge request is
		# answered by Caddy before it reaches the sites' routes.
		ca_route=""
		ca_handle="	# Nothing is served in the clear here. There is no private CA to distribute,
	# and the ACME http-01 challenge is answered before the route below."
	fi

	render_template "$SCRIPT_DIR/Caddyfile.tmpl" \
		"DOMAIN=$DOMAIN" "HTTPS_PORT=$HTTPS_PORT" "HTTP_PORT=$HTTP_PORT" \
		"TLS_DIRECTIVE=$tls_directive" "CA_ROUTE=$ca_route" "CA_HANDLE=$ca_handle"
}

write_caddyfile() {
	local tmp
	tmp="$(caddyfile_render)"

	# `caddy validate` provisions the config without serving it. Run it as the
	# caddy user with its own XDG dirs so anything it creates on the way (the
	# certmagic data dir) is owned by the service account, not by root.
	#
	# That choice means the caddy user must be able to read the draft, which rules
	# out both WORK_DIR (0700) and anything inside it: traversal is denied at the
	# parent, so a readable file underneath is still unreachable.
	#
	# The draft is therefore staged inside the caddy user's own home, which it can
	# already traverse. Widening WORK_DIR instead would have exposed the frp token
	# that lives beside the rendered templates.
	#
	# This is the failure that only shows up on the real target: with no runuser
	# (macOS, minimal containers) the fallback branch runs as root and reads the
	# file fine, so local verification passes while a real Ubuntu host fails with
	# "permission denied".
	local stage="/var/lib/caddy/.validate"
	install -d -m 0700 -o caddy -g caddy "$stage"
	install -m 0644 "$tmp" "$stage/Caddyfile"

	local -a validate_cmd=()
	if command -v runuser >/dev/null 2>&1 && id -u caddy >/dev/null 2>&1; then
		validate_cmd=(runuser -u caddy -- env "XDG_DATA_HOME=/var/lib/caddy/.local/share" "XDG_CONFIG_HOME=/var/lib/caddy/.config")
	else
		validate_cmd=(env "XDG_DATA_HOME=$WORK_DIR/caddydata" "XDG_CONFIG_HOME=$WORK_DIR/caddycfg")
	fi
	"${validate_cmd[@]}" "$BIN_DIR/caddy" validate --config "$stage/Caddyfile" --adapter caddyfile >/dev/null 2>&1 ||
		"${validate_cmd[@]}" "$BIN_DIR/caddy" validate --config "$stage/Caddyfile" --adapter caddyfile ||
		die "caddy rejected the rendered Caddyfile"

	install_config "$tmp" "$CADDYFILE" 0644 root:root
}

# ---------------------------------------------------------------------------
# systemd
# ---------------------------------------------------------------------------
write_units() {
	local tmp
	tmp="$(mktemp "$WORK_DIR/unit.XXXXXX")"

	cat >"$tmp" <<'EOF'
[Unit]
Description=frp server (dsh-gateway tunnel endpoint)
Documentation=https://github.com/fatedier/frp
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# Unprivileged: frps binds 7000 and (via proxyBindAddr) 127.0.0.1:18787, both
# above 1024, so it needs no capability and no root.
User=frp
Group=frp
ExecStart=/usr/local/bin/frps -c /etc/frp/frps.toml
Restart=on-failure
RestartSec=2

# Hardening that costs frps nothing: it reads one config file and opens sockets.
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=full
ProtectHome=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictRealtime=true
LockPersonality=true
# A tunnel is many concurrent sockets, and the default 1024 is easy to exhaust.
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
	install_config "$tmp" "$FRPS_UNIT" 0644 root:root

	tmp="$(mktemp "$WORK_DIR/unit.XXXXXX")"
	cat >"$tmp" <<'EOF'
[Unit]
Description=Caddy (dsh-gateway TLS ingress)
Documentation=https://caddyserver.com/docs/
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
User=caddy
Group=caddy
ExecStart=/usr/local/bin/caddy run --environ --config /etc/caddy/Caddyfile
ExecReload=/usr/local/bin/caddy reload --config /etc/caddy/Caddyfile --force
TimeoutStopSec=10s
Restart=on-failure
RestartSec=2

# Binding a privileged port (80, 443) as a non-root user needs exactly this
# capability. The default ports (8443, 8080) do not need it, but the operator
# who moves to the standard ones should not have to discover this too.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

# Where Caddy keeps its CA private key (internal mode), its ACME account key
# (acme mode) and its certificates. Without these it would try to write into the
# service account's real home.
Environment=XDG_DATA_HOME=/var/lib/caddy/.local/share
Environment=XDG_CONFIG_HOME=/var/lib/caddy/.config

LimitNOFILE=1048576
PrivateTmp=true
ProtectSystem=full

[Install]
WantedBy=multi-user.target
EOF
	install_config "$tmp" "$CADDY_UNIT" 0644 root:root
}

# ---------------------------------------------------------------------------
# Firewall
# ---------------------------------------------------------------------------
# Every port ufw opens is a port an attacker can talk to. This deployment needs
# exactly four: SSH, the two Caddy ports, and the single frp control port the
# Mac dials — where "the two Caddy ports" are whatever --https-port and
# --http-port selected, not necessarily 443 and 80. The proxy port (18787) is
# deliberately absent — frps binds it on loopback, so a firewall rule for it
# would be meaningless except as evidence that something else is listening
# publicly.
detect_ssh_ports() {
	local -a ports=()
	if [[ -n ${SSH_CONNECTION:-} ]]; then
		# "client_ip client_port server_ip server_port" — the last field is the
		# port we are actually connected to right now. Allowing it first is what
		# keeps a custom SSH port from locking the operator out.
		local server_port="${SSH_CONNECTION##* }"
		[[ $server_port =~ ^[0-9]+$ ]] && ports+=("$server_port")
	fi

	local -a confs=(/etc/ssh/sshd_config)
	# nullglob so a missing sshd_config.d does not leave the literal pattern in
	# the array; restore the caller's setting rather than assuming it.
	local had_nullglob=0
	if shopt -q nullglob; then
		had_nullglob=1
	fi
	shopt -s nullglob
	confs+=(/etc/ssh/sshd_config.d/*.conf)
	if [[ $had_nullglob -eq 0 ]]; then
		shopt -u nullglob
	fi

	local conf port
	for conf in "${confs[@]}"; do
		[[ -r $conf ]] || continue
		while read -r port; do
			[[ -n $port ]] && ports+=("$port")
		done < <(awk 'tolower($1) == "port" && $2 ~ /^[0-9]+$/ { print $2 }' "$conf")
	done

	[[ ${#ports[@]} -gt 0 ]] || ports=(22)
	printf '%s\n' "${ports[@]}" | sort -un
}

configure_ufw() {
	log "configuring ufw"
	# Default deny first: everything below is an explicit exception.
	ufw --force default deny incoming >/dev/null
	ufw --force default allow outgoing >/dev/null

	# SSH before anything else, and before `ufw enable`, so enabling the
	# firewall cannot cut the connection running this script.
	local port
	while read -r port; do
		[[ -n $port ]] || continue
		ufw allow "$port/tcp" comment 'ssh' >/dev/null
	done < <(detect_ssh_ports)

	# Comment text follows the mode, so `ufw status` does not explain a CA that
	# this host does not serve.
	local http_comment
	if [[ $TLS_MODE == internal ]]; then
		http_comment='caddy: serves the CA certificate, redirects everything else'
	else
		http_comment='caddy: ACME http-01 challenges, redirects everything else'
	fi
	ufw allow "$HTTP_PORT/tcp" comment "$http_comment" >/dev/null
	ufw allow "$HTTPS_PORT/tcp" comment 'caddy: https' >/dev/null

	if [[ -n ${FRP_CONTROL_ALLOW_FROM:-} ]]; then
		if [[ ! $FRP_CONTROL_ALLOW_FROM =~ ^[0-9a-fA-F:.]+/[0-9]{1,3}$ ]]; then
			die "FRP_CONTROL_ALLOW_FROM must be a CIDR like 198.51.100.7/32, got: $FRP_CONTROL_ALLOW_FROM"
		fi
		log "restricting the frp control port to $FRP_CONTROL_ALLOW_FROM"
		ufw allow from "$FRP_CONTROL_ALLOW_FROM" to any port "$FRP_CONTROL_PORT" proto tcp \
			comment 'frp control (narrowed)' >/dev/null
	else
		# Required: the Mac dials in from a residential address that changes, so
		# there is no stable source to allow. Protection here is the token plus
		# forced TLS, not the firewall.
		ufw allow "$FRP_CONTROL_PORT/tcp" comment 'frp control (token+tls protected)' >/dev/null
	fi

	# Defensive: if a previous experiment left the proxy port open, close it.
	ufw --force delete allow "$FRP_PROXY_PORT/tcp" >/dev/null 2>&1 || true

	ufw --force enable >/dev/null
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
print_summary() {
	local token="$1" ip firewall_state
	ip="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
	# awk rather than `head -n1`: head closes the pipe early, and with pipefail
	# that would abort the script on its very last line.
	firewall_state="$(ufw status | awk 'NR == 1' || true)"

	# The trust step and the port table genuinely differ per mode, so build the
	# mode-specific lines here rather than printing instructions that would be
	# wrong half the time. A phone that is told to install a CA that does not
	# exist will fail in a confusing way.
	local tls_label http_port_label phone_trust check_ca ca_by_ip

	# sslip.io encodes the address in the name, so the IP form of the CA URL can
	# be derived when the domain is one. That form matters: a provider's
	# plain-HTTP filter keys on a hostname in the Host header, so the address
	# slips past it — today. It is a fallback, not a promise.
	ca_by_ip=""
	if [[ $DOMAIN =~ ^([0-9]{1,3})-([0-9]{1,3})-([0-9]{1,3})-([0-9]{1,3})\.sslip\.io$ ]]; then
		ca_by_ip="

     This host's plain-HTTP copy is also reachable by address, which the filter
     does not match (it may start to, at any time):

         http://${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.${BASH_REMATCH[3]}.${BASH_REMATCH[4]}:$HTTP_PORT/ca.crt"
	fi

	if [[ $TLS_MODE == internal ]]; then
		tls_label="internal CA (the phone installs it once)"
		http_port_label="$HTTP_PORT/tcp   (serves the CA certificate, nothing else)"
		phone_trust="  3. Teach the phone to trust this server. Do this BEFORE opening the app:
     the certificate is signed by a CA this server generated for itself,
     because every ACME challenge that sslip.io could satisfy needs port 80 or
     443 — and this provider blocks both.

     The copy to use is the one over HTTPS. It shows a certificate warning until
     the root is installed, which is expected — a phone cannot verify a
     certificate before it has the root that signs it:

         https://$DOMAIN:$HTTPS_PORT/ca.crt

       iOS:     accept the warning (Show Details → visit this website), then
                Settings → General → VPN & Device Management → install the
                profile, then Settings → General → About → Certificate Trust
                Settings → turn it ON. Both steps are required; iOS ignores an
                installed-but-untrusted root.
       Android: accept the warning (Advanced → Proceed), then Settings →
                Security → Encryption & credentials → Install a certificate →
                CA certificate.

     The same root is served in the clear too, which needs no warning but can
     come back as a provider's ICP block page (403) instead, because plain HTTP
     exposes the hostname:

         http://$DOMAIN:$HTTP_PORT/ca.crt$ca_by_ip

     If neither works, copy the root out of band and send it to the phone:

         ssh root@$DOMAIN 'cat /var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt' > ca.crt

     A root certificate is public information. Remove it from the phone when you
     stop using this deployment."
		check_ca="       curl -skS -o /dev/null -w '%{http_code}\n' https://$DOMAIN:$HTTPS_PORT/ca.crt   # expect 200
       curl -sS  -o /dev/null -w '%{http_code}\n' http://$DOMAIN:$HTTP_PORT/ca.crt   # 200, or 403 if your provider filters hostnames
"
	else
		tls_label="ACME (publicly trusted)"
		http_port_label="$HTTP_PORT/tcp   (ACME http-01 challenges, then redirects)"
		phone_trust="  3. The certificate is publicly trusted, so the phone needs nothing installed.
     Caddy can only obtain it while the challenge ports stay reachable from the
     internet: $HTTP_PORT for http-01 and $HTTPS_PORT for tls-alpn-01."
		check_ca=""
	fi

	cat >&2 <<EOF

=============================================================================
VPS setup complete.
=============================================================================

  Public URL          https://$DOMAIN:$HTTPS_PORT
  VPS address         ${ip:-<unknown>}
  HTTPS port          $HTTPS_PORT/tcp
  HTTP port           $http_port_label
  Certificate         $tls_label
  frp control port    $FRP_CONTROL_PORT/tcp  (frpc dials this from the Mac)
  frp proxy port      $FRP_PROXY_PORT        (LOOPBACK ONLY — never expose it)

  FRP TOKEN (paste into the Mac installer; it is not regenerated):
      $token

  Files written:
      $FRP_CONFIG      (0640 root:frp)
      $CADDYFILE
      $FRPS_UNIT
      $CADDY_UNIT

  Firewall: $firewall_state

Next steps, on the Mac:

  1. Get this repository onto the Mac (git clone ... ; cd dsh-gateway).

  2. Install the Mac half, passing the token above:

       FRP_TOKEN=$token bash deploy/mac/install.sh

     Add --server <ip> if the VPS address is not the default, and
     --public-url https://$DOMAIN:$HTTPS_PORT if the domain differs.

$phone_trust

  4. Pair the phone:

       tail -f ~/Library/Logs/dsh-gateway/gateway.log   # watch it come up
       dsh-gateway pair                                 # prints a one-time code
       # then open https://$DOMAIN:$HTTPS_PORT on the phone and enter the code

  5. Sanity checks from anywhere:

${check_ca}       curl -skS -o /dev/null -w '%{http_code}\n' https://$DOMAIN:$HTTPS_PORT/healthz  # expect 200
       journalctl -u frps -u caddy -f

     The -k is only because this shell has not installed the CA; a phone that has
     will verify normally. Drop it under --tls acme, or check the chain
     explicitly with: curl --cacert ca.crt ...
EOF
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
cleanup() {
	[[ -n $WORK_DIR && -d $WORK_DIR ]] && rm -rf "$WORK_DIR"
}

main() {
	parse_args "$@"
	require_root "$@"
	require_supported_os

	local arch
	arch="$(detect_arch)"
	log "Ubuntu on $arch detected"

	WORK_DIR="$(mktemp -d)"
	trap cleanup EXIT

	ensure_base_packages
	install_frp_binary "$arch"
	install_caddy_binary "$arch"
	ensure_frp_user
	ensure_caddy_user

	local token
	token="$(resolve_frp_token)"
	write_frps_config "$token"
	write_caddyfile
	write_units

	systemctl daemon-reload
	systemctl enable --now frps caddy >/dev/null 2>&1 || true

	# Only bounce what needs bouncing. reload-or-restart: Caddy reloads the
	# Caddyfile in place (no dropped connections, no re-issued certificates)
	# because its unit has ExecReload; frps, which has none, is restarted. A
	# re-run that changed nothing leaves both services — and the tunnel —
	# untouched.
	if [[ $CONFIG_CHANGED -eq 1 ]]; then
		systemctl reload-or-restart frps caddy
	else
		log "no configuration changed; services left as they were"
	fi

	configure_ufw

	if ! systemctl is-active --quiet frps; then
		die "frps failed to start; inspect: journalctl -u frps -n 50 --no-pager"
	fi
	if ! systemctl is-active --quiet caddy; then
		die "caddy failed to start; inspect: journalctl -u caddy -n 50 --no-pager"
	fi
	log "frps and caddy are active"

	print_summary "$token"
}

main "$@"
