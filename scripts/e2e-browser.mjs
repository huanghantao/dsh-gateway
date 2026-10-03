#!/usr/bin/env node
/*
 * End-to-end test of the real mobile app against a real, running gateway.
 *
 * Why this exists, and why it is not a unit test:
 *
 * There is no frontend unit test suite: `tsc` proves the types line up and
 * nothing proves the app works. The Go tests exercise the API directly, which is
 * the other side of the same boundary. Neither crosses where the two meet — and
 * every bug this script has caught so far lived exactly there:
 *
 *   1. `GET /me` returned deviceId/deviceName/pairedAt while `GET /devices`
 *      returned id/name/createdAt. The app decoded both with one function, so
 *      pairing succeeded and the *next* request failed with "the server returned
 *      an unexpected principal".
 *   2. A phone that had never paired got a 401 from `/me`, which paused the event
 *      stream; the post-pairing bootstrap then called `connect()`, which returns
 *      immediately when paused. The result was a working session list and a
 *      permanently dead live feed — visible only on a genuinely unpaired device,
 *      which is precisely what a mock never provides.
 *   3. "Copy session id" called `navigator.clipboard?.writeText(id).then(…)`, and
 *      an optional chain short-circuits everything after it: where the property
 *      is absent — any plain-HTTP origin, since the clipboard only exists in a
 *      secure context — the tap copied nothing and said nothing. A refused write
 *      was no better: it printed the bare id where a status message belongs.
 *      Both failures are invisible without a real browser and a real clipboard.
 *
 * So this drives real Chrome at a phone viewport, against the real binary, with
 * a real pairing code, doing a real model turn.
 *
 * Usage:
 *   node scripts/e2e-browser.mjs [--base URL] [--chrome PATH] [--keep-shots DIR]
 *                                [--insecure] [--viewport 390x844]
 *
 * --insecure tells Chrome to accept the certificate without verifying it, which
 * is needed when testing a deployment whose CA has not been installed in this
 * browser — a fresh profile has not. It is for that case only: the trust chain
 * itself is better checked with `curl --cacert <ca.crt>`, which fails loudly if
 * the chain is wrong, whereas this flag would hide it.
 *
 * It exits non-zero on the first failure and prints what it saw. Screenshots for
 * each step are written to a temporary directory and reported, so a failure can
 * be looked at rather than guessed at.
 */

import { spawn, execSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

// --- arguments --------------------------------------------------------------

const argv = process.argv.slice(2);
const arg = (name, fallback) => {
  const i = argv.indexOf(`--${name}`);
  return i >= 0 && argv[i + 1] ? argv[i + 1] : fallback;
};

const BASE = arg('base', 'http://127.0.0.1:8787');
const CHROME = arg('chrome', defaultChromePath());
const SHOTS = arg('keep-shots', fs.mkdtempSync(path.join(os.tmpdir(), 'dsh-e2e-')));
const DEBUG_PORT = Number(arg('port', '9333'));
const INSECURE = argv.includes('--insecure');
// The phone this drives. 390x844 is an iPhone 12/13/14-class logical viewport —
// a sensible default and an easy thing to over-fit to, which is why a narrower
// Android-class size is checked alongside it below. Override with
// --viewport 360x800.
const VIEWPORT = arg('viewport', '390x844');
const [VIEWPORT_W, VIEWPORT_H] = VIEWPORT.split('x').map(Number);
if (!Number.isFinite(VIEWPORT_W) || !Number.isFinite(VIEWPORT_H)) {
  throw new Error(`--viewport must look like 390x844, got ${JSON.stringify(VIEWPORT)}`);
}
// A second width to prove the layout is not tuned to one device. 360 is the
// most common Android logical width and the narrowest phone worth supporting.
const NARROW = { width: 360, height: 800 };
// A desktop-side DSH, for the step that watches a session the gateway is not
// driving. `dsh headless` is one turn and exits, which is exactly the shape a
// test needs.
const DSH = arg('dsh', 'dsh');

/** Finds a Chrome-family browser. */
function defaultChromePath() {
  const candidates = [
    '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    '/Applications/Chromium.app/Contents/MacOS/Chromium',
    '/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge',
    '/usr/bin/google-chrome',
    '/usr/bin/google-chrome-stable',
    '/usr/bin/chromium',
    '/usr/bin/chromium-browser',
    '/snap/bin/chromium',
    'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
    'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
  ];
  for (const c of candidates) if (fs.existsSync(c)) return c;
  // Also try PATH, which is how a Chrome installed outside a package manager
  // (a CI runner's preinstalled browser, nix, a homebrew cask on Linux) shows up.
  for (const name of ['google-chrome', 'google-chrome-stable', 'chromium', 'chromium-browser', 'chrome']) {
    const found = whichSync(name);
    if (found !== null) return found;
  }
  throw new Error(
    'no Chrome-family browser found; pass --chrome /path/to/chrome\n' +
      'tried:\n  ' + candidates.join('\n  ') + '\n  and on PATH: google-chrome, chromium, chrome',
  );
}

/** whichSync is `which`, without depending on a shell being present. */
function whichSync(name) {
  for (const dir of (process.env.PATH ?? '').split(path.delimiter)) {
    if (dir === '') continue;
    const candidate = path.join(dir, name);
    try {
      fs.accessSync(candidate, fs.constants.X_OK);
      return candidate;
    } catch {
      // Not here; keep looking.
    }
  }
  return null;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const step = (n, s) => console.log(`${String(n).padStart(2)}. ${s}`);
const note = (s) => console.log(`    ${s}`);

// --- a minimal CDP client ---------------------------------------------------

let chrome;
let ws;
let nextId = 0;
const pending = new Map();
const consoleErrors = [];
const wsTrace = [];

function send(method, params = {}, timeoutMs = 90000) {
  const id = ++nextId;
  ws.send(JSON.stringify({ id, method, params }));
  return new Promise((resolve, reject) => {
    // The timer is cleared as soon as the reply lands. Without that, every call
    // leaves a live timer behind, and a run that finished successfully would sit
    // there for up to timeoutMs afterwards looking like a hang.
    const timer = setTimeout(() => {
      if (pending.delete(id)) reject(new Error(`CDP timeout: ${method}`));
    }, timeoutMs);
    pending.set(id, {
      resolve: (value) => {
        clearTimeout(timer);
        resolve(value);
      },
      reject: (err) => {
        clearTimeout(timer);
        reject(err);
      },
    });
  });
}

async function evaluate(expression) {
  const r = await send('Runtime.evaluate', {
    expression,
    returnByValue: true,
    awaitPromise: true,
  });
  if (r.exceptionDetails) {
    throw new Error(
      'page error: ' +
        (r.exceptionDetails.exception?.description ?? JSON.stringify(r.exceptionDetails)),
    );
  }
  return r.result.value;
}

async function waitFor(label, expression, timeoutMs = 25000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await evaluate(expression)) return;
    await sleep(250);
  }
  throw new Error(`timed out waiting for ${label}`);
}

/**
 * A tap that goes through the input pipeline.
 *
 * `element.click()` is everywhere else in this script and is fine there, but it
 * is not a user gesture, and Chrome gates the Clipboard API on one: a synthetic
 * click makes `writeText` reject for reasons that have nothing to do with the
 * app. Anything that copies has to be tapped the way a finger would.
 */
async function tap(selector) {
  const box = await evaluate(`(() => {
    const node = [...document.querySelectorAll(${JSON.stringify(selector)})].find((n) => n.offsetParent !== null);
    if (!node) throw new Error('no visible element for ' + ${JSON.stringify(selector)});
    node.scrollIntoView({ block: 'center' });
    const r = node.getBoundingClientRect();
    return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
  })()`);
  for (const type of ['mousePressed', 'mouseReleased']) {
    await send('Input.dispatchMouseEvent', {
      type,
      x: box.x,
      y: box.y,
      button: 'left',
      buttons: type === 'mousePressed' ? 1 : 0,
      clickCount: 1,
    });
  }
  await sleep(300);
}

async function screenshot(name) {
  const r = await send('Page.captureScreenshot', { format: 'png' });
  const file = path.join(SHOTS, `${name}.png`);
  fs.writeFileSync(file, Buffer.from(r.data, 'base64'));
  note(`screenshot: ${file}`);
  return file;
}

function cleanup() {
  try { ws?.close(); } catch {}
  try { chrome?.kill(); } catch {}
  // The profile is a throwaway directory; leaving it behind would accumulate a
  // full browser profile per run.
  try { fs.rmSync(PROFILE, { recursive: true, force: true }); } catch {}
}

/**
 * Runs once the test has settled. Closing the socket and killing Chrome is what
 * lets the event loop drain; the timer is the backstop for when they do not —
 * a CDP socket that has not fully closed, or a browser that ignores SIGTERM,
 * would otherwise keep node alive after the verdict was already printed. It is
 * unref'd, so a clean shutdown still exits immediately, and the five seconds are
 * only ever spent before forcing an exit that has already been decided.
 */
function finish() {
  cleanup();
  setTimeout(() => process.exit(process.exitCode ?? 0), 5000).unref();
}

const PROFILE = fs.mkdtempSync(path.join(os.tmpdir(), 'dsh-e2e-profile-'));
process.on('exit', cleanup);
process.on('SIGINT', () => { cleanup(); process.exit(130); });

// --- the test ---------------------------------------------------------------

async function main() {
  // A pairing code from the same binary the gateway is running, so the derived
  // code matches the one the server will accept.
  const stateDir = arg('state-dir', path.join(os.homedir(), '.dsh-gateway'));
  const binary = arg('binary', path.join(os.homedir(), '.local', 'bin', 'dsh-gateway'));
  const raw = execSync(`"${binary}" pair -state-dir "${stateDir}" -json`, {
    stdio: ['ignore', 'pipe', 'pipe'],
  }).toString().trim();
  const { code } = JSON.parse(raw);
  step(1, `pairing code from the live gateway: ${code}`);

  chrome = spawn(
    CHROME,
    [
      '--headless=new',
      `--remote-debugging-port=${DEBUG_PORT}`,
      `--user-data-dir=${PROFILE}`,
      '--no-first-run',
      '--no-default-browser-check',
      '--disable-gpu',
      // The target is a phone, so the viewport is a phone.
      `--window-size=${VIEWPORT_W},${VIEWPORT_H}`,
      ...(INSECURE ? ['--ignore-certificate-errors'] : []),
      'about:blank',
    ],
    { stdio: 'ignore' },
  );

  let target = null;
  for (let i = 0; i < 40 && !target; i++) {
    await sleep(500);
    try {
      const list = await (await fetch(`http://127.0.0.1:${DEBUG_PORT}/json`)).json();
      target = list.find((t) => t.type === 'page');
    } catch {
      // Chrome is still starting.
    }
  }
  if (!target) throw new Error('Chrome never exposed a page target');

  ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => {
    ws.addEventListener('open', res);
    ws.addEventListener('error', rej);
  });
  ws.addEventListener('message', (ev) => {
    const m = JSON.parse(ev.data);
    if (m.id && pending.has(m.id)) {
      const p = pending.get(m.id);
      pending.delete(m.id);
      m.error ? p.reject(new Error(`${m.error.message}`)) : p.resolve(m.result ?? {});
      return;
    }
    if (m.method === 'Runtime.consoleAPICalled' && m.params.type === 'error') {
      consoleErrors.push(m.params.args.map((a) => a.value ?? a.description ?? '').join(' '));
    }
    if (m.method === 'Runtime.exceptionThrown') {
      consoleErrors.push(
        'EXCEPTION: ' + (m.params.exceptionDetails?.exception?.description ?? ''),
      );
    }
    if (m.method === 'Network.webSocketCreated') wsTrace.push(`created ${m.params.url}`);
    if (m.method === 'Network.webSocketHandshakeResponseReceived') {
      wsTrace.push(`handshake ${m.params.response.status}`);
    }
  });
  await send('Page.enable');
  await send('Runtime.enable');
  await send('Network.enable');
  step(2, `headless Chrome ready at a ${VIEWPORT} viewport`);

  // --- the QR flow, exactly as a scan would drive it ------------------------

  await send('Page.navigate', { url: `${BASE}/m/#/pair?code=${code}` });
  await sleep(2500);
  step(3, `app shell loaded: ${JSON.stringify(await evaluate('document.title'))}`);

  await waitFor(
    'the pair screen with the scanned code prefilled',
    `(() => {
       const i = document.querySelector('input');
       return !!i && i.value.toUpperCase() === ${JSON.stringify(code)};
     })()`,
  );
  step(4, `QR scan prefilled the code: ${code}`);

  const deviceName = 'e2e-browser';
  await evaluate(`(() => {
    const inputs = [...document.querySelectorAll('input')];
    const name = inputs[1] ?? inputs[0];
    name.value = ${JSON.stringify(deviceName)};
    name.dispatchEvent(new Event('input', { bubbles: true }));
    return true;
  })()`);
  await screenshot('1-pair');

  await evaluate(`(() => {
    const btn = [...document.querySelectorAll('button')]
      .find((b) => /pair|connect|submit/i.test(b.textContent || ''));
    if (!btn) throw new Error('no submit button; found: ' +
      [...document.querySelectorAll('button')].map((b) => b.textContent).join(' | '));
    btn.click();
    return true;
  })()`);

  await waitFor('the pair screen to be left behind', `!location.hash.includes('/pair')`);
  step(5, `paired; the app moved to ${await evaluate('location.hash')}`);
  await sleep(2000);
  await screenshot('2-sessions');

  // --- the live feed must actually be live ----------------------------------
  //
  // This is the assertion that caught the paused-stream bug. The badge reading
  // "Live" is the app's own report that its event stream is open, and the trace
  // below confirms a real 101 came back.

  await waitFor(
    'the event stream to report itself live',
    `/Live|Connected/.test(document.body.innerText)`,
    30000,
  );
  step(6, `event stream is live (${wsTrace.filter((t) => t.startsWith('handshake')).join(', ') || 'no handshake seen'})`);

  // --- discover what the gateway offers, as a client must --------------------

  const workspaces = await evaluate(
    `(async () => (await (await fetch('/api/v1/workspaces')).json()).workspaces.map((w) => w.path))()`,
  );
  if (!workspaces.length) throw new Error('the gateway offers no workspace, so no session can be created');
  step(7, `gateway offers ${workspaces.length} workspace(s): ${workspaces.join(', ')}`);

  const madeHere = [];
  const created = await evaluate(`(async () => {
    const r = await fetch('/api/v1/sessions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ workspace: ${JSON.stringify(workspaces[0])} }),
    });
    const j = await r.json();
    return { status: r.status, id: j.id, problem: r.ok ? null : j };
  })()`);
  if (!created.id) {
    throw new Error(`could not create a session: ${created.status} ${JSON.stringify(created.problem)}`);
  }
  step(8, `created a session: ${created.status} ${created.id}`);
  madeHere.push(created.id);

  // --- a real model turn ----------------------------------------------------

  // The prompt asks for something only a tool can answer. A turn that merely
  // produces text would leave the tool rendering — the part of the screen this
  // script now checks — untested, which is how a 2px tool card survived a green
  // end-to-end run: every earlier check read the API, and none looked at the
  // screen it feeds.
  const prompt = await evaluate(`(async () => {
    const r = await fetch('/api/v1/sessions/${created.id}/prompt', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ blocks: [{ type: 'text', text: 'Use the bash tool to run \`echo probe-ok\`, then tell me exactly what it printed.' }] }),
    });
    return { status: r.status, body: await r.json() };
  })()`);
  if (prompt.status !== 202) {
    throw new Error(`prompt rejected: ${prompt.status} ${JSON.stringify(prompt.body)}`);
  }
  step(9, `prompt accepted: ${prompt.status} ${JSON.stringify(prompt.body)}`);

  // Watch the turn arrive over a fresh socket, so the check does not depend on
  // the app's own connection having been open first.
  const events = await evaluate(`(async () => {
    return await new Promise((resolve) => {
      const types = [];
      const socket = new WebSocket(
        (location.protocol === 'https:' ? 'wss:' : 'ws:') + '//' + location.host + '/api/v1/events',
      );
      socket.onmessage = (e) => types.push(JSON.parse(e.data).type);
      socket.onopen = () => socket.send(JSON.stringify({ type: 'subscribe', sessionId: ${JSON.stringify(created.id)} }));
      setTimeout(() => { socket.close(); resolve(types); }, 60000);
    });
  })()`);
  const counts = events.reduce((a, t) => ((a[t] = (a[t] || 0) + 1), a), {});
  step(10, `live events over the app's own WebSocket: ${JSON.stringify(counts)}`);

  if (!counts['session.message']) {
    throw new Error('the turn produced no assistant message on the event stream');
  }
  if (!counts['session.tool']) {
    throw new Error(
      'the turn used no tool, so nothing about tool rendering was exercised; ' +
        `the event types were ${JSON.stringify(counts)}`,
    );
  }

  const transcript = await evaluate(`(async () => {
    const r = await fetch('/api/v1/sessions/${created.id}/transcript?limit=20');
    const j = await r.json();
    return { status: r.status, total: j.total, roles: (j.items || []).map((i) => i.role), problem: r.ok ? null : j };
  })()`);
  step(11, `transcript for that session: total=${transcript.total} roles=${JSON.stringify(transcript.roles)}`);
  if (!transcript.roles.includes('tool')) {
    // Say what the server said. A bare "no tool entry" hides a 429 from the
    // gateway's own rate limiter — which this script can trip when anything
    // else on the machine is talking to the same gateway at the same time.
    throw new Error(
      'the transcript kept no tool entry, so history would lose the call' +
        (transcript.problem === null ? '' : ` (HTTP ${transcript.status}: ${JSON.stringify(transcript.problem)})`),
    );
  }

  await screenshot('3-conversation');

  // --- the pickers must offer the catalog the API serves --------------------
  //
  // This is the seam that hid for months: the new-session sheet decoded a
  // payload shape the server has never sent, so a gateway advertising five
  // models rendered one "Gateway default" row. Nothing failed — the Go tests
  // never read the client, and the client had never met a real server. The
  // check is deliberately relative: whatever /models advertises must appear in
  // the pickers, one entry each, plus the default. It also checks what the sheet
  // *starts* on, because a picker that says "Gateway default" while the server
  // applies a configured default is the same lie in a quieter form.
  const pickers = await evaluate(`(async () => {
    const api = await (await fetch('/api/v1/models')).json();
    const optionsOf = (id) => (((api.config || []).find((o) => o.id === id)) || {}).options || [];

    const newButton = [...document.querySelectorAll('button')]
      .find((b) => (b.textContent || '').trim() === 'New');
    if (!newButton) throw new Error('no New button on the sessions screen');
    newButton.click();

    // The sheet is drawn after a refresh when the catalog is cold, so wait for
    // it rather than assuming one tick is enough.
    const deadline = Date.now() + 15000;
    let sheet = null;
    while (sheet === null && Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 250));
      sheet = document.querySelector('#sheet-host .sheet');
    }
    if (sheet === null) throw new Error('the new-session sheet never opened');

    const selects = [...sheet.querySelectorAll('select')];
    if (selects.length < 2) throw new Error('expected a model and a reasoning-effort select');
    const shown = selects.map((s) => [...s.options].map((o) => o.textContent));
    return {
      model: optionsOf('model').map((o) => o.label),
      effort: optionsOf('reasoning_effort').map((o) => o.label),
      modelIDs: optionsOf('model').map((o) => o.id),
      effortIDs: optionsOf('reasoning_effort').map((o) => o.id),
      defaults: api.defaults || { model: '', reasoningEffort: '' },
      shown,
      selected: selects.map((s) => s.value),
    };
  })()`);

  const shownModels = pickers.shown[0] ?? [];
  const shownEfforts = pickers.shown[1] ?? [];
  for (const [what, advertised, rendered] of [
    ['model', pickers.model, shownModels],
    ['reasoning effort', pickers.effort, shownEfforts],
  ]) {
    if (rendered.length !== advertised.length + 1) {
      throw new Error(
        `the ${what} picker shows ${rendered.length} option(s) for ${advertised.length} advertised value(s)`,
      );
    }
    const missing = advertised.filter((label) => !rendered.includes(label));
    if (missing.length > 0) {
      throw new Error(`the ${what} picker is missing advertised value(s): ${JSON.stringify(missing)}`);
    }
  }

  // And it must open on what the server will actually do. "Gateway default" as
  // the preselected row is only honest when there is no configured default or
  // when the catalog no longer offers it.
  for (const [what, configured, offered, selected] of [
    ['model', pickers.defaults.model, pickers.modelIDs, pickers.selected[0]],
    ['reasoning effort', pickers.defaults.reasoningEffort, pickers.effortIDs, pickers.selected[1]],
  ]) {
    const expected = configured !== '' && offered.includes(configured) ? configured : '';
    if (selected !== expected) {
      throw new Error(
        `the ${what} picker opens on ${JSON.stringify(selected)}, want ${JSON.stringify(expected)} ` +
          `(the gateway default is ${JSON.stringify(configured)})`,
      );
    }
  }
  step(
    12,
    `pickers match the catalog: ${pickers.model.length} model(s), ${pickers.effort.length} effort(s) offered`,
  );
  // Printed because a collapsed <select> shows only its current value, so the
  // screenshot cannot prove what the picker offers; this can.
  note(`models:  ${shownModels.join(' | ')}`);
  note(`efforts: ${shownEfforts.join(' | ')}`);
  note(
    `preselected: ${pickers.defaults.model || '(harness default)'} / ` +
      `${pickers.defaults.reasoningEffort || '(harness default)'}`,
  );
  // The sheet is still open: this is the screenshot that shows what the phone
  // actually offers. Close it afterwards so the run ends where it started.
  await screenshot('4-new-session');
  await evaluate(`(() => {
    const close = document.querySelector('#sheet-host .sheet button[aria-label="Close"]');
    if (close) close.click();
    return true;
  })()`);

  // --- the conversation must actually show the tools ------------------------
  //
  // Every check above reads bytes: events, JSON, a selector's options. None of
  // them can tell a rendered tool card from one that is present but invisible,
  // and the difference was the bug: `.tool` is a <details> with `overflow:
  // hidden`, the feed is a flex column, and a flex item that clips its own
  // overflow loses its automatic minimum size. In a conversation taller than the
  // phone, every tool card was squeezed to its two borders — 2px of a hairline
  // between two message cards — while the messages beside them could not shrink.
  // So this step opens the real session and measures what the reader would see.
  await evaluate(`(() => { location.hash = '#/sessions/${created.id}'; return true; })()`);

  // A short viewport is the point, not a convenience. The cards only collapsed
  // when the log was taller than the screen — which is every real conversation
  // and no test session — so measuring at the phone's full height would have
  // passed against the broken build too. Squeezed to 320px, the three rows here
  // already overflow, and the row that clips its own overflow has to shrink
  // unless the stylesheet says otherwise.
  await send('Emulation.setDeviceMetricsOverride', {
    width: 390,
    height: 320,
    deviceScaleFactor: 1,
    mobile: true,
  });

  let feed = null;
  const feedDeadline = Date.now() + 25000;
  while (Date.now() < feedDeadline) {
    await sleep(500);
    feed = await evaluate(`(() => {
      const cards = [...document.querySelectorAll('.tool')];
      return {
        messages: document.querySelectorAll('.msg').length,
        names: cards.map((c) => (c.querySelector('.tool-name') || {}).textContent || ''),
        details: cards.map((c) => (c.querySelector('.tool-detail') || {}).textContent || ''),
        heights: cards.map((c) => Math.round(c.getBoundingClientRect().height)),
        logOverflows: (() => {
          const log = document.querySelector('.log');
          return log === null ? null : log.scrollHeight > log.clientHeight;
        })(),
      };
    })()`);
    if (feed.messages > 0 && feed.names.length > 0) break;
  }
  if (feed === null || feed.messages === 0) {
    throw new Error('the conversation rendered no messages');
  }
  if (feed.names.length === 0) {
    throw new Error('the conversation rendered no tool card for a turn that used a tool');
  }
  if (feed.logOverflows !== true) {
    throw new Error(
      'the feed did not overflow its viewport, so the layout that hid the tool cards was not ' +
        'exercised; this check is only meaningful while the log scrolls',
    );
  }
  const unreadable = feed.heights.filter((height) => height < 24);
  if (unreadable.length > 0) {
    throw new Error(
      `${unreadable.length} of ${feed.heights.length} tool card(s) rendered too short to read ` +
        `(${unreadable.join('px, ')}px) while the feed overflowed; the cards are in the DOM but ` +
        `not on the screen`,
    );
  }
  const nameless = feed.names.filter((name) => name.trim() === '');
  if (nameless.length > 0) {
    throw new Error(`${nameless.length} tool card(s) rendered without a tool name`);
  }
  // A card that says only "bash" makes the reader open it to find out what the
  // agent is doing; DSH's own timeline leads with the description and the path,
  // and this screen should read the same way.
  const unexplained = feed.details.filter((detail) => detail.trim() === '');
  if (unexplained.length > 0) {
    throw new Error(
      `${unexplained.length} of ${feed.details.length} tool card(s) rendered with no summary line ` +
        '(expected a path for a file tool, a description for a command)',
    );
  }
  step(
    13,
    `conversation renders ${feed.messages} message(s) and ${feed.names.length} tool card(s), ` +
      `shortest ${Math.min(...feed.heights)}px at 390x320`,
  );
  note(`tools: ${feed.names.map((name, i) => `${name} · ${feed.details[i]}`).slice(0, 5).join(' | ')}`);

  // Back to a phone for the screenshot a human will look at.
  await send('Emulation.clearDeviceMetricsOverride');
  await sleep(1000);
  await screenshot('5-tools');

  // --- a turn that happens while the phone is away ---------------------------
  //
  // The phone's socket dies whenever the screen locks, the radio changes, or the
  // browser backgrounds the tab: all normal, none of them an error the user can
  // see. What must not happen is a conversation that stops moving until the
  // reader pulls to refresh — which is what it did, because a client that has
  // not yet seen a frame has no sequence number to resume from, so the server
  // resumes "from now" and nothing ever told the client it may have missed
  // something.
  //
  // The page is reloaded first. That is not scene-setting: it is what puts the
  // client in the state that broke — a fresh app, connected, watching a quiet
  // conversation, with no frame yet seen and therefore no resume point.
  await send('Page.navigate', { url: `${BASE}/m/` });
  await sleep(4000);
  await evaluate(`location.hash = '#/sessions/${created.id}'`);

  let cold = 0;
  const coldDeadline = Date.now() + 20000;
  while (Date.now() < coldDeadline) {
    await sleep(500);
    cold = await evaluate(`document.querySelectorAll('.msg').length`);
    if (cold > 0) break;
  }
  if (cold === 0) throw new Error('the conversation did not render after a reload');
  note(`after a reload the conversation shows ${cold} message(s), from history alone`);

  const cookies = await send('Network.getCookies', { urls: [BASE] });
  const session = (cookies.cookies || []).find((c) => c.name === 'dsh_gw_session');
  if (!session) throw new Error('no session cookie in the browser after pairing');

  await send('Network.emulateNetworkConditions', {
    offline: true, latency: 0, downloadThroughput: 0, uploadThroughput: 0,
  });
  await sleep(1500);

  const awayText = `reconnect-check-${Date.now()}`;
  const away = await fetch(`${BASE}/api/v1/sessions/${created.id}/prompt`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Origin: BASE,
      Cookie: `dsh_gw_session=${session.value}`,
    },
    body: JSON.stringify({ blocks: [{ type: 'text', text: `Reply with exactly: ${awayText}` }] }),
  });
  if (away.status !== 202) throw new Error(`prompt while away rejected: ${away.status}`);

  // Wait for the turn to finish on the server, still with the app offline. The
  // role matters: the prompt itself echoes the same text, and treating that as
  // "the turn is over" would let the app come back online mid-turn and watch the
  // rest of it live — which is a different, much easier case than the one this
  // step exists to cover.
  let committed = false;
  for (let i = 0; i < 45 && !committed; i++) {
    await sleep(2000);
    const transcript = await fetch(`${BASE}/api/v1/sessions/${created.id}/transcript?limit=20`, {
      headers: { Origin: BASE, Cookie: `dsh_gw_session=${session.value}` },
    });
    const body = await transcript.json();
    committed = (body.items || []).some(
      (item) => item.role === 'assistant' && (item.text || '').includes(awayText),
    );
  }
  if (!committed) throw new Error('the turn never committed while the app was offline');

  await send('Network.emulateNetworkConditions', {
    offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1,
  });

  let caught = null;
  const catchDeadline = Date.now() + 30000;
  while (Date.now() < catchDeadline) {
    await sleep(1000);
    // Counted by role, not by text alone: the prompt echoes the same string, so a
    // single "does the text appear" test cannot tell the reply from its own
    // question, and a message that arrived from *both* history and the stream
    // would still look like one.
    caught = await evaluate(`(() => {
      const of = (selector) =>
        [...document.querySelectorAll(selector)].filter((row) =>
          (row.querySelector('.msg-body') || {}).textContent?.includes(${JSON.stringify(awayText)}),
        ).length;
      return {
        rows: document.querySelectorAll('.msg').length,
        asked: of('.msg-user'),
        answered: of('.msg-assistant'),
      };
    })()`);
    // Both rows, because they can arrive by different routes: the reply may be
    // replayed from the ring buffer while the prompt is still coming back with
    // the refetched transcript. Asserting between the two would be asserting a
    // transient state.
    if (caught.answered > 0 && caught.asked > 0) break;
  }
  if (caught === null || caught.answered === 0) {
    throw new Error(
      'the turn that ran while the app was offline never appeared after reconnecting; ' +
        'the reader would have to refresh the page',
    );
  }
  if (caught.answered > 1) {
    throw new Error(
      `the reconnected reply rendered ${caught.answered} times; history and the live stream must collapse`,
    );
  }
  if (caught.asked !== 1) {
    throw new Error(`the offline prompt rendered ${caught.asked} times, want exactly one`);
  }
  step(
    14,
    `a turn missed while the app was offline arrived on reconnect, once ` +
      `(${caught.rows} row(s) in the feed)`,
  );
  await screenshot('6-reconnect');

  // --- a reload in the middle of a turn must keep Stop -----------------------
  //
  // The event stream carries no snapshot of a turn already in flight: the
  // `turn.state: running` frame was published before this page existed, and a
  // fresh client resumes "from now", so a session the gateway is actively
  // running looks idle to a client that has just loaded. Stop is the honest
  // action while a turn is in flight, and the reload has to preserve it.
  //
  // Send is expected beside it, not instead of it. A prompt that arrives
  // mid-turn is *queued* — that is what the deployment's default
  // `session.promptQueueDepth` buys — so both controls are legitimately live at
  // once: Stop stops the turn, Send queues a follow-up. A deployment that sets
  // the depth to zero would show Send disabled instead, which is why the
  // assertion below checks the enablement as well as the visibility.
  //
  // So this step drives a turn long enough to reload inside it, and then uses
  // the Stop that the reload preserved. The tool call is deliberately far longer
  // than this step's patience, so a Stop that did not actually stop would be
  // caught waiting for a turn that cannot end by itself.
  const composerState = async () =>
    evaluate(`(async () => {
      const stop = document.querySelector('.composer-stop');
      const send = document.querySelector('.composer-send');
      const session = await (await fetch('/api/v1/sessions/${created.id}')).json();
      return {
        stop: stop !== null && !stop.hidden,
        send: send !== null && !send.hidden,
        sendDisabled: send === null || send.disabled,
        busy: session.busy === true,
        leased: session.leased === true,
      };
    })()`);

  const slowText = `reload-stop-${Date.now()}`;
  const slowPrompt = await evaluate(`(async () => {
    const r = await fetch('/api/v1/sessions/${created.id}/prompt', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ blocks: [{ type: 'text', text: 'Use the bash tool to run \`sleep 90\`, then reply with exactly: ${slowText}' }] }),
    });
    return r.status;
  })()`);
  if (slowPrompt !== 202) throw new Error(`the slow prompt was rejected: ${slowPrompt}`);

  // The live path first, so the reload is the only difference between the two
  // readings below.
  let live = null;
  for (let i = 0; i < 60 && live === null; i++) {
    const seen = await composerState();
    if (seen.stop && seen.send && !seen.sendDisabled) live = seen;
    else await sleep(250);
  }
  if (live === null) {
    throw new Error(
      'a turn this phone started did not offer both Stop and an enabled Send; a mid-turn prompt is ' +
        'queued rather than refused, so Send stays live',
    );
  }
  note(`mid-turn: Stop offered beside an enabled Send, busy=${live.busy}, leased=${live.leased}`);

  await send('Page.reload', {});
  await sleep(3000);

  // The turn frame is gone with the document; `busy` from the session snapshot
  // is the only thing left that says a turn is running.
  let reloaded = null;
  let last = null;
  const reloadDeadline = Date.now() + 30000;
  while (Date.now() < reloadDeadline) {
    last = await composerState();
    if (last.stop && last.send) {
      reloaded = last;
      break;
    }
    await sleep(500);
  }
  if (reloaded === null) {
    throw new Error(
      'after a reload the composer did not offer Stop while the gateway was still running the turn ' +
        `(busy=${last === null ? '?' : last.busy}, leased=${last === null ? '?' : last.leased}, ` +
        `stop=${last === null ? '?' : last.stop}, send=${last === null ? '?' : last.send})`,
    );
  }
  step(15, `a reload mid-turn kept Stop (busy=${reloaded.busy}, leased=${reloaded.leased})`);
  await screenshot('7-reload-stop');

  // And the button has to mean it. Send coming back can only be the Stop: the
  // tool call it was offered for is still running.
  await evaluate(`(() => {
    const stop = document.querySelector('.composer-stop');
    if (stop === null) throw new Error('the Stop button vanished before it could be used');
    stop.click();
    return true;
  })()`);

  let stopped = null;
  const stoppedDeadline = Date.now() + 60000;
  while (Date.now() < stoppedDeadline) {
    await sleep(500);
    const seen = await composerState();
    if (!seen.stop && seen.send && !seen.busy) {
      stopped = seen;
      break;
    }
  }
  if (stopped === null) {
    throw new Error('Stop did not end the turn: the composer never returned to Send');
  }
  step(16, 'Stop ended the turn it was offered for, without the tool finishing on its own');

  // --- the controls the queue and change work added -------------------------
  //
  // Four surfaces the run had never reached: the attach control, the queue
  // strip, the turn clock, and the change screen. There is no frontend unit test
  // suite, so this tier is the only thing that drives the compiled app; the
  // seams between it and the API are exactly where this project's bugs have
  // lived. The strongest check here is the disagreement one: an attach button
  // offered by a deployment whose harness cannot take images is a button that
  // produces an error, and the two ends have to agree about it.
  const composer = await evaluate(`(async () => {
    const chip = document.querySelector('.changes-chip');
    const attach = document.querySelector('.attach-button');
    const me = await (await fetch('/api/v1/me')).json();
    return {
      chip: chip !== null && !chip.hidden,
      attach: attach !== null,
      imagesOffered: me.features.imagePrompts === true,
      queueDepth: me.features.promptQueueDepth,
      interval: typeof window !== 'undefined',
    };
  })()`);
  if (!composer.chip) throw new Error('the conversation header offers no way into the change screen');
  if (composer.attach !== composer.imagesOffered) {
    throw new Error(
      'the attach control and the deployment disagree about images: ' +
        `button=${composer.attach}, imagePrompts=${composer.imagesOffered}`,
    );
  }
  note(
    `composer: change chip present, attach=${composer.attach} matching imagePrompts, ` +
      `queueDepth=${composer.queueDepth}`,
  );

  await evaluate(`(() => {
    const chip = document.querySelector('.changes-chip');
    if (chip === null) throw new Error('the change chip vanished between checks');
    chip.click();
    return true;
  })()`);
  await sleep(1500);
  const changes = await evaluate(`(() => {
    const sheet = document.querySelector('.sheet');
    return { open: sheet !== null, text: sheet === null ? '' : sheet.textContent };
  })()`);
  if (!changes.open || !changes.text.includes('What changed')) {
    throw new Error(`the change screen did not open from the header: ${JSON.stringify(changes).slice(0, 200)}`);
  }
  step(29, 'the change screen opened from the conversation header, and read the session log');
  await screenshot('8-changes');

  // Dismiss it again, so the rest of the run is where it was. The backdrop's own
  // handler is what closes a sheet; a click on it is the same gesture a reader
  // makes.
  await evaluate(`(() => {
    const backdrop = document.querySelector('.sheet-backdrop');
    if (backdrop !== null) backdrop.click();
    return true;
  })()`);
  await sleep(300);

  // --- a session the gateway is not driving ---------------------------------
  //
  // Everything above runs through the gateway's own DSH child. A session started
  // at the desk is a different process: the gateway heard nothing about it, so
  // the phone saw its rows only by asking again, and a session being written to
  // looked exactly like one that had stopped. The gateway follows such a log
  // now, and this step proves it with a real headless DSH turn — outside the
  // gateway entirely — watched from a phone that was already looking.
  //
  // The order matters. The first run only creates the session and exits; the app
  // then opens it and settles on its history; only then does a second run make
  // something happen. Anything the app shows after that point arrived over the
  // stream, because nothing else asks the gateway for a transcript again.
  const listSessions = async () => {
    const r = await fetch(`${BASE}/api/v1/sessions?limit=60`, {
      headers: { Origin: BASE, Cookie: `dsh_gw_session=${session.value}` },
    });
    const body = await r.json();
    return body.sessions || [];
  };
  const known = new Set((await listSessions()).map((s) => s.id));

  const opening = spawn(DSH, ['headless', 'Reply with exactly: peer-session-open'], {
    cwd: workspaces[0],
    stdio: ['ignore', 'ignore', 'pipe'],
  });
  let openingError = '';
  opening.stderr.on('data', (chunk) => {
    openingError += String(chunk);
  });
  // Waiting for the process, not for a flag, is what makes the second run
  // possible: the first one holds the session's writer lock until it exits, and
  // a resume that arrives early fails with nothing on the phone to show for it.
  await new Promise((resolve) => opening.on('exit', resolve));

  let deskID = null;
  const deskDeadline = Date.now() + 20000;
  while (Date.now() < deskDeadline && deskID === null) {
    await sleep(400);
    const fresh = (await listSessions()).find((s) => !known.has(s.id) && s.workspace === workspaces[0]);
    if (fresh) deskID = fresh.id;
  }
  if (deskID === null) {
    throw new Error(`the headless run produced no session the gateway could list: ${openingError.trim()}`);
  }
  madeHere.push(deskID);
  note(`a desk session exists outside the gateway: ${deskID}`);

  await evaluate(`(() => { location.hash = '#/sessions/${deskID}'; return true; })()`);
  let attached = null;
  const attachDeadline = Date.now() + 20000;
  while (Date.now() < attachDeadline && attached === null) {
    await sleep(400);
    const seen = await evaluate(`(() => ({
      hash: location.hash,
      messages: document.querySelectorAll('.msg').length,
      tools: document.querySelectorAll('.tool').length,
    }))()`);
    if (seen.hash.includes(deskID) && seen.messages > 0) attached = seen;
  }
  if (attached === null) throw new Error('the conversation for the desk session never opened');
  note(`watching it with ${attached.messages} message(s) and ${attached.tools} tool card(s) on screen`);

  // Now the desk gets back to work. The tool call is deliberately slow so the
  // reading happens while the turn is genuinely in flight.
  const deskText = `desk-${Date.now()}`;
  const desk = spawn(
    DSH,
    ['headless', '--session-id', deskID, `Run the bash tool with \`sleep 4\`, then reply with exactly: ${deskText}`],
    { cwd: workspaces[0], stdio: ['ignore', 'ignore', 'pipe'] },
  );
  let deskError = '';
  desk.stderr.on('data', (chunk) => {
    deskError += String(chunk);
  });
  desk.unref();

  const sessionRow = async () => (await listSessions()).find((s) => s.id === deskID);

  // The list first: a session with no lease here is the one case it cannot
  // infer, and a reader who sees "idle" concludes nothing is happening.
  let deskBusy = false;
  for (let i = 0; i < 24 && !deskBusy; i++) {
    await sleep(500);
    const row = await sessionRow();
    deskBusy = row !== undefined && row.busy === true;
  }
  if (!deskBusy) throw new Error('a session running outside the gateway was listed as idle');
  note('the list reports it as busy while it runs');

  // Then the conversation: it has to say who is working, or a session being
  // written elsewhere still looks frozen on the screen where it matters.
  let status = null;
  for (let i = 0; i < 20 && status === null; i++) {
    const seen = await evaluate(`(() => {
      const node = document.querySelector('.turn-status');
      return node !== null && !node.hidden ? node.textContent.trim() : '';
    })()`);
    if (typeof seen === 'string' && seen.includes('another client')) status = seen;
    else await sleep(500);
  }
  if (status === null) {
    throw new Error(
      'the conversation did not say that another client is running this session' +
        (deskError.trim() === '' ? '' : ` (the desk run said: ${deskError.trim().slice(0, 300)})`),
    );
  }
  note(`the conversation says: ${status}`);

  let deskSeen = null;
  const deskSeenDeadline = Date.now() + 45000;
  while (Date.now() < deskSeenDeadline) {
    await sleep(1000);
    deskSeen = await evaluate(`(() => {
      const of = (selector) =>
        [...document.querySelectorAll(selector)].filter((row) =>
          (row.querySelector('.msg-body') || {}).textContent?.includes(${JSON.stringify(deskText)}),
        ).length;
      return {
        messages: document.querySelectorAll('.msg').length,
        tools: document.querySelectorAll('.tool').length,
        answered: of('.msg-assistant'),
      };
    })()`);
    if (deskSeen.answered > 0) break;
  }
  if (deskSeen === null || deskSeen.answered === 0) {
    throw new Error(
      'a session running outside the gateway never reached the phone; the reader would have to refresh' +
        (deskError.trim() === '' ? '' : ` (the desk run said: ${deskError.trim().slice(0, 300)})`),
    );
  }
  if (deskSeen.answered > 1) {
    throw new Error(`the desk reply rendered ${deskSeen.answered} times`);
  }
  if (deskSeen.tools <= attached.tools) {
    throw new Error(
      `no tool card arrived for the desk turn (had ${attached.tools}, has ${deskSeen.tools}); ` +
        'the conversation is not being followed live',
    );
  }
  step(
    17,
    `a desk session arrived live: ${deskSeen.messages} message(s), ` +
      `${deskSeen.tools} tool card(s), reply once`,
  );
  await screenshot('7-peer-session');

  // --- notifications are wired end to end -----------------------------------
  //
  // A real push cannot be delivered in a headless browser — there is no push
  // service behind it — but the half that can fail silently is worth pinning:
  // the gateway must offer a key, and the settings screen must offer the switch
  // that uses it. The encryption itself has a byte-for-byte test against RFC
  // 8291 where it belongs, next to the code.
  const push = await evaluate(`(async () => {
    const key = await (await fetch('/api/v1/push/key')).json();
    location.hash = '#/settings';
    return { enabled: key.enabled, keyLength: (key.publicKey || '').length, threshold: key.turnThresholdSeconds };
  })()`);
  await sleep(1500);
  const notifications = await evaluate(`(() => {
    const card = [...document.querySelectorAll('.card')].find(
      (c) => (c.querySelector('.card-title') || {}).textContent === 'Notifications',
    );
    if (!card) return null;
    return {
      action: (card.querySelector('button') || {}).textContent || '',
      status: (card.querySelector('.muted') || {}).textContent || '',
    };
  })()`);
  if (!push.enabled || push.keyLength < 80) {
    throw new Error(`the gateway did not offer a usable push key: ${JSON.stringify(push)}`);
  }
  if (notifications === null) {
    throw new Error('the settings screen has no notifications card');
  }
  step(18, `notifications: key offered (${push.keyLength} chars), switch reads "${notifications.action}"`);
  await screenshot('8-notifications');

  // --- the receipt answers "what did it do" ---------------------------------
  //
  // The conversation says what was said; the receipt says which tools ran, how
  // many tokens went through and how long it worked. It is derived from the log,
  // so the only way it can be wrong is by disagreeing with the session it
  // describes — which is what this checks.
  await evaluate(`(() => { location.hash = '#/sessions'; return true; })()`);
  // The screen refreshes itself on open, so the row arrives asynchronously; a
  // fixed sleep would be a race that passes on a fast machine and fails on a
  // slow one.
  let rowReady = false;
  for (let i = 0; i < 20 && !rowReady; i++) {
    await sleep(500);
    rowReady = await evaluate(
      `document.querySelector('[data-session-id="' + ${JSON.stringify(created.id)} + '"]') !== null`,
    );
  }
  if (!rowReady) {
    const seen = await evaluate(`[...document.querySelectorAll('[data-session-id]')].map((r) => r.getAttribute('data-session-id'))`);
    throw new Error(`the session under test is not in the list; it holds ${JSON.stringify(seen)}`);
  }
  await evaluate(`(() => {
    const row = document.querySelector('[data-session-id="' + ${JSON.stringify(created.id)} + '"]');
    row.querySelector('.row-actions').click();
    return true;
  })()`);
  await sleep(700);
  await evaluate(`(() => {
    const button = [...document.querySelectorAll('#sheet-host button')].find((b) => b.textContent.trim() === 'Receipt…');
    if (!button) throw new Error('the row menu has no Receipt entry');
    button.click();
    return true;
  })()`);
  await sleep(2500);

  const receipt = await evaluate(`(() => {
    const sheet = document.querySelector('#sheet-host .sheet');
    if (!sheet) throw new Error('no receipt sheet');
    const rows = {};
    for (const row of sheet.querySelectorAll('.receipt-row')) {
      const label = (row.querySelector('.receipt-label') || {}).textContent || '';
      const value = (row.querySelector('.receipt-value') || {}).innerText || '';
      if (label) rows[label] = value.split(String.fromCharCode(10))[0];
    }
    return { text: sheet.innerText, rows };
  })()`);
  for (const label of ['Messages', 'Tokens', 'Tools']) {
    if (!(label in receipt.rows)) {
      throw new Error(`the receipt has no ${label} row: ${JSON.stringify(receipt.rows)}`);
    }
  }
  // The tools are the interesting part: the turn ran a bash command, and a
  // receipt that did not notice would be a receipt nobody uses.
  if (!receipt.rows.Tools.includes('bash')) {
    throw new Error(`the receipt does not name the tool that ran: ${JSON.stringify(receipt.rows.Tools)}`);
  }
  const messages = Number((receipt.rows.Messages || '0').split(' ')[0]);
  if (!(messages >= 2)) {
    throw new Error(`the receipt counts ${messages} message(s), want the ones on screen`);
  }
  step(22, `receipt: ${receipt.rows.Messages} message(s), tokens ${receipt.rows.Tokens}, tools ${receipt.rows.Tools}`);
  await screenshot('10-receipt');
  await evaluate(`(() => { document.querySelector('#sheet-host .sheet-close, #sheet-host button').click(); return true; })()`);
  await sleep(500);

  // --- search inside the sessions themselves --------------------------------
  //
  // The list filter answers "which session"; this answers "where did I write
  // that". It reads logs on the gateway, so it is asked for explicitly — and the
  // result has to say how much of the history it read, or "nothing found" means
  // two different things.
  await evaluate(`(() => { location.hash = '#/sessions'; return true; })()`);
  await sleep(2000);
  await evaluate(`(() => {
    const toggle = document.querySelector('button[aria-label="Search sessions"]');
    if (!toggle) throw new Error('the header has no search toggle');
    toggle.click();
    return true;
  })()`);
  await sleep(500);
  await evaluate(`(() => {
    const input = document.querySelector('.search-bar .input');
    if (!input) throw new Error('no search box');
    input.value = 'probe-ok';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    return true;
  })()`);
  await sleep(1200);
  await evaluate(`(() => {
    const button = [...document.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Inside');
    if (!button) throw new Error('the search bar has no inside-sessions control');
    button.click();
    return true;
  })()`);

  let search = null;
  for (let i = 0; i < 20 && search === null; i++) {
    await sleep(500);
    const seen = await evaluate(`(() => {
      const snippets = [...document.querySelectorAll('.search-snippet')];
      if (snippets.length === 0) return null;
      return {
        scope: (document.querySelector('.search-scope') || {}).innerText || '',
        snippets: snippets.slice(0, 3).map((s) => s.textContent || ''),
      };
    })()`);
    if (seen !== null) search = seen;
  }
  if (search === null) {
    throw new Error('searching inside sessions found nothing, though this run wrote that text');
  }
  if (!search.scope.includes('most recent sessions')) {
    throw new Error(`the result does not say how much was read: ${JSON.stringify(search.scope)}`);
  }
  if (!search.snippets.some((snippet) => snippet.includes('probe-ok'))) {
    throw new Error(`no snippet shows the match: ${JSON.stringify(search.snippets)}`);
  }
  step(23, `search inside sessions: ${search.scope.split(String.fromCharCode(10))[0]}`);
  await screenshot('11-search');
  await evaluate(`(() => {
    const back = [...document.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Back to sessions');
    if (back) back.click();
    return true;
  })()`);
  await sleep(500);

  // --- the New sheet ---------------------------------------------------------
  //
  // It comes up with a workspace to pick. Creating a session from it is
  // exercised earlier against the API; here the point is that the sheet itself
  // still opens, and that it closes again below.
  await evaluate(`(() => {
    const button = [...document.querySelectorAll('button')].find((b) => b.textContent.trim() === 'New');
    if (!button) throw new Error('the header has no New button');
    button.click();
    return true;
  })()`);
  await sleep(1500);
  const newSheet = await evaluate(`(() => {
    const sheet = document.querySelector('#sheet-host .sheet');
    if (!sheet) throw new Error('the New sheet did not open');
    return {
      workspaces: [...sheet.querySelectorAll('.choice-list .choice-title')].map((c) => c.textContent.trim()),
      hasWorkspacePicker: sheet.querySelector('.choice-list') !== null,
    };
  })()`);
  if (!newSheet.hasWorkspacePicker) {
    throw new Error('the New sheet lost its workspace picker');
  }
  step(24, `new session sheet offers ${newSheet.workspaces.length} workspace(s): ${newSheet.workspaces.join(', ')}`);
  await screenshot('12-new-session');
  await evaluate(`(() => {
    const close = document.querySelector('#sheet-host button[aria-label="Close"]');
    if (!close) throw new Error('the sheet has no close control');
    close.click();
    return true;
  })()`);
  await sleep(600);
  const sheetGone = await evaluate(`document.querySelector('#sheet-host .sheet') === null`);
  if (!sheetGone) {
    throw new Error('the New sheet stayed open');
  }

  // --- deleting is a move, and the app has to say so ------------------------
  //
  // The one button in this product that could destroy work is the one that must
  // not: it moves the session to the gateway's trash, the row leaves the list
  // immediately, and the same session comes back from the Trash screen. A test
  // that only checked the API would miss the half that matters — what the reader
  // is told and what they can undo.
  const scratch = await evaluate(`(async () => {
    const r = await fetch('/api/v1/sessions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ workspace: ${JSON.stringify(workspaces[0])} }),
    });
    const session = await r.json();
    await fetch('/api/v1/sessions/' + session.id + '/lease', { method: 'DELETE' });
    return session.id;
  })()`);

  // The filter from the search above is still in the box, and a filtered list is
  // the right answer to it — so clear it first, the way a reader would.
  await evaluate(`(() => {
    const input = document.querySelector('.search-bar .input');
    if (!input) return true;
    input.value = '';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    return true;
  })()`);
  await sleep(900);

  // A user who creates a session elsewhere and comes back to a list that is
  // already on screen pulls it up to date; so does this, through the same
  // control, because "the row appeared by itself" is not something the app
  // promises.
  await evaluate(`(() => {
    const more = document.querySelector('button[aria-label="More"]');
    if (!more) throw new Error('the header has no overflow menu');
    more.click();
    return true;
  })()`);
  await sleep(500);
  await evaluate(`(() => {
    const item = [...document.querySelectorAll('#sheet-host button')].find((b) => b.textContent.trim() === 'Refresh');
    if (!item) throw new Error('the overflow menu has no Refresh');
    item.click();
    return true;
  })()`);

  let scratchReady = false;
  for (let i = 0; i < 20 && !scratchReady; i++) {
    await sleep(500);
    scratchReady = await evaluate(
      `document.querySelector('[data-session-id="' + ${JSON.stringify(scratch)} + '"]') !== null`,
    );
  }
  if (!scratchReady) {
    throw new Error('the scratch session never reached the list after a refresh');
  }

  // Its menu, through the row the reader would use.
  await evaluate(`(() => {
    const row = document.querySelector('[data-session-id="' + ${JSON.stringify(scratch)} + '"]');
    row.querySelector('.row-actions').click();
    return true;
  })()`);
  await sleep(600);

  let deleted = null;
  const trashDeadline = Date.now() + 20000;
  while (Date.now() < trashDeadline) {
    const state = await evaluate(`(async () => {
      const menu = [...document.querySelectorAll('#sheet-host button')].map((b) => b.textContent.trim());
      const sheetTitle = (document.querySelector('#sheet-host .sheet-title') || {}).textContent || '';
      return { menu, sheetTitle };
    })()`);
    if (state.menu.includes('Delete…')) break;
    await sleep(300);
  }

  await evaluate(`(() => {
    const button = [...document.querySelectorAll('#sheet-host button')].find((b) => b.textContent.trim() === 'Delete…');
    if (!button) throw new Error('the row menu has no Delete');
    button.click();
    return true;
  })()`);
  await sleep(600);

  const confirm = await evaluate(`(() => {
    const sheet = document.querySelector('#sheet-host .sheet');
    const text = sheet ? sheet.innerText : '';
    return { title: (sheet.querySelector('.sheet-title') || {}).textContent || '', text };
  })()`);
  if (!confirm.text.includes('trash')) {
    throw new Error(`the delete confirmation does not say where the session goes: ${JSON.stringify(confirm.text.slice(0, 200))}`);
  }

  await evaluate(`(() => {
    const button = [...document.querySelectorAll('#sheet-host button')].find((b) => b.textContent.trim() === 'Move to trash');
    if (!button) throw new Error('the confirmation has no Move to trash');
    button.click();
    return true;
  })()`);
  await sleep(2500);

  const after = await evaluate(`(async () => ({
    toast: (document.querySelector('#toast-host') || {}).innerText || '',
    rows: document.querySelectorAll('.row').length,
    gone: document.querySelector('[data-session-id="' + ${JSON.stringify(scratch)} + '"]') === null,
    trash: (await (await fetch('/api/v1/trash')).json()).trash.map((e) => e.id),
  }))()`);
  if (!after.gone) {
    throw new Error('the deleted row is still in the list');
  }
  if (!after.trash.includes(scratch)) {
    throw new Error(`the session did not reach the trash: ${JSON.stringify(after.trash)}`);
  }
  if (!after.toast.includes('Undo')) {
    throw new Error(`deleting offered no undo: ${JSON.stringify(after.toast)}`);
  }
  step(20, `delete moved a session to the trash (${after.trash.length} held), row gone, undo offered`);
  await screenshot('9-deleted');

  // And back, from the screen that promises it.
  await evaluate(`(() => {
    const more = document.querySelector('button[aria-label="More"]');
    more.click();
    return true;
  })()`);
  await sleep(600);
  await evaluate(`(() => {
    const item = [...document.querySelectorAll('#sheet-host button')].find((b) => b.textContent.trim() === 'Trash…');
    if (!item) throw new Error('the menu has no Trash entry');
    item.click();
    return true;
  })()`);
  await sleep(2000);

  const trashSheet = await evaluate(`(() => {
    const sheet = document.querySelector('#sheet-host .sheet');
    const items = [...sheet.querySelectorAll('.tidy-item')].map(
      (item) => item.innerText.split(String.fromCharCode(10))[0],
    );
    const restore = [...sheet.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Restore');
    if (restore) restore.click();
    return { items };
  })()`);
  if (trashSheet.items.length === 0) {
    throw new Error('the trash screen lists nothing after a delete');
  }
  await sleep(2500);

  const restored = await evaluate(`(async () => ({
    trash: (await (await fetch('/api/v1/trash')).json()).trash.map((e) => e.id),
    listed: (await (await fetch('/api/v1/sessions?state=all&limit=100')).json()).sessions.some((s) => s.id === ${JSON.stringify(scratch)}),
  }))()`);
  if (restored.trash.includes(scratch)) {
    throw new Error('the session is still in the trash after Restore');
  }
  if (!restored.listed) {
    throw new Error('the restored session did not come back to the store');
  }
  step(21, 'the trash screen restored it, and it is listed again');
  madeHere.push(scratch);

  // --- a tap on Copy has to reach the clipboard -----------------------------
  //
  // This is the seam a mock cannot cover. `navigator.clipboard` does not exist
  // outside a secure context — the app is meant to be reachable over plain HTTP
  // on a LAN — and even where it exists `writeText` rejects for reasons the page
  // cannot see. The failure mode that hid here was silence: the button did
  // nothing, said nothing, and left the previous clipboard contents in place.
  //
  // So the check taps the real button, reads the real clipboard back, and then
  // breaks the API in the two ways the app has to survive. It runs last and on
  // whichever row the list leads with, so that it shares neither a menu nor a
  // race with the delete flow above.
  await send('Browser.grantPermissions', {
    origin: BASE,
    permissions: ['clipboardReadWrite', 'clipboardSanitizedWrite'],
  });

  const copyTarget = await evaluate(`(async () => {
    // Whatever the trash screen left open comes down first: a sheet over the list
    // would swallow the tap on the row beneath it.
    const close = document.querySelector('#sheet-host button[aria-label="Close"]');
    if (close) close.click();
    for (let i = 0; i < 25; i++) {
      if (document.querySelector('#sheet-host .sheet') === null) break;
      await new Promise((r) => setTimeout(r, 200));
    }
    const deadline = Date.now() + 15000;
    while (Date.now() < deadline && document.querySelector('[data-session-id]') === null) {
      await new Promise((r) => setTimeout(r, 250));
    }
    const row = document.querySelector('[data-session-id]');
    if (row === null) throw new Error('no session row to copy an id from');
    return row.getAttribute('data-session-id');
  })()`);
  note(`copying the id of ${copyTarget}`);

  await tap('[data-session-id="' + copyTarget + '"] .row-actions');
  await sleep(600);
  await evaluate(`(() => {
    const button = [...document.querySelectorAll('#sheet-host .sheet button')]
      .find((b) => /copy session id/i.test(b.textContent || ''));
    if (!button) throw new Error('the row menu has no Copy session id');
    button.setAttribute('data-e2e-copy', '1');
    // Kept before anything below can replace it: this is the only way back to a
    // clipboard that can be read while the page pretends not to have one.
    window.__e2eClipboard = navigator.clipboard;
    return true;
  })()`);

  const copyAttempt = async (label, breakIt) => {
    await evaluate(`window.__e2eClipboard.writeText('not-the-session-id').catch(() => {})`);
    await evaluate(`(() => {
      const status = document.querySelector('#sheet-host [role=status]');
      if (status) status.replaceChildren();
      return true;
    })()`);
    if (breakIt !== '') await evaluate(breakIt);
    await tap('[data-e2e-copy]');
    const state = await evaluate(`(async () => {
      const field = document.querySelector('#sheet-host input[aria-label="Session id"]');
      return {
        status: (document.querySelector('#sheet-host [role=status]') || {}).textContent || '',
        manual: field !== null && !field.hidden,
        clipboard: await window.__e2eClipboard.readText().catch((e) => 'unreadable: ' + e.name),
      };
    })()`);
    if (state.clipboard !== copyTarget) {
      throw new Error(
        `${label}: the clipboard holds ${JSON.stringify(state.clipboard)}, not the session id ` +
          `(the sheet said ${JSON.stringify(state.status)})`,
      );
    }
    if (state.manual) {
      throw new Error(`${label}: copying worked but the sheet still showed the manual field`);
    }
    note(`${label}: ${JSON.stringify(state.status)}`);
  };

  step(25, 'copy session id, tapped for real, with the API present');
  await copyAttempt('clipboard API available', '');

  step(26, 'copy session id where navigator.clipboard does not exist (a plain-HTTP LAN origin)');
  await copyAttempt(
    'navigator.clipboard undefined',
    `(() => { Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true }); return true; })()`,
  );
  await evaluate(`(() => { delete navigator.clipboard; return true; })()`);

  step(27, 'copy session id where writeText rejects (a denied or unfocused clipboard)');
  await copyAttempt(
    'writeText rejects',
    `(() => {
       Object.defineProperty(navigator, 'clipboard', {
         value: { writeText: () => Promise.reject(new DOMException('denied', 'NotAllowedError')) },
         configurable: true,
       });
       return true;
     })()`,
  );
  await evaluate(`(() => { delete navigator.clipboard; return true; })()`);
  await screenshot('13-copy-session-id');

  // Closed the way the reader would, so nothing is left over the list.
  await evaluate(`(() => {
    const close = document.querySelector('#sheet-host button[aria-label="Close"]');
    if (!close) throw new Error('the sheet has no close control');
    close.click();
    return true;
  })()`);
  await sleep(400);

  // --- the layout is not tuned to one phone ---------------------------------
  //
  // Everything above ran at one viewport, and a stylesheet can pass at 390px
  // while breaking at 360 — the width most Android phones actually have. This
  // does not re-run the suite; it re-checks the cheapest invariant that a
  // wrong-width layout violates first: nothing may overflow horizontally, so
  // the page must never be wider than the screen it is on. A single fixed width
  // in a header, a `min-width` on a table, or an unshrinkable model id in a
  // toolbar all show up here as a document wider than the viewport.
  const overflows = [];
  for (const size of [{ width: VIEWPORT_W, height: VIEWPORT_H, label: VIEWPORT }, { ...NARROW, label: '360x800' }]) {
    await send('Emulation.setDeviceMetricsOverride', {
      width: size.width,
      height: size.height,
      deviceScaleFactor: 1,
      mobile: true,
    });
    await sleep(250);
    const measured = await evaluate(`(() => {
      const doc = document.documentElement;
      const widest = [...document.querySelectorAll('body *')]
        .map((el) => Math.round(el.getBoundingClientRect().right))
        .reduce((a, b) => Math.max(a, b), 0);
      return { scrollWidth: doc.scrollWidth, clientWidth: doc.clientWidth, widest };
    })()`);
    if (measured.scrollWidth > measured.clientWidth) {
      overflows.push(
        `${size.label}: the document is ${measured.scrollWidth}px wide in a ` +
          `${measured.clientWidth}px viewport (widest element ends at ${measured.widest}px)`,
      );
    }
  }
  if (overflows.length > 0) {
    throw new Error(
      'the layout overflows horizontally, so part of the UI is off-screen:\n  ' +
        overflows.join('\n  '),
    );
  }
  step(28, `no horizontal overflow at ${VIEWPORT} or 360x800`);

  // Back to the phone the rest of the run used, so the final screenshots match.
  await send('Emulation.setDeviceMetricsOverride', {
    width: VIEWPORT_W,
    height: VIEWPORT_H,
    deviceScaleFactor: 1,
    mobile: true,
  });
  await sleep(200);

  // --- leave the desk as we found it ----------------------------------------
  //
  // This script creates sessions in the operator's own list — a tool-calling
  // turn, a headless run — and a test that litters is a test that makes the
  // next reading of the product worse. They are archived, not deleted: the run
  // that failed is sometimes the only evidence of why.
  if (madeHere.length > 0) {
    const r = await fetch(`${BASE}/api/v1/sessions/curate`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Origin: BASE, Cookie: `dsh_gw_session=${session.value}` },
      body: JSON.stringify({ ids: madeHere, archived: true }),
    });
    step(19, r.ok ? `tidied up after itself: ${madeHere.length} session(s) archived` : `cleanup failed: HTTP ${r.status}`);
  }

  // --- verdict --------------------------------------------------------------

  console.log('');
  if (consoleErrors.length > 0) {
    console.log(`FAILED: the page reported ${consoleErrors.length} console error(s):`);
    for (const e of consoleErrors.slice(0, 10)) console.log('   ! ' + e);
    process.exitCode = 1;
    return;
  }
  console.log('PASS: the real app drove the real gateway end to end, with no console errors.');
  console.log(`      screenshots in ${SHOTS}`);
}

main().then(finish, async (err) => {
  console.log(`\nFAILED: ${err.message}`);
  try { await screenshot('failure'); } catch {}
  console.log(`      screenshots in ${SHOTS}`);
  process.exitCode = 1;
  finish();
});
