#!/usr/bin/env bash
#
# install.sh — set up the Mac half of the dsh-gateway deployment.
#
# What this builds:
#
#   launchd: dev.dsh-gateway.frpc      frpc ──tunnel──> frps on the VPS
#   launchd: dev.dsh-gateway.gateway   dsh-gateway on 127.0.0.1:8787
#                                        └── spawns `dsh --profile acp`
#
# Both jobs run as you (never as root): they need your DSH home, your
# workspaces, and your node/nvm-installed `dsh`.
#
# The script is idempotent. It rebuilds and reinstalls the gateway binary, reuses
# the tunnel token it finds in ~/.dsh-gateway/frpc.toml when none is supplied,
# and NEVER overwrites ~/.dsh-gateway/config.yaml once it exists — that file is
# yours to edit.
#
# Usage:
#   FRP_TOKEN=<token from the VPS> bash deploy/mac/install.sh \
#     --server <vps address> --public-url https://<domain>:8443 \
#     --workspaces "$HOME/code"
#   bash deploy/mac/install.sh --help
#
# Environment:
#   FRP_TOKEN     Tunnel token printed by deploy/vps/install.sh. Required on the
#                 first run; later runs reuse the token already on disk.
#   WORKSPACES    Whitespace- or comma-separated workspace roots the phone may
#                 open. Required, and never the whole of $HOME.
#   INSTALL_DIR   Where to put the binaries. Default: ~/.local/bin (no sudo).

set -euo pipefail

# ---------------------------------------------------------------------------
# Pinned versions. Bump by checking
# https://github.com/fatedier/frp/releases and updating both lines together:
# the checksum asset is downloaded and verified, so a mismatched pair fails
# loudly instead of installing something unverified.
# ---------------------------------------------------------------------------
FRP_VERSION="0.71.0"
FRP_CHECKSUMS_ASSET="frp_sha256_checksums.txt"
readonly FRP_DOWNLOAD_BASE="https://github.com/fatedier/frp/releases/download"

# The VPS, and the public URL the phone uses. Both are required, and neither has
# a default: a default here is somebody else's server, and pointing a stranger's
# frpc at it would hand that server the tunnel token.
FRP_SERVER=""
FRP_CONTROL_PORT=7000

# Must match what Caddy serves, because the gateway puts it in the pairing link
# and uses its scheme to decide whether the session cookie may be marked Secure.
PUBLIC_URL=""

FRP_TOKEN="${FRP_TOKEN:-}"
WORKSPACES="${WORKSPACES:-}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

# 1 leaves the agent host — and whatever turn it is running — alone. The deploy
# that needs this is the one an agent is running: the turn doing the deploying is
# a turn the host is running, so restarting it ends the conversation that asked
# for the deploy. See --skip-host-restart.
SKIP_HOST_RESTART="${SKIP_HOST_RESTART:-0}"

GATEWAY_LABEL="dev.dsh-gateway.gateway"
# The agent host is a *peer* job, not a child of the gateway's, and that is
# load-bearing rather than tidiness: launchd kills every process in a job's
# process group when the job dies, so a host spawned by the gateway would be
# killed by exactly the event it exists to survive. Two labels, two jobs, two
# lifetimes.
AGENT_HOST_LABEL="dev.dsh-gateway.agent-host"

# How long launchd waits between SIGTERM and SIGKILL for either job. It has to
# exceed the interval between two heartbeats, and there are two of them: a
# gateway draining before a redeploy, and a host draining before its own
# restart. Long enough for a turn that is nearly finished, short enough that a
# wedged process does not hold a deploy forever.
EXIT_TIMEOUT_SECONDS=180
FRPC_LABEL="dev.dsh-gateway.frpc"

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
TEMPLATE_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly TEMPLATE_DIR
readonly CONFIG_DIR="$HOME/.dsh-gateway"
readonly CONFIG_FILE="$CONFIG_DIR/config.yaml"
readonly FRPC_CONFIG="$CONFIG_DIR/frpc.toml"
readonly LOG_DIR="$HOME/Library/Logs/dsh-gateway"
readonly AGENT_DIR="$HOME/Library/LaunchAgents"

WORK_DIR=""

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
install.sh — install the Mac side of dsh-gateway (gateway + frpc under launchd).

Usage:
  FRP_TOKEN=<token> bash install.sh --server HOST --public-url URL \
                                   --workspaces "DIR [DIR…]" [options]

Required:
  --server HOST        VPS address frpc dials. No default: it is your server,
                       and the tunnel token goes to whatever is named here.
  --public-url URL     Public https URL of the deployment, used for the pairing
                       link and Secure cookies. Must match what Caddy serves.
  --workspaces "A B"   Workspace roots the phone may open (space- or
                       comma-separated). Required, and never the whole of $HOME:
                       this is the allowlist that decides what a phone — or
                       anyone holding it — can reach on this machine.

Options:
  --token TOKEN        Tunnel token from the VPS installer. Same as FRP_TOKEN.
  --install-dir DIR    Where to install the binaries. Default: ~/.local/bin.
  --skip-host-restart  Install everything and restart the gateway and the
                       tunnel, but leave the agent host running what it is
                       running. Use it when a turn is in flight that must not be
                       ended — including the turn running this installer, which
                       is itself a turn the host is running. The new host binary
                       and plist take effect at its next restart; the command to
                       do that is printed when the install finishes.
  -h, --help           Show this help.

What it touches:
  <repo>/web/dist                       compiled from web/src with `npm run build`
  <repo>/cmd/dsh-gateway                built with `go build`
  INSTALL_DIR/{dsh-gateway,dsh-agent-host}
                                        the two binaries
  INSTALL_DIR/frpc                      frp client, pinned and checksum-verified
  ~/.dsh-gateway/config.yaml            written ONLY if missing
  ~/.dsh-gateway/frpc.toml              rewritten from the template (0600)
  ~/Library/LaunchAgents/dev.dsh-gateway.{gateway,agent-host,frpc}.plist
  ~/Library/Logs/dsh-gateway/{gateway,agent-host,frpc}.log

Pairing: after this finishes, run `dsh-gateway pair` and enter the code it
prints on the phone.
EOF
}

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------
parse_args() {
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--token)
			[[ $# -ge 2 ]] || die "--token needs a value"
			FRP_TOKEN="$2"
			shift 2
			;;
		--server)
			[[ $# -ge 2 ]] || die "--server needs a value"
			FRP_SERVER="$2"
			shift 2
			;;
		--public-url)
			[[ $# -ge 2 ]] || die "--public-url needs a value"
			PUBLIC_URL="$2"
			shift 2
			;;
		--workspaces)
			[[ $# -ge 2 ]] || die "--workspaces needs a value"
			WORKSPACES="$2"
			shift 2
			;;
		--install-dir)
			[[ $# -ge 2 ]] || die "--install-dir needs a value"
			INSTALL_DIR="$2"
			shift 2
			;;
		--skip-host-restart)
			SKIP_HOST_RESTART=1
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*) die "unknown argument: $1 (try --help)" ;;
		esac
	done
}

# ---------------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------------
require_macos() {
	[[ "$(uname -s)" == "Darwin" ]] ||
		die "this script installs the Mac side (launchd); run deploy/vps/install.sh on the VPS instead"
}

# require_inputs enforces the three values this script will not invent.
#
# Each one is a decision about somebody's infrastructure — which server holds the
# tunnel, what name the phone will trust, and what on this machine the phone may
# reach — and a default would silently make that decision wrong for anyone who is
# not the person who wrote this file.
require_inputs() {
	[[ -n $FRP_SERVER ]] || die "--server is required: the address of the VPS
       running frps (the one deploy/vps/install.sh just configured). There is no
       default, because a default would be somebody else's server and it would
       receive your tunnel token."
	[[ -n $PUBLIC_URL ]] || die "--public-url is required: the https URL the phone
       will open, exactly as Caddy serves it (e.g. https://203-0-113-9.sslip.io:8443)."
	[[ $PUBLIC_URL == https://* ]] || die "--public-url must be https, got $PUBLIC_URL"
	[[ -n $WORKSPACES ]] || die "--workspaces is required: the directories the
       phone may open, e.g. --workspaces \"\$HOME/code\". It is not defaulted on
       purpose — the allowlist is the only thing standing between a phone and
       every file you own, so it should name what you meant and nothing else."
	# Resolve them now, discarding the result, so that a workspace this script
	# will refuse costs a second instead of a build. resolve_workspaces dies on
	# anything unusable, which is exactly the check wanted here.
	resolve_workspaces >/dev/null
}

detect_arch() {
	local machine
	machine="$(uname -m)"
	if [[ $machine == "x86_64" ]] &&
		[[ "$(sysctl -in sysctl.proc_translated 2>/dev/null || printf 0)" == "1" ]] &&
		[[ "$(sysctl -in hw.optional.arm64 2>/dev/null || printf 0)" == "1" ]]; then
		# An x86_64 shell under Rosetta on Apple Silicon: download the native
		# build rather than the translated one, which would run slower forever.
		machine="arm64"
	fi
	case "$machine" in
	arm64) printf 'arm64' ;;
	x86_64 | amd64) printf 'amd64' ;;
	*) die "unsupported Mac architecture: $machine" ;;
	esac
}

# probe_help SECONDS CMD [ARGS...]
#
# Runs CMD with a hard deadline, prints everything it wrote (stdout *and* stderr,
# because flag-parsing CLIs such as this one print their usage to stderr), and
# returns 124 if the command had to be killed. The deadline matters because the
# whole point of a probe is to ask a CLI a question, and a CLI that decides to
# start serving instead of printing help would otherwise hang the install.
probe_help() {
	local secs="$1"
	shift
	local captured="" rc=0
	captured="$(
		"$@" 2>&1 &
		local pid=$!
		local status=0
		(
			sleep "$secs"
			kill -TERM "$pid" 2>/dev/null || true
		) 2>/dev/null &
		local watchdog=$!
		wait "$pid" || status=$?
		kill -TERM "$watchdog" 2>/dev/null || true
		wait "$watchdog" 2>/dev/null || true
		exit "$status"
	)" 2>/dev/null || rc=$?

	if [[ $rc -eq 143 ]]; then
		printf 'probe_help: killed after %ss\n' "$secs" >&2
		return 124
	fi
	printf '%s' "$captured"
	return "$rc"
}

check_go() {
	command -v go >/dev/null 2>&1 || die "go not found on PATH.
       Install Go 1.25+ (https://go.dev/dl/ or 'brew install go') and re-run."
	local raw version major minor
	raw="$(go env GOVERSION 2>/dev/null || go version | awk '{print $3}')"
	version="${raw#go}"
	major="${version%%.*}"
	minor="${version#*.}"
	# Strip non-digits so a pre-release like 1.25rc1 does not break arithmetic.
	major="${major//[!0-9]/}"
	minor="${minor//[!0-9]/}"
	if [[ -z $major || -z $minor ]] || ((major < 1 || (major == 1 && minor < 25))); then
		die "Go 1.25 or newer is required by go.mod; found ${raw:-unknown}.
       Upgrade Go (https://go.dev/dl/) and re-run."
	fi
	log "go $version ok"
}

# check_npm proves the web app can be compiled. The Go binary embeds the bundle
# and the bundle is not committed, so npm is a build prerequisite in the same
# sense Go is — not an optional extra. It is never far away: `dsh` is itself an
# npm package, so any machine that passes check_dsh has node and npm on PATH.
check_npm() {
	command -v node >/dev/null 2>&1 ||
		die "node not found on PATH.
       The gateway embeds its web app, so node is needed to build it. Install
       Node 22+ (https://nodejs.org/, or 'brew install node') and re-run."
	command -v npm >/dev/null 2>&1 ||
		die "npm not found on PATH.
       The gateway embeds its web app, so npm is needed to build it. Install
       Node 22+ (https://nodejs.org/, or 'brew install node') and re-run."
	log "node $(node --version) / npm $(npm --version) ok"
}

# check_dsh proves two things at once: that `dsh` is reachable, and that the ACP
# profile actually resolves — which is the profile the gateway launches. If the
# profile is missing or renamed, this fails here instead of at 3am from a phone.
check_dsh() {
	command -v dsh >/dev/null 2>&1 || die "dsh not found on PATH.
       Install it (npm i -g @deepseek-ai/dsh) or fix PATH so this shell can see
       the node/nvm bin directory that holds it, then re-run."
	log "checking that the acp profile resolves (dsh --profile acp --help)"
	if ! probe_help 30 dsh --profile acp --help >/dev/null; then
		die "'dsh --profile acp --help' failed or timed out.
       That command is how this script proves the ACP profile — the one the
       gateway launches as a child process — resolves. Run it by hand to see the
       real error, then fix the DSH install before deploying."
	fi
	log "acp profile ok"
}

# detect_search_path builds the PATH the launchd jobs will use.
#
# launchd starts agents with a minimal PATH (/usr/bin:/bin:/usr/sbin:/sbin) and
# no shell profile, so a node/nvm-installed `dsh` is invisible unless the
# directories are listed explicitly in the plist. Rather than hardcoding an nvm
# version (which changes on every node upgrade) this asks the shell that is
# running the installer where node, npm and dsh actually live. A goenv-managed
# `go` is invisible for the same reason, so its directories are detected too.
detect_search_path() {
	local -a dirs=()
	local cmd path dir

	for cmd in node npm dsh; do
		path="$(command -v "$cmd" 2>/dev/null || true)"
		if [[ -n $path ]]; then
			dir="$(cd -- "$(dirname -- "$path")" && pwd)"
			dirs+=("$dir")
		fi
	done
	# nvm exports NVM_BIN in login shells; it is the strongest hint available.
	if [[ -n ${NVM_BIN:-} ]]; then
		dirs+=("$NVM_BIN")
	fi
	# goenv keeps every toolchain behind one shim directory, so the shims are
	# what belongs on this PATH — not the bin of whichever version happens to be
	# selected today, which would freeze the agent at one Go and ignore a
	# project's .go-version. GOENV_ROOT is exported by `goenv init` in a login
	# shell; ~/.goenv is the default the shim itself falls back to.
	local goenv_root="${GOENV_ROOT:-$HOME/.goenv}"
	if [[ -d $goenv_root ]]; then
		path="$(command -v goenv 2>/dev/null || true)"
		if [[ -n $path ]]; then
			dirs+=("$(cd -- "$(dirname -- "$path")" && pwd)")
		fi
		dirs+=("$goenv_root/shims" "$goenv_root/bin")
	fi
	# Homebrew (Apple Silicon and Intel), the install dir, and the system paths
	# launchd would have had anyway.
	dirs+=("$INSTALL_DIR" "/opt/homebrew/bin" "/usr/local/bin" "/usr/bin" "/bin" "/usr/sbin" "/sbin")

	local seen=""
	for dir in "${dirs[@]}"; do
		[[ -d $dir ]] || continue
		case ":$seen:" in
		*":$dir:"*) continue ;;
		esac
		seen="${seen}:${dir}"
		printf '%s\n' "$dir"
	done | paste -sd: -
}

# ---------------------------------------------------------------------------
# Downloads
# ---------------------------------------------------------------------------
download() {
	local url="$1" dest="$2"
	# -f: fail on HTTP errors rather than writing an error page.
	# -L: release downloads redirect; without it the file is written empty.
	# --retry: laptops suspend and resume mid-download.
	curl -fsSL --retry 3 --retry-delay 2 --connect-timeout 15 -o "$dest" "$url" ||
		die "download failed: $url"
}

verify_sha256() {
	local sumfile="$1" asset="$2" file="$3" want got
	want="$(awk -v a="$asset" '$2 == a { print $1; exit }' "$sumfile")"
	[[ -n $want ]] || die "no checksum entry for $asset in $(basename "$sumfile")"
	got="$(shasum -a 256 "$file" | awk '{print $1}')"
	[[ $got == "$want" ]] || die "checksum mismatch for $asset
       expected $want
       got      $got"
	log "checksum ok: $asset"
}

install_frpc() {
	local arch="$1"
	if [[ -x $INSTALL_DIR/frpc ]]; then
		local current
		current="$("$INSTALL_DIR/frpc" --version 2>/dev/null || true)"
		if [[ $current == "$FRP_VERSION" ]]; then
			log "frpc $FRP_VERSION already installed"
			return 0
		fi
		log "frpc ${current:-unknown} installed; replacing with $FRP_VERSION"
	fi

	local asset="frp_${FRP_VERSION}_darwin_${arch}.tar.gz"
	log "downloading $asset"
	download "${FRP_DOWNLOAD_BASE}/v${FRP_VERSION}/${asset}" "$WORK_DIR/$asset"
	download "${FRP_DOWNLOAD_BASE}/v${FRP_VERSION}/${FRP_CHECKSUMS_ASSET}" "$WORK_DIR/$FRP_CHECKSUMS_ASSET"
	verify_sha256 "$WORK_DIR/$FRP_CHECKSUMS_ASSET" "$asset" "$WORK_DIR/$asset"

	tar -xzf "$WORK_DIR/$asset" -C "$WORK_DIR"
	# Only frpc is installed: the server binary has no business on the laptop.
	install -m 0755 "$WORK_DIR/frp_${FRP_VERSION}_darwin_${arch}/frpc" "$INSTALL_DIR/frpc"
	log "installed $INSTALL_DIR/frpc $FRP_VERSION"
}

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
# build_web produces web/dist, which the Go build embeds and which is not
# committed. It runs before build_gateway for the same reason the Makefile builds
# the frontend first: `go build` fails outright without it, and a checkout that
# never built it would otherwise fail with an embed error that says nothing about
# the fix.
#
# The cost is one `tsc` pass over web/src plus a single devDependency, so there is
# no bundler and nothing to configure. node_modules is reused when present, which
# is what makes a re-run cheap.
build_web() {
	local web="$REPO_ROOT/web"
	[[ -f $web/package.json ]] || die "no web/package.json under $REPO_ROOT.
       This checkout is incomplete. The installer compiles the embedded web app
       from web/src, so the whole repository is needed, not just deploy/."

	if [[ ! -f $web/package-lock.json ]]; then
		die "no web/package-lock.json under $REPO_ROOT.
       The web app's build dependency is installed with \`npm ci\`, which needs
       the committed lockfile rather than resolving versions fresh. This checkout
       is incomplete."
	fi

	if [[ ! -d $web/node_modules ]]; then
		log "installing the web app's build dependency (typescript)"
		# `npm ci` rather than `npm install`: the lockfile is committed, so the
		# build uses exactly the version this project was tested against instead
		# of whatever the registry serves today.
		if ! (cd "$web" && npm ci --no-audit --no-fund --silent); then
			die "npm ci failed in $web.
       The frontend build needs the TypeScript compiler from the npm registry.
       Check network access and any proxy settings, then re-run."
		fi
	fi

	log "building the web app (web/dist)"
	if ! (cd "$web" && npm run build --silent); then
		die "npm run build failed in $web.
       That command runs tsc over web/src and copies the static shell into
       web/dist, which the Go binary embeds. Run it by hand to see the errors."
	fi
	[[ -f $web/dist/index.html ]] || die "npm run build produced no $web/dist/index.html.
       The bundle is what the binary serves at /m/, so refusing to continue is the
       only honest outcome."
}

# build_binary builds one command into the staging directory and installs it.
#
# It exists because there are now two binaries, and the properties that made the
# original safe apply to each: built into a temp directory so a failed build
# cannot leave a half-written file where launchd will exec it, and installed
# only after the build succeeded.
build_binary() {
	local name="$1" pkg="$2"
	log "building $name: go build ./$pkg${BUILD_STAMP:+ (version $BUILD_STAMP)}"
	# ${ldflags[@]+...} rather than ${ldflags[@]}: this machine's /bin/bash is
	# 3.2, where expanding an empty array under `set -u` is an unbound-variable
	# error rather than an empty expansion.
	if ! (cd "$REPO_ROOT" && go build -trimpath ${BUILD_LDFLAGS[@]+"${BUILD_LDFLAGS[@]}"} -o "$WORK_DIR/$name" "./$pkg"); then
		die "go build ./$pkg failed. The usual causes:
       * this checkout has no main package at $pkg (a partial checkout still
         needs the source, not just deploy/);
       * dependencies are missing: (cd $REPO_ROOT && go mod download);
       * the embedded web app is missing: (cd $REPO_ROOT && make web);
       * Go is older than go.mod requires (1.25+)."
	fi
	install -m 0755 "$WORK_DIR/$name" "$INSTALL_DIR/$name"
	log "installed $INSTALL_DIR/$name ($("$INSTALL_DIR/$name" version 2>/dev/null || printf 'version unknown'))"
}

build_gateway() {
	# Stamp the build so `dsh-gateway version` means something when something is
	# wrong six months from now. git describe needs a repository with commits and
	# tags; a source tarball has neither, hence the fallback instead of a failure.
	BUILD_STAMP="$(git -C "$REPO_ROOT" describe --tags --always --dirty 2>/dev/null || true)"
	BUILD_LDFLAGS=()
	if [[ -n $BUILD_STAMP && $BUILD_STAMP != *" "* ]]; then
		BUILD_LDFLAGS=(-ldflags "-X main.version=$BUILD_STAMP")
	fi

	build_binary dsh-gateway cmd/dsh-gateway
	build_binary dsh-agent-host cmd/dsh-agent-host
}

# ---------------------------------------------------------------------------
# Configuration files
# ---------------------------------------------------------------------------
# install_file_if_changed TEMP DEST MODE  → echoes 1 if DEST changed, else 0
install_file_if_changed() {
	local tmp="$1" dest="$2" mode="$3"
	local staged
	staged="$(mktemp "$(dirname "$dest")/.$(basename "$dest").XXXXXX")"
	cat "$tmp" >"$staged"
	chmod "$mode" "$staged"

	if [[ -f $dest ]] && cmp -s "$staged" "$dest"; then
		rm -f "$staged"
		printf '0'
		return 0
	fi
	if [[ -f $dest ]]; then
		cp -p "$dest" "$dest.bak"
	fi
	mv -f "$staged" "$dest"
	printf '1'
}

# resolve_workspaces prints one absolute workspace root per line.
resolve_workspaces() {
	local raw="$WORKSPACES" item
	# Accept spaces and commas so both `WORKSPACES="a b"` and `--workspaces a,b`
	# do what an operator expects.
	[[ -n $raw ]] || die "no workspaces given; see --workspaces"

	local -a items=()
	local IFS=', '
	read -r -a items <<<"$raw"
	for item in "${items[@]}"; do
		[[ -n $item ]] || continue
		[[ -d $item ]] || die "workspace does not exist: $item"
		case "$item" in
		*'"'* | *$'\n'*) die "workspace paths may not contain double quotes or newlines: $item" ;;
		esac
		item="${item/#\~/$HOME}"
		# Home itself (or the root) as a workspace root hands the agent — and
		# anyone holding a paired phone — `~/.ssh`, `~/.dsh` and every browser
		# profile. Refused rather than warned about, because the failure is
		# silent and the blast radius is the whole account.
		case "$(cd -- "$item" && pwd)" in
		"$HOME" | /)
			die "refusing to use $item as a workspace root.
       That is your whole home directory, so the phone could open ~/.ssh, ~/.dsh
       and anything else you own. Name the directories you actually work in
       instead, e.g. --workspaces \"\$HOME/code \$HOME/notes\"."
			;;
		esac
		printf '%s\n' "$item"
	done
}

write_config_yaml() {
	if [[ -f $CONFIG_FILE ]]; then
		log "$CONFIG_FILE exists; leaving it untouched"
		return 0
	fi

	local ws_yaml="" ws
	while read -r ws; do
		# Quoted so a path containing spaces or '#' survives YAML.
		ws_yaml+="  - \"${ws}\""$'\n'
	done < <(resolve_workspaces)
	[[ -n $ws_yaml ]] || die "could not determine any workspace roots"

	local body tmp
	body="$(cat "$TEMPLATE_DIR/config.yaml.tmpl")"
	body="${body//__PUBLIC_URL__/$PUBLIC_URL}"
	# The trailing newline in ws_yaml is intentional: the template's
	# __WORKSPACES__ token sits on a line of its own.
	body="${body//__WORKSPACES__/$ws_yaml}"

	local leftover
	leftover="$(printf '%s\n' "$body" | grep -oE '__[A-Z][A-Z0-9_]*__' | sort -u | tr '\n' ' ' || true)"
	[[ -z $leftover ]] || die "config.yaml.tmpl has unresolved placeholders: ${leftover}"

	mkdir -p "$CONFIG_DIR"
	chmod 700 "$CONFIG_DIR"
	tmp="$(mktemp "$WORK_DIR/config.XXXXXX")"
	printf '%s\n' "$body" >"$tmp"
	install -m 0600 "$tmp" "$CONFIG_FILE"
	log "wrote $CONFIG_FILE (edit it freely; it is never overwritten again)"
}

resolve_token() {
	if [[ -n $FRP_TOKEN ]]; then
		printf '%s' "$FRP_TOKEN"
		return 0
	fi
	if [[ -f $FRPC_CONFIG ]]; then
		local existing
		existing="$(awk -F'"' '/^[[:space:]]*auth\.token[[:space:]]*=/{print $2; exit}' "$FRPC_CONFIG" || true)"
		if [[ -n $existing ]]; then
			log "reusing the tunnel token already in $FRPC_CONFIG"
			printf '%s' "$existing"
			return 0
		fi
	fi
	die "no tunnel token.
       Copy the FRP TOKEN printed by deploy/vps/install.sh on the VPS and run:
         FRP_TOKEN=<token> bash $0
       (or pass --token <token>)."
}

write_frpc_config() {
	local token="$1" body tmp changed
	body="$(cat "$TEMPLATE_DIR/frpc.toml.tmpl")"
	body="${body//__SERVER_ADDR__/$FRP_SERVER}"
	body="${body//__TOKEN__/$token}"

	local leftover
	leftover="$(printf '%s\n' "$body" | grep -oE '__[A-Z][A-Z0-9_]*__' | sort -u | tr '\n' ' ' || true)"
	[[ -z $leftover ]] || die "frpc.toml.tmpl has unresolved placeholders: ${leftover}"

	mkdir -p "$CONFIG_DIR"
	chmod 700 "$CONFIG_DIR"
	tmp="$(mktemp "$WORK_DIR/frpc.XXXXXX")"
	printf '%s\n' "$body" >"$tmp"
	# 0600: the file carries the tunnel token, which is the whole credential for
	# the control connection.
	changed="$(install_file_if_changed "$tmp" "$FRPC_CONFIG" 0600)"
	if [[ $changed == "1" ]]; then
		log "wrote $FRPC_CONFIG"
	else
		log "$FRPC_CONFIG unchanged"
	fi

	# Verify with the real frpc before launchd ever runs it: frp parses TOML in
	# strict mode, so standard spacing is not enough — an unknown key is a hard
	# startup error.
	"$INSTALL_DIR/frpc" verify -c "$FRPC_CONFIG" >/dev/null ||
		die "frpc rejected $FRPC_CONFIG; run: $INSTALL_DIR/frpc verify -c $FRPC_CONFIG"
}

# ---------------------------------------------------------------------------
# launchd
# ---------------------------------------------------------------------------
xml_escape() {
	local s="$1"
	s="${s//&/&amp;}"
	s="${s//</&lt;}"
	s="${s//>/&gt;}"
	printf '%s' "$s"
}

write_plists() {
	local search_path dsh_bin gw_bin gw_args_xml
	search_path="$(detect_search_path)"
	dsh_bin="$(command -v dsh || true)"
	[[ -n $dsh_bin ]] || die "dsh disappeared from PATH between checks; fix PATH and re-run"
	gw_bin="$INSTALL_DIR/dsh-gateway"

	# Ask the gateway how it wants to be started rather than assuming, because
	# guessing wrong means a job that crash-loops under launchd. Two shapes are
	# supported: a subcommand CLI (`dsh-gateway run`, which is what this repo
	# builds) and one that takes flags directly.
	#
	# `run -h` is the probe of choice: with Go's flag package -h prints usage and
	# exits 0 without starting anything.
	local run_help="" help_rc=0
	run_help="$(probe_help 20 "$gw_bin" run -h)" || help_rc=$?
	if [[ $help_rc -eq 124 ]]; then
		die "$gw_bin run -h did not return within 20s; refusing to guess how to run it"
	fi

	local -a gw_argv=("$gw_bin")
	if [[ $help_rc -eq 0 ]]; then
		gw_argv+=("run")
	else
		# No `run` subcommand; check whether this build uses another name before
		# falling back to running with flags alone.
		local top_help="" top_rc=0
		top_help="$(probe_help 20 "$gw_bin" --help)" || top_rc=$?
		if [[ $top_rc -eq 124 ]]; then
			die "$gw_bin --help did not return within 20s; refusing to guess how to run it"
		fi
		run_help="$top_help"
		if [[ $top_help == *serve* ]]; then
			gw_argv+=("serve")
		fi
	fi

	# Go's flag package accepts -config and --config alike, and PrintDefaults
	# renders it with one dash.
	if [[ $run_help == *-config* ]]; then
		gw_argv+=("-config" "$CONFIG_FILE")
	else
		warn "$gw_bin does not advertise a -config flag; starting it without one.
       The default is <stateDir>/config.yaml, which is $CONFIG_FILE, so this is
       usually still right — but workspaces come only from that file, so if the
       gateway exits complaining about workspaces, add the flag to
       $AGENT_DIR/$GATEWAY_LABEL.plist by hand."
	fi
	log "gateway command: ${gw_argv[*]}"

	gw_args_xml=""
	local arg
	for arg in "${gw_argv[@]}"; do
		gw_args_xml+="		<string>$(xml_escape "$arg")</string>"$'\n'
	done

	# The agent host is the same binary shape: a subcommand CLI that takes
	# -config. Its arguments are derived from the gateway's probe rather than
	# guessed again, so the two cannot disagree about how this repository's
	# commands are invoked.
	local host_bin="$INSTALL_DIR/dsh-agent-host" host_args_xml=""
	local -a host_argv=("$host_bin")
	# Both binaries come from this repository and share a CLI shape, so the
	# gateway's probe decides for both: `-h` exits 0 when the build has
	# subcommands, and -config is advertised in the same usage text.
	if [[ $help_rc -eq 0 ]]; then
		host_argv+=("serve")
	fi
	if [[ $run_help == *"-config"* ]]; then
		host_argv+=("-config" "$CONFIG_FILE")
	fi
	for arg in "${host_argv[@]}"; do
		host_args_xml+="		<string>$(xml_escape "$arg")</string>"$'\n'
	done
	log "agent host command: ${host_argv[*]}"

	local tmp changed
	tmp="$(mktemp "$WORK_DIR/plist.XXXXXX")"
	cat >"$tmp" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$GATEWAY_LABEL</string>
	<key>ProgramArguments</key>
	<array>
$gw_args_xml	</array>
	<!-- Start at login and keep it running; the gateway supervises its own
	     DSH child, so a crash loop here is the only way it goes down. -->
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<!-- launchd's default respawn throttle is 10s; stating it keeps a crash
	     loop from spamming the log faster than a human can read it. -->
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>WorkingDirectory</key>
	<string>$(xml_escape "$HOME")</string>
	<!-- launchd gives jobs a minimal PATH and no shell profile. PATH here must
	     contain the node/nvm bin directory or the gateway cannot find \`dsh\`
	     and will fail to spawn its child process. -->
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>$(xml_escape "$search_path")</string>
		<key>HOME</key>
		<string>$(xml_escape "$HOME")</string>
		<!-- Belt and braces: these are also in config.yaml, but if the config
		     path is ever lost, the gateway still knows where it lives and what
		     its public URL is. -->
		<key>DSH_GATEWAY_STATE_DIR</key>
		<string>$(xml_escape "$CONFIG_DIR")</string>
		<key>DSH_GATEWAY_PUBLIC_URL</key>
		<string>$(xml_escape "$PUBLIC_URL")</string>
		<!-- The gateway writes this file itself and rotates it. launchd never
		     rotates StandardErrorPath, and macOS has no user-level rotator to
		     hand it to (newsyslog needs root, which this installer deliberately
		     never asks for) — so without this the log grows for as long as the
		     job runs and only a human with rm ever removes a byte. It names the
		     file the plist used to capture, so `tail -f gateway.log` still
		     reads the same file it always did. -->
		<key>DSH_GATEWAY_LOG_FILE</key>
		<string>$(xml_escape "$LOG_DIR/gateway.log")</string>
		<key>DSH_GATEWAY_LOG_MAX_MB</key>
		<string>16</string>
	</dict>
	<!-- What launchd captures from here is only what the gateway said before its
	     own log was open — a config file that would not parse, a panic before the
	     first line — plus whatever the standard library writes to stderr on its
	     own. Rare and small, which matters: this is the file nothing rotates.
	     gateway.log holds everything worth reading. -->
	<key>StandardErrorPath</key>
	<string>$(xml_escape "$LOG_DIR/gateway.stderr.log")</string>
	<key>StandardOutPath</key>
	<string>$(xml_escape "$LOG_DIR/gateway.stdout.log")</string>
	<!-- How long launchd waits between SIGTERM and SIGKILL. Its default is
	     shorter than a gateway finishing its drain, so without this a redeploy
	     would SIGKILL the process it just asked to stop politely - which is the
	     failure the drain exists to prevent. -->
	<key>ExitTimeOut</key>
	<integer>${EXIT_TIMEOUT_SECONDS}</integer>
</dict>
</plist>
EOF
	changed="$(install_file_if_changed "$tmp" "$AGENT_DIR/$GATEWAY_LABEL.plist" 0644)"
	if [[ $changed == "1" ]]; then
		log "wrote $AGENT_DIR/$GATEWAY_LABEL.plist"
	else
		log "$GATEWAY_LABEL.plist unchanged"
	fi


	# The agent host's own job. It is a peer of the gateway's, not its child:
	# launchd kills every process in a job's process group when the job dies, so
	# a host started by the gateway would be killed by exactly the event it
	# exists to survive - and the two-tier design would appear to work while
	# doing nothing.
	tmp="$(mktemp "$WORK_DIR/plist.XXXXXX")"
	cat >"$tmp" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$AGENT_HOST_LABEL</string>
	<key>ProgramArguments</key>
	<array>
$host_args_xml	</array>
	<!-- Kept running: this process holds the agent and every turn in flight, so
	     its lifetime is deliberately not tied to any client's. -->
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>WorkingDirectory</key>
	<string>$(xml_escape "$HOME")</string>
	<!-- Same PATH reasoning as the gateway's: launchd gives a job a minimal PATH
	     and no shell profile, so the node/nvm bin directory has to be named or
	     the host cannot find `dsh` to launch it with. -->
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>$(xml_escape "$search_path")</string>
		<key>HOME</key>
		<string>$(xml_escape "$HOME")</string>
		<key>DSH_GATEWAY_STATE_DIR</key>
		<string>$(xml_escape "$CONFIG_DIR")</string>
		<!-- Same reason as the gateway's: the host logs its own file and rotates
		     it. The host is the longer-lived of the two jobs, so a file nothing
		     rotates matters more here, not less. -->
		<key>DSH_GATEWAY_LOG_FILE</key>
		<string>$(xml_escape "$LOG_DIR/agent-host.log")</string>
		<key>DSH_GATEWAY_LOG_MAX_MB</key>
		<string>16</string>
	</dict>
	<!-- A host restart is the one restart that can end a turn, so its shutdown
	     drains first. This is the ceiling launchd allows that drain before it
	     stops asking. -->
	<key>ExitTimeOut</key>
	<integer>${EXIT_TIMEOUT_SECONDS}</integer>
	<!-- Only what the host said before its own log was open. See the gateway's
	     plist for the reasoning. -->
	<key>StandardErrorPath</key>
	<string>$(xml_escape "$LOG_DIR/agent-host.stderr.log")</string>
	<key>StandardOutPath</key>
	<string>$(xml_escape "$LOG_DIR/agent-host.stdout.log")</string>
</dict>
</plist>
EOF
	changed="$(install_file_if_changed "$tmp" "$AGENT_DIR/$AGENT_HOST_LABEL.plist" 0644)"
	if [[ $changed == "1" ]]; then
		log "wrote $AGENT_DIR/$AGENT_HOST_LABEL.plist"
	else
		log "$AGENT_HOST_LABEL.plist unchanged"
	fi

	tmp="$(mktemp "$WORK_DIR/plist.XXXXXX")"
	cat >"$tmp" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$FRPC_LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>$(xml_escape "$INSTALL_DIR/frpc")</string>
		<string>-c</string>
		<string>$(xml_escape "$FRPC_CONFIG")</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>WorkingDirectory</key>
	<string>$(xml_escape "$HOME")</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>$(xml_escape "$search_path")</string>
		<key>HOME</key>
		<string>$(xml_escape "$HOME")</string>
	</dict>
	<key>StandardOutPath</key>
	<string>$(xml_escape "$LOG_DIR/frpc.log")</string>
	<key>StandardErrorPath</key>
	<string>$(xml_escape "$LOG_DIR/frpc.err.log")</string>
</dict>
</plist>
EOF
	changed="$(install_file_if_changed "$tmp" "$AGENT_DIR/$FRPC_LABEL.plist" 0644)"
	if [[ $changed == "1" ]]; then
		log "wrote $AGENT_DIR/$FRPC_LABEL.plist"
	else
		log "$FRPC_LABEL.plist unchanged"
	fi

	# Keep the resolved values for the summary without recomputing them.
	RESOLVED_DSH_BIN="$dsh_bin"
	RESOLVED_SEARCH_PATH="$search_path"
	# Tracked so the pairing command printed at the end matches how the job is
	# actually started. `pair` shares resolveConfig with `run`, so it takes the
	# same flag.
	if [[ $run_help == *-config* ]]; then
		RESOLVED_CONFIG_FLAG=1
	else
		RESOLVED_CONFIG_FLAG=0
	fi
}

# stop_job asks a running job to stop and waits for it to go, bounded by
# EXIT_TIMEOUT_SECONDS.
#
# It replaces the unconditional `bootout` this script used to do, and the
# difference is the whole point of the agent-host tier. `bootout` sends SIGTERM
# and then stops waiting, so a gateway with a minute of work left in it was
# killed on launchd's schedule rather than its own - and a turn that had been
# running for twelve minutes was destroyed by a deploy that could have waited.
#
# It waits for the *process* rather than for launchd's bookkeeping: `kill -0` on
# the pid launchd reports is the only answer to "has it actually stopped", and
# the pid is free the moment it has.
stop_job() {
	local label="$1" uid
	uid="$(id -u)"

	launchctl print "gui/$uid/$label" >/dev/null 2>&1 || return 0

	local pid
	pid="$(launchctl print "gui/$uid/$label" 2>/dev/null | awk '/^[[:space:]]*pid = /{print $3; exit}')"
	if [[ -z $pid ]]; then
		# Loaded but not running: nothing to wait for.
		launchctl bootout "gui/$uid/$label" >/dev/null 2>&1 || true
		return 0
	fi

	log "asking $label (pid $pid) to stop; it may be finishing a turn"
	kill -TERM "$pid" 2>/dev/null || true

	local waited=0
	while kill -0 "$pid" 2>/dev/null; do
		if (( waited >= EXIT_TIMEOUT_SECONDS )); then
			warn "$label (pid $pid) did not stop within ${EXIT_TIMEOUT_SECONDS}s.
       Its drain window is the same length, so this usually means a turn is
       still running. Inspect it, then either wait for it or stop the job by
       hand:
         tail -30 $LOG_DIR/${label##*.}.log
         launchctl kickstart -k gui/$uid/$label"
			break
		fi
		sleep 0.5
		waited=$((waited + 1))
	done

	# Whatever happened, the job must be out of the way before bootstrapping the
	# replacement: bootstrap fails with "service already loaded" otherwise, and
	# the fallback below fails the same way - which used to leave the service
	# DOWN while reporting only a warning.
	launchctl bootout "gui/$uid/$label" >/dev/null 2>&1 || true

	waited=0
	while launchctl print "gui/$uid/$label" >/dev/null 2>&1; do
		if (( waited >= 20 )); then
			warn "$label is still loaded 10s after bootout; trying anyway"
			break
		fi
		sleep 0.5
		waited=$((waited + 1))
	done
}

# job_pid prints the pid launchd reports for a job, or nothing.
job_pid() {
	local label="$1" uid
	uid="$(id -u)"
	launchctl print "gui/$uid/$label" 2>/dev/null \
		| sed -n 's/^[[:space:]]*pid = \([0-9][0-9]*\).*/\1/p' | head -1
}

# wait_for_stable watches a job for a few seconds and reports whether it was
# replaced while being watched.
#
# `launchctl bootstrap` returning success means launchd accepted the definition,
# not that a process is up and staying up — and every other check here is happy
# with a job that is being restarted repeatedly, because a *previous* instance is
# still answering. Observed on a real deployment: two processes claimed the
# listening socket about half a second apart on every single deploy, while the
# installer reported success each time. The service was fine within a second, so
# nothing was broken — but "the deploy succeeded" was true for a reason nobody
# had checked, and a half-second window with no gateway in it is exactly the kind
# of thing that is blamed on the phone.
#
# The pid changing is the signal, and it cannot be faked by a process that is
# merely busy. The first sample is taken *immediately* and then watched: the
# replacement observed in practice landed at about 500ms, so a reading taken after
# a settling sleep would have missed it entirely — which the first version of
# this check did.
wait_for_stable() {
	local label="$1" uid first last pid changes=0 waited=0
	uid="$(id -u)"

	# launchd has already started it by the time bootstrap returns, so the pid
	# to compare against exists now.
	first="$(job_pid "$label")"
	last="$first"

	while (( waited < 32 )); do
		sleep 0.2
		waited=$((waited + 1))
		pid="$(job_pid "$label")"
		if [[ -n $pid && $pid != "$last" ]]; then
			changes=$((changes + 1))
			last="$pid"
		fi
	done

	if (( changes > 0 )); then
		warn "$label was replaced $changes time(s) in the 6s after it was loaded
       (first pid ${first:-none}, now ${last:-none}). It is serving, and the
       deployment works, but the process answering is not the one that was
       started — so a request in that window saw no gateway at all. Worth
       knowing before blaming the phone. Inspect it:
         launchctl print gui/$uid/$label | grep -E 'pid|runs|last exit'
         tail -40 $LOG_DIR/${label##*.}.log"
		return 1
	fi
	return 0
}

bootstrap_agent() {
	local label="$1" plist="$2" uid
	uid="$(id -u)"

	stop_job "$label"

	# Retry as well as wait: the teardown window is not the only source of a
	# transient failure, and a second attempt is cheap next to leaving a service
	# down. Five attempts is the whole policy, so the counter is never read.
	for _ in 1 2 3 4 5; do
		if launchctl bootstrap "gui/$uid" "$plist" 2>/dev/null; then
			launchctl enable "gui/$uid/$label" 2>/dev/null || true
			wait_for_stable "$label"
			log "loaded $label"
			return 0
		fi
		sleep 1
	done

	# Older macOS and managed environments where bootstrap is unavailable;
	# `load -w` is the pre-10.10 spelling of the same thing.
	warn "launchctl bootstrap failed for $label after 5 attempts; falling back to launchctl load"
	if launchctl load -w "$plist" 2>/dev/null; then
		log "loaded $label via launchctl load"
		return 0
	fi

	die "$label could not be loaded. The service is NOT running.
       Inspect:  launchctl print gui/$uid/$label
       Recover:  launchctl bootstrap gui/$uid $plist"
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
print_summary() {
	local gw_cmd="$INSTALL_DIR/dsh-gateway pair"
	if [[ ${RESOLVED_CONFIG_FLAG:-0} == "1" ]]; then
		gw_cmd+=" -config $CONFIG_FILE"
	fi

	local path_note=""
	case ":$PATH:" in
	*":$INSTALL_DIR:"*) ;;
	*) path_note="
  NOTE: $INSTALL_DIR is not on your PATH in this shell, so \`dsh-gateway\` will
        not resolve by name. Either add it:
            echo 'export PATH=\"$INSTALL_DIR:\$PATH\"' >> ~/.zshrc && exec zsh
        or call the binary by its full path." ;;
	esac

	# The root certificate a phone has to install in the default (--tls internal)
	# mode, as the first pairing step. Both URLs are derived from PUBLIC_URL
	# rather than hardcoded, because the hostname is the part that differs
	# between deployments and a wrong hostname is worse than no hint at all: the
	# HTTPS copy is the public URL's own host, the plain-HTTP copy is the same
	# host on the port that serves it (8080 unless --http-port changed it).
	local ca_step
	if [[ $PUBLIC_URL == https://* ]]; then
		local authority="${PUBLIC_URL#https://}"
		authority="${authority%%/*}" # drop any path
		authority="${authority%%:*}" # drop the HTTPS port
		ca_step="  1. Teach the phone to trust this deployment, if the VPS was installed with the
     default --tls internal certificate. That CA signs the app's certificate,
     and the phone has no way to know it yet; this Mac cannot see the VPS's
     configuration, so nothing here can tell you which mode was used — the
     \"Certificate\" line at the end of the VPS installer's summary says.

         ${PUBLIC_URL}/ca.crt

     Open that on the phone. It warns about the certificate until the root is
     installed, which is expected — a phone cannot verify a certificate before it
     has the root that signs it. Accept the warning (iOS: Show Details → visit
     this website; Android: Advanced → Proceed), then install:

       iOS:     Settings → General → VPN & Device Management → install the
                profile, then Settings → General → About → Certificate Trust
                Settings → turn it ON. Both steps are required; iOS ignores an
                installed-but-untrusted root.
       Android: Settings → Security → Encryption & credentials → Install a
                certificate → CA certificate.

     (Under --tls acme, skip this: the certificate is publicly trusted. If the
     HTTPS download is refused, the same root is also served in the clear at
     http://${authority}:8080/ca.crt — a provider may answer that with an ICP
     block page instead — and can always be copied out of band from
     /var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt on the
     VPS.)"
	else
		ca_step="  1. No CA to install: this public URL is plain HTTP, so there is no
     certificate to trust. A real deployment uses https."
	fi

	cat >&2 <<EOF

=============================================================================
Mac setup complete.
=============================================================================

  Gateway binary      $INSTALL_DIR/dsh-gateway
  Agent host binary   $INSTALL_DIR/dsh-agent-host
  frpc binary         $INSTALL_DIR/frpc ($FRP_VERSION)
  Tunnel              $FRP_SERVER:$FRP_CONTROL_PORT  →  local 127.0.0.1:8787
  Public URL          $PUBLIC_URL
  Config              $CONFIG_FILE  (yours; never overwritten)
  frpc config         $FRPC_CONFIG  (0600)
  Logs                $LOG_DIR/

  launchd jobs        $AGENT_HOST_LABEL, $GATEWAY_LABEL, $FRPC_LABEL
  PATH for jobs       $RESOLVED_SEARCH_PATH
  dsh binary          $RESOLVED_DSH_BIN

Pair a phone:

$ca_step

  2. Open $PUBLIC_URL on the phone.

  3. On the Mac, as your normal user — NOT with sudo, or it will read a
     different state directory and print a code the gateway will reject:

         $gw_cmd

     A code is valid for 10 minutes and the previous window is also accepted,
     so a code printed just before a boundary still works. Re-run the command
     for a fresh code; there is no way to re-print an old one.

  4. Type the code into the phone. The response enrols the device and stores a
     session cookie; the device then appears in the app's device list and can be
     revoked from there.

Useful commands:

     launchctl print gui/$(id -u)/$GATEWAY_LABEL | head -30
     tail -f $LOG_DIR/gateway.log
     tail -f $LOG_DIR/frpc.log
     $INSTALL_DIR/frpc verify -c $FRPC_CONFIG
     bash $REPO_ROOT/deploy/mac/install.sh --help
$path_note
EOF
}

# verify_host_up asks the agent host whether it is serving, using its own status
# command rather than a socket probe: "listening" and "answering" are different
# facts, and the one that matters is the second.
verify_host_up() {
	local out=""
	for _ in $(seq 1 20); do
		out="$("$INSTALL_DIR/dsh-agent-host" status -config "$CONFIG_FILE" -timeout 2s 2>&1 || true)"
		if [[ $out == *"agent host"* ]]; then
			log "agent host is serving"
			return 0
		fi
		sleep 1
	done
	warn "the agent host did not answer within 20s.

       Read the log first:
         tail -40 $LOG_DIR/agent-host.log

       To see what launchd thinks:
         launchctl print gui/$(id -u)/$AGENT_HOST_LABEL | head -40

       The gateway will keep retrying the connection, so this is not fatal - but
       nothing can run until it is up."
}

# verify_gateway_up asks the gateway's own liveness endpoint whether it came up,
# rather than trusting `launchctl print` (a crash-looping job is still "loaded").
verify_gateway_up() {
	for _ in $(seq 1 15); do
		if curl -fsS -m 2 -o /dev/null "http://127.0.0.1:8787/healthz" 2>/dev/null; then
			log "gateway answered on http://127.0.0.1:8787/healthz"
			return 0
		fi
		sleep 1
	done
	# Reaching here means the job is loaded but not answering.
	#
	# This is not fatal in itself — the operator may have changed listen: in the
	# config, in which case the probe is aimed at the wrong address. But it must
	# be loud and it must name the recovery, because the alternative is an
	# operator who believes the install succeeded and finds out otherwise from
	# their phone.
	warn "the gateway is loaded but did not answer on http://127.0.0.1:8787/healthz within 15s.

       Read the log first — it almost always says why:
         tail -40 $LOG_DIR/gateway.log

       If you changed listen: in $CONFIG_FILE, probe that address instead:
         grep '^listen:' $CONFIG_FILE

       To see what launchd thinks:
         launchctl print gui/$(id -u)/$GATEWAY_LABEL | head -40

       To restart it by hand:
         launchctl bootout gui/$(id -u)/$GATEWAY_LABEL
         launchctl bootstrap gui/$(id -u) $AGENT_DIR/$GATEWAY_LABEL.plist"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
cleanup() {
	if [[ -n $WORK_DIR && -d $WORK_DIR ]]; then
		rm -rf "$WORK_DIR"
	fi
}

main() {
	parse_args "$@"
	require_macos
	require_inputs

	local arch
	arch="$(detect_arch)"
	WORK_DIR="$(mktemp -d)"
	trap cleanup EXIT

	check_go
	check_npm
	check_dsh

	mkdir -p "$INSTALL_DIR" "$CONFIG_DIR" "$LOG_DIR" "$AGENT_DIR"
	[[ -w $INSTALL_DIR ]] || die "$INSTALL_DIR is not writable.
       Use --install-dir to pick somewhere you own (e.g. ~/.local/bin), or create
       it with the right ownership first."
	# 700 on both, and the log directory matters as much as the config one: the
	# gateway prints a live pairing code at startup, and that code enrols a fully
	# authorised device. A world-readable log would hand it to any local account
	# for as long as the code's window lasts.
	chmod 700 "$CONFIG_DIR" "$LOG_DIR"

	install_frpc "$arch"
	build_web
	build_gateway

	local token
	token="$(resolve_token)"
	write_frpc_config "$token"
	write_config_yaml
	write_plists

	# Order matters, and not for tidiness: the gateway connects to the host at
	# start-up, and starting it first would make every deploy begin with a
	# reconnect loop. The host is a peer job, so it survives the gateway being
	# replaced a moment later.
	if [[ $SKIP_HOST_RESTART == "1" ]]; then
		# The plist and the binary on disk are the new ones; the process keeps
		# the ones it started with, and the turn it is running with them. This is
		# the only reason to leave a job behind, and the instruction to finish the
		# job is printed rather than implied — an operator who forgets it is
		# running two versions of the host and does not know it.
		warn "--skip-host-restart: $AGENT_HOST_LABEL keeps the binary and plist it started with"
		warn "  finish the deploy when no turn is running:"
		warn "    launchctl bootout gui/$(id -u)/$AGENT_HOST_LABEL"
		warn "    launchctl bootstrap gui/$(id -u) $AGENT_DIR/$AGENT_HOST_LABEL.plist"
		# Still checked: a host that is not answering is worth knowing about even
		# though this run did not restart it.
		verify_host_up
	else
		bootstrap_agent "$AGENT_HOST_LABEL" "$AGENT_DIR/$AGENT_HOST_LABEL.plist"
		verify_host_up
	fi
	bootstrap_agent "$GATEWAY_LABEL" "$AGENT_DIR/$GATEWAY_LABEL.plist"
	bootstrap_agent "$FRPC_LABEL" "$AGENT_DIR/$FRPC_LABEL.plist"

	verify_gateway_up
	if launchctl print "gui/$(id -u)/$FRPC_LABEL" >/dev/null 2>&1; then
		log "$FRPC_LABEL loaded"
	else
		warn "$FRPC_LABEL is not loaded; inspect: launchctl print gui/$(id -u)/$FRPC_LABEL"
	fi

	print_summary
}

main "$@"
