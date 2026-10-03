/**
 * Turning feed rows into nodes.
 *
 * This is the only place that knows what a row *looks like*, and it is
 * deliberately thin: each kind is mounted by the module that owns its behaviour —
 * a tool card by `tools/card.ts`, a message by the markdown renderer — and this
 * file is the dispatch and the shared chrome (the usage line, the attachments
 * marker, the header over a run of calls).
 *
 * Every handle can be updated in place. That is not an optimisation: a tool card
 * is published when its call starts and completed when the result lands, and a
 * view that could only mount would either rebuild the whole log for every frame
 * or — as this one used to — leave the card saying "running" forever.
 */

import { actorLabel, outcomeLabel } from "../activity.js";
import { el } from "../dom.js";
import type { FeedRow } from "../feed.js";
import { formatTokens, relativeTime } from "../format.js";
import { renderMarkdown } from "../markdown.js";
import type { RowHandle } from "../rows.js";
import { formatElapsed, mountToolCard } from "../tools/card.js";
import { toolCallView } from "../tools/present.js";
import type { TokenUsage } from "../types.js";

/** What a row may ask the view to do. */
export interface FeedRowActions {
  /** The reader folded or unfolded one run of tool calls. */
  readonly toggleRun: (runKey: string) => void;
}

/** Mounts one row, returning the handle the list keeps. */
export function mountFeedRow(row: FeedRow, actions: FeedRowActions): RowHandle<FeedRow> {
  switch (row.kind) {
    case "message":
      return mountMessage(row);
    case "notice":
      return mountNotice(row);
    case "toolrun":
      return mountRun(row, actions);
    case "tool":
      return mountTool(row);
  }
}

/**
 * Applies an update, which is always the same row kind: a row's key encodes its
 * kind, so the list never hands a message row the data of a tool row.
 */
function updated<T extends FeedRow["kind"]>(
  node: HTMLElement,
  handle: { dispose?(): void },
  kind: T,
  apply: (next: Extract<FeedRow, { kind: T }>) => void,
): RowHandle<FeedRow> {
  return {
    node,
    update: (next) => {
      if (next.kind === kind) apply(next as Extract<FeedRow, { kind: T }>);
    },
    // Always defined, so a handle type with an optional hook stays optional
    // rather than becoming `undefined`-valued under exact optional properties.
    dispose: () => handle.dispose?.(),
  };
}

/* ---------------------------------------------------------------- messages */

function usageLine(usage: TokenUsage): HTMLElement {
  const parts = [`${formatTokens(usage.inputTokens)} in`, `${formatTokens(usage.outputTokens)} out`];
  if (usage.totalTokens !== null) parts.push(`${formatTokens(usage.totalTokens)} total`);
  if (usage.contextWindow !== null) parts.push(`of ${formatTokens(usage.contextWindow)}`);
  return el("p", { class: "usage", text: parts.join(" · ") });
}

/**
 * How many images a prompt carried.
 *
 * The image itself is not projected — see the contract — so a prompt that was
 * only a screenshot would otherwise be an empty row, which reads as a rendering
 * failure rather than as a message.
 */
function attachmentsMarker(count: number): HTMLElement | null {
  if (count <= 0) return null;
  return el("p", { class: "msg-attachments", text: count === 1 ? "1 image" : `${count} images` });
}

/**
 * A message row is immutable once it is in the log: a committed message never
 * changes, so this handle has nothing to update. Only a tool row does — it is
 * published when the call starts and completed when the result lands — and its
 * card is the one mount here that provides `update`.
 */
function mountMessage(row: Extract<FeedRow, { kind: "message" }>): RowHandle<FeedRow> {
  const { item } = row;
  if (item.role === "user") {
    // A prompt is what the operator typed, shown verbatim. It never changes once
    // it is in the log, so this row has nothing to update.
    const node = el(
      "article",
      { class: "msg msg-user" },
      el(
        "header",
        { class: "msg-head" },
        el("span", { class: "msg-role", text: "You" }),
        item.time === null ? null : el("time", { class: "msg-time", attrs: { datetime: item.time }, text: relativeTime(item.time) }),
      ),
      el("div", { class: "msg-body" }, el("p", { class: "msg-plain", text: item.text })),
      attachmentsMarker(item.attachments),
    );
    return { node };
  }

  const head = el("header", { class: "msg-head" }, el("span", { class: "msg-role", text: "Assistant" }));
  if (item.time !== null) {
    head.appendChild(el("time", { class: "msg-time", attrs: { datetime: item.time }, text: relativeTime(item.time) }));
  }
  if (item.model !== null) head.appendChild(el("span", { class: "msg-model", text: item.model }));

  const node = el(
    "article",
    { class: "msg msg-assistant" },
    head,
    item.thinking !== null && item.thinking.trim() !== ""
      ? el(
          "details",
          { class: "thinking" },
          el("summary", { class: "thinking-summary", text: "Thinking" }),
          el("div", { class: "thinking-body" }, renderMarkdown(item.thinking)),
        )
      : null,
    el("div", { class: "msg-body" }, renderMarkdown(item.text)),
    item.usage === null ? null : usageLine(item.usage),
  );
  return { node };
}

/* ----------------------------------------------------------------- notices */

function mountNotice(row: Extract<FeedRow, { kind: "notice" }>): RowHandle<FeedRow> {
  const { item } = row;
  const time =
    item.time === null
      ? null
      : el("time", { class: "notice-time", attrs: { datetime: item.time }, text: relativeTime(item.time) });

  // The app's own annotations — "the turn was stopped", "queued" — stay quiet
  // lines. An agent's report does not: it is drawn as the agent that sent it,
  // because the reader's two questions about a report are "which agent is this?"
  // and "did it finish?".
  if (item.actor === null || item.actor.kind === "system") {
    return { node: el("p", { class: "notice" }, time, item.text) };
  }

  const head = el(
    "header",
    { class: "notice-head" },
    el("span", { class: `notice-actor is-${item.actor.kind}`, text: actorLabel(item.actor) }),
    item.outcome === null ? null : el("span", { class: `notice-outcome is-${item.outcome}`, text: outcomeLabel(item.outcome) }),
    time,
  );
  // The subject is the harness's own line, kept as the evidence for which child
  // this is: `list_agents` names children by the same id.
  const subject = item.summary === "" ? null : el("p", { class: "notice-subject", text: item.summary });
  // A report can be long, and this row is a summary of it; the activity screen
  // keeps the whole thing.
  const preview = item.detail.length > 240 ? `${item.detail.slice(0, 239)}…` : item.detail;
  const body = preview === "" ? null : el("div", { class: "notice-report" }, renderMarkdown(preview));
  return { node: el("article", { class: "notice notice-report-row" }, head, subject, body) };
}

/* --------------------------------------------------------------- tool runs */

/**
 * The header over a run of tool calls.
 *
 * It answers "what happened in this stretch" in one line — how many calls, how
 * many of them went wrong, how long they took — and folds the cards away when
 * the reader wants to get to the answer underneath. Folding is the *reader's*
 * choice and is held by the view, not here: this row only draws the state and
 * reports the tap.
 */
function mountRun(row: Extract<FeedRow, { kind: "toolrun" }>, actions: FeedRowActions): RowHandle<FeedRow> {
  const caret = el("span", { class: "tool-caret", attrs: { "aria-hidden": "true" } });
  const title = el("span", { class: "tool-run-title" });
  const problems = el("span", { class: "tool-run-problems" });
  const time = el("span", { class: "tool-run-time" });
  const button = el(
    "button",
    { class: "tool-run-head", attrs: { type: "button" } },
    caret,
    title,
    problems,
    time,
  );
  const node = el("div", { class: "tool-run" }, button);

  const render = (data: Extract<FeedRow, { kind: "toolrun" }>): void => {
    button.setAttribute("aria-expanded", data.collapsed ? "false" : "true");
    button.setAttribute("aria-label", data.collapsed ? "Show these tool calls" : "Hide these tool calls");
    title.textContent = data.count === 1 ? "1 tool call" : `${data.count} tool calls`;

    // A failure and a non-zero exit are different news and are counted apart: the
    // first is the harness saying the call broke, the second is the command
    // saying it failed, and a reader triaging a run wants to know which.
    const parts: string[] = [];
    if (data.failed > 0) parts.push(`${data.failed} failed`);
    if (data.nonzero > 0) parts.push(`${data.nonzero} non-zero exit`);
    problems.textContent = parts.join(" · ");
    problems.hidden = parts.length === 0;
    button.classList.toggle("is-bad", parts.length > 0);

    const duration = durationOf(data.startedAt, data.endedAt);
    time.textContent = data.running ? "running…" : duration;
  };

  render(row);

  button.addEventListener("click", () => {
    actions.toggleRun(row.runKey);
  });

  return updated(node, {}, "toolrun", render);
}

/** `1m 04s` for a finished run, or "" when the timestamps do not support one. */
function durationOf(startedAt: string | null, endedAt: string | null): string {
  if (startedAt === null || endedAt === null) return "";
  const start = Date.parse(startedAt);
  const end = Date.parse(endedAt);
  if (Number.isNaN(start) || Number.isNaN(end) || end < start) return "";
  return formatElapsed((end - start) / 1000);
}

/* -------------------------------------------------------------- tool cards */

function mountTool(row: Extract<FeedRow, { kind: "tool" }>): RowHandle<FeedRow> {
  const card = mountToolCard({ call: toolCallView(row.item, row.workspace), collapsed: row.collapsed });
  return updated(card.node, card, "tool", (next) => {
    card.update?.({ call: toolCallView(next.item, next.workspace), collapsed: next.collapsed });
  });
}
