# Contributing

Thanks for looking. This is a small project with an unusually sharp edge — it
puts a shell on the internet — so the notes below are mostly about the things
that will bite you on your first change.

## Getting set up

You need Go 1.25+, and Node 22+ with npm. You do **not** need DeepSeek Harness
installed: the integration tests skip themselves when `dsh` is not on `PATH`, and
the one test that calls a real model is opt-in.

```sh
git clone https://github.com/huanghantao/dsh-gateway
cd dsh-gateway
make build
```

**The frontend is built first, and this is load-bearing.** `web/dist` is
generated and gitignored, and the Go binary embeds it via `//go:embed`. A bare
`go build ./...` in a fresh clone therefore fails with `pattern dist: no matching
files found`. That failure is deliberate — a binary that serves a blank page is
harder to diagnose than a build that stops — but it means every Go command needs
`make web` to have run at least once. The Makefile targets all depend on it, so
use them:

```sh
make web     # just the frontend
make build   # web, then the binary
```

The same ordering is why `go install github.com/huanghantao/dsh-gateway/cmd/dsh-gateway@latest`
cannot work: the module proxy has no `web/dist` to embed. Build from a checkout.

## What CI runs

`make check` is the fast local loop:

```sh
make check   # gofmt -w, tsc --noEmit, go vet, go test
make lint    # golangci-lint (install it first; the tree is lint-clean)
```

CI runs a superset, and it is worth knowing which parts are not in `make check`:

| Job | What it does | In `make check`? |
|---|---|---|
| test | `go test -race ./...` on Ubuntu **and** macOS | no `-race`, one platform |
| lint | `gofmt -l` (fails, does not fix), `tsc --noEmit`, golangci-lint | gofmt fixes instead; no lint |
| build | `make build` and uploads the binary | no |
| shellcheck | `shellcheck --severity=warning` on both installers, plus an argument-parser test | no |

So before opening a pull request, run `make lint` and `make build` too. macOS is
in the matrix because the desktop side is macOS-only and the process supervision
and file locking behave differently there; if you only have Linux, say so in the
PR and it is fine.

Both installers are shellchecked. If you touch `deploy/*/install.sh`, run
`shellcheck --severity=warning` on it locally — a `case` branch that forgets
`shift 2` does not fail loudly, it spins forever on the same argument.

## Tests

Three tiers, and the reason each exists is in the [README](README.md#development):

- **Unit** — no external dependencies, run everywhere.
- **Integration** — drive a real `dsh --profile acp` child. Skipped automatically
  when `dsh` is missing.
- **Opt-in** — `make test-llm` makes a real model call and **costs money**.
- **Browser** — `make e2e` drives the real app in real Chrome against a running
  gateway. This one is not in CI; it needs a gateway you started yourself.

There is **no frontend unit test suite**, and the README used to imply otherwise.
`web/` has no `test` script and never has. The frontend's verification is `tsc`
plus the browser tier, so a change to `web/src` that is not exercised by
`make e2e` is a change nothing checks automatically. If you are touching the
session screen, run `make e2e`.

## Commit messages

The history is written in a specific style, and matching it matters more than
usual here because the messages are where the design reasoning lives — there is
no separate design doc for most decisions.

The style is:

- **Imperative mood, sentence case.** "Let Copy report what happened", not
  "Fixed copy" and not "fix: copy".
- **No type or scope prefix.** Not Conventional Commits. No `feat:`, no `fix(ui):`.
- **No issue references and no trailers** — no `Co-authored-by`, no
  `Signed-off-by`, no `Closes #12`.
- **A long subject, 60–90 characters**, of the form *what changed, because why it
  matters*:

  ```
  Let Copy report what happened, because a tap that copies nothing says nothing
  Read a log by what was appended, because a live session is folded every second
  Keep a fingerprint for every log, because a forgotten one is decoded every sweep
  ```

- **A substantial body** when the change had a non-obvious alternative. Explain
  the failure mode you found, what you rejected and why. Several existing commits
  do this at length, and it is the most valuable thing in the repository.

Write the subject so that `git log --oneline` reads as a list of decisions rather
than a list of files.

## Security-sensitive changes

Anything touching authentication, the approval path, the workspace allowlist, the
session-log parser or the proxy needs a careful look, and the reasoning belongs
in the commit body. Two rules that are not negotiable in review:

- **Approvals fail closed.** An unanswered approval is refused, never allowed.
  Nothing is ever auto-approved.
- **A security invariant is enforced, not documented.** If a value would weaken
  one — a non-loopback bind, a plaintext webhook, a workspace root that is the
  whole home directory — it is rejected at startup or at install time rather than
  warned about.

If you are reporting a vulnerability rather than fixing one, **do not open a
public issue**: see [SECURITY.md](SECURITY.md).

## Conventions worth knowing

- **Comments explain *why*.** The codebase is heavily commented, and almost
  always with the reason a decision was made and what was rejected — not with a
  restatement of what the line does. A comment that says `// increment i` would
  be deleted in review; a comment that says why the counter is not reset would
  not.
- **Errors name the fix.** `die`/`fail` messages should tell the operator what to
  do, not just what is wrong.
- **The docs are part of the change.** If behaviour an operator can see changes,
  `docs/` and `README.md` change with it. `docs/security.md` in particular makes
  claims about what is and is not stored; if the code stops matching it, the doc
  is the bug.
- **Line endings are LF** and the installers are `bash`. `.gitattributes` handles
  this, but do not fight it.

## Licence

Contributions are accepted under the MIT licence in [LICENSE](LICENSE). There is
no CLA and no DCO requirement — by opening a pull request you are agreeing that
your contribution may be distributed under that licence.
