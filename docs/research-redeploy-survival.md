# Research: surviving a redeploy — sockets, graceful restart, durable execution, resumable streams, fencing

Scope: the gateway runs on a macOS desktop under launchd, serves a phone PWA over
WebSocket + REST, and drives a long-lived ACP child over stdio. A redeploy today is
`launchctl bootout` + `bootstrap`: the WebSocket drops, the child is killed, the
in-flight turn dies.

Method note: every URL below was fetched and read during this research. Quotes are
verbatim from the fetched document. Apple man pages were read from the local system
(macOS 26, Darwin) and are additionally linked to Apple's published source where one
exists. Claims I could not source are explicitly marked **INFERENCE**.

---

## 1. Socket activation / listener handoff on macOS

### Mechanism

launchd does not exec your process and hope it binds. When a job's `launchd.plist`
contains a `Sockets` dictionary, **launchd itself creates, binds and listens on the
socket** while parsing the job, and holds it on the job's behalf:

> "This optional key is used to specify launch on demand sockets that can be used to let
> launchd know when to run the job. The job must check-in to get a copy of the file
> descriptors using the `launch_activate_socket(3)` API."
> — [launchd.plist(5), Apple OSS source](https://github.com/apple-oss-distributions/launchd/blob/main/man/launchd.plist.5)

> "The `launch_activate_socket()` routine allows a launchd(8) job to retrieve a set of
> file descriptors corresponding to a socket service that **launchd(8) has created** and
> advertised on behalf of the job by parsing the `Sockets` entry in the job's
> `launchd.plist(5)`."
> — [launch(3), Xcode man pages](https://keith.github.io/xcode-man-pages/launch_activate_socket.3.html) (identical text locally in `man 3 launch_activate_socket`)

The caller names the socket by its `Sockets` key; the returned array is heap-allocated
and the caller must `free(3)` it. Depending on `SockFamily`, one entry can yield several
fds (one per address family). Errors: `ENOENT` (no such key), `ESRCH` (process not
managed by launchd), `EALREADY` (already activated) — same source. Keys are described in
`launchd.plist(5)`: `SockType` (default `stream`), `SockPassive` (default `true`, i.e.
`listen(2)` rather than `connect(2)`), `SockNodeName`, `SockServiceName`, `SockFamily`
(`IPv4` / `IPv6` / `IPv4v6`), `SockProtocol` (`TCP`/`UDP`), `SockPathName`.

### What actually survives a restart

The decisive question is *who owns the socket inode*. launchd does. So:

| State | Survives a job restart? | Why |
|---|---|---|
| Listening socket + **unaccepted backlog** | **Yes**, provided the service definition stays loaded | The socket object (including its kernel accept queue) belongs to launchd, not the job. Nothing closes it when the job dies. |
| **Established TCP connections** | **No** | An accepted connection is a *different* socket, whose fd lives in the process. Process exit → kernel closes fds → FIN/RST. |
| **In-flight HTTP request / WebSocket frame** | **No** | Pure userspace state. |

**INFERENCE (mechanism, well-supported but not spelled out by Apple):** the backlog
survives *only while the service stays loaded*. `launchctl kickstart -k` restarts the
same service — "If the service is already running, kill the running instance before
restarting the service" ([launchctl(1)](https://keith.github.io/xcode-man-pages/launchctl.1.html),
local `man 1 launchctl`) — so the socket is untouched. `bootout` + `bootstrap` **removes
and re-creates the service definition**, so the launchd-owned socket is closed and
re-created; connections queued in the old backlog at that instant are dropped (RST),
and clients get `ECONNREFUSED` for the gap. I could not find Apple documentation stating
this explicitly; it follows from launchd owning the socket. **Practical rule: prefer
`kickstart -k` over `bootout`+`bootstrap` whenever the socket matters.** systemd states
the equivalent property plainly:

> "if a daemon is restarted (and its associated sockets are not) it will receive file
> descriptors to the very same sockets as the earlier invocations, thus all socket
> options applied then will still apply."
> — [sd_listen_fds(3)](https://raw.githubusercontent.com/systemd/systemd/main/man/sd_listen_fds.xml)

That same source documents two footguns that apply to launchd by analogy: the fds the
daemon receives are **duplicates** of the manager's, so "any socket option changes …
will be visible to the service manager too", and "it is generally not a good idea to
invoke `shutdown(2)` on such sockets, since it will shut down communication on the file
descriptor the service manager holds for the same socket too."

### The launchd behaviour that will bite the two-tier design

> "**AbandonProcessGroup** `<boolean>` — When a job dies, launchd kills any remaining
> processes with the same process group ID as the job. Setting this key to true disables
> that behavior."
> — [launchd.plist(5)](https://github.com/apple-oss-distributions/launchd/blob/main/man/launchd.plist.5) (local `man 5 launchd.plist`)

This is the single most consequential finding for ADR 0007. **If `dsh-agent-host` is
spawned as a child of the gateway's job, launchd kills it when the gateway dies — the
whole tiered design silently reverts to the status quo.** The host must be its own
launchd job (its own label, its own plist), not a child of the gateway. Conversely, if
you ever *want* the child to die with the gateway, this key is what guarantees it, and
you should not set `AbandonProcessGroup=true`. Related lifecycle keys, same source:
`KeepAlive` (boolean or condition dictionary; implies `RunAtLoad`; "Jobs that exit
quickly and frequently when configured to be kept alive will be throttled"),
`ThrottleInterval` ("by default, jobs will not be spawned more than once every 10
seconds" — this is the 10 s throttle observed in the ADR), `ExitTimeOut` ("The amount of
time launchd waits between sending the SIGTERM signal and before sending a SIGKILL
signal when the job is to be stopped"), `EnableTransactions`.

launchd also has an inetd-compatibility path (`inetdCompatibility` → `Wait`), where
"if true, then the listening socket is passed via the stdio(3) file descriptors. If
false, then `accept(2)` is called on behalf of the job, and the result is passed via the
`stdio(3)` descriptors" — the man page says "For new projects, this key should be
avoided", and the `Wait=false` form is exactly systemd's `Accept=yes` (one process per
connection), which is the opposite of what a WebSocket server wants.

### Contrast with systemd, and the missing fd store

systemd factorises the socket into its own unit type. `Accept=no` (the default) gives the
service the listening socket; `Accept=yes` makes the manager accept and spawn a
`foo@.service` **instance per connection**
([systemd.socket(5)](https://raw.githubusercontent.com/systemd/systemd/main/man/systemd.socket.xml)).
Go consumes this via `LISTEN_FDS`/`LISTEN_PID` starting at fd 3, and the library guards
against fd leakage by checking the PID:

```go
pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
if err != nil || pid != os.Getpid() { return nil }
```
— [coreos/go-systemd `activation/files_unix.go`](https://github.com/coreos/go-systemd/blob/main/activation/files_unix.go)

Untrusted `unsetEnv=true` also clears `LISTEN_FDS`/`LISTEN_PID`/`LISTEN_FDNAMES` "to
avoid leaking environment flags to child processes". `SD_LISTEN_FDS_START` is 3
([sd_listen_fds(3)](https://raw.githubusercontent.com/systemd/systemd/main/man/sd_listen_fds.xml)).
This PID guard is the analogue of tableflip's sentinel env var (§2), and both exist
because inherited fds are ambient authority.

Crucially, **systemd has an fd store and launchd does not**:

> "Any open sockets and other file descriptors which should not be closed during a
> restart may be stored this way. … This functionality should be used to implement
> services that can restart after an explicit request **or a crash** without losing state."
> … "When a service is stopped, its file descriptor store is discarded and all file
> descriptors in it are closed, except when overridden with `FileDescriptorStorePreserve=`."
> — [sd_notify(3)](https://raw.githubusercontent.com/systemd/systemd/main/man/sd_notify.xml)

There is no `FDSTORE`-equivalent key in `launchd.plist(5)` (I read the full key list).
So on macOS the only way to keep serving across a full job replacement is for the
*socket to be owned by launchd* (the `Sockets` route) or for a **separate process** to
hold it.

### Go libraries for launchd socket activation

- `github.com/coreos/go-systemd/activation` is Linux/systemd only: it reads
  `LISTEN_FDS`/`LISTEN_PID`, which launchd never sets.
- `cloudflare/tableflip` does **not** use launchd socket activation — it inherits fds
  from *its own parent* (§2).
- I found **no mature Go module that wraps `launch_activate_socket`**. It is a libSystem
  function declared in `<launch.h>` (present in the macOS SDK: `/Library/Developer/CommandLineTools/SDKs/MacOSX.sdk/usr/include/launch.h`, with the prototype at line 197 and a doc comment pointing at `launchd.plist(5)`), not a syscall — so reaching it from Go requires cgo. **INFERENCE:** a ~30-line cgo shim is the only supported route; a pure-Go reimplementation would have to speak launchd's private Mach IPC, which is not a supportable dependency. I was unable to run web searches (no search API key in this environment), so "no such library exists" is a negative finding from pkg.go.dev/GitHub inspection rather than an exhaustive survey.

---

## 2. Uninterrupted / graceful restart libraries and patterns in Go

### cloudflare/tableflip — fd inheritance, not `SO_REUSEPORT`

> "An upgrade spawns a new copy of `argv[0]` and **passes file descriptors of used
> listening sockets to the new process**. The old process exits once the new process
> signals readiness. Thus new code can use sockets allocated in the old process. This is
> similar to the approach used by nginx, but as a library."
> — [`tableflip/doc.go`](https://github.com/cloudflare/tableflip/blob/master/doc.go)

The mechanism is `fork`/`exec` fd inheritance, not `SCM_RIGHTS` and not `SO_REUSEPORT`
(`grep -r REUSEPORT` over the package: **no matches**):

```go
attr := &syscall.ProcAttr{ Dir: initialWD, Env: env, Files: fds }
pid, _, err := syscall.StartProcess(executable, args, attr)
```
— [`process.go`](https://github.com/cloudflare/tableflip/blob/master/process.go)

The child identifies itself by env var `TABLEFLIP_HAS_PARENT_7DIU3`, then reads a
gob-encoded list of listener names from **fd 4**, adopts listeners at **fd 5+i**, and
signals readiness by writing a byte to **fd 3**
([`parent.go`](https://github.com/cloudflare/tableflip/blob/master/parent.go),
[`child.go`](https://github.com/cloudflare/tableflip/blob/master/child.go)). Because the
child `dup2`s the *same* open file description, parent and child share one kernel socket
and therefore **one accept queue** — the backlog is genuinely preserved across the
overlap. Documented goals: "No old code keeps running after a successful upgrade", "The
new process has a grace period for performing initialisation", "Crashing during
initialisation is OK", "Only a single upgrade is ever run in parallel"; "**`tableflip`
works on Linux and macOS**" ([README](https://github.com/cloudflare/tableflip/blob/master/README.md));
"Tableflip does not work on Windows" ([doc.go](https://github.com/cloudflare/tableflip/blob/master/doc.go)).
`DefaultUpgradeTimeout = time.Minute`; an unready child is killed after that
([`upgrader.go`](https://github.com/cloudflare/tableflip/blob/master/upgrader.go)).

What it does **not** do: drain. The parent must stop accepting and wait for established
connections itself — the canonical example is

```go
<-upg.Exit()
time.AfterFunc(30*time.Second, func() { os.Exit(1) })   // hard deadline
server.Shutdown(context.Background())                    // wait for connections to drain
```
— [`http_example_test.go`](https://github.com/cloudflare/tableflip/blob/master/http_example_test.go)

So: listeners and backlog survive; **established connections and in-flight requests are
the application's problem**, and the drain window is bounded by the process's own timer.

**INFERENCE — tableflip under launchd is a trap.** The parent is the job's main process
and exits once the child is ready. launchd sees the job's process exit; with `KeepAlive`
it starts *another* copy, which is neither the parent nor the supervised child, and which
tries to bind a port it no longer owns. The library's documented supervisor integration is
systemd-with-`PIDFile` ([doc.go](https://github.com/cloudflare/tableflip/blob/master/doc.go)),
which has no launchd equivalent. Under launchd, the process-supervision and
listener-handoff roles must not both be played by the process — let launchd own the
socket and the lifecycle, and let the process own only the drain. See Cloudflare's own
write-up of the design space at
[blog.cloudflare.com/graceful-upgrades-in-go](https://blog.cloudflare.com/graceful-upgrades-in-go/).

### facebookgo/grace — archived, systemd-compatible env protocol

Now at [`facebookarchive/grace`](https://github.com/facebookarchive/grace) (archived; last
push 2019-02-13). It advertises "socket activation compatibility" and implements it by
literally speaking systemd's protocol — `envCountKey = "LISTEN_FDS"`, with the parent
re-exporting `LISTEN_FDS=<n>` to the child and the child calling
`net.FileListener(file)` — [`gracenet/net.go`](https://github.com/facebookarchive/grace/blob/master/gracenet/net.go).
Because the protocol is just an env var plus inherited fds, it works on macOS too, and it
composes with systemd. The failure mode is stewardship: it is unmaintained and predates
`http.Server.Shutdown`, so graceful drain semantics are its own.

### fvbock/endless — fork/exec + a 60 s guillotine

Same family: `ENDLESS_CONTINUE` / `ENDLESS_SOCKET_ORDER` env vars, `os.NewFile(uintptr(3+ptrOffset), "")`
to adopt fds, `os/exec` to re-exec ([endless.go](https://github.com/fvbock/endless/blob/master/endless.go)).
Signals: `SIGHUP` forks, `SIGINT`/`SIGTERM` shut down gracefully, `SIGUSR2` triggers
`hammerTime`. The named behaviour is the honest part:

> "To deal with hanging requests on the parent after restarting endless will *hammer* the
> parent 60 seconds after receiving the shutdown signal from the forked child process.
> When hammered still running requests get terminated."
> — [endless README](https://github.com/fvbock/endless/blob/master/README.md)

Also documented: "**Limitation: No changing of ports**". For a multi-minute agent turn,
this is the archetype of the wrong tool: the drain window is a constant, and past it the
work is destroyed.

### sozu — the `SO_REUSEPORT` school (not Go)

> "All of the listening TCP sockets are opened with the `SO_REUSEPORT` option, allowing
> multiple process to listen on the same address."
> — [sozu `doc/architecture.md`](https://github.com/sozu-proxy/sozu/blob/main/doc/architecture.md)

sozu is Rust, and its reload therefore relies on the kernel to spread *new* connections
across old and new processes.

### Caddy — the pattern that sidesteps the problem entirely

Caddy applies configuration changes **in-process** through its admin API, so there is no
listener handoff at all: `caddy reload` is documented as "the correct, semantic way to
change/reload the running configuration", and a reload "may reload dependent files like
TLS certificates from disk" without restarting the process
([command-line docs](https://caddyserver.com/docs/command-line)). Its
`caddy upgrade` command is explicit about the limit:

> "Upgrades do not interrupt running servers; currently, the command only replaces the
> binary on disk."
> — [command-line docs](https://caddyserver.com/docs/command-line)

And for long-lived connections it has exactly the two knobs this problem needs:

> "**grace_period** — Defines the grace period for shutting down HTTP servers (i.e. during
> config changes or when Caddy is stopping). During the grace period, no new connections
> are accepted, idle connections are closed, and active connections are impatiently waited
> upon to finish their requests. If clients do not finish their requests within the grace
> period, the server will be forcefully terminated… By default, the grace period is eternal."
>
> "**shutdown_delay** — Defines a duration **before** the grace period during which a
> server that is going to be stopped continues to operate normally, except the
> `{http.shutting_down}` placeholder evaluates to true"
> — [Caddyfile options](https://caddyserver.com/docs/caddyfile/options)

`shutdown_delay` + `{http.shutting_down}` is the "tell the clients early, then drain"
protocol; "grace period is eternal" is the honest default for a box you control.

### `SO_REUSEPORT`, precisely

Linux `socket(7)`: "**SO_REUSEPORT** (since Linux 3.9) Permits multiple `AF_INET` or
`AF_INET6` sockets to be bound to an identical socket address. This option must be set on
each socket (including the first socket) prior to calling `bind(2)` on the socket. To
prevent port hijacking, all of the processes binding to the same address must have the
same effective UID." The kernel then keeps a **reuseport group**, with new sockets
inheriting any attached BPF program, and "When a socket is removed from a reuseport group
(via `close(2)`), the last socket in the group will be moved into the closed socket's
position" ([socket(7)](https://man7.org/linux/man-pages/man7/socket.7.html)).

Selection is per-connection by 4-tuple hash, which means *existing* connections are never
rebalanced: "if a client uses the same socket to send a series of datagrams to the server
port, then those datagrams will all be directed to the same receiving server (as long as
it continues to exist)" ([LWN, "The SO_REUSEPORT socket option"](https://lwn.net/Articles/542629/)).
The same article documents the defect that matters for rolling restarts:

> "If the number of listening sockets bound to a port changes because new servers are
> started or existing servers terminate, it is possible that **incoming connections can be
> dropped during the three-way handshake**… the client connection will be reset."
> — [LWN](https://lwn.net/Articles/542629/)

On macOS/BSD the semantics are stated much more weakly: "**SO_REUSEPORT** allows
completely duplicate bindings by multiple processes if they all set `SO_REUSEPORT` before
binding the port. This option permits multiple instances of a program to each receive
UDP/IP multicast or broadcast datagrams destined for the bound port." (local
`man 2 setsockopt`). Note what it does *not* say: Apple's man page says nothing about
load-balancing TCP connections across the group, and there is no `SO_REUSEPORT_LB`
(FreeBSD 12+ has one). **INFERENCE:** portable code cannot rely on macOS distributing
connections across a `SO_REUSEPORT` group; treat macOS `SO_REUSEPORT` as "duplicate binds
allowed", not as "kernel load balancer".

Bottom line for `SO_REUSEPORT` in this system: it gives you **two separate sockets with
two separate accept queues**. It preserves neither the backlog nor established
connections, it can silently drop connections that are mid-handshake when the group
changes size, and a reconnecting phone can be routed to the process that is about to
exit. It is the right tool for stateless horizontal scale-out; it is the wrong tool for
"keep this one listener's queue alive across a replacement".

---

## 3. Durable execution: making in-flight work outlive its process

### Supervision restarts processes; it does not save work

Erlang/OTP is explicit that a supervisor is a *liveness* mechanism: "A supervisor is
responsible for starting, stopping, and monitoring its child processes. The basic idea of
a supervisor is that it is to keep its child processes alive by restarting them when
necessary", with strategies `one_for_one | one_for_all | rest_for_one` and a bounded
`intensity`/`period` restart budget
([Supervisor Behaviour](https://www.erlang.org/doc/system/sup_princ.html)). A restarted
process comes back with empty state unless state lives elsewhere. The same is true of
systemd `Restart=`/`WatchdogSec=` and of container restart policies: Docker's
`no | on-failure[:max] | always | unless-stopped`
([Docker docs](https://docs.docker.com/engine/containers/start-containers-automatically/))
and Kubernetes' `restartPolicy: Always` plus a 30 s grace term — "A Pod is granted a term
to terminate gracefully, which defaults to 30 seconds"
([Pod Lifecycle](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/)).
Launchd's `KeepAlive`/`ThrottleInterval` belongs on the same list. **Every one of these
restarts the worker; none of them resumes the work.**

### The durable-execution school: journal + deterministic replay

The generalisable design is an append-only journal of *effects*, plus workers that are
disposable because they can rebuild their state by replaying it.

- **Temporal.** Workflow state is an Event History; the worker replays it. The constraint
  is stated as a rule: "Workflow code must be deterministic to support replay. To handle
  non-deterministic operations like API calls, LLM/AI invocations, database queries, and
  other external interactions, put them in Activities. Activities execute outside the
  replay path and are automatically retried so they don't cause non-determinism errors"
  ([Workflow Definition](https://docs.temporal.io/workflow-definition)). For the residual
  non-determinism that must happen *inside* the workflow, the journal records the result
  instead of re-running: "A Side Effect … executes the provided function once and records
  its result into the Workflow Execution Event History. A Side Effect does not re-execute
  upon replay, but instead returns the recorded result. Do not ever have a Side Effect that
  could fail, because failure could result in the Side Effect function executing more than
  once" ([Events and Event History](https://docs.temporal.io/workflow-execution/event)).
- **Restate.** "Restate tracks every step of your code execution in a journal. When you
  call other services, update databases, set timers, or perform any side-effecting
  operation, Restate records both the operation and its result. If your function crashes
  or fails, Restate replays the journal, skipping completed steps and resuming from exactly
  where it left off" ([Key concepts](https://docs.restate.dev/foundations/key-concepts)).
  Restate also publishes the single-writer property as a first-class guarantee: Virtual
  Objects have "Single writer per key (+ concurrent readers)" concurrency
  ([Services](https://docs.restate.dev/foundations/services)).
- **Azure Durable Functions** names the discipline and its sharp edges: "Orchestrator
  functions use event sourcing to ensure reliable execution and to maintain local variable
  state. The replay behavior of orchestrator code creates constraints on the type of code
  you can write… orchestrator functions must be **deterministic**: an orchestrator function
  replays multiple times, and it must produce the same result each time"; and concretely,
  "Don't use `DateTime.Now`, `DateTime.UtcNow`, or equivalent APIs for getting the current
  time" ([Code constraints](https://learn.microsoft.com/en-us/azure/azure-functions/durable/durable-functions-code-constraints)).
- **DBOS** puts the journal in Postgres: "While your application runs, DBOS checkpoints
  those workflows and steps to a Postgres database. When failures occur, whether from
  crashes, interruptions, or restarts, DBOS uses those checkpoints to recover each of your
  workflows from the last completed step" ([Architecture](https://docs.dbos.dev/architecture)).
- **AWS Step Functions** covers the "the work is being done by someone else" case with a
  durable wait: "Wait for a Callback with Task Token — Call a service with a task token and
  have Step Functions wait until that token is returned with a payload"
  ([Service integration patterns](https://docs.aws.amazon.com/step-functions/latest/dg/connect-to-resource.html)).

### The principle, extracted

1. There is **one append-only, ordered log per unit of work**, with monotonically
   increasing sequence numbers. The log is the state; it is written before or atomically
   with the effect it describes.
2. Every process's in-memory state is a **derived cache** that can be rebuilt by replay.
   Losing a process costs latency, not correctness.
3. **Non-determinism is captured at first execution** (Temporal's Side Effect, Azure's
   `CurrentUtcDateTime`) and read back from the log on replay. Determinism constraints
   exist precisely so that replay lands in the same place.
4. **Side effects are recorded, or made idempotent.** Where retry can re-execute, the
   contract is at-least-once plus a dedupe key; "exactly-once" is always
   *at-least-once + idempotent effect + journaled attempt*.
5. **A lease/heartbeat decides when another worker may take over**, and takeover is a
   *new epoch*, not a re-use of the old identity (§5).

Applied here: the DSH session `.jsonl` is already a journal ("append-only facts", per ADR
0007), and the gateway's event bus is a second, weaker one. The bug in the current design
is not the absence of a journal — it is that the process that *owns the turn* is the
process that gets replaced, and the gateway's sequence space is re-minted at zero on every
boot, so the cursor contract cannot distinguish "you are up to date" from "you are from a
previous generation".

---

## 4. Long-lived streaming connections across deploys

### Resume by cursor — Discord is the reference implementation

Discord's model is exactly what the gateway needs, including the failure branch:

> "When your app is disconnected, Discord has a process for reconnecting and resuming,
> which allows your app to **replay any lost events starting from the last sequence number
> it received**. After Resuming, your app will receive the missed events in the same way it
> would have had the connection had stayed active."
> — [Gateway docs](https://discord.com/developers/docs/topics/gateway)

The `Ready` event carries a `resume_gateway_url` to reconnect to; the client then sends
`{"op": 6, "d": {"token": …, "session_id": …, "seq": 1337}}`. The server replays the
buffer and terminates the replay with a `Resumed` event. When the buffer no longer covers
the cursor, the client gets `Invalid Session` (opcode 9) with `d=false`, and must
re-identify and re-fetch state over REST — the **snapshot + delta** path. Two further
details are directly copyable: the session survives a non-graceful TCP close ("If you
simply close the TCP connection or use a different close code, the session will remain
active and timeout after a few minutes"), and a "zombied" connection (heartbeats not
ACKed) must be "immediately terminate[d] … then reconnect and attempt to Resume". All
quotes: [Gateway docs](https://discord.com/developers/docs/topics/gateway).

### Server-Sent Events — the same contract, standardised in the browser

The HTML standard defines the client half of this for free: an `id:` field sets the
EventSource's "last event ID string", and on reconnect "If the `EventSource` object's
last event ID string is not the empty string: … Set (`Last-Event-ID`, lastEventIDValue) in
request's header list", then re-`fetch`. The header "reports an `EventSource` object's last
event ID string to the server when the user agent is to reestablish the connection"
([HTML Standard §9.2.3–9.2.4](https://html.spec.whatwg.org/multipage/server-sent-events.html)).
If the phone PWA ever stops hand-rolling WebSocket resume, `Last-Event-ID` is the
interoperable spelling of "since".

### "The work runs in the background; the stream is a view"

OpenAI's Responses API is the closest commercial analogue to this exact problem — a
multi-minute generation with a client that may drop:

> "You can create a background Response and start streaming events from it right away.
> This may be helpful if you expect the client to drop the stream and want the option of
> picking it back up later. … You will want to keep track of a 'cursor' corresponding to
> the `sequence_number` you receive in each streaming event."
> — [Background mode](https://platform.openai.com/docs/guides/background)

and the resume is literally `GET /v1/responses/resp_123?stream=true&starting_after=42`.
Note the architecture: the *work* is a server-side object with an identity; the *stream*
is a cursor over its event log. That is the shape ADR 0007's `journal.read {from}` is
reaching for.

### Epoch fencing for cursors: "your cursor is from a different generation"

- **Kubernetes** made the stale-cursor case a first-class HTTP status: "Servers are not
  required to serve all older resource versions and may return a HTTP **410 (Gone)** status
  code if a client requests a `resourceVersion` older than the server has retained.
  **Clients must be able to tolerate 410 (Gone) responses.**" The recovery is the canonical
  snapshot+delta: re-LIST to get a fresh snapshot and a new `resourceVersion`, then re-WATCH
  from it. `sendInitialEvents=true` + `allowWatchBookmarks=true` +
  `resourceVersionMatch=NotOlderThan` even folds the snapshot into the watch stream: "the
  API server starts the watch stream with synthetic init events (of type `ADDED`) to build
  the whole state of all existing objects followed by a `BOOKMARK` event … The bookmark
  event includes the resource version to which is synced."
  — [Kubernetes API concepts](https://kubernetes.io/docs/reference/using-api/api-concepts/)
- **Kafka** fences the *writer*, not just the cursor. A consumer instance that has been
  replaced is rejected by the broker: `poll()` may throw `FencedInstanceIdException` "if
  this consumer instance gets fenced by broker", and a lagging consumer that loses its
  group membership gets a `CommitFailedException` on commit, because "This is a safety
  mechanism which guarantees that only active members of the group are able to commit
  offsets. So to stay in the group, you must continue to call `poll`"
  ([KafkaConsumer javadoc](https://kafka.apache.org/38/javadoc/org/apache/kafka/clients/consumer/KafkaConsumer.html)).
  This is the precise precedent for ADR 0007's "a stale gateway that believes it is still
  the active control connection" — Kafka's answer is not "close the old socket", it is
  "the resource refuses the old generation's writes".
- **Redis Streams** implements takeover-by-idleness: `XAUTOCLAIM` with a `min-idle-time`
  reassigns pending entries from a presumably-dead consumer to a new one
  ([XAUTOCLAIM](https://redis.io/docs/latest/commands/xautoclaim/)).
- **NATS JetStream** makes the at-least-once contract explicit: "An acknowledgment
  advances the consumer's cursor — if a message isn't acknowledged in time, the server
  redelivers it, which is what gives you at-least-once delivery"
  ([Acknowledgements](https://docs.nats.io/nats-concepts/jetstream/streams/acknowledgements)).
- **MQTT 5** spells out the session-retention knob and the "did I actually resume?" bit:
  "If the Session Expiry Interval is absent the value 0 is used. If it is set to 0, or is
  absent, the Session ends when the Network Connection is closed. … The Client and Server
  MUST store the Session State after the Network Connection is closed if the Session Expiry
  Interval is greater than 0 [MQTT-3.1.2-23]", and CONNACK carries `Session Present`
  ([MQTT 5.0 §3.1.2.11.2, §3.2.2.1.1](https://docs.oasis-open.org/mqtt/mqtt/v5.0/os/mqtt-v5.0-os.html)).
  `Session Present = false` is MQTT's `resync` frame in one bit.
- **Slack** chooses at-least-once with an explicit dedupe key and an explicit
  decoupling instruction: "Your app should respond to the event request with an HTTP 2xx
  within three seconds. If it does not, we'll consider the event delivery attempt failed.
  After a failure, we'll retry three times, backing off exponentially… With each retry
  attempt, you'll also be given a `x-slack-retry-num` HTTP header"; best practice is
  "Avoid actually processing and reacting to events within the same process. Implement a
  queue to handle inbound events after they are received"
  ([Events API](https://api.slack.com/apis/events-api)).

### Nobody migrates a live connection — they drain and reconnect

- `kubectl logs -f`, the tool everyone actually uses for a long-lived server stream, is
  **not resumable**; the only related flag is `--ignore-errors`: "If watching / following
  pod logs, allow for any errors that occur to be non-fatal"
  ([kubectl logs reference](https://kubernetes.io/docs/reference/kubectl/generated/kubectl_logs/)).
  When the connection breaks the user re-runs the command. That is the floor of the
  industry, and it is worth naming so the gateway's contract can be seen as *above* it.
- Load balancers do draining, not migration: "The load balancer stops routing requests to a
  target as soon as it is deregistered. The target enters the **draining** state until
  in-flight requests have completed"
  ([ALB target groups](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/load-balancer-target-groups.html)).
- Kubernetes bounds the same window with `terminationGracePeriodSeconds` (default 30 s), and
  in-place restart explicitly does *not* honour it: "All running containers in the Pod are
  terminated. The configured `terminationGracePeriodSeconds` is not respected, and any
  configured `preStop` hooks are not executed"
  ([Pod Lifecycle](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/)).
- Caddy's `shutdown_delay` (§2) is the missing piece most systems lack: a window in which
  the server still works normally but can *tell* long-lived clients to reconnect, before
  the drain begins.

The consensus is unambiguous: **you never hand a live TCP connection to another process.
You stop accepting, you tell long-lived clients to reconnect (or let them notice), you
drain what is in flight, you die; the client resumes from a cursor against a log that
outlived you.**

---

## 5. Single-writer / fencing: handing off a lock when the old holder may be alive

### The core argument

Kleppmann's scenario is the whole problem in one page: a client acquires a lease, is
paused (GC, scheduler, `SIGSTOP`, a swapped-out page in EBS), the lease expires, another
client takes it, and the first wakes up still believing it holds the lock. "You cannot fix
this problem by inserting a check on the lock expiry just before writing back to storage.
Remember that GC can pause a running thread at *any point*, including the point that is
maximally inconvenient for you (between the last check and the write operation)." The fix:

> "you need to include a **fencing token** with every write request to the storage service.
> In this context, a fencing token is simply a number that increases (e.g. incremented by
> the lock service) every time a client acquires the lock. … Note this requires the storage
> server to take an active role in checking tokens, and rejecting any writes on which the
> token is stale."
> — [How to do distributed locking](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html)

The load-bearing sentence is the last one: **mutual exclusion is not the guarantee; the
resource rejecting stale tokens is.**

### Chubby: the sequencer

> "At any time, a lock holder may request a **sequencer**, an opaque byte-string that
> describes the state of the lock immediately after acquisition. It contains the name of the
> lock, the mode in which it was acquired (exclusive or shared), and the **lock generation
> number**. The client passes the sequencer to servers (such as file servers) if it expects
> the operation to be protected by the lock. **The recipient server is expected to test
> whether the sequencer is still valid and has the appropriate mode; if not, it should
> reject the request.**"
> — [Chubby, OSDI '06, §2.4](https://www.usenix.org/legacy/events/osdi06/tech/full_papers/burrows/burrows.pdf)

Chubby also documents the fallback for resources that cannot check tokens — and is honest
that it is inferior: if a lock is freed because the holder failed, "the lock server will
prevent other clients from claiming the lock for a period called the **lock-delay**.
Clients may specify any lock-delay up to some bound, currently one minute"; "A lock-delay
may be used with services that cannot check sequencers". Locks are "advisory": "holding a
lock called F neither is necessary to access the file F, nor prevents other clients from
doing so" (same source).

### ZooKeeper and etcd: leases, and a token the storage can check

ZooKeeper's recipes build locks from ephemeral sequential nodes and warn about the
ambiguity that distributed handoff always produces: "When creating a sequential ephemeral
node there is an error case in which the `create()` succeeds on the server but the server
crashes before returning the name of the node to the client. When the client reconnects its
session is still valid and, thus, the node is not removed. The implication is that it is
difficult for the client to know if its node was created or not."
([ZooKeeper Recipes](https://zookeeper.apache.org/doc/current/recipes.html); the zxid/epoch
design is in the [ZooKeeper paper](https://www.usenix.org/legacy/event/atc10/tech/full_papers/Hunt.pdf).)

etcd goes further and hands you the token *and* the check. `LeaseGrant` returns a lease
with a TTL, and "each key may be attached to at most one lease. When a lease expires or is
revoked, all keys attached to that lease will be deleted"
([API reference](https://etcd.io/docs/v3.5/dev-guide/api_concurrency_reference_v3/)).
The election API's `Campaign` returns a `LeaderKey`, and the proto documents the fencing
use explicitly:

```proto
message LeaderKey {
  bytes name = 1;
  bytes key  = 2;  // opaque key representing ownership; if deleted, leadership is lost
  // rev is the creation revision of the key. It can be used to test for ownership
  // of an election during transactions by testing the key's creation revision
  // matches rev.
  int64 rev  = 3;
  int64 lease = 4;
}
```
— [`v3election.proto`](https://github.com/etcd-io/etcd/blob/main/server/etcdserver/api/v3election/v3electionpb/v3election.proto)

`rev` is a fencing token; a compare-and-swap transaction on it is the storage-side check
("only active members … are able to commit", in Kafka's words).

### Kubernetes: the lease record, and planned handoff

A `coordination.k8s.io/v1` Lease carries `holderIdentity` ("contains the identity of the
holder of a current lease"), `acquireTime`, `renewTime`, `leaseDurationSeconds`
("a duration that candidates for a lease need to wait to force acquire it. This is measured
against the time of last observed `renewTime`"), and `leaseTransitions` ("the number of
transitions of a lease between holders") — i.e. **owner identity plus a monotonic
transition counter**, updated with optimistic concurrency on `resourceVersion`
([Lease API reference](https://kubernetes.io/docs/reference/kubernetes-api/cluster-resources/lease-v1/)).
The same object now models *graceful* handoff instead of disruption:

> "`preferredHolder` signals to a lease holder that the lease has a more optimal holder and
> should be given up. This field can only be set if `Strategy` is also set."
> — [Lease API reference](https://kubernetes.io/docs/reference/kubernetes-api/cluster-resources/lease-v1/)

with `LeaseCandidate` and a selection strategy (`OldestEmulationVersion`) used to
"deterministically select a leader via coordinated leader election"
([Coordinated Leader Election](https://kubernetes.io/docs/concepts/cluster-administration/coordinated-leader-election/)).
This is the closest published model to "the operator is redeploying; please step down" —
which is strictly better than "the new process arrives and the old one finds out later".

### OS-level locks: `flock(2)` on the actual target platform

Local `man 2 flock` (macOS) is emphatic that the lock belongs to the *open file
description*, not the process:

> "Locks are on files, not file descriptors. That is, file descriptors duplicated through
> `dup(2)` or `fork(2)` do not result in multiple instances of a lock, but rather multiple
> references to a single lock. If a process holding a lock on a file forks and the child
> explicitly unlocks the file, **the parent will lose its lock**."

and Linux states the same model: "Locks created by `flock()` are associated with an open
file description… this lock may be modified or released using any of these file
descriptors. Furthermore, the lock is released either by an explicit `LOCK_UN` operation on
any of these duplicate file descriptors, or **when all such file descriptors have been
closed**" ([flock(2)](https://man7.org/linux/man-pages/man2/flock.2.html)). Consequences
for this system, all of which matter:

- If the gateway takes the flock and ever hands that fd to the child (e.g. via
  `exec.Cmd.ExtraFiles`), the lock is now shared: the child's exit can release it while the
  gateway still believes it holds it, and vice versa. **The process that writes the session
  should be the process that holds the lock.** ADR 0007's model — DSH takes the lock at
  `session/new`/`session/resume` — is the correct side of this line, and the gateway should
  not hold a copy.
- A `SIGKILL`ed holder releases the lock (all its fds close), so "a dead holder never blocks
  a successor" is right. But a **`SIGSTOP`ped or deadlocked holder keeps it forever** —
  `flock` has no expiry. That is exactly Kleppmann's paused-client case, and it is why the
  control-connection handoff must fence by epoch rather than rely on the lock.
- `flock` over network filesystems is a different animal: Linux emulates it over NFS as
  `fcntl` byte-range locks on the whole file since 2.6.12, so "`fcntl(2)` and `flock()`
  locks **do** interact with one another over NFS. It also means that in order to place an
  exclusive lock, the file must be opened for writing", and there is a `local_lock` mount
  option that makes locks node-local ([flock(2)](https://man7.org/linux/man-pages/man2/flock.2.html)).
  If the session directory is ever on iCloud Drive / a network home, the single-writer
  guarantee evaporates. (`fcntl` POSIX record locks are worse still: they are dropped when
  the process closes *any* fd to the file.)

### Making sure the old holder is actually dead

Fencing is the second line of defence; killing the old holder is the first.

- **launchd already does it for you, by default** — "When a job dies, launchd kills any
  remaining processes with the same process group ID as the job" (§1). So today, when the
  gateway job dies, its ACP child dies with it. In the two-tier design this becomes a
  hazard *and* a tool: the host must be a separate job so it is *not* killed, and the
  gateway's own children are cleaned up for free.
- macOS has no `prctl(PR_SET_PDEATHSIG)`. The portable substitute is the one this system
  already has: the ACP child is bound to its client's stdin, and stdin EOF is its shutdown
  signal (ADR 0007). Where a pipe is not available, `kqueue` gives a direct process-death
  notification: `EVFILT_PROC` with `NOTE_EXIT` — "The process has exited"
  ([kqueue(2)](https://keith.github.io/xcode-man-pages/kqueue.2.html), local `man 2 kqueue`).
- HDFS takes the aggressive route and then documents why the gentle route is insufficient:
  "when using the Quorum Journal Manager, **only one NameNode will ever be allowed to write
  to the JournalNodes**, so there is no potential for corrupting the file system metadata
  from a split-brain scenario", with `dfs.ha.fencing.methods` ("a list of scripts or Java
  classes which will be used to fence the Active NameNode during a failover") for the
  remaining cases ([HDFS HA with QJM](https://hadoop.apache.org/docs/stable/hadoop-project-dist/hadoop-hdfs/HDFSHighAvailabilityWithQJM.html)).
  The lesson: put the fence at the *log* (the JournalNodes reject the old writer), and use
  killing only as belt-and-braces.
- Cloud object stores expose exactly this primitive, if state ever moves off-box: GCS
  `ifGenerationMatch` / the `x-goog-if-generation-match` header — "Request proceeds if the
  **generation** of the target resource matches the value used in the precondition. If the
  values don't match, the request fails with a `412 Precondition Failed` response"
  ([Request preconditions](https://cloud.google.com/storage/docs/request-preconditions)) —
  and S3 conditional writes via `If-None-Match` / `If-Match`
  ([S3 conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html)).
  A "generation" in GCS *is* a fencing token.

---

## Synthesis: what the literature says is the correct shape for this problem

1. **Separate the lifetime of the listener from the lifetime of the work — and make the
   long-lived tier a peer job, not a child.** Every system that survives a redeploy with
   work in flight either keeps the resource in a supervisor (launchd/systemd own the
   socket; §1) or keeps the work in a process that the deploy does not touch (Temporal
   workers, Restate invocations, DBOS "no separate orchestration server", §3). On macOS the
   critical corollary is `AbandonProcessGroup`: a `dsh-agent-host` spawned as a child of the
   gateway's job is killed by launchd the moment the gateway dies, so it must have its own
   label. ([launchd.plist(5)](https://github.com/apple-oss-distributions/launchd/blob/main/man/launchd.plist.5))
2. **Never try to migrate a connection; make the cursor durable instead.** Established
   connections die with their process under every mechanism surveyed; only the *listening
   socket and its unaccepted backlog* can be preserved, and only by a supervisor or by
   fd inheritance (launchd `Sockets` → `launch_activate_socket(3)`; tableflip's
   `StartProcess` fd passing). The industry answer is reconnect-and-resume from a cursor
   over a log that outlives the process: Discord's `seq` + replay buffer with an
   `Invalid Session` escape hatch, SSE's `Last-Event-ID`, OpenAI's `sequence_number` +
   `starting_after`, Kubernetes' `resourceVersion` + `410 Gone` → re-LIST.
   ([Discord](https://discord.com/developers/docs/topics/gateway),
   [SSE](https://html.spec.whatwg.org/multipage/server-sent-events.html),
   [OpenAI](https://platform.openai.com/docs/guides/background),
   [K8s](https://kubernetes.io/docs/reference/using-api/api-concepts/))
3. **A cursor must carry its generation, and every invalid cursor must resolve to exactly
   one answer: snapshot + deltas.** Discord's `resume_gateway_url`/`session_id` pair,
   MQTT's `Session Present`, and Kubernetes' `410` all encode "this cursor belongs to a
   sequence space". The failure in ADR 0007 defect 2 — `since=900` against a fresh
   `Bus.seq=0` producing neither replay nor `resync`, leaving a healthy-looking frozen UI —
   is precisely the bug class these designs exist to eliminate. The literature's rule is
   that there is no fourth case in which a stale resume "succeeds".
   ([Discord](https://discord.com/developers/docs/topics/gateway), [MQTT 5](https://docs.oasis-open.org/mqtt/mqtt/v5.0/os/mqtt-v5.0-os.html), [K8s](https://kubernetes.io/docs/reference/using-api/api-concepts/))
4. **Fence the writer at the resource, not at the lock.** Mutual exclusion is not a
   guarantee: `flock` has no expiry and a `SIGSTOP`ped holder keeps it forever, exactly
   Kleppmann's paused client. What makes takeover safe is a monotonically increasing
   generation that the *consumer of the writes* checks and rejects: Chubby's sequencer,
   etcd's `LeaderKey.rev` CAS, GCS's `ifGenerationMatch`, Kafka's group generation
   (`FencedInstanceIdException`, "only active members of the group are able to commit
   offsets"). "Newest control connection pre-empts the old one" is necessary but not
   sufficient — a stale connection's already-queued message can arrive after the
   pre-emption, so the host must stamp an epoch on every request and refuse anything
   below the current one. ([Kleppmann](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html),
   [Chubby](https://www.usenix.org/legacy/events/osdi06/tech/full_papers/burrows/burrows.pdf),
   [etcd](https://github.com/etcd-io/etcd/blob/main/server/etcdserver/api/v3election/v3electionpb/v3election.proto),
   [Kafka](https://kafka.apache.org/38/javadoc/org/apache/kafka/clients/consumer/KafkaConsumer.html))
5. **Make the journal the source of truth and the process a cache; then a deploy is a
   latency event, not a correctness event.** Temporal (event history + deterministic
   replay, side effects recorded), Restate (journal replay "skipping completed steps"),
   DBOS (checkpoints in Postgres, recover from the last completed step), and Azure Durable
   Functions (event sourcing with explicit determinism constraints) all reach the same
   place: the unit of work has an identity and a log; workers are disposable; side effects
   are either journaled so replay skips them, or idempotent under an at-least-once retry.
   The corollary for deploy mechanics is Caddy's `shutdown_delay`: give long-lived clients a
   window in which the server still works but says "I am going away", then drain —
   and never let a constant like `shutdownTimeout=20s` be the thing that decides whether
   twelve minutes of work survives.
   ([Temporal](https://docs.temporal.io/workflow-definition),
   [Restate](https://docs.restate.dev/foundations/key-concepts),
   [DBOS](https://docs.dbos.dev/architecture),
   [Azure](https://learn.microsoft.com/en-us/azure/azure-functions/durable/durable-functions-code-constraints),
   [Caddy](https://caddyserver.com/docs/caddyfile/options))

### Three places the literature refines ADR 0007 as written

- **Pre-emption by "newer connection wins" needs an epoch, and the epoch should be
  durable.** The ADR says a newer control connection pre-empts an older one and argues
  this is safe because a stale gateway can only answer an approval. Add the fencing: the
  host mints `controlEpoch` from a monotonically increasing value persisted in the state
  directory, returns it in `host.hello`, and rejects any request carrying a lower epoch —
  so a message that was in flight when the pre-emption happened cannot execute. This is
  Kafka's generation check and etcd's `rev` CAS, applied to your socket.
- **`hostEpoch` minted randomly is weaker than an epoch minted monotonically.**
  "A cursor from another generation … produce[s] a `resync`" works with a random boot id
  because the client only needs *inequality*. But if you later want "the newest gateway
  wins" to be decidable rather than heuristic (e.g. after a crash-loop, or when a stale
  gateway reconnects with a valid token), a persisted counter is the primitive the
  literature uses (Chubby's "lock generation number", Kubernetes' `leaseTransitions`).
- **If you ever want the listener itself to survive, the whole deploy shape changes.**
  `Sockets` + `launch_activate_socket(3)` keeps the socket and its backlog alive across a
  restart, but only if the service is restarted with `kickstart -k`; `bootout` +
  `bootstrap` removes and recreates the service and therefore the socket, which is exactly
  what `deploy/mac/install.sh` does today. Reaching the API from Go needs cgo (no
  pure-Go library exists), and `tableflip` is not a substitute under launchd because the
  library's parent-exits-after-handoff model fights launchd's supervision. The ADR's
  conclusion — accept ~1 s of listener downtime and let the resume contract hide it — is
  the right trade; the research just pins down *why* it is the right trade rather than a
  limitation being tolerated.
