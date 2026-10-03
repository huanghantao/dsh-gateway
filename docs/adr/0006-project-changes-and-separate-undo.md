# 6. Project "what changed" from the log, and make undo a separate port

Date: 2026-10-03
Status: Accepted

## Context

The receipt answers "what did this session cost" — tokens, tools, duration, and a
list of file *names*. It does not answer the question an operator actually has
when they pick the phone back up: **what did it change, and how do I put it
back?**

Every ingredient is in the session log. DSH records each `edit` as the exact
`old_string` and `new_string` it replaced, and each `write` as the content it
wrote. That is a hunk, or a whole file, with the file's path attached.

The obvious alternative — diff the workspace against git, or against a snapshot
— was rejected. The gateway is deliberately read-only over the workspace: it is
the property that lets the whole security story be "an attacker who finds this
URL gets nothing they cannot already do". A feature that needs the gateway to
read every file it is asked about, or to keep a shadow copy of the tree, spends
that property on a convenience.

## Decision

**Projection.** `internal/sessionlog/changes.go` folds a session log into a list
of files the agent changed. For each file: the paths, how many edits, added and
removed line counts, and the hunks themselves, in the order they were applied.
It reads only the log. It never touches the workspace, and it says so in the
type: `Changes` is derived from what the agent recorded, so it is complete for
the session and silent about anything done outside it.

The projection is honest about what it cannot know. A `write` to a file whose
previous content never appears in the log is reported as a whole-file write with
`added` lines and unknown removals, not as a diff against content it would have
to invent.

**Undo.** Reverting is a different capability with a different blast radius, so
it is a different package behind a different port: `internal/workspace` owns
every write the gateway makes to an operator's files, and nothing else in the
tree may open a file for writing.

Revert walks the projected changes backwards and applies each in reverse: an
`edit` swaps `new_string` back to `old_string`, a `write` restores the previous
content when the log recorded it. It is exact-match or it refuses: a hunk whose
`new_string` is not present exactly once means the file has moved on, and the
answer is to say which file and stop, never to guess with a fuzzy match.

It is **off by default** (`changes.revert.enabled`). A deployment that never
turns it on has no code path that writes to a workspace at all, which is the
posture the rest of the design is built around. When it is on, it is bounded by
the same allowlist as every other path, it refuses any file outside it, and the
whole operation is one audit event per file.

## Rationale

Splitting projection from mutation is what keeps the common case free. Reading
what changed is a phone-shaped question asked constantly; writing to a
repository is a desktop-shaped action taken rarely. Bundling them would mean
every deployment carries the write path in order to get the read path.

Deriving from the log rather than the filesystem also makes the answer *stable*.
A file the operator has since edited by hand still shows what the agent did to
it, which is exactly the record someone reviewing a session wants — a live diff
would silently rewrite history as the tree moved on.

## Consequences

- `GET /sessions/{id}/changes` is read-only and always available while the log
  is readable; `POST /sessions/{id}/revert` answers `503` with
  `revert_disabled` when the deployment has not opted in.
- The app shows the same diff rendering in three places — a transcript tool card,
  the approval sheet, and the changes screen — so the interpretation of a hunk
  lives in one client-side module.
- A revert that only partly applies reports per file: reverted, skipped with a
  reason, or refused. There is no transaction, because a half-reverted tree with
  a clear report is more useful than a rollback that hides which files it
  touched.
