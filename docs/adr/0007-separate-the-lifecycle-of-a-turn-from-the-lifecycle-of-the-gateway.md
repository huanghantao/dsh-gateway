# 7. Separate the lifetime of a turn from the lifetime of the gateway

Date: 2026-10-03
Status: Accepted

> The mechanisms below were checked against the installed DSH 0.1.7-rc.2 packages
> and against `launchd.plist(5)` on this machine; the prior art these decisions
> lean on is collected, with primary sources, in
> [`docs/research-redeploy-survival.md`](../research-redeploy-survival.md).

## Context

Redeploying the gateway kills the conversations running on the phone. The
operator's report is "the sessions sent from the phone kill it", and the
mechanism is worth writing down precisely, because three separate defects
combine into one symptom.

**1. Everything that a turn needs lives in one process.** `cmd/dsh-gateway`
owns the HTTP listener, the auth surface, the event bus, the lease manager, the
turn scheduler, the approval broker, *and* the ACP child that actually runs the
agent (`internal/harness/acp`). A deploy is a `launchctl bootout` followed by a
`bootstrap` (`deploy/mac/install.sh`), so the whole graph dies at once:

- the WebSocket to the phone drops;
- `leases.ReleaseAll` closes every attached session;
- `adapter.Close` closes the child's stdin, which is DSH's orderly-shutdown
  signal, and escalates to `SIGKILL` after `dsh.stopTimeout`;
- the in-flight turn is cancelled inside the agent, and the model response it was
  waiting for is gone.

DSH *binds* an ACP child to its client's stdin, so this is not something the
gateway can opt out of. `dsh-acp-app/lib/index.js:34` calls
`exitOnStdinEnd(ctx, "acp-app.stdin")` when the profile is invoked, and
`dsh-cmdline/lib/index.js:50-72` implements it as "stdin EOF requests the
launcher's bounded successful shutdown". The bounded part is real and matters:
`lib/profile-boot-*.js` arms `PROCESS_SHUTDOWN_TIMEOUT_MS = 5000` before
disposing, so DSH has five seconds to cancel the turn, quiesce, and flush before
it is force-exited. The lifetime of the agent is therefore, by construction, the
lifetime of whatever holds the other end of that pipe. Today that is the gateway,
so restarting the gateway restarts the work.

What follows is that *no* amount of in-process cleverness fixes this. A child
whose client dies always dies; the only fix is for the client of the child to be
a process that a deploy does not replace.

Two properties of DSH bound how bad this is, and both were established by
reading the installed 0.1.7-rc.2 packages rather than assumed:

- **The loss is bounded to the in-flight model response.** Session progress is
  appended incrementally, drained within ~200ms, and flushed at explicit barriers
  before every model request, before every top-level tool dispatch, and at every
  pre-step. A kill loses the current step's response, not the turn's history.
- **The next process recovers the session automatically, and honestly.** Re-opening
  an interrupted log appends synthetic closers: `tool/result` with
  `ToolOutcomeUnknownError` for a call that was already durable, then `step/end`,
  then `turn/end {reason: "interrupted"}`. ACP reports that as a `cancelled` turn.

So the floor is already high: a crash yields a truthful transcript and a
resumable session, not a corrupted one. What it does not yield is the work — the
turn ends, and the resumed session must be prompted again. (That `turn/end`
behaviour also means a host restart is a destructive operation that must be
drained, not a free one; see "Deploy protocol".)



**2. A client cannot tell that the sequence space restarted, so it goes deaf
without an error.** `Bus.seq` is an `atomic.Uint64` that starts at zero on every
process start, and the API tells clients to resume with the last `seq` they saw
(`docs/api.md`, "Event stream"). A phone that was on `seq: 900` when the gateway
restarted reconnects with `since=900`. In `Bus.Subscribe`, the empty-ring case
computes `replayMissing = since < b.seq.Load()` — `900 < 0` is false — so no
replay and no `resync` frame are produced (`internal/app/events/bus.go:166-176`).
The client's `#lastSeq` stays at 900 while the new process emits 1, 2, 3, so
`decoded.seq > this.#lastSeq` is never true and every frame is dropped
(`web/src/events.ts:185`). The socket is open and the heartbeat succeeds, so the
UI reports "connected" while the conversation is frozen: the one recovery path
the client has, `onStale`, only fires when a reconnect has no resume point at all
(`web/src/events.ts:166`).

**3. Shutdown is not a deploy protocol, and the deploy does not wait for it.**
`serve` shuts down in a deliberate order (`cmd/dsh-gateway/gateway.go:648-666`)
but nothing in it distinguishes "no turns are running" from "a turn has been
running for twelve minutes". `cfg.Limits.ShutdownTimeout` defaults to 20s, so a
long turn is killed at second 20. Worse, the deploy is racing that shutdown: the
installer's `bootstrap_agent` boots the job out and polls `launchctl print` for
up to about 10s (`deploy/mac/install.sh:796-840`) before bootstrapping the new
plist. A graceful stop can outlast that window, and the new binary then starts
while the old one still holds `127.0.0.1:8787` — a bind failure, a
`KeepAlive` restart on a 10s throttle, and a phone that is down for a reason
with nothing to do with its own session.

There is also a hard floor that the shutdown budget must respect: DSH's own
5s `PROCESS_SHUTDOWN_TIMEOUT_MS` window, before which it is force-exited with its
final flush incomplete (`dsh.stopTimeout` is the gateway's half of that
handshake).


Taken together, the gateway's lifetime *is* the turn's lifetime, and there is no
handoff protocol at the boundary. That is the architectural defect; the three
symptoms above are its consequences.

## Decision

Give a turn a lifetime that does not depend on the process that serves the
phone, by introducing one long-lived tier between the harness and the gateway.

```
CLIENT TIER      phone PWA · installed PWA                 (owns: its cursor)
      │  HTTPS + WebSocket, cursor = (generation, seq)
GATEWAY TIER     dsh-gateway                                (replaceable, seconds)
      │  HTTP, auth, devices, approvals + grants, event fan-out, static bundle
      │  control socket: <stateDir>/agent-host.sock
AGENT HOST TIER  dsh-agent-host                             (stable, weeks)
      │  the ACP child · session leases + flock · turn scheduler + queue
      │  approval relay · durable event journal + snapshot
HARNESS TIER     dsh --profile acp                           (per session, live)
      │
DURABLE STATE    $DSH_HOME/sessions/**.jsonl.zstd            (append-only facts)
                 <stateDir>/journal/**.ndjson                (gateway's view)
```

The gateway keeps everything that is a *decision about a person*: devices,
cookies, CSRF, rate limits, approval grants, the push service, the web app. The
agent host keeps everything that is a *fact about work in progress*: the child
process, the sessions it holds, the turn and its queue, and the journal that
makes those facts replayable. Neither tier may reach into the other's state
directly; the socket is the only seam, and it carries a cursor.

A redeploy then replaces one stateless tier while the tier that holds the work
does not move.

### Constraints this has to respect

These are properties of the installed DSH, not choices, and each one rules out a
plausible-looking shortcut:

- **One live writer per session, for the session's whole lifetime.**
  `<session-dir>/session.lock` is a non-blocking `flock(2)` taken at
  `session/new` or `session/resume` and released only when the handle closes.
  So two gateway instances cannot share a session, and neither can a gateway and
  the desktop. The tier that holds the session is therefore the *only* tier that
  can talk about it.
- **No multi-client attach and no way to observe a session you do not own.** ACP
  registers no `session/load` and no subscribe method; the only channel is
  `session/update` on the owning connection. "The phone watches while the desk
  drives" stays what it is today: the read-only log projection
  (`internal/sessionwatch`), not a second writer.
- **No daemon, no control socket, nothing to build on.** DSH's only listener is
  the web profile's HTTP server. The socket in this design is ours to define.
- **A dead holder never blocks a successor.** The kernel drops the flock on any
  process death, including `SIGKILL`, and there is deliberately no lock expiry.
  A successor can always resume; it just gets DSH's `interrupted` closers.
- **Recovery is not resumption of the work.** After an interruption the turn is
  over (`turn/end {reason: "interrupted"}`, surfaced as `cancelled`). Nothing in
  DSH re-drives it. If the operator wants the work, they prompt again — which is
  exactly why the tier that holds a *live* turn must not be the tier being
  deployed.

Two further constraints are macOS deployment facts rather than DSH facts, and
the first of them would silently defeat this whole design if it were missed:

- **`AbandonProcessGroup` defaults to false, and launchd kills the process
  group.** From `launchd.plist(5)`: "When a job dies, launchd kills any remaining
  processes with the same process group ID as the job." So an agent host spawned
  as a *child of the gateway's job* is killed on every redeploy, and the two-tier
  design would look like it works while not working. **The host must be its own
  launchd job with its own label**, installed alongside the gateway's.
- **The listener's fate depends on how the job is restarted.** launchd creates and
  owns the socket when a job declares `Sockets`, so the listening socket and its
  unaccepted backlog survive while the *service definition stays loaded* —
  `launchctl kickstart -k`, the restart path for a redeploy — but not
  `bootout` + `bootstrap`, which is what `deploy/mac/install.sh` does today and
  which removes and recreates the service. Reaching the fd from Go needs
  `launch_activate_socket(3)` and therefore cgo, so the design does not depend on
  it; it is a cheaper alternative worth taking only if cgo is acceptable.


### The interface between the tiers

Newline-delimited JSON-RPC over a Unix domain socket, mirroring the shape the ACP
adapter already implements, so `internal/harness/acp` moves into the host almost
unchanged and a new `internal/harness/hostclient` implements the same
`harness.Harness` port that the rest of the gateway is already written against
(`internal/harness/harness.go`). The seam does not move; only its implementation
does.

- `host.hello {protocol, gatewayBootId}` → `{protocol, hostEpoch, hostBootId, seq}`
  — negotiation, and the identity of the current sequence space.
- `host.snapshot` → `{sessions[], turns[], pendingApprovals[], seq}` — the state a
  fresh gateway needs, answered from live memory rather than by replaying
  anything. This is the frame that makes a gateway restart cheap.
- `session.{list,new,resume,close,prompt,cancel,setConfigOption,claim,release}`
  — the existing turn scheduler and lease manager, moved.
- `journal.read {from}` → events — the retained journal, so a gateway that
  restarts can resume rather than resync. Optional for correctness, valuable for
  latency.
- `event` notifications host→gateway: `harness.state`, `session.*`, `turn.*`,
  `approval.requested`, `approval.resolved`, `usage.*`.
- `approval.answer {id, optionId}` gateway→host.

Three rules make the seam safe:

- **The host never decides.** It relays `approval.requested` with a deadline and
  blocks the agent's permission callback on the answer, exactly as
  `internal/app/approvals` does today. Policy stays in the gateway, which is the
  tier authenticated to a human.
- **The host owns identity.** `turnId` is minted by the host and is unique within
  a `hostEpoch`, because a turn can now outlive the gateway that admitted it.
  (`turns.go:57` already documents the current limitation: "identifies this turn
  within the gateway's lifetime".)
- **The approval relay is restartable, and stays fail-closed.** The host journals
  `approval.requested` and keeps the agent's permission callback blocked until an
  answer arrives or the deadline passes. A gateway that reconnects rebuilds its
  pending map from the snapshot and can still answer a prompt that arrived while
  it was gone; with no gateway connected, the deadline expires and the tool is
  refused, exactly as a timeout does today (`internal/app/approvals`). Grants
  still live only in the gateway and still die with it, so ADR 5's rule — "a rule
  that outlived a restart would be one nobody remembers agreeing to" — is
  untouched, and the host has no path by which it could decide anything itself.

### The resume contract, restated

The wire contract gains one word: a cursor is `(generation, seq)`, never a bare
`seq`.

- The `hello` frame carries `generation` (a random boot id) and the gateway's
  `seq` watermark.
- A client resumes with `since` **and** the generation it saw. A cursor from
  another generation, or ahead of this process's watermark, or older than the
  retained buffer, all produce the same answer: a `resync` with a `reason`, then
  the state the client should render. There is no fourth case, and in particular
  no case in which a resume silently produces a working-looking connection.
- A `resync` is defined as *snapshot + deltas*: the gateway sends the current
  turn, queue, pending approvals and harness state for the subscribed sessions,
  then streams changes. The client's existing recovery — refetch transcript,
  approvals and session list on `resync` (`web/src/actions.ts:804`) — is already
  correct; it simply never fires today.
- Heartbeats stay: they are what tells a phone its radio died, and they are also
  why defect 2 is silent.

### The deploy protocol

The installer already knows which tier changed; it should act on it.

1. Build to a staging path, verify `--version` and `/healthz`.
2. **Gateway changed:** `SIGTERM` the old gateway. It enters *draining*, in the
   shape Caddy's `shutdown_delay` describes: first a short window in which it
   still works normally but says so (`gateway.draining` on the event stream, and
   a `shutting_down` flag on `/readyz`), so long-lived clients can reconnect
   before the old listener goes away rather than discovering it by failure; then
   stop admitting turns and attachments, give an in-flight turn its window, end
   the event streams with a "gateway restarting" close reason, and stop. No
   session is released; the host still holds them.
3. Wait for the pid to exit (bounded, and longer than the gateway's own
   `ShutdownTimeout`), *then* bootstrap the new plist, then poll `/readyz` until
   the harness handshake completes. The new gateway dials the existing host,
   reads the snapshot, and republishes state to connected clients.
4. **Host changed:** this is the only restart that can end a turn. Drain first —
   refuse new turns, let running ones finish within a bounded window — and refuse
   to restart while turns are running unless `--force` is given.
5. A single-instance guard (an advisory lock on the state directory, plus
   `ExitTimeOut` in the plist) replaces "hope the old one is gone".

macOS has no supported `launch_activate_socket(3)` route for a Go program
without cgo, so the listener is not handed across the restart. That is
acceptable, and it is worth being exact about why: a process death is a *fast
failure* for every client that was talking to it. Measured on this machine, a
connection established to a dying listener gets a FIN/RST immediately, a
connection still sitting in the accept queue gets `ECONNRESET`, and a new
connection while nothing is listening gets `ECONNREFUSED` — immediately, not
after a timeout. So the cost of the gap is one failed request and a WebSocket
close, both of which are already handled: the client reconnects with backoff and
resumes from its cursor, and the REST layer needs only a short bounded retry on
`ECONNRESET`/`ECONNREFUSED` to make the gap invisible. The listener was never
the fragile part; the cursor is.

## Consequences

- A gateway redeploy no longer interrupts a turn, stops a conversation, or
  silently deafens a client. Deploying the web app or an HTTP-layer change does
  not touch the tier that holds the work.
- There is a second process and a control protocol to operate: `host.status`,
  a log file, and a supervised restart path. The host must survive the gateway's
  absence — a session with a running turn stays held, and an idle one is released
  on the existing idle timer, so the desktop can still take a session back.
  That timer now carries more weight than it did: a host that held sessions
  forever would lock the desktop out of conversations the phone merely looked at,
  which is the exact failure ADR 3 exists to prevent. Only the *process* holding
  the lock changes; the policy does not.
- The gateway's log watcher already excludes sessions a writer holds
  (`internal/sessionwatch/watcher.go:267`), so the host holding a session does
  not double-report, but it does mean live events for a held session now travel
  over the socket. A host that is down degrades to the transcript projection plus
  a truthful `harness.state: failed`, not to a silently frozen screen.
- A new failure mode appears and must be fenced: a stale gateway that believes it
  is still the active control connection. The host accepts one control
  connection, and a newer connection pre-empts an older one — but pre-emption
  alone is not enough, because a request already queued on the old connection can
  arrive after the swap. The host therefore stamps a **control epoch** on
  `host.hello` and refuses any request carrying a lower one. Mutual exclusion is
  never the guarantee; the resource refusing the stale writer is (Kleppmann,
  Chubby's sequencer, etcd's `LeaderKey.rev`, Kafka's generation fencing). The
  epoch is monotonic within a host process and need not survive a host restart —
  a restarted host has no continuity to hand over — so an in-memory counter is
  the correct scope, and only the wire shape has to be right from the start.
- This is the largest structural change since ADR 1, and it is worth doing
  incrementally. The phases below are separable, and the first one is worth
  shipping on its own.

## Rollout

**Done: Phases 0 and 1.**

Phase 0 is in, and it is the half that fixes the incident rather than the
architecture: a cursor is `(generation, seq)` and every unusable one resolves to
a `resync` plus a `snapshot`; `GET /events` names its generation in `hello`;
`readyz` reports draining as not-ready; `SIGTERM` drains, refuses new work and
tells connected clients; the web client echoes its generation, applies
snapshots, and retries a read that failed because the process it was talking to
was replaced. The regression tests are in `internal/app/events/bus_test.go`
(the four ways a resume can fail) and `internal/httpapi/v1/handlers_events_test.go`
(over a real WebSocket, including the stale cursor across a restart that started
all of this).

Phase 1 is in as the agent host: `internal/agenthost` holds the child, the
sessions and the turns; `hostwire` is the protocol; `hostlink` owns the socket
and its permissions; `cmd/dsh-agent-host` is the service; the gateway drives it
through the same `harness.Harness` port it always had, chosen by `dsh.mode`. The
installer installs both binaries as two peer launchd jobs and now *waits* for a
job to stop before starting its replacement, which is what stops a redeploy from
SIGKILLing the process it just asked to drain. `make e2e-host` exercises the
deployment surface against the real binaries.

The rollout that follows is the record of what was intended, and of the two
things Phase 1 deliberately did not do: a durable journal, and reattaching a
turn that outlived its gateway to the turn *scheduler* rather than merely
observing it. Both are described in Phase 2 below.

**And one thing that was removed after it shipped.** Phase 1 landed with a
`dsh.mode` switch, so a deployment could still run the child inside the gateway.
It has since been deleted, along with the code that could: the gateway no longer
links `internal/harness/acp` at all, because that package now lives at
`internal/agenthost/acp` and is imported only by the host binary. The reasoning is
worth recording because "keep the old path as a fallback" sounds prudent and is
not. The fallback was a switch back to the behaviour this ADR exists to
eliminate, reachable at the worst possible moment — an operator whose host will
not start, at 2am, whose quickest fix would have been to tell the gateway to run
the agent itself and quietly start losing turns again. It could not even have
worked as a rescue: DSH's single-writer lock belongs to the host, so a second
writer cannot take over a session regardless. What replaces it is a refusal that
names the fix, and a validation error for a config that still asks for the old
mode.

**Phase 0 — make the failure honest and the redeploy polite (no new tier).**

1. `Bus.Subscribe` treats a cursor ahead of its watermark as a missing replay
   (`since > b.seq.Load()` ⇒ `replayMissing`), so a restart produces `resync`
   rather than a deaf but healthy-looking stream. Test: restart the bus and
   resume with a stale high `since`.
2. `hello` carries `generation`; the client echoes it on reconnect and treats a
   change as `onStale`. Defensive, because a page that survived the restart is
   rare, but it is the same fence the two-tier design needs.
3. A `snapshot` frame — turn, queue, pending approvals, harness state, derived
   from the existing `turns.Queue` and approval list — is sent after `resync` and
   on demand, so a reconnecting client renders the truth without a refetch
   round trip.
4. `SIGTERM` enters draining: publish `gateway.draining`, refuse new turns and
   new attachments, give an in-flight turn a bounded window to settle (a
   configurable `deployDrainTimeout`, minutes rather than `ShutdownTimeout`'s
   seconds), then end the event streams with a "gateway restarting" close reason
   and stop. Keep `shutdownTimeout` as the ceiling on handing off, and let the
   drain proceed even when the ceiling is hit — a killed turn is reported
   honestly, which is the point of this phase, not a sin.
5. The installer waits for the old pid to exit, boots the new plist only then,
   and retries the bind; the plist gains `ExitTimeOut` and a single-instance
   lock. The web client adds a short bounded retry on `ECONNRESET`/`ECONNREFUSED`
   and a `gateway.draining` notice, so a redeploy that lands mid-request costs a
   retry rather than an error.

Phase 0 removes the silent deafness and, on a deploy that can afford to wait,
the loss; what it cannot do is protect a turn that outlives the deploy window.

**Phase 1 — the agent host tier.** Extract `internal/harness/acp`, the lease
manager, and the turn scheduler behind the socket, with `host.snapshot` and the
journal. Verify on the real deployment that (a) a turn started before
`launchctl kickstart -k` of the gateway finishes after it, and (b) a phone that
was watching it loses no rows.

**Phase 2 — durability and identity.** Journal-first turn admission with
client-supplied idempotency keys, turn ids scoped to a host epoch, journal
rotation with retention, and the atomic binary swap so a crash mid-deploy cannot
leave two versions installed. The end-to-end test for both phases needs a stub
harness that can run a long turn without a model call — `internal/harness/acptest`
is the empty directory that should hold it — because a restart-survival property
that is only tested against a paid model call will not be tested.

