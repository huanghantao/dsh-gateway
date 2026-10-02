/**
 * Screen 3 — Conversation. The screen the product exists for.
 *
 * Three decisions shape this file:
 *
 * 1. **Append, do not rebuild.** A turn emits a frame per tool and per message.
 *    The log keeps the nodes it already rendered and appends only the rows whose
 *    keys are new, so a hundred-frame turn costs a hundred appends rather than a
 *    hundred full re-renders. Prepending history ("load older") or replacing the
 *    feed after `resync` changes the key prefix, which falls back to a full
 *    rebuild — correct, and rare.
 * 2. **Scroll follows the reader, not the stream.** If the reader has scrolled up
 *    to read something, a new message must not yank them to the bottom. The
 *    "was at the bottom" test happens before the DOM changes.
 * 3. **The lease is surfaced, not hidden — but not at the cost of the screen.**
 *    While this screen holds the session, the desktop cannot open it, and that
 *    is a real cost of using the phone. It is also a cost that lasts for the
 *    whole of every working session, so it is a chip in the header rather than
 *    a banner: the explanation and the one-tap release are one tap away, and
 *    cost nothing until they are wanted.
 */

import type { Ctx } from "../actions.js";
import { el, on, pinAboveKeyboard, prettyJson, scrollToEnd } from "../dom.js";
import { formatTokens, modelLabel, relativeTime, shortWorkspace, utf8Length } from "../format.js";
import { renderMarkdown } from "../markdown.js";
import { toolDetail } from "../toolinfo.js";
import { openSheet, type Sheet } from "./ui.js";
import type { ActiveSession, AppStore } from "../store.js";
import type { FeedItem, HarnessStateData, Session, TokenUsage } from "../types.js";

/**
 * The contract bounds prompts by `limits.maxPromptBytes` but exposes no endpoint
 * that reports the value, so the composer enforces the shipped default. The
 * server remains the authority: exceeding it returns a problem document, which
 * the error strip shows verbatim.
 */
const MAX_PROMPT_BYTES = 256 * 1024;
const COUNTER_VISIBLE_AT = 0.7;
const COMPOSER_MIN_ROWS = 1;
const COMPOSER_MAX_ROWS = 8;
const NEAR_BOTTOM_PX = 48;

/* ------------------------------------------------------------------ rows */

function messageRow(item: Extract<FeedItem, { kind: "message" }>): HTMLElement {
  const article = el("article", { class: `msg msg-${item.role}` });

  const head = el(
    "header",
    { class: "msg-head" },
    el("span", { class: "msg-role", text: item.role === "user" ? "You" : "Assistant" }),
  );
  if (item.time !== null) {
    head.appendChild(el("time", { class: "msg-time", attrs: { datetime: item.time }, text: relativeTime(item.time) }));
  }
  if (item.role === "assistant" && item.model !== null) {
    head.appendChild(el("span", { class: "msg-model", text: item.model }));
  }
  article.appendChild(head);

  if (item.thinking !== null && item.thinking.trim() !== "") {
    article.appendChild(
      el(
        "details",
        { class: "thinking" },
        el("summary", { class: "thinking-summary", text: "Thinking" }),
        el("div", { class: "thinking-body" }, renderMarkdown(item.thinking)),
      ),
    );
  }

  const body = el("div", { class: "msg-body" });
  if (item.role === "user") {
    // User text is never markdown: it is what they typed, shown verbatim.
    body.appendChild(el("p", { class: "msg-plain", text: item.text }));
  } else {
    body.appendChild(renderMarkdown(item.text));
  }
  article.appendChild(body);

  if (item.usage !== null) {
    article.appendChild(usageLine(item.usage));
  }
  return article;
}

function usageLine(usage: TokenUsage): HTMLElement {
  const parts = [`${formatTokens(usage.inputTokens)} in`, `${formatTokens(usage.outputTokens)} out`];
  if (usage.totalTokens !== null) parts.push(`${formatTokens(usage.totalTokens)} total`);
  if (usage.contextWindow !== null) parts.push(`of ${formatTokens(usage.contextWindow)}`);
  return el("p", { class: "usage", text: parts.join(" · ") });
}

function toolRow(item: Extract<FeedItem, { kind: "tool" }>, workspace: string): HTMLElement {
  // What the call is doing, not just which tool it is: a path for an edit, the
  // agent's own description for a command. Empty for a tool with nothing worth
  // saying, in which case the card is the name and the status as before.
  const detail = toolDetail(item.tool, item.input, workspace);
  const summary = el(
    "summary",
    { class: "tool-summary" },
    el("span", { class: "tool-name", text: item.tool }),
    detail === "" ? null : el("span", { class: "tool-detail", text: detail }),
    el("span", {
      class: `tool-status tool-status-${item.open ? "running" : item.isError ? "error" : "ok"}`,
      text: item.open ? "running" : item.isError ? "failed" : "done",
    }),
  );

  const body = el("div", { class: "tool-body" });
  if (item.input !== null && item.input !== "") {
    body.appendChild(el("h3", { class: "tool-label", text: "Input" }));
    body.appendChild(el("pre", { class: "tool-pre", text: prettyJson(item.input) }));
  }
  if (item.output !== null && item.output !== "") {
    body.appendChild(el("h3", { class: "tool-label", text: "Output" }));
    body.appendChild(el("pre", { class: "tool-pre", text: item.output }));
  }
  if (item.output === null && !item.open) {
    body.appendChild(el("p", { class: "muted", text: "No output was recorded for this call." }));
  }

  const details = el(
    "details",
    { class: `tool tool-${item.isError ? "error" : item.open ? "running" : "ok"}` },
    summary,
    body,
  );
  // A running card is open so the reader can watch progress; a finished one is
  // collapsed so a long turn stays scannable. `<details open>` is an attribute,
  // not a style, so it is unaffected by the CSP.
  if (item.open) details.setAttribute("open", "");
  return details;
}

function noticeRow(item: Extract<FeedItem, { kind: "notice" }>): HTMLElement {
  return el(
    "p",
    { class: "notice" },
    item.time === null ? null : el("time", { class: "notice-time", attrs: { datetime: item.time }, text: relativeTime(item.time) }),
    item.text,
  );
}

function rowFor(item: FeedItem, workspace: string): HTMLElement {
  switch (item.kind) {
    case "message":
      return messageRow(item);
    case "tool":
      return toolRow(item, workspace);
    case "notice":
      return noticeRow(item);
  }
}

function commonPrefixLength(a: readonly FeedItem[], b: readonly FeedItem[]): number {
  const limit = Math.min(a.length, b.length);
  let index = 0;
  while (index < limit && a[index]?.key === b[index]?.key) index += 1;
  return index;
}

/* ---------------------------------------------------------------- banners */

function harnessBanner(harness: HarnessStateData): HTMLElement | null {
  if (harness.state === "ready") return null;
  const copy: Record<HarnessStateData["state"], string> = {
    starting: "The agent process is starting. Prompts will queue until it is ready.",
    restarting: "The agent process is restarting. The current turn may be interrupted.",
    failed: "The agent process is not running. Nothing can be prompted until it recovers.",
    ready: "",
  };
  const variant = harness.state === "failed" ? "alert-error" : "alert-warn";
  return el(
    "p",
    { class: `alert ${variant}`, attrs: { role: "status" } },
    el("strong", { text: `Agent ${harness.state}. ` }),
    harness.detail ?? copy[harness.state],
  );
}

/* ------------------------------------------------------------------ view */

/**
 * True when this turn belongs to this phone.
 *
 * `leased` is the distinction that matters. A turn frame says a turn is running,
 * not who is running it: the gateway reports both the sessions it drives (which
 * this phone holds a lease on) and, by following the log, the ones a desk
 * process is writing. Only the first can be cancelled from here, and only the
 * first should replace Send with Stop.
 *
 * A turn frame is live truth only while the client holds one. The stream carries
 * no snapshot of a turn already in flight — the `running` frame was published
 * before this page loaded — so a reload lands on a session the gateway is
 * actively running with no frame to say so. The session's own `busy` flag is the
 * snapshot that covers that gap: it is the same fact `POST /prompt` checks
 * before refusing a second prompt with `prompt_in_flight`, and the same one the
 * session list renders as "running". It is consulted only while there is no
 * frame, because a frame is newer than any snapshot a metadata fetch can carry.
 */
function runningHere(active: ActiveSession | null): boolean {
  if (active === null || active.session?.leased !== true) return false;
  if (active.turn !== null) return active.turn.state === "running";
  return active.session.busy;
}

/**
 * True when someone else is running this session — the desktop, a headless run
 * — rather than this phone.
 *
 * The reader has to be told, because otherwise a conversation being written
 * somewhere else looks frozen on the one screen where they are watching it, and
 * the composer has to stay out of the way, because a prompt sent now would be
 * refused while another writer owns the session.
 */
function runningElsewhere(active: ActiveSession | null): boolean {
  if (active === null || active.session?.leased === true) return false;
  return active.turn?.state === "running" || active.session?.busy === true;
}

export function mountConversation(root: HTMLElement, ctx: Ctx): () => void {
  const store: AppStore = ctx.store;

  const title = el("h1", { class: "conv-title", text: "Session" });
  const subtitle = el("p", { class: "conv-sub" });

  const back = el("button", {
    class: "btn btn-ghost btn-icon",
    attrs: { type: "button", "aria-label": "Back to sessions" },
    text: "\u2039",
  });
  /**
   * The lease chip.
   *
   * The hold is taken the moment this phone sends a prompt, and it lingers for
   * minutes after the last activity — so the banner it replaces was on screen
   * for the whole of every working session, which is exactly when the reader
   * can least afford 200px of it. A 30px pill in the header says the desktop is
   * locked out; the explanation and the release button live behind it, where
   * they cost nothing until they are wanted.
   */
  const leaseChip = el("button", {
    class: "lease-chip",
    attrs: { type: "button", "aria-haspopup": "dialog", hidden: "" },
    text: "Held",
  });

  const head = el(
    "header",
    { class: "conv-head" },
    back,
    el("div", { class: "conv-head-main" }, title, subtitle),
    leaseChip,
  );

  const banners = el("div", { class: "banners" });
  const loadOlder = el("button", { class: "btn btn-ghost btn-block", attrs: { type: "button" }, text: "Load older messages" });
  const olderWrap = el("div", { class: "older" }, loadOlder);
  const log = el("div", { class: "log", attrs: { role: "log", "aria-live": "polite", "aria-relevant": "additions text" } });
  const turnStatus = el("p", { class: "turn-status", attrs: { role: "status", hidden: "" } });

  /* --------------------------------------------------------- composer */

  const textarea = el("textarea", {
    class: "composer-input",
    attrs: {
      rows: String(COMPOSER_MIN_ROWS),
      placeholder: "Ask the agent…",
      "aria-label": "Message",
      autocomplete: "off",
      autocapitalize: "sentences",
      enterkeyhint: "enter",
    },
  });

  const counter = el("p", { class: "counter", attrs: { "aria-live": "polite" } });

  const send = el("button", { class: "btn btn-primary composer-send", attrs: { type: "submit" }, text: "Send" });
  const stop = el("button", {
    class: "btn btn-danger composer-stop",
    attrs: { type: "button", hidden: "" },
    text: "Stop",
  });

  const composer = el("form", { class: "composer", attrs: { novalidate: "" } }, textarea, send, stop);
  const composerWrap = el("div", { class: "composer-wrap" }, counter, composer);

  const view = el("section", { class: "view view-conversation" }, head, banners, olderWrap, log, turnStatus, composerWrap);
  root.appendChild(view);

  let rendered: readonly FeedItem[] = [];
  let activeSnapshot: ActiveSession | null = null;

  /**
   * The session's root, used to shorten the paths in tool summaries. Read at
   * render time rather than captured, because the header's session arrives a
   * moment after the feed does.
   */
  const workspace = (): string => store.state.active?.session?.workspace ?? "";

  const atBottom = (): boolean => log.scrollTop + log.clientHeight >= log.scrollHeight - NEAR_BOTTOM_PX;

  /** Replaces the whole log; used on first paint, `resync` and "load older". */
  const rebuildLog = (feed: readonly FeedItem[]): void => {
    const stick = atBottom();
    log.replaceChildren(...feed.map((item) => rowFor(item, workspace())));
    rendered = feed;
    if (stick) scrollToEnd(log);
  };

  const syncLog = (feed: readonly FeedItem[]): void => {
    const common = commonPrefixLength(rendered, feed);
    if (common === 0 && rendered.length > 0 && feed.length > 0) {
      rebuildLog(feed);
      return;
    }
    const stick = atBottom();
    while (log.childElementCount > common) log.lastElementChild?.remove();
    for (let index = common; index < feed.length; index += 1) {
      const item = feed[index];
      if (item !== undefined) log.appendChild(rowFor(item, workspace()));
    }
    rendered = feed;
    if (stick) scrollToEnd(log);
  };

  /* ------------------------------------------------------------ composer */

  const promptBytes = (): number => utf8Length(textarea.value);

  const syncComposer = (active: ActiveSession | null): void => {
    const running = runningHere(active);
    // A prompt would be refused while another writer owns the session, so the
    // button says so instead of failing after the fact.
    const held = runningElsewhere(active);
    const bytes = promptBytes();
    const over = bytes > MAX_PROMPT_BYTES;

    if (over) {
      counter.textContent = `Message is ${Math.round(bytes / 1024)} KiB; the limit is ${MAX_PROMPT_BYTES / 1024} KiB.`;
      counter.className = "counter is-over";
    } else if (bytes / MAX_PROMPT_BYTES >= COUNTER_VISIBLE_AT) {
      counter.textContent = `${Math.round(bytes / 1024)} KiB of ${MAX_PROMPT_BYTES / 1024} KiB`;
      counter.className = "counter is-warn";
    } else {
      counter.textContent = "";
      counter.className = "counter";
    }

    send.disabled = running || held || over || textarea.value.trim() === "";
    stop.hidden = !running;
    send.hidden = running;
    // `field-sizing: content` handles growth on current browsers; `rows` is the
    // fallback for the rest. Neither needs a style attribute, which the CSP
    // forbids.
    const lines = textarea.value.split("\n").length;
    textarea.rows = Math.min(COMPOSER_MAX_ROWS, Math.max(COMPOSER_MIN_ROWS, lines));
  };

  const submitPrompt = (): void => {
    const text = textarea.value.trim();
    if (text === "" || promptBytes() > MAX_PROMPT_BYTES) return;
    const active = store.state.active;
    if (active === null || runningHere(active) || runningElsewhere(active)) return;
    // The composer is cleared only once the server accepts the prompt, so a
    // `409 prompt_in_flight` leaves the text where the user can resend it.
    void ctx.sendPrompt(text).then(() => {
      const current = store.state.active;
      if (current !== null && current.promptError === null) {
        textarea.value = "";
        syncComposer(current);
      }
    });
  };

  /* -------------------------------------------------------------- wiring */

  const offBack = on(back, "click", () => {
    ctx.navigate({ kind: "sessions" });
  });

  const offOlder = on(loadOlder, "click", () => {
    void ctx.loadOlder();
  });

  const offStop = on(stop, "click", () => {
    void ctx.cancelTurn();
  });

  const offInput = on(textarea, "input", () => {
    syncComposer(store.state.active);
  });

  const offKey = on(textarea, "keydown", (event) => {
    // Enter inserts a newline on a phone keyboard; on a hardware keyboard
    // Cmd/Ctrl+Enter sends, which is the convention people already know.
    if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      submitPrompt();
    }
  });

  const offSubmit = on(composer, "submit", (event) => {
    event.preventDefault();
    submitPrompt();
  });

  /**
   * The lease sheet: everything the removed banner used to say, one tap away
   * instead of permanently on screen.
   *
   * `leaseSheet` is held in a closure rather than rebuilt per render so that a
   * release in flight — and the error it may come back with — updates the sheet
   * the operator is actually looking at.
   */
  let leaseSheet: { sheet: Sheet; error: HTMLElement; button: HTMLButtonElement } | null = null;

  const syncLeaseSheet = (active: ActiveSession | null): void => {
    if (leaseSheet === null) return;
    const releasing = active?.releasing === true;
    const message = active?.releaseError ?? null;
    leaseSheet.error.textContent = message ?? "";
    leaseSheet.error.hidden = message === null;
    leaseSheet.button.disabled = releasing;
    leaseSheet.button.textContent = releasing ? "Releasing…" : "Release session";
    // Nothing is left to offer once the hold is gone, and leaving the sheet up
    // would make the operator dismiss a dialog about a condition that ended.
    if (active?.session?.leased !== true && !releasing) leaseSheet.sheet.close();
  };

  const openLeaseSheet = (): void => {
    if (leaseSheet !== null) return;
    const error = el("p", { class: "alert alert-error", attrs: { role: "alert", hidden: "" } });
    const release = el("button", {
      class: "btn btn-danger btn-block",
      attrs: { type: "button" },
      text: "Release session",
    });
    on(release, "click", () => {
      void ctx.releaseSession();
    });
    const body = el(
      "div",
      {},
      el("p", {
        text: "This phone is holding the session, so the desktop cannot open it until you release it.",
      }),
      el("p", {
        class: "muted",
        text: "The hold is dropped automatically a few minutes after you leave this screen. Releasing now hands it straight back.",
      }),
      error,
      release,
    );
    const sheet = openSheet({
      title: "Held by this phone",
      body,
      onClose: () => {
        leaseSheet = null;
      },
    });
    leaseSheet = { sheet, error, button: release };
    syncLeaseSheet(activeSnapshot);
  };

  const offLease = on(leaseChip, "click", () => {
    openLeaseSheet();
  });

  const unpin = pinAboveKeyboard(composerWrap);

  /* ------------------------------------------------------------ render */

  const renderHead = (session: Session | null, names: ReadonlyMap<string, string>): void => {
    title.textContent = session?.title ?? "Session";
    const parts: string[] = [];
    if (session !== null) {
      parts.push(shortWorkspace(session.workspace));
      parts.push(modelLabel(session.model, names));
    }
    subtitle.textContent = parts.join(" · ");
    subtitle.hidden = parts.length === 0;
    // The chip carries the lease, so the subtitle does not repeat it.
    leaseChip.hidden = session?.leased !== true;
  };

  const renderBanners = (active: ActiveSession | null, harness: HarnessStateData): void => {
    const nodes: HTMLElement[] = [];

    const harnessNode = harnessBanner(harness);
    if (harnessNode !== null) nodes.push(harnessNode);

    if (active?.unsupported === true) {
      nodes.push(
        el("p", {
          class: "alert alert-warn",
          attrs: { role: "status" },
          text: "This session's history was written by a newer version of DSH than the gateway understands. Open the desktop app to read it; new messages still appear below.",
        }),
      );
    }
    if (active !== null && active.historyError !== null) {
      nodes.push(el("p", { class: "alert alert-error", attrs: { role: "alert" }, text: active.historyError }));
    }
    if (active !== null && active.promptError !== null) {
      nodes.push(el("p", { class: "alert alert-error", attrs: { role: "alert" }, text: active.promptError }));
    }
    // A failed release is rare and momentary, so it gets one line here rather
    // than the full explanation, which stays in the sheet that caused it.
    if (active !== null && active.releaseError !== null) {
      nodes.push(el("p", { class: "alert alert-error", attrs: { role: "alert" }, text: active.releaseError }));
    }
    banners.replaceChildren(...nodes);
  };

  const renderUsage = (active: ActiveSession | null): void => {
    const usage = active?.usage ?? null;
    const running = runningHere(active);
    const elsewhere = runningElsewhere(active);
    if (usage === null && !running && !elsewhere) {
      turnStatus.hidden = true;
      turnStatus.textContent = "";
      return;
    }
    const counts =
      usage === null ? "" : `${formatTokens(usage.inputTokens)} in · ${formatTokens(usage.outputTokens)} out`;
    const note = running ? " · working…" : elsewhere ? " · working in another client" : "";
    if (counts === "") {
      turnStatus.textContent = running ? "Working…" : "The agent is working in another client.";
    } else {
      turnStatus.textContent = `${counts}${note}`;
    }
    turnStatus.hidden = false;
  };

  const render = (active: ActiveSession | null): void => {
    const names = new Map(store.state.models.models.map((option) => [option.id, option.name]));
    renderHead(active?.session ?? null, names);
    renderBanners(active, store.state.harness);
    syncLeaseSheet(active);

    if (active === null) {
      loadOlder.hidden = true;
      return;
    }

    if (active.feed !== rendered) syncLog(active.feed);

    loadOlder.hidden = active.nextBefore === null;
    loadOlder.disabled = active.loadingOlder;
    loadOlder.textContent = active.loadingOlder ? "Loading…" : "Load older messages";

    renderUsage(active);
    syncComposer(active);
  };

  const offActive = store.select(
    (state) => state.active,
    (active) => {
      activeSnapshot = active;
      render(active);
    },
  );

  const offHarness = store.select(
    (state) => state.harness,
    () => {
      render(activeSnapshot);
    },
  );

  const offModels = store.select(
    (state) => state.models,
    () => {
      render(activeSnapshot);
    },
  );

  // The shell opens the session before mounting this view, and the subscription
  // above has already painted it. The guard covers the one ordering that can
  // slip through: a direct hit on the URL while bootstrap was still resolving.
  const sessionId = store.state.route.kind === "conversation" ? store.state.route.sessionId : null;
  if (sessionId !== null && store.state.active?.sessionId !== sessionId) {
    void ctx.openSession(sessionId);
  }

  return () => {
    offActive();
    offHarness();
    offModels();
    offBack();
    offOlder();
    offStop();
    offInput();
    offKey();
    offSubmit();
    offLease();
    leaseSheet?.sheet.close();
    unpin();
    view.remove();
  };
}
