## What this changes, and why

<!--
The subject line of your commit is the most important part of this pull request.
The history is written as "what changed, because why it matters" — see
CONTRIBUTING.md for the style. A one-line summary here is fine; the reasoning
belongs in the commit body.
-->

## Why it is not a smaller change

<!--
Optional, but the most useful thing you can write. If you considered a simpler
approach and rejected it, say so and say why — that is exactly the reasoning the
existing history preserves, and it is what a future reader will want.
-->

## How it was checked

<!-- Tick what you actually ran. "I did not run it" is an acceptable answer; a
     ticked box that was not run is not. -->

- [ ] `make check` (gofmt, tsc, vet, tests)
- [ ] `make lint` (golangci-lint — CI fails on findings and the tree is clean)
- [ ] `make build`
- [ ] `make test-race`
- [ ] `shellcheck --severity=warning deploy/vps/install.sh deploy/mac/install.sh` (installer changes only)
- [ ] `make e2e` against a running gateway (web/src changes only; not in CI)
- [ ] `make test-llm` (only if the change needs a real model turn — **this costs money**)

## Security review

<!--
The approval path, authentication, the workspace allowlist, the session-log
parser and the proxy all fail loudly rather than quietly, and a change to any of
them needs an explicit statement here.
-->

- [ ] This change does not touch authentication, approvals, the workspace allowlist, the session-log parser, or the proxy.
- [ ] **Or** it does, and I have said below what the failure mode is and how it fails closed.

<!-- If it touches those: what happens when the new code takes the wrong branch?
     Which security invariant in docs/security.md does it rely on or alter? -->

## Documentation

- [ ] Operator-visible behaviour is unchanged.
- [ ] **Or** `README.md` / `docs/` have been updated to match.

<!--
docs/security.md makes specific claims about what is stored, logged and sent. If
this change makes one of those claims false, updating that document is part of
the change, not a follow-up.
-->

## Related

<!-- Issues this closes, or context a reviewer needs. The repository does not use
     "Closes #12" trailers in commit messages; mention it here instead. -->
