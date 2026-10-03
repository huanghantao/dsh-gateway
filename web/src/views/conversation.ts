/**
 * Screen 3 — Conversation. The screen the product exists for.
 *
 * Four decisions shape this file:
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
 * 4. **The composer is not the thing that refuses.** A turn in flight is a
 *    reason to wait, not a reason to disable the box the reader is typing in:
 *    on a deployment that queues, Send stays exactly where the thumb left it
 *    and the strip above says what is waiting. The two refusals this screen
 *    does make — a session another client holds, an image past the deployment's
 *    cap — are facts it can know before it sends, and it says so instead of
 *    letting a round trip say it.
 */

import type { Ctx, OutgoingImage } from "../actions.js";
import { el, on, pinAboveKeyboard, prettyJson, scrollToEnd } from "../dom.js";
import { formatTokens, modelLabel, relativeTime, shortWorkspace, utf8Length } from "../format.js";
import { renderMarkdown } from "../markdown.js";
import { describeTool, toolDetail } from "../toolinfo.js";
import { openChangesSheet } from "./changes.js";
import { openSheet, type Sheet } from "./ui.js";
import type { ActiveSession, AppStore } from "../store.js";
import type { FeedItem, HarnessStateData, Session, TokenUsage, Turn } from "../types.js";

/**
 * The composer's fallback bound, used only until `GET /me` answers.
 *
 * The deployment reports its own `limits.maxPromptBytes`, and that is what the
 * counter and the over-limit refusal use — an operator who raised the limit
 * should not be told their message is too long by a client that hardcoded the
 * shipped default. The server stays the authority either way: a prompt that
 * exceeds the real limit comes back as a problem document, which the error strip
 * shows verbatim.
 */
const FALLBACK_MAX_PROMPT_BYTES = 256 * 1024;
const COUNTER_VISIBLE_AT = 0.7;
const COMPOSER_MIN_ROWS = 1;
const COMPOSER_MAX_ROWS = 8;
const NEAR_BOTTOM_PX = 48;

/**
 * The box a picked image is fitted into before it is encoded.
 *
 * 1568px on the longer edge is the size current vision models are happiest
 * with, and it lands a phone photograph at a few hundred kilobytes — small
 * enough to fit under any sane per-image cap, sharp enough that the text in a
 * screenshot is still text. Upscaling is never worth the bytes.
 */
const MAX_IMAGE_EDGE = 1568;

/** High enough that a photographed document stays readable, low enough to matter. */
const JPEG_QUALITY = 0.85;

/** The elapsed timer's beat. A turn's age does not change faster than this. */
const TICK_MS = 1000;

/**
 * One image on its way out, plus what it takes to show it.
 *
 * `data` is base64 with no `data:` prefix, which is what the contract carries.
 * `bytes` is the decoded size of that payload — the number a byte limit is
 * actually about, and a third smaller than the string that encodes it.
 */
interface PendingImage {
  readonly mimeType: string;
  readonly data: string;
  readonly bytes: number;
  /** A data URL for the thumbnail: the base64 above, dressed for an `<img>`. */
  readonly preview: string;
}

/** An image the composer holds, with the thumbnail that stands for it. */
interface Attachment {
  readonly image: PendingImage;
  readonly thumb: HTMLElement;
}

/** A decoded image, and the handle that frees whatever decoding it held. */
interface DecodedImage {
  readonly source: CanvasImageSource;
  readonly width: number;
  readonly height: number;
  readonly release: () => void;
}

/**
 * Decodes a picked file into something drawable.
 *
 * `createImageBitmap` is the fast path, and the one that never needs an object
 * URL. It is not everywhere, and it refuses some files outright — a HEIC on an
 * older iOS, a truncated download — so the `<img>` route is a fallback rather
 * than a nicety. Both are freed by the caller through `release`.
 */
async function decodeImage(file: File): Promise<DecodedImage> {
  if (typeof createImageBitmap === "function") {
    try {
      const bitmap = await createImageBitmap(file);
      // `close` hands the decoded pixels back now instead of at the next
      // collection. Four photographs held at once is a real amount of a phone's
      // memory, so it is worth being explicit about.
      return { source: bitmap, width: bitmap.width, height: bitmap.height, release: () => bitmap.close() };
    } catch {
      // Fall through to the element route.
    }
  }
  const url = URL.createObjectURL(file);
  const image = new Image();
  try {
    await new Promise<void>((resolve, reject) => {
      image.onload = () => resolve();
      image.onerror = () => reject(new Error("the image could not be decoded"));
      image.src = url;
    });
  } catch (error) {
    URL.revokeObjectURL(url);
    throw error;
  }
  return {
    source: image,
    width: image.naturalWidth,
    height: image.naturalHeight,
    release: () => URL.revokeObjectURL(url),
  };
}

/**
 * The base64 of a blob, without the `data:` prefix the contract does not carry.
 *
 * `FileReader` rather than hand-rolled `btoa`: the usual
 * `String.fromCharCode(...bytes)` runs past the argument limit somewhere around
 * a hundred kilobytes, which every phone photograph exceeds.
 */
async function base64Of(blob: Blob): Promise<string> {
  const dataUrl = await new Promise<string>((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(typeof reader.result === "string" ? reader.result : "");
    reader.onerror = () => reject(new Error("the image could not be read"));
    reader.readAsDataURL(blob);
  });
  const comma = dataUrl.indexOf(",");
  if (comma === -1) throw new Error("the image could not be read");
  return dataUrl.slice(comma + 1);
}

/**
 * Re-encodes one picked file as a bounded JPEG.
 *
 * The file's own type is not trusted. iOS hands the photo library's HEIC over
 * as something the gateway refuses, and a `File` can claim anything at all —
 * so the answer is not to check the type but to make the question moot:
 * whatever went in, a JPEG of a known size comes out.
 */
async function prepareImage(file: File): Promise<PendingImage> {
  const decoded = await decodeImage(file);
  try {
    if (decoded.width === 0 || decoded.height === 0) throw new Error("the image has no pixels");
    const scale = Math.min(1, MAX_IMAGE_EDGE / Math.max(decoded.width, decoded.height));
    const width = Math.max(1, Math.round(decoded.width * scale));
    const height = Math.max(1, Math.round(decoded.height * scale));

    const canvas = el("canvas");
    canvas.width = width;
    canvas.height = height;
    const context = canvas.getContext("2d");
    if (context === null) throw new Error("the image could not be prepared");
    // JPEG has no alpha channel, and the encoder's answer for transparent
    // pixels is black. A PNG screenshot would come back with a black
    // background; painting white underneath is what keeps it looking like
    // itself.
    context.fillStyle = "#ffffff";
    context.fillRect(0, 0, width, height);
    context.drawImage(decoded.source, 0, 0, width, height);

    const blob = await new Promise<Blob | null>((resolve) => {
      canvas.toBlob(resolve, "image/jpeg", JPEG_QUALITY);
    });
    if (blob === null) throw new Error("the image could not be encoded");
    const data = await base64Of(blob);
    return { mimeType: "image/jpeg", data, bytes: blob.size, preview: `data:image/jpeg;base64,${data}` };
  } finally {
    decoded.release();
  }
}

/**
 * A duration as a reader says it: `12s`, `1m 04s`, `1h 02m`.
 *
 * Minutes and seconds are zero-padded so the figure does not change width as it
 * ticks; a status line that jitters once a second is a status line that pulls
 * the eye every second.
 */
function formatElapsed(seconds: number): string {
  const total = Math.max(0, Math.floor(seconds));
  if (total < 60) return `${total}s`;
  if (total < 3600) return `${Math.floor(total / 60)}m ${String(total % 60).padStart(2, "0")}s`;
  return `${Math.floor(total / 3600)}h ${String(Math.floor((total % 3600) / 60)).padStart(2, "0")}m`;
}

/** A byte count the way this file's counter already writes them. */
function formatBytes(bytes: number): string {
  if (bytes >= 1024 * 1024) {
    const mib = bytes / (1024 * 1024);
    return `${Number.isInteger(mib) ? mib : mib.toFixed(1)} MiB`;
  }
  return `${Math.max(1, Math.round(bytes / 1024))} KiB`;
}

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

  /**
   * The way into what the session changed.
   *
   * It sits in the header rather than in the transcript because the answer is
   * not a row: it is the whole turn's edits collected, which is what the reader
   * wants when they look up from a phone and ask what the agent actually did.
   * With no session loaded there is nothing to collect, so it is not shown.
   */
  const changesChip = el("button", {
    class: "btn btn-ghost btn-icon changes-chip",
    attrs: { type: "button", "aria-label": "What changed", hidden: "" },
    text: "\u00b1",
  });

  const head = el(
    "header",
    { class: "conv-head" },
    back,
    el("div", { class: "conv-head-main" }, title, subtitle),
    changesChip,
    leaseChip,
  );

  const banners = el("div", { class: "banners" });
  const loadOlder = el("button", { class: "btn btn-ghost btn-block", attrs: { type: "button" }, text: "Load older messages" });
  const olderWrap = el("div", { class: "older" }, loadOlder);
  const log = el("div", { class: "log", attrs: { role: "log", "aria-live": "polite", "aria-relevant": "additions text" } });
  /**
   * The status line is three nodes rather than one string because its middle is
   * rewritten once a second while the rest of it changes only when the store
   * does.
   *
   * That middle is also the one part held out of the accessibility tree. It
   * sits in a `role="status"` region, and a value that changes every second in
   * one of those is announced every second — which is noise, not a status. The
   * transcript beside it is where a reader who cannot see the clock learns what
   * happened.
   */
  const statusStep = el("span");
  const statusElapsed = el("span", { attrs: { "aria-hidden": "true" } });
  const statusUsage = el("span");
  const turnStatus = el("p", { class: "turn-status", attrs: { role: "status", hidden: "" } }, statusStep, statusElapsed, statusUsage);

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

  /**
   * The attach control, and the picker behind it.
   *
   * A file input cannot be dressed as a composer button without a stylesheet
   * this file does not own, so the input stays hidden and a real button opens
   * it — synchronously, inside the tap, which is the only way iOS will show the
   * picker at all. Both are inserted into the composer only for a deployment
   * whose harness takes images: a control that can produce nothing but an error
   * is worse than no control.
   */
  const picker = el("input", {
    attrs: {
      type: "file",
      accept: "image/png,image/jpeg,image/webp,image/gif",
      multiple: "",
      hidden: "",
      tabindex: "-1",
    },
  });
  const attach = el("button", {
    class: "btn btn-ghost btn-icon attach-button",
    attrs: { type: "button", "aria-label": "Attach an image" },
    text: "\u{1F4CE}",
  });

  const queueStrip = el("div", { class: "queue-strip", attrs: { hidden: "" } });
  const attachStrip = el("div", { class: "attach-strip", attrs: { hidden: "" } });

  const send = el("button", { class: "btn btn-primary composer-send", attrs: { type: "submit" }, text: "Send" });
  const stop = el("button", {
    class: "btn btn-danger composer-stop",
    attrs: { type: "button", hidden: "" },
    text: "Stop",
  });

  // Stop joins the composer rather than replacing anything in it, so Send keeps
  // the place a thumb already knows even while a turn is running. The two can
  // be on screen together now, and the one that moves under a thumb is the one
  // that gets pressed by mistake.
  const composer = el("form", { class: "composer", attrs: { novalidate: "" } }, textarea, send, stop);
  const composerWrap = el("div", { class: "composer-wrap" }, queueStrip, attachStrip, counter, composer, picker);

  const view = el("section", { class: "view view-conversation" }, head, banners, olderWrap, log, turnStatus, composerWrap);
  root.appendChild(view);

  let rendered: readonly FeedItem[] = [];
  let activeSnapshot: ActiveSession | null = null;
  /** What the composer is holding, in the order it was picked. */
  let attached: readonly Attachment[] = [];
  /** A refusal decided on this phone; see `renderBanners`. */
  let attachError: string | null = null;
  /** The elapsed timer's handle; zero while no turn is running. */
  let ticker = 0;
  /** Identity of the queue strip's contents, so an unchanged queue is not rebuilt. */
  let queueKey: string | null = null;

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

  /**
   * The cap a single image has to fit.
   *
   * `maxImageBytes` is the deployment's per-image bound, and zero means it is
   * unset — at which point the request body is the bound that applies, because
   * a prompt carrying one image cannot exceed that either. Zero for both means
   * this deployment has named no such bound, and the server stays the authority
   * it has always been.
   */
  const imageByteCap = (): number => {
    const limits = store.state.limits;
    return limits.maxImageBytes > 0 ? limits.maxImageBytes : limits.maxBodyBytes;
  };

  /** Whether a prompt sent mid-turn is queued rather than refused. */
  const queueOffered = (): boolean => store.state.features.promptQueueDepth > 0;

  /**
   * A refusal decided here rather than by the server.
   *
   * It does not go into `active.promptError`: that field reports what the
   * gateway said about a prompt it saw, and this prompt never left the phone.
   * It is rendered beside that banner, which is where the reader is already
   * looking when a send does not happen.
   */
  const showAttachError = (message: string | null): void => {
    attachError = message;
    renderBanners(activeSnapshot, store.state.harness);
  };

  /** Puts one prepared image in the strip, wired to the button that removes it. */
  const addAttachment = (image: PendingImage): void => {
    const remove = el("button", {
      class: "attach-remove",
      attrs: { type: "button", "aria-label": "Remove image" },
      text: "\u00d7",
    });
    const thumb = el(
      "div",
      { class: "attach-thumb" },
      // The preview is the image's own base64: a data URL costs no object URL
      // to manage, and there is nothing to revoke when the thumb goes away.
      el("img", { attrs: { src: image.preview, alt: "" } }),
      remove,
    );
    on(remove, "click", () => {
      thumb.remove();
      attached = attached.filter((entry) => entry.thumb !== thumb);
      attachStrip.hidden = attached.length === 0;
      // Whatever the banner was about, it was about an image that has just
      // gone; leaving it up would have the reader fixing a fixed problem.
      showAttachError(null);
      syncComposer(store.state.active);
    });
    attached = [...attached, { image, thumb }];
    attachStrip.appendChild(thumb);
    attachStrip.hidden = false;
  };

  const clearAttachments = (): void => {
    attached = [];
    attachStrip.replaceChildren();
    attachStrip.hidden = true;
  };

  /**
   * Re-encodes what the picker returned, one file at a time.
   *
   * Sequential rather than concurrent: four 12-megapixel photographs decoded at
   * once is how a phone runs out of memory while its owner watches a frozen
   * screen. Nothing is accepted or refused until it has been downscaled and
   * measured, because the size that matters is the size that would be sent.
   */
  const addImages = async (files: readonly File[]): Promise<void> => {
    showAttachError(null);
    for (const file of files) {
      const name = file.name === "" ? "That image" : file.name;
      const cap = imageByteCap();
      let image: PendingImage;
      try {
        image = await prepareImage(file);
      } catch {
        showAttachError(`${name} could not be read as an image.`);
        continue;
      }
      if (cap > 0 && image.bytes > cap) {
        showAttachError(
          `${name} is ${formatBytes(image.bytes)} after downscaling, over this deployment's ` +
            `${formatBytes(cap)} per-image limit.`,
        );
        continue;
      }
      addAttachment(image);
    }
    syncComposer(store.state.active);
  };

  /**
   * Offers the attach control only where the harness takes images, and drops
   * anything already attached when it stops doing so: an image the deployment
   * cannot accept must not be left sitting in the composer as something to
   * send.
   */
  const syncAttach = (active: ActiveSession | null): void => {
    const offered = store.state.features.imagePrompts;
    const mounted = attach.parentNode !== null;
    if (offered && !mounted) {
      composer.insertBefore(attach, textarea);
      composerWrap.appendChild(picker);
    } else if (!offered && mounted) {
      attach.remove();
      picker.remove();
      clearAttachments();
      showAttachError(null);
    }
    // Same rule as Send: while another client owns the session there is nothing
    // to attach an image *to*, so the button does not invite the work.
    attach.disabled = runningElsewhere(active);
  };

  /**
   * The queue, above the composer.
   *
   * A row is labelled by where it stands, never by what it says: the gateway's
   * queue is a list of admitted prompts and carries no text, and inventing a
   * summary of a prompt nobody can read would be worse than admitting that.
   * Rebuilding is keyed on the queue's identity so frames that only moved the
   * feed do not churn the buttons under a thumb.
   */
  const syncQueue = (active: ActiveSession | null): void => {
    const queue = active?.queue ?? [];
    const waiting = queue.length;
    const depth = store.state.features.promptQueueDepth;
    const key = `${depth}|${queue.map((turn) => `${turn.turnId}@${turn.position}`).join(",")}`;
    if (key === queueKey) return;
    queueKey = key;

    const rows = queue.map((turn, index) => {
      const place = turn.position > 0 ? turn.position : index + 1;
      const drop = el("button", {
        class: "queue-drop",
        attrs: { type: "button", "aria-label": "Drop this waiting prompt" },
        text: "\u00d7",
      });
      on(drop, "click", () => {
        void ctx.dropQueued(turn.turnId);
      });
      return el(
        "div",
        { class: "queue-item" },
        el("span", { class: "queue-position", text: String(place) }),
        el("span", {
          class: "queue-text",
          // Counting is only worth a sentence when there is more than one thing
          // to count and the deployment can hold more than one; "1 of 1 waiting"
          // says less than the plain sentence does.
          text:
            waiting > 1 && depth > 1
              ? `${place} of ${waiting} waiting`
              : "Waiting · runs when the current turn finishes",
        }),
        drop,
      );
    });
    queueStrip.replaceChildren(...rows);
    queueStrip.hidden = rows.length === 0;
  };

  /**
   * The prompt bound this deployment reports, or the shipped default until
   * `GET /me` has answered.
   */
  const maxPromptBytes = (): number => {
    const reported = ctx.store.state.limits.maxPromptBytes;
    return reported > 0 ? reported : FALLBACK_MAX_PROMPT_BYTES;
  };

  const syncComposer = (active: ActiveSession | null): void => {
    const running = runningHere(active);
    // A prompt would be refused while another writer owns the session, so the
    // button says so instead of failing after the fact.
    const held = runningElsewhere(active);
    const bytes = promptBytes();
    const limit = maxPromptBytes();
    const over = bytes > limit;
    const empty = textarea.value.trim() === "" && attached.length === 0;

    if (over) {
      counter.textContent = `Message is ${Math.round(bytes / 1024)} KiB; the limit is ${Math.round(limit / 1024)} KiB.`;
      counter.className = "counter is-over";
    } else if (bytes / limit >= COUNTER_VISIBLE_AT) {
      counter.textContent = `${Math.round(bytes / 1024)} KiB of ${Math.round(limit / 1024)} KiB`;
      counter.className = "counter is-warn";
    } else {
      counter.textContent = "";
      counter.className = "counter";
    }

    // A turn running *here* is no longer a refusal: the gateway queues the
    // prompt and the strip above the composer shows it waiting. Only a
    // deployment with no queue at all still answers `prompt_in_flight`, and
    // only there is the old swap — Send gone, Stop in its place — the truth.
    //
    // Mid-turn the button is answering a second question as well — queued, or
    // refused? — and that answer does not depend on whether anything has been
    // typed yet. So an empty draft disables Send only at rest, where sending
    // what is in the box is the button's whole job.
    const queueing = queueOffered();
    send.disabled = held || over || (running && !queueing) || (!running && empty);
    send.hidden = running && !queueing;
    stop.hidden = !running;
    // `field-sizing: content` handles growth on current browsers; `rows` is the
    // fallback for the rest. Neither needs a style attribute, which the CSP
    // forbids.
    const lines = textarea.value.split("\n").length;
    textarea.rows = Math.min(COMPOSER_MAX_ROWS, Math.max(COMPOSER_MIN_ROWS, lines));
  };

  const submitPrompt = (): void => {
    const text = textarea.value.trim();
    const images: readonly OutgoingImage[] = attached.map((entry) => entry.image);
    // Images alone are a prompt. A screenshot with no caption is the common
    // case, and demanding a sentence to send one would be this screen inventing
    // a rule the contract does not have.
    if (text === "" && images.length === 0) return;
    if (promptBytes() > maxPromptBytes()) return;
    const active = store.state.active;
    if (active === null || runningElsewhere(active)) return;
    if (runningHere(active) && !queueOffered()) return;
    // The composer is cleared only once the server accepts the prompt, so a
    // `409 prompt_in_flight` leaves the text — and the images — where the user
    // can resend them.
    void ctx.sendPrompt(text, images).then(() => {
      const current = store.state.active;
      if (current !== null && current.promptError === null) {
        textarea.value = "";
        clearAttachments();
        showAttachError(null);
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

  const offAttach = on(attach, "click", () => {
    // Synchronously, inside the tap: a picker opened from a promise is a picker
    // iOS refuses to show.
    picker.click();
  });

  const offPicked = on(picker, "change", () => {
    const files = Array.from(picker.files ?? []);
    // Cleared before the files are read. A file input reports no change when
    // the same file is chosen twice, so without this the second attempt at the
    // same photograph would silently do nothing.
    picker.value = "";
    if (files.length > 0) void addImages(files);
  });

  const offChanges = on(changesChip, "click", () => {
    const session = store.state.active?.session ?? null;
    if (session !== null) openChangesSheet(ctx, session);
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
    changesChip.hidden = session === null;
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
    // A refusal made here, beside the one the server made there: they are the
    // same kind of news to the reader, told in the same place.
    if (attachError !== null) {
      nodes.push(el("p", { class: "alert alert-error", attrs: { role: "alert" }, text: attachError }));
    }
    // A failed release is rare and momentary, so it gets one line here rather
    // than the full explanation, which stays in the sheet that caused it.
    if (active !== null && active.releaseError !== null) {
      nodes.push(el("p", { class: "alert alert-error", attrs: { role: "alert" }, text: active.releaseError }));
    }
    banners.replaceChildren(...nodes);
  };

  /**
   * What the turn is doing right now.
   *
   * The last tool card still marked open is the call the agent is inside. The
   * feed is append-only, so the scan starts at the end and normally stops on
   * the first row; a settled turn has no open card and gets no step at all,
   * rather than the name of a tool that finished minutes ago.
   */
  const currentStep = (): string => {
    const feed = store.state.active?.feed ?? [];
    for (let index = feed.length - 1; index >= 0; index -= 1) {
      const item = feed[index];
      if (item === undefined || item.kind !== "tool" || !item.open) continue;
      const headline = describeTool(item.tool, item.input, workspace()).headline;
      return headline === "" ? item.tool : `${item.tool} · ${headline}`;
    }
    return "";
  };

  /**
   * How long the turn has been running, in the terms a reader would use.
   *
   * Anchored to the server's `startedAt` rather than to the moment this page
   * first saw the turn: a reload in the middle of a long turn must not restart
   * the clock at zero. A timestamp that is missing or unreadable yields no
   * figure at all, because a wrong figure is worse than none.
   */
  const elapsedLabel = (turn: Turn | null): string => {
    if (turn === null || turn.startedAt === null) return "";
    const started = Date.parse(turn.startedAt);
    if (Number.isNaN(started)) return "";
    return formatElapsed((Date.now() - started) / 1000);
  };

  const stopTicker = (): void => {
    if (ticker === 0) return;
    window.clearInterval(ticker);
    ticker = 0;
  };

  /**
   * Runs the elapsed timer only while there is a figure for it to move, and
   * only ever runs one. A second interval would double the work for the same
   * number; one left behind after the turn settles is a phone waking every
   * second to redraw something nobody is reading.
   */
  const syncTicker = (ticking: boolean): void => {
    if (!ticking) {
      stopTicker();
      return;
    }
    if (ticker === 0) ticker = window.setInterval(() => renderStatus(activeSnapshot), TICK_MS);
  };

  /**
   * The status line: what the turn is doing, how long it has been at it, and
   * what it has cost so far.
   *
   * The order answers the reader's question in the order they ask it. The
   * usage half is the line this element has always carried, kept where it was.
   */
  const renderStatus = (active: ActiveSession | null): void => {
    const usage = active?.usage ?? null;
    const running = runningHere(active);
    const elsewhere = runningElsewhere(active);
    const counts =
      usage === null ? "" : `${formatTokens(usage.inputTokens)} in · ${formatTokens(usage.outputTokens)} out`;
    const step = running ? currentStep() : "";
    const elapsed = running ? elapsedLabel(active?.turn ?? null) : "";
    // A turn known only from the session's `busy` flag has no anchor to count
    // from, so it gets no clock — and no interval to run one.
    syncTicker(elapsed !== "");

    if (usage === null && !running && !elsewhere) {
      statusStep.textContent = "";
      statusElapsed.textContent = "";
      statusUsage.textContent = "";
      turnStatus.hidden = true;
      return;
    }

    let note = "";
    if (elsewhere) {
      note = counts === "" ? "The agent is working in another client." : "working in another client";
    } else if (running && elapsed === "") {
      // The reload gap: `busy` says a turn is in flight but no frame carried its
      // start, so there is no clock to imply it. The words are what this line
      // had before it had one.
      note = "working…";
    }
    // Each segment carries its own separator, so an absent one leaves no
    // dangling dot behind it.
    const segments = [
      { node: statusStep, text: step },
      { node: statusElapsed, text: elapsed },
      { node: statusUsage, text: [counts, note].filter((part) => part !== "").join(" · ") },
    ];
    let written = 0;
    for (const segment of segments) {
      const text = segment.text === "" ? "" : written === 0 ? segment.text : ` · ${segment.text}`;
      segment.node.textContent = text;
      if (text !== "") written += 1;
    }
    // Nothing to say beyond "it is running", which is what this line said
    // before it had a clock to say it with.
    if (written === 0) statusUsage.textContent = "Working…";
    turnStatus.hidden = false;
  };

  const render = (active: ActiveSession | null): void => {
    const names = new Map(store.state.models.models.map((option) => [option.id, option.name]));
    renderHead(active?.session ?? null, names);
    renderBanners(active, store.state.harness);
    syncLeaseSheet(active);
    // The composer's own state is settled before the early return: with no
    // session loaded there is still a composer on screen, and it has to be an
    // honest one — Send offered for nothing, and a status line left over from
    // the session that just closed, would both be lies.
    syncAttach(active);
    syncQueue(active);
    renderStatus(active);
    syncComposer(active);

    if (active === null) {
      loadOlder.hidden = true;
      return;
    }

    if (active.feed !== rendered) syncLog(active.feed);

    loadOlder.hidden = active.nextBefore === null;
    loadOlder.disabled = active.loadingOlder;
    loadOlder.textContent = active.loadingOlder ? "Loading…" : "Load older messages";
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

  // Features arrive with the principal, in a commit that leaves `active`'s
  // identity alone — so the composer has to be listening for them itself, or an
  // attach button that appeared on the first paint would never appear at all.
  const offFeatures = store.select(
    (state) => state.features,
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
    offFeatures();
    offBack();
    offOlder();
    offStop();
    offInput();
    offKey();
    offSubmit();
    offAttach();
    offPicked();
    offChanges();
    offLease();
    stopTicker();
    leaseSheet?.sheet.close();
    unpin();
    view.remove();
  };
}
