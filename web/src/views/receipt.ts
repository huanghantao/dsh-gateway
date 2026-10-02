/**
 * The receipt: what a session actually did.
 *
 * A conversation answers "what was said". This answers the question asked a day
 * later — which files changed, how many tokens went through, how long it ran,
 * what it cost — and it answers from the session's own log, so it cannot drift
 * from the conversation it describes.
 *
 * Two numbers are deliberately separate. *Active* is time the agent was
 * demonstrably working; *open* is the session's whole life, including the hours
 * it sat untouched. A panel that showed only one of them would either flatter a
 * session left open overnight or hide a long-running job.
 */

import type { Ctx } from "../actions.js";
import { el } from "../dom.js";
import { formatDateTime, formatDuration, formatMoney, formatTokens } from "../format.js";
import type { Session, SessionReceipt } from "../types.js";
import { openSheet, spinner } from "./ui.js";

export function openReceiptSheet(ctx: Ctx, session: Session): void {
  const body = el("div", { class: "stack" }, spinner("Adding it up…"));
  openSheet({ title: "Session receipt", body });

  void ctx
    .receipt(session.id)
    .then((receipt) => {
      body.replaceChildren(...render(receipt));
    })
    .catch((cause: unknown) => {
      body.replaceChildren(
        el("p", {
          class: "alert alert-error",
          attrs: { role: "alert" },
          text: cause instanceof Error ? cause.message : "The receipt could not be read.",
        }),
      );
    });
}

/** One labelled row. */
function row(label: string, value: string, hint?: string): HTMLElement {
  return el(
    "div",
    { class: "receipt-row" },
    el("span", { class: "receipt-label", text: label }),
    el("span", { class: "receipt-value" }, el("span", { text: value }), hint === undefined ? null : el("span", { class: "receipt-hint", text: hint })),
  );
}

function render(receipt: SessionReceipt): readonly Node[] {
  const nodes: Node[] = [];

  const model = [receipt.model, receipt.reasoningEffort].filter((part) => part !== "").join(" · ");
  if (model !== "") nodes.push(row("Model", model));
  if (receipt.workspace !== "") nodes.push(row("Workspace", receipt.workspace));

  const when = [receipt.startedAt, receipt.endedAt].filter((part) => part !== "").map((part) => formatDateTime(part));
  if (when.length === 2 && when[0] !== when[1]) {
    nodes.push(row("Ran", `${when[0]} → ${when[1]}`));
  } else if (when.length > 0) {
    nodes.push(row("Started", when[0] ?? ""));
  }
  if (receipt.activeSeconds > 0 || receipt.spanSeconds > 0) {
    nodes.push(
      row("Working", formatDuration(receipt.activeSeconds), `${formatDuration(receipt.spanSeconds)} open, including idle time`),
    );
  }

  nodes.push(row("Messages", `${receipt.messages}`, `${receipt.turns} turn${receipt.turns === 1 ? "" : "s"}`));

  const tokens = `${formatTokens(receipt.inputTokens)} in · ${formatTokens(receipt.outputTokens)} out`;
  const cached = receipt.cacheReadTokens > 0 ? `${formatTokens(receipt.cacheReadTokens)} from cache` : undefined;
  nodes.push(row("Tokens", tokens, cached));

  if (receipt.cost !== undefined) {
    // The arithmetic is shown because the prices are the operator's, not ours: a
    // number nobody can check is a number nobody should trust.
    const parts = `${formatTokens(receipt.inputTokens)} × in + ${formatTokens(receipt.outputTokens)} × out`;
    nodes.push(
      row("Cost", `≈ ${formatMoney(receipt.cost.total, receipt.cost.currency)}`, `estimated from ${parts}${receipt.cacheReadTokens > 0 ? " + cached" : ""}`),
    );
  } else {
    nodes.push(row("Cost", "not configured", "set receipt.pricing to estimate it"));
  }
  if (receipt.contextTokens > 0) {
    nodes.push(row("Context", formatTokens(receipt.contextTokens), "at the last message"));
  }

  if (receipt.tools.length > 0) {
    const list = el("div", { class: "receipt-tools" });
    for (const tool of receipt.tools) {
      list.appendChild(
        el(
          "span",
          { class: "receipt-tool" },
          el("code", { text: tool.name }),
          el("span", { class: "receipt-tool-count", text: `×${tool.calls}` }),
          tool.failed > 0 ? el("span", { class: "receipt-tool-failed", text: `${tool.failed} failed` }) : null,
        ),
      );
    }
    nodes.push(
      el(
        "div",
        { class: "receipt-row" },
        el("span", { class: "receipt-label", text: "Tools" }),
        // Wrapped like every other value: the row is a two-column grid, and a
        // second child that is not a cell would be laid out as one anyway —
        // leaving the DOM and the stylesheet disagreeing about what a row is.
        el("div", { class: "receipt-value" }, list),
      ),
    );
  }

  if (receipt.files.length > 0) {
    const list = el("div", { class: "receipt-files" });
    for (const file of receipt.files.slice(0, 40)) {
      list.appendChild(el("code", { class: "receipt-file", text: file }));
    }
    if (receipt.files.length > 40) {
      list.appendChild(el("span", { class: "muted", text: `and ${receipt.files.length - 40} more` }));
    }
    nodes.push(
      el(
        "div",
        { class: "receipt-row" },
        el("span", { class: "receipt-label", text: `Files (${receipt.files.length})` }),
        el("div", { class: "receipt-value" }, list),
      ),
    );
  }

  if (receipt.messages === 0) {
    nodes.push(el("p", { class: "muted", text: "Nothing has happened in this session yet." }));
  }

  return nodes;
}
