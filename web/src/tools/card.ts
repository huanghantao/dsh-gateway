/**
 * The tool card: one call, drawn once, kept current.
 *
 * It is a `<details>` because that is a disclosure widget a browser, a keyboard
 * and a screen reader already understand — no JavaScript is needed to open one,
 * and the caret the stylesheet draws is decoration over that behaviour rather
 * than a replacement for it.
 *
 * Four decisions shape the rest:
 *
 * 1. **The card is updated, not rebuilt.** `update()` re-renders the row's
 *    contents in place, which is what lets a running call turn into a finished
 *    one and show its result. It is also why the reader's own choice to open or
 *    close the card survives: that choice is remembered here, not in the DOM
 *    attribute that re-rendering replaces.
 * 2. **Running is not a reason to shout.** A call in flight opens its card, but
 *    what it shows while it runs is the small, useful part — the command, the
 *    path, the diff — never the raw argument JSON. The full arguments are one tap
 *    away, in the fold at the bottom.
 * 3. **Nothing disappears without being named.** A result the harness cut short,
 *    a payload this deployment trimmed, arguments that no longer match the diff
 *    drawn from them: each says so, in the card, in words.
 * 4. **Time is shown where it is known.** A finished call shows how long it took;
 *    a running one counts, which is the only progress signal the ACP seam
 *    actually provides — there is no partial output to stream.
 */

import { copyButton } from "../copybutton.js";
import { el } from "../dom.js";
import type { RowHandle } from "../rows.js";
import { diffBlock } from "./diff.js";
import { presentTool, type ToolBlock, type ToolCallView, type ToolView } from "./present.js";

/** What one card renders: the call, and whether its run has been folded away. */
export interface ToolCardData {
  readonly call: ToolCallView;
  /** The run this card belongs to is collapsed, so the card is out of sight. */
  readonly collapsed: boolean;
}

const TICK_MS = 1000;

/**
 * A duration as a reader says it: `12s`, `1m 04s`, `1h 02m`.
 *
 * Minutes and seconds are zero-padded so the figure does not change width as it
 * ticks; a label that jitters once a second pulls the eye every second.
 */
export function formatElapsed(seconds: number): string {
  const total = Math.max(0, Math.floor(seconds));
  if (total < 60) return `${total}s`;
  if (total < 3600) return `${Math.floor(total / 60)}m ${String(total % 60).padStart(2, "0")}s`;
  return `${Math.floor(total / 3600)}h ${String(Math.floor((total % 3600) / 60)).padStart(2, "0")}m`;
}

/** Milliseconds between two ISO timestamps, or null when either is unusable. */
function elapsedMs(from: string | null, to: string | null): number | null {
  if (from === null || to === null) return null;
  const start = Date.parse(from);
  const end = Date.parse(to);
  if (Number.isNaN(start) || Number.isNaN(end) || end < start) return null;
  return end - start;
}

/** A block of text with a label and, when it is worth copying, a copy button. */
function codeBlock(label: string, text: string, copy: boolean): HTMLElement {
  const bar = el("div", { class: "tool-block-bar" }, el("span", { class: "tool-block-label", text: label }));
  if (copy) bar.appendChild(copyButton(text));
  return el("div", { class: "tool-block" }, bar, el("pre", { class: "tool-pre", text }));
}

/** The body of one block, dispatched by kind. */
function blockNode(block: ToolBlock): Node {
  switch (block.kind) {
    case "diff":
      return el("div", { class: "tool-diff" }, ...diffBlock(block.change));
    case "code":
      return codeBlock(block.label, block.text, block.copy);
    case "text":
      return el(
        "div",
        { class: "tool-block" },
        el("div", { class: "tool-block-bar" }, el("span", { class: "tool-block-label", text: block.label })),
        el("p", { class: "tool-text", text: block.text }),
      );
  }
}

/** Everything below the summary line. */
function bodyNodes(view: ToolView): readonly Node[] {
  const nodes: Node[] = view.blocks.map(blockNode);

  if (view.facts.length > 0) {
    const list = el("dl", { class: "tool-facts" });
    for (const fact of view.facts) {
      list.append(el("dt", { text: fact.label }), el("dd", { text: fact.value }));
    }
    nodes.push(list);
  }

  for (const note of view.notes) {
    nodes.push(el("p", { class: note.tone === "warn" ? "tool-note is-warn" : "tool-note", text: note.text }));
  }

  if (view.raw !== null) {
    nodes.push(
      el(
        "details",
        { class: "tool-raw" },
        el("summary", { class: "tool-raw-summary", text: "Raw arguments" }),
        el("pre", { class: "tool-pre", text: view.raw }),
      ),
    );
  }

  if (nodes.length === 0) {
    nodes.push(el("p", { class: "muted", text: "Nothing was recorded for this call." }));
  }
  return nodes;
}

/** Mounts one tool card and returns the handle the list keeps. */
export function mountToolCard(initial: ToolCardData): RowHandle<ToolCardData> {
  const caret = el("span", { class: "tool-caret", attrs: { "aria-hidden": "true" } });
  const name = el("span", { class: "tool-name" });
  const detail = el("span", { class: "tool-detail" });
  const meta = el("span", { class: "tool-meta" });
  const status = el("span", { class: "tool-status" });
  const summary = el("summary", { class: "tool-summary" }, caret, name, detail, meta, status);
  // `aria-live="off"` inside the log's polite region: the summary line is what a
  // screen reader should hear when a call starts and when it settles, not the
  // several hundred lines of output that arrive with it.
  const body = el("div", { class: "tool-body", attrs: { "aria-live": "off" } });
  const node = el("details", { class: "tool" }, summary, body);

  let data = initial;
  let view = presentTool(initial.call);
  let ticker = 0;
  /** What the reader chose, once they have chosen. Null means "follow the call". */
  let chosenOpen: boolean | null = null;
  /** What this component last set `open` to, so a toggle can be attributed. */
  let intendedOpen = false;

  node.addEventListener("toggle", () => {
    if (node.open !== intendedOpen) chosenOpen = node.open;
  });

  const setOpen = (open: boolean): void => {
    intendedOpen = open;
    if (node.open !== open) node.open = open;
  };

  const refreshMeta = (): void => {
    const { call } = data;
    if (call.running) {
      const started = call.startedAt === null ? Number.NaN : Date.parse(call.startedAt);
      meta.textContent = Number.isNaN(started) ? "" : formatElapsed((Date.now() - started) / 1000);
      return;
    }
    const duration = elapsedMs(call.startedAt, call.endedAt);
    meta.textContent = duration === null ? "" : formatElapsed(duration / 1000);
  };

  const syncTicker = (): void => {
    const wanted = data.call.running && data.call.startedAt !== null;
    if (wanted && ticker === 0) {
      ticker = window.setInterval(refreshMeta, TICK_MS);
    } else if (!wanted && ticker !== 0) {
      window.clearInterval(ticker);
      ticker = 0;
    }
  };

  const render = (): void => {
    view = presentTool(data.call);
    node.className = `tool tool-${view.status}`;
    node.hidden = data.collapsed;
    name.textContent = view.tool;
    detail.textContent = view.headline;
    // The headline is the row's meaning; when a tool has nothing to say, the
    // status still has to sit at the end of the row rather than at the front.
    detail.hidden = view.headline === "";
    status.textContent = view.statusLabel;
    status.className = `tool-status tool-status-${view.status}`;
    refreshMeta();
    body.replaceChildren(...bodyNodes(view));
    setOpen(chosenOpen ?? data.call.running);
    syncTicker();
  };

  render();

  return {
    node,
    update: (next) => {
      data = next;
      render();
    },
    dispose: () => {
      if (ticker !== 0) window.clearInterval(ticker);
      ticker = 0;
    },
  };
}
