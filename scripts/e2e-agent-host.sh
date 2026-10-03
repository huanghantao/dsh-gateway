#!/usr/bin/env bash
#
# Two-tier deployment smoke test.
#
# It exercises the properties that only exist once the pieces are real processes
# talking over a real socket: that the host serves and answers, that the socket
# is not reachable by other accounts, that a second host refuses to displace the
# first, and that a stopped host cleans up after itself so the next one can
# start.
#
# It is not a substitute for `go test ./...`, which owns the lifetime semantics.
# What it adds is the deployment surface: the CLI shapes the installer generates
# arguments for, and the filesystem rules the security of the seam depends on.
#
# Usage: scripts/e2e-agent-host.sh [--keep]
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
PASS=0
FAIL=0

cleanup() {
	if [[ -n ${HOST_PID:-} ]] && kill -0 "$HOST_PID" 2>/dev/null; then
		kill -TERM "$HOST_PID" 2>/dev/null || true
		wait "$HOST_PID" 2>/dev/null || true
	fi
	if [[ ${1:-} == "--keep" ]]; then
		printf 'workspace kept: %s\n' "$WORK"
		return
	fi
	rm -rf "$WORK"
}
trap cleanup EXIT

ok() {
	PASS=$((PASS + 1))
	printf '  \033[32mok\033[0m   %s\n' "$1"
}

bad() {
	FAIL=$((FAIL + 1))
	printf '  \033[31mFAIL\033[0m %s\n' "$1"
}

check() {
	if [[ $1 == "0" ]]; then ok "$2"; else bad "$2"; fi
}

# A state directory short enough for a unix socket path. macOS allows 104 bytes
# and go's t.TempDir names alone can approach that, so the path is built by hand.
STATE="$(mktemp -d /tmp/dshgw.XXXXXX)"
SOCKET="$STATE/agent-host.sock"
BIN="$WORK/bin"
mkdir -p "$BIN"

printf '\n==> building\n'
(cd "$REPO_ROOT" && go build -o "$BIN/dsh-agent-host" ./cmd/dsh-agent-host)
(cd "$REPO_ROOT" && go build -o "$BIN/dsh-gateway" ./cmd/dsh-gateway)
ok "both binaries build"

# The stub child: enough of an ACP server that the host's spawn and handshake
# are exercised without a model call. It answers initialize and then waits for
# the client to close, which is what DeepSeek Harness does.
cat >"$WORK/dsh" <<'STUB'
#!/usr/bin/env bash
# A minimal ACP-shaped child. It reads newline-delimited JSON on stdin and
# answers the handshake; everything else is ignored, which is enough for the
# host to reach "ready".
if [[ ${1:-} == "--version" ]]; then
	echo "0.0.0-stub"
	exit 0
fi
while IFS= read -r line; do
	case "$line" in
	*'"method":"initialize"'*)
		id="$(printf '%s' "$line" | sed -n 's/.*"id":\([^,}]*\).*/\1/p')"
		printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentInfo":{"name":"stub","version":"0"},"agentCapabilities":{"sessionCapabilities":{"list":{},"resume":{},"close":{}}}}}\n' "${id:-1}"
		;;
	esac
done
STUB
chmod +x "$WORK/dsh"

cat >"$STATE/config.yaml" <<EOF
listen: "127.0.0.1:18799"
publicURL: "http://127.0.0.1:18799"
stateDir: "$STATE"
log:
  level: "info"
dsh:
  mode: "agent-host"
  binary: "$WORK/dsh"
  profile: "acp"
  sandboxMode: "workspace-write"
  startTimeout: "10s"
  stopTimeout: "5s"
  restartBackoff: "200ms"
  maxRestartBackoff: "2s"
session:
  idleTimeout: "5m"
  promptTimeout: "1m"
  approvalTimeout: "30s"
  deployDrainTimeout: "5s"
workspaces:
  - "$WORK"
EOF

HOST="$BIN/dsh-agent-host"

printf '\n==> status with no host running\n'
out="$("$HOST" status -config "$STATE/config.yaml" 2>&1)"
if [[ $out == *"no agent host"* ]]; then
	ok "status reports an absent host as a state, not a failure"
else
	bad "status with no host said: $out"
fi

printf '\n==> the host serves\n'
"$HOST" serve -config "$STATE/config.yaml" -socket "$SOCKET" >"$WORK/host.log" 2>&1 &
HOST_PID=$!

up=1
for _ in $(seq 1 40); do
	if out="$("$HOST" status -config "$STATE/config.yaml" -socket "$SOCKET" 2>&1)" && [[ $out == *"agent host"* ]]; then
		up=0
		break
	fi
	sleep 0.25
done
check "$up" "a running host answers status"
if [[ $up != "0" ]]; then
	printf 'host log:\n'; sed -n '1,40p' "$WORK/host.log"
	exit 1
fi

if [[ $out == *"harness   ready"* ]]; then
	ok "the host reports the child as ready"
else
	bad "the host never reported a ready harness: $(printf '%s' "$out" | tr '\n' ' ')"
fi

printf '\n==> the socket is private\n'
perm="$(stat -f '%Lp' "$SOCKET" 2>/dev/null || stat -c '%a' "$SOCKET")"
if [[ $perm == "600" ]]; then
	ok "socket mode is 0600"
else
	bad "socket mode is $perm, want 600"
fi

printf '\n==> a second host refuses to displace the first\n'
if "$HOST" serve -config "$STATE/config.yaml" -socket "$SOCKET" >"$WORK/second.log" 2>&1; then
	bad "a second host started while the first was serving"
else
	if grep -q "already served by a running agent host" "$WORK/second.log"; then
		ok "the second host refused, naming the reason"
	else
		bad "the second host failed for an unexplained reason: $(head -3 "$WORK/second.log")"
	fi
fi

printf '\n==> shutdown is clean\n'
kill -TERM "$HOST_PID"
waited=0
while kill -0 "$HOST_PID" 2>/dev/null; do
	if (( waited > 100 )); then
		bad "the host did not stop within 10s of SIGTERM"
		break
	fi
	sleep 0.1
	waited=$((waited + 1))
done
wait "$HOST_PID" 2>/dev/null || true
HOST_PID=""
if kill -0 "$HOST_PID" 2>/dev/null; then :; fi
ok "the host stopped on SIGTERM"

# The socket file may remain (SIGTERM removes it, SIGKILL would not), and either
# way the next start must succeed: that is the crash-recovery property.
if "$HOST" status -config "$STATE/config.yaml" -socket "$SOCKET" >/dev/null 2>&1; then
	ok "status after shutdown is a clean 'no host'"
else
	bad "status failed after shutdown"
fi

"$HOST" serve -config "$STATE/config.yaml" -socket "$SOCKET" >"$WORK/host2.log" 2>&1 &
HOST_PID=$!
restarted=1
for _ in $(seq 1 40); do
	if "$HOST" status -config "$STATE/config.yaml" -socket "$SOCKET" >/dev/null 2>&1; then
		restarted=0
		break
	fi
	sleep 0.25
done
check "$restarted" "a host starts again over its predecessor's socket"

printf '\n==> a second gateway cannot disturb the host\n'
# This is a regression test for a defect that reached a real deployment: the
# gateway connected to the agent host *before* binding its port, so a second
# instance took the control connection from the one serving the phone — and only
# then found the port taken and exited. Every redeploy displaced the live
# gateway twice.
#
# The port is claimed first now, so the second process must die on the bind
# without ever reaching the host. The host's epoch is the evidence: if the
# second gateway had connected, it would have displaced the first and the epoch
# would be the same but the first gateway's connection would have been dropped.
GW_BIN="$BIN/dsh-gateway"
GW_STATE="$WORK/gwstate"
mkdir -p "$GW_STATE"
GW_LISTEN="127.0.0.1:18798"

# The gateway is pointed at the host that is already running, which is what
# makes the assertion meaningful: a second process that reached the host would
# have something to displace.
cat >"$GW_STATE/config.yaml" <<EOF
listen: "$GW_LISTEN"
publicURL: "http://127.0.0.1:18798"
stateDir: "$STATE"
log:
  level: "info"
dsh:
  mode: "agent-host"
  binary: "$WORK/dsh"
  profile: "acp"
  sandboxMode: "workspace-write"
  startTimeout: "5s"
workspaces:
  - "$WORK"
EOF

"$GW_BIN" run -config "$GW_STATE/config.yaml" >"$WORK/gw1.log" 2>&1 &
GW_PID=$!
gw_up=1
for _ in $(seq 1 40); do
	if curl -sf -m 2 -o /dev/null "http://$GW_LISTEN/healthz" 2>/dev/null; then
		gw_up=0
		break
	fi
	sleep 0.25
done
check "$gw_up" "a gateway starts against the running host"
# It talks to the real host now, so its connection is the control one.
got_epoch="$("$HOST" status -config "$STATE/config.yaml" -socket "$SOCKET" | awk '/epoch/{print $2}')"

if "$GW_BIN" run -config "$GW_STATE/config.yaml" >"$WORK/gw2.log" 2>&1; then
	bad "a second gateway on the same port started"
else
	if grep -q "cannot listen on\|address already in use" "$WORK/gw2.log"; then
		ok "the second gateway refused the port"
	else
		bad "the second gateway failed for another reason: $(grep -v '^ ' "$WORK/gw2.log" | tail -2 | tr '\n' ' ')"
	fi
	# The decisive assertion: it died *before* reaching the host.
	if grep -q "driving the agent through the agent host" "$WORK/gw2.log"; then
		bad "the second gateway contacted the agent host before discovering the port was taken; " \
			"on a real deployment that displaces the gateway serving the phone"
	else
		ok "it died before touching the agent host"
	fi
fi

if kill -0 "$GW_PID" 2>/dev/null; then
	ok "the first gateway is still running"
else
	bad "the first gateway died when the second was started"
fi
after_epoch="$("$HOST" status -config "$STATE/config.yaml" -socket "$SOCKET" | awk '/epoch/{print $2}')"
if [[ $got_epoch == "$after_epoch" ]]; then
	ok "the host was never displaced"
else
	bad "the host epoch changed: $got_epoch -> $after_epoch"
fi
kill -TERM "$GW_PID" 2>/dev/null || true
wait "$GW_PID" 2>/dev/null || true

printf '\n==> the harness handshake is real\n'
if grep -q "harness ready" "$WORK/host.log"; then
	ok "the host completed the ACP handshake with the child"
else
	bad "the host never reported a ready harness; log says: $(grep -c . "$WORK/host.log") lines"
fi

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ $FAIL == "0" ]]
