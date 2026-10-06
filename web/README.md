# dsh-gateway web console

The mobile-first PWA that drives a desktop DeepSeek Harness session from a
phone browser. It speaks the frozen contract in [`../docs/api.md`](../docs/api.md)
and nothing else.

## Why there is no bundler

The design forbids runtime dependencies, and a bundler is the dependency you
cannot see: once one exists, every source file implicitly depends on its loader
semantics, its chunking and its dev server. This app is small enough that the
platform alone is sufficient, so the toolchain is **one devDependency
(`typescript`)** and the output is the input, file for file.

What follows from that, concretely:

- **Explicit `.js` extensions on every relative import.** `tsc` is a
  transpiler here, not a resolver: it never rewrites a specifier, so
  `import { api } from "./api.js"` is what the browser actually requests.
  `tsconfig.json` uses `module: ES2022` + `moduleResolution: Bundler` with
  `verbatimModuleSyntax`, which permits the extensions without inventing any.
- **Static assets are copied, not hashed.** `npm run build` runs `tsc`, and
  npm's `prebuild` hook copies `index.html`, `src/styles.css` and `public/*`
  into `dist/`. `dist/` is never committed: the Go server embeds it, so every
  path that compiles the gateway builds it first.
- **No dead-code elimination.** Every module in `dist/` is fetched, so the
  modules are kept few and their imports acyclic. A stray `console.log` costs
  bytes; a stray module costs a round trip.
- **No transpilation below ES2022.** The browser floor, stated concretely rather
  than as a vibe, is the newest of these per feature:

  | Feature | Used for | Shipped in |
  |---|---|---|
  | `dvh` | the keyboard-safe layout | Chrome 108, Safari 15.4, Firefox 101 |
  | top-level `await` in modules | module init | Chrome 89, Safari 15, Firefox 89 |
  | private class fields | encapsulation | Chrome 74, Safari 14.1, Firefox 90 |
  | `field-sizing` | the self-growing composer | **Chrome 123 (2024), Safari 18.4** |

  So the real floor is set by `field-sizing`, and the honest statement is
  **Chrome/Edge 123+, Safari/iOS 18.4+, Firefox 125+** for the full experience.
  Older browsers are not blocked: the composer falls back to a fixed `rows`, and
  everything else was already available. The app is exercised against Chrome and
  Safari only — see `scripts/e2e-browser.mjs`.

## Layout

```
web/
  index.html            shell template, carries {{NONCE}} placeholders
  tsconfig.json         strict; see "Type strictness" below
  scripts/
    copy-public.mjs     prebuild: index.html + styles.css + public/ -> dist/
    dev-server.mjs      optional; serves dist/ with the real CSP and a nonce
  public/               copied verbatim into dist/
    manifest.webmanifest
    sw.js               service worker (app shell only)
    icon.svg            maskable-safe variants live here too
    icon-maskable.svg
  src/
    app.ts              router, shell, bootstrap, service-worker registration
    actions.ts          controller: user intent -> API call -> store commit
    api.ts              the only file that calls fetch
    events.ts           the WebSocket client (backoff, heartbeat, subscribe)
    store.ts            the reactive store + the app state shape
    decode.ts           runtime narrowing for everything off the wire
    types.ts            hand-written mirror of docs/api.md
    feed.ts             folds transcript pages and live frames into one list,
                        and turns that list into rows (grouping runs of tools)
    activity.ts         what has finished, kept on the device: who settled, how
                        it went, what it amounted to
    settlement.ts       reads the harness's own account of a delegated task,
                        which is prose because ACP has no subagent scope
    rows.ts             keyed row reconciler: mounts once, updates in place
    copybutton.ts       the copy control, shared by code blocks and tool cards
    markdown/
      parse.ts          a message -> a tree of blocks and spans; no imports, no DOM
      render.ts         that tree -> DOM nodes, and the only place elements are built
    clipboard.ts        copy, with a fallback for origins without the API
    dom.ts              DOM builder, focus trap, keyboard/scroll helpers
    format.ts           pure presentation helpers
    styles.css          all styling; no inline styles anywhere
    tools/              everything that reads a tool call
      args.ts           tolerant access to an argument payload
      diff.ts           the change an edit or write made, and its rendering
      risk.ts           "looks destructive", and why
      present.ts        per-tool presenter registry -> one view model
      card.ts           the transcript's tool card: mount, update, keep time
    views/
      pair.ts           screen 1
      sessions.ts       screen 2
      conversation.ts   screen 3
      feedrows.ts       row kinds -> nodes (message, tool, tool run, notice)
      approval.ts       screen 4 (mounted globally, not inside a view)
      settings.ts       screen 5
      activity.ts       screen 6, reached from the app bar's bell
      ui.ts             shared badge / bottom sheet / select
  test/                 node --test against dist/; no browser, no framework
    markdown.test.js    the grammar, asserted against the tree
    markdown-render.test.js  the tree, asserted against the DOM
    support/dom.js      the shim that throws on innerHTML; not collected as a test
  dist/                 build output, generated and gitignored; this is what Go embeds
```

## Build

```sh
cd web
npm ci                 # typescript only, exactly the version package-lock.json pins
npm run build          # prebuild copies assets, then tsc -> dist/
npm test               # prebuild + build, then node --test (see below)
npm run typecheck      # tsc --noEmit
npm run watch          # tsc --watch (re-run `npm run assets` after CSS/HTML edits)
npm run serve          # optional dev server on 127.0.0.1:8099
```

### Tests

`npm test` is `node --test`, with no runner and no DOM library. It reads `dist/`
rather than `src/`, so it asserts what the browser is actually served, and its
`pretest` hook builds first so the command is correct from a clean checkout.

That the tests need no DOM environment is the reason `markdown/` is two files.
`markdown/parse.ts` has no imports at all, so the whole grammar — headings,
tables, nested lists, the emphasis flanking rules, the scheme allow-list, the
nesting caps — is testable from plain Node. `markdown/render.ts` is then a
translation with nothing to decide, and `test/support/dom.js` is a DOM small
enough to fail on the things the renderer must not do: it implements only the
surface the renderer is allowed to touch and **throws** from `innerHTML`,
`outerHTML` and `insertAdjacentHTML`, and it records every tag created so a test
can hold the renderer to a fixed set of elements. The safety argument in
`render.ts` is executable rather than asserted.

What this tier cannot see is layout and CSS. `make e2e` is the tier for that.

`npm ci` rather than `npm install`: the lockfile is committed and resolves from
`registry.npmjs.org`, so a build here uses the same tarball everywhere.
`npm install` would rewrite the lock whenever it disagreed with `package.json`,
which turns a dependency bump into an unreviewed diff — and a build on a machine
configured against a different registry into a different lock than the one in
the tree.

`npm run serve` is a development convenience, not part of the product. It
substitutes the nonce and sends the same `Content-Security-Policy` as the Go
middleware, so a `style` attribute or a `style.setProperty` added by mistake
fails locally instead of in production. It answers `/api/**` with `501`, so the
API needs the real gateway.

## How nonce injection works

The Go middleware (`internal/httpcore/middleware.go`) generates a fresh nonce
per response and sends:

```
default-src 'self'; script-src 'self' 'nonce-<N>'; style-src 'self' 'nonce-<N>';
img-src 'self' data: blob:; font-src 'self'; connect-src 'self';
frame-ancestors 'none'; base-uri 'none'; object-src 'none'
```

`web/index.html` is a **template**, not a finished document. It contains the
literal token `{{NONCE}}` in exactly four places — two `<meta name="theme-color">`
tags do not need it, but these do:

| Element | Why it needs the nonce |
|---|---|
| `<link rel="stylesheet" href="./styles.css">` | `'self'` already allows it; the nonce keeps the rule uniform |
| inline `<style>` (critical first-paint rules) | `style-src` has no `'unsafe-inline'`, so this is the only reason a nonce exists at all |
| `<script type="module" src="./app.js">` | same reasoning as the stylesheet |

The Go handler must replace every `{{NONCE}}` with the same nonce it put in the
header, **for the same response**. The token appears nowhere else in the app, so
a template that forgets a substitution is caught by the browser rather than
silently degrading.

### What the CSP forbids, and what replaced it

`style-src` has a nonce and no `'unsafe-inline'`, which in a CSP3 browser blocks
both the `style="…"` attribute **and** CSSOM writes such as
`element.style.setProperty()`. There is therefore no dynamic geometry in this
codebase. Every case that would normally reach for an inline style has a
structural equivalent:

| Normally | Here |
|---|---|
| `style="width: 62%"` progress bar | `<progress max value>` |
| `style="display:none"` | the `hidden` attribute + `[hidden]{display:none!important}` |
| `style="height: 84px"` growing textarea | `field-sizing: content`, with `rows` as the CSP-safe fallback |
| `style="transform: translateY(-56px)"` pull-to-refresh | a class on the scroll container, discrete positions |
| `element.style.setProperty('--vh', …)` keyboard inset | `interactive-widget=resizes-content` in the viewport meta, plus a `visualViewport` → `window.scrollBy` fallback in `dom.ts` |
| `style="open: true"` disclosure | `<details open>` |

Views toggle classes and attributes; they never touch `.style`.

## Behaviour worth knowing

**Realtime.** One socket to `/api/v1/events`. The last `seq` is remembered and
replayed via `?since=`; backoff is equal-jitter, 500 ms doubling to a 10 s cap;
`offline` pauses and `online` resumes with the backoff reset; a `ping` goes out
every 20 s. `subscribe`/`unsubscribe` are sent on session open/close and
re-sent after every reconnect.

**`resync` and `hello`** both trigger a refetch of approvals, sessions and — if
a conversation is open — its metadata and transcript. `hello` counts because a
fresh connection may replay from a sequence the client never had.

**Leases.** Sending a prompt attaches, so the app never leases on open. While a
session is leased the conversation's header carries a "Held" chip explaining that
the desktop cannot open it, with a one-tap release behind it.

**Whose turn it is.** A turn this phone holds shows Stop beside Send rather than
instead of it: a prompt sent while a turn is running is *queued*, and the answer
says where it stands. The queue strip above the composer lists what is waiting
and lets one be dropped without stopping the turn. `session.promptQueueDepth: 0`
restores the older behaviour, where Send is disabled mid-turn and a prompt sent
anyway is refused with `prompt_in_flight` — the client reads the depth from
`GET /me` rather than assuming either.

The live signal is `turn.state`, and the client keeps two things from it: the
turn in flight and the queue behind it. A `queued` frame adds to the queue, a
`running` frame promotes one out of it, and a settled frame clears whichever
ticket it names — so a stale `completed` for an older turn cannot blank a newer
one. When the client holds no turn frame at all — a page loaded mid-turn — the
session resource's own `turn` and `queue` decide, which is why they are on it:
the event that announced the turn may be long past the replay window (see
judgement call 13). A session another writer holds keeps Send disabled and says
so instead.

**Approvals** are mounted on `document.body`, outside `#app`, so they are
answerable from every screen; while one is pending the rest of the shell is
`inert`.

### What the Markdown renderer draws

`markdown/parse.ts` decides what a model's answer contains and `markdown/render.ts`
draws it: ATX headings, fenced code, blockquotes, ordered/bullet/task lists, pipe
tables, thematic breaks and paragraphs, plus inline code, bold, italic,
strikethrough and links. That is the whole list, and it is a subset of CommonMark
rather than a small parser for it — `#` needs a space, tables need a delimiter
row, and a list item's continuation lines have to be indented.

Three omissions are decisions rather than gaps:

- **Raw HTML is text.** `<script>` reaches the screen as those eight characters,
  because the renderer creates elements itself and never calls `innerHTML`. The
  invariant is worth more than the markup: there is no sanitizer to get wrong
  because there is nothing to sanitize.
- **Images do not load.** A remote image is a beacon — the host learns the
  reader's address every time the transcript is opened — and `PRIVACY.md`
  undertakes to list every third party a deployment involves. `![alt](url)`
  draws its alt text as a link instead, which is honest about what was there.
- **Setext headings, indented code blocks, reference links, footnotes and lazy
  continuation lines are literal text.** Each is rare in what a model writes,
  and each costs a branch that has to stay right forever.

Headings render one level below what was written: the conversation view already
owns the page's `h1`, so a message's `#` opens at `h2`. Block nesting stops at
`MAX_BLOCK_DEPTH` and inline nesting at `MAX_INLINE_DEPTH`, so a pathological
`>>>>…` costs a bad render rather than the stack.

## Type strictness

`strict`, `noUncheckedIndexedAccess`, `exactOptionalPropertyTypes`,
`verbatimModuleSyntax`, `noUnusedLocals`, `noImplicitReturns`. There is no
`any`, no `@ts-ignore` and no cast of untrusted data: `decode.ts` converts every
response and every WebSocket frame into a fully-populated domain object or
`null`, so the rest of the app never sees a maybe-shaped value.

## Judgement calls

Points where the contract left a choice, or where the app makes an assumption
the server side should confirm:

1. **Health endpoint path.** The contract's base path is `/api/v1`, but its
   health table lists `/healthz` and `/readyz` without the prefix. `api.health()`
   tries `/healthz` first and falls back to `/api/v1/healthz`; `api.ready()`
   uses `/api/v1/readyz`, matching the base-path convention. The router has since
   landed and the server serves both spellings deliberately; the fallback stays
   as compatibility, not as a stopgap.
2. **`limits.maxPromptBytes` is not exposed by any endpoint.** The composer
   enforces 256 KiB, the shipped default from `internal/config`. The server
   stays authoritative: an over-limit prompt returns a problem document that the
   error strip shows verbatim. Consider adding `limits` to `GET /me` or a
   `GET /limits`.
3. **Transcript/live overlap.** Opening a session fetches history and subscribes
   at the same moment, so an assistant message can arrive twice. The fold drops
   a live message whose `data.id` is already present, and otherwise drops one
   that repeats the immediately preceding row verbatim. **Sending `data.id` on
   `session.message` makes this exact instead of heuristic.**
4. **Sign out is `DELETE /devices/{me.id}`.** There is no sign-out endpoint and
   the cookie is `HttpOnly`, so the contract's "revoking the calling device
   takes effect immediately" is the only available mechanism.
5. **Session pagination** stores `nextCursor` from the previous response and
   passes it back as `cursor`. If the server does not return `nextCursor`, the
   "Load older sessions" button never appears — degraded, not broken.
6. **Pairing QR.** `routeFromHash` parses a query string after the path, so
   `#/pair?code=K7M2QPX4` prefills the code field (normalised through the same
   alphabet as typed input) and focuses the device-name field. It is never
   auto-submitted: opening a link must not spend a one-time code or a
   rate-limit slot. `hashFor` always emits a bare `#/pair`, so navigating away
   and back cannot re-inject a spent code. This assumes the link is
   `<origin>/m/#/pair?code=…` — the fragment is client-side, so the `/m/` prefix
   only affects where `dist/` is mounted, and every asset reference is relative.
7. **Mount path and `sw.js` scope.** `web/embed.go` serves `dist/` at `/m/`, and
   every asset reference in `index.html` is document-relative (`./app.js`), so
   the prefix costs nothing. The service worker lands at `/m/sw.js` and its
   scope is therefore `/m/`, which covers the whole shell; its precache list is
   relative for the same reason. Note that the Go handler deliberately answers
   404 for unknown paths instead of falling back to the shell — correct here,
   because routing is entirely in the URL **fragment**, which never reaches the
   server. Moving to history-based routing would require adding that fallback.
8. **Cached shell vs. per-response nonce.** Navigations are network-first, so
   the served shell normally carries the current nonce. The cached copy is an
   offline fallback only; its stale nonce costs the inline critical CSS and
   nothing else, because `script-src 'self'` already permits the external
   module.
9. **Icons.** The contract's CSP allows `img-src 'self'`, so icons are local SVG
   (authored here, `any` and `maskable`). There is no PNG, so iOS will not use
   the SVG for `apple-touch-icon` and falls back to a screenshot; add raster
   icons if that matters.
10. **`approval.options` fallback.** Emphasis is derived from the option id
    (`allow*` primary, `reject*`/`deny*` danger). If a frame arrives with no
    options, the documented `allow-once`/`reject-once` pair is rendered so a
    blocking request is never unanswerable.
11. **`turn.state: failed`** appends a notice row carrying `stopReason`; the
    other terminal states are silent, since the transcript and the tool cards
    already say what happened.
12. **Offline is never faked.** When the socket drops, the header pill says so
    and the session list keeps its last data rather than clearing.
13. **No snapshot of the turn in flight.** `turn.state: running` is published
    when a prompt is admitted, and a client that arrives later never sees it: a
    fresh socket resumes "from now", and replay only covers a sequence number the
    client already holds. So a page loaded mid-turn has no turn frame at all, and
    the composer falls back to the session's `busy` flag — reported by
    `GET /sessions/{id}` and already rendered as "running" in the list. The same
    fallback carries a `resync`: that frame is dropped and the session refetched,
    because the stream may have missed the `completed` that ended the turn. A
    `turn.state` frame on subscribe, or `turn` in the session view, would make
    that restore exact instead of snapshot-based.
