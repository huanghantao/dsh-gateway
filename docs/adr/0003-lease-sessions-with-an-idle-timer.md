# 3. Lease sessions with an idle timer, not a client reference count

Date: 2026-09-30
Status: Accepted

## Context

DSH enforces one live writer per session with a non-blocking `flock(2)` on the
session's lock file. A session attached to the gateway's ACP child cannot be
opened on the desktop, and vice versa.

If the gateway simply attached sessions and held them, opening your laptop would
report "already open elsewhere" for a conversation you last touched on your phone
hours ago.

## Decision

Hold a session on a short, self-renewing idle timer:

- Attaching refreshes the lease. So does any client activity — a WebSocket
  subscribe, a heartbeat, a metadata or transcript fetch.
- An in-flight turn pins the lease regardless of activity.
- An explicit `pinned` flag keeps a lease alive indefinitely for an operator
  working entirely from the phone.
- Everything else is released after `session.idleTimeout` (default 5 minutes).

## Rationale

Liveness is expressed as a last-used timestamp rather than a subscriber or
connection count because a phone that walks into a lift never sends a clean
goodbye. A reference count would leak the lease permanently, and the failure mode
— the desktop silently refusing to open a session — is exactly the confusing
behaviour this design exists to prevent.

The release path is also what frees DSH's lock, so a leaked lease is not merely
untidy; it blocks a real workflow.

## Consequences

- The API must report lease state, so the app can explain *why* the desktop
  refuses to open something.
- Releasing is checked under the same lock that marks a session busy, so a turn
  that starts between the scan and the release is safely skipped rather than
  abandoned.
- `DELETE /sessions/{id}/lease?force=true` can release a busy session. That
  abandons the in-flight turn, so the app warns before offering it.
