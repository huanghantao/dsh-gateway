/**
 * Screen 4 — the approval sheet.
 *
 * This is the one genuinely blocking interaction in the product: the agent is
 * stopped mid-turn until a human answers, and the contract is explicit that
 * silence is a rejection. So the sheet is:
 *
 * - **Mounted at the app root**, not inside a view, so a request that arrives
 *   while the user is in Settings or on the session list is still answerable.
 * - **A real modal dialog** with a focus trap and `aria-modal`, because a
 *   blocking decision a keyboard cannot reach is not blocking, it is a deadlock.
 * - **Honest about the clock.** The countdown is derived from `expiresAt`, and at
 *   zero the buttons are disabled rather than left to fail with a 409.
 *
 * Requests are shown one at a time: `alertdialog` semantics only hold for a
 * single decision, and stacking them invites a mis-tap on the wrong one.
 */

import type { Ctx } from "../actions.js";
import { el, isolateBackground, trapFocus } from "../dom.js";
import { formatCountdown } from "../format.js";
import type { Approval, ApprovalOption } from "../types.js";

/** Mirrors the contract's documented options; used only if the server omits them. */
const FALLBACK_OPTIONS: readonly ApprovalOption[] = [
  { id: "allow-once", name: "Allow once" },
  { id: "reject-once", name: "Reject" },
];

const TICK_MS = 1000;
const URGENT_MS = 15_000;

/** Emphasis is derived from the id, so an unknown option still renders sanely. */
function optionClass(option: ApprovalOption): string {
  if (option.id.startsWith("allow")) return "btn btn-primary";
  if (option.id.startsWith("reject") || option.id.startsWith("deny")) return "btn btn-danger";
  return "btn btn-ghost";
}

interface ApprovalCard {
  readonly node: HTMLElement;
  /** Re-reads store-backed state (busy flag, error text) without a rebuild. */
  refresh(): void;
  /**
   * Installs the focus trap. Called by the mount *after* the node is in the
   * document — focusing a detached dialog silently leaves focus behind it.
   */
  activate(): void;
  dispose(): void;
}

function buildCard(approval: Approval, queued: number, ctx: Ctx): ApprovalCard {
  const options = approval.options.length > 0 ? approval.options : FALLBACK_OPTIONS;
  const expiresAt = approval.expiresAt === "" ? Number.NaN : new Date(approval.expiresAt).getTime();
  const requestedAt = approval.requestedAt === "" ? Number.NaN : new Date(approval.requestedAt).getTime();
  // Without a usable window there is nothing honest to draw, so the clock is
  // hidden rather than faked.
  const totalWindow = Number.isNaN(expiresAt) ? Number.NaN : Math.max(1, expiresAt - (Number.isNaN(requestedAt) ? expiresAt - 1 : requestedAt));

  const countdown = el("span", { class: "countdown-text", text: "" });
  const progress = el("progress", { class: "countdown-bar", attrs: { max: "100", value: "100" } });
  const timerRow = el(
    "div",
    { class: "approval-timer" },
    el("span", { class: "countdown-label", text: "Expires in " }),
    countdown,
    progress,
  );
  const errorBox = el("p", { class: "alert alert-error", attrs: { role: "alert", hidden: "" } });
  const buttons = el("div", { class: "approval-actions" });

  let settled = false;

  const controls = options.map((option) =>
    el("button", {
      class: optionClass(option),
      attrs: { type: "button" },
      text: option.name,
      on: {
        click: () => {
          if (settled) return;
          // One decision per card: a double tap must not send two POSTs, the
          // second of which would 409.
          settled = true;
          sync();
          void ctx.decideApproval(approval, option.id);
        },
      },
    }),
  );
  buttons.append(...controls);

  const card = el(
    "div",
    {
      class: "sheet approval",
      attrs: { role: "alertdialog", "aria-modal": "true", "aria-labelledby": "approval-title", tabindex: "-1" },
    },
    el(
      "header",
      { class: "approval-head" },
      el("h2", { class: "approval-title", attrs: { id: "approval-title" }, text: `Allow “${approval.tool}”?` }),
      el("p", { class: "muted", text: "The agent is paused until you decide. If nobody answers, the request is rejected." }),
      queued > 0 ? el("p", { class: "approval-queue", text: `${queued} more waiting after this one` }) : null,
    ),
    timerRow,
    el("h3", { class: "tool-label", text: "Requested input" }),
    el("pre", { class: "approval-input", text: approval.input === "" ? "(no input)" : approval.input }),
    errorBox,
    buttons,
  );

  const panel = el("div", { class: "sheet-backdrop approval-backdrop" }, card);

  let releaseTrap: (() => void) | null = null;

  function sync(): void {
    const state = ctx.store.state;
    const disabled = settled || state.approvalBusyId === approval.id;
    for (const control of controls) control.disabled = disabled;

    const message = state.approvalError;
    if (message === null) {
      errorBox.setAttribute("hidden", "");
    } else {
      errorBox.textContent = message;
      errorBox.removeAttribute("hidden");
    }
  }

  const tick = (): void => {
    if (Number.isNaN(expiresAt)) {
      countdown.textContent = "no deadline given";
      timerRow.classList.add("is-unknown");
      return;
    }
    const remaining = expiresAt - Date.now();
    countdown.textContent = formatCountdown(remaining);
    progress.setAttribute("value", String(Math.max(0, Math.min(100, Math.round((remaining / totalWindow) * 100)))));
    card.classList.toggle("is-urgent", remaining <= URGENT_MS);

    if (remaining <= 0 && !settled) {
      settled = true;
      countdown.textContent = "expired";
      sync();
      // The server rejects on its own schedule; refreshing is how the card
      // disappears once the request has actually closed there.
      void ctx.loadApprovals();
    }
  };

  const timer = window.setInterval(tick, TICK_MS);
  tick();
  sync();

  return {
    node: panel,
    refresh: sync,
    activate: () => {
      releaseTrap = trapFocus(card);
      // `trapFocus` focuses the first control; for a destructive decision that
      // is the wrong default, so the dialog itself takes focus and a stray
      // Enter does nothing.
      card.focus({ preventScroll: true });
    },
    dispose: () => {
      window.clearInterval(timer);
      releaseTrap?.();
      panel.remove();
    },
  };
}

export function mountApprovals(root: HTMLElement, ctx: Ctx): () => void {
  // An inner host, so `replaceChildren` swaps the card without touching
  // whatever else lives in `root` (which is `body`, and would be erased).
  const host = el("div", { class: "approval-host" });
  root.appendChild(host);

  /**
   * Everything on `body` is made inert while a decision is pending, except this
   * host and the notice region. The host is mounted outside `#app` precisely so
   * it can stay interactive; the notice region is spared because inert also
   * removes a live region from the accessibility tree, and "that approval had
   * already expired" is exactly the kind of message that must still be read out.
   */
  const noticeHost = document.getElementById("notice-host");
  const keepAlive: HTMLElement[] = noticeHost === null ? [host] : [host, noticeHost];

  let currentId: string | null = null;
  let current: ApprovalCard | null = null;
  let restoreBackground: (() => void) | null = null;

  const clear = (): void => {
    current?.dispose();
    current = null;
    currentId = null;
    restoreBackground?.();
    restoreBackground = null;
  };

  const offList = ctx.store.select(
    (state) => state.approvals,
    (approvals) => {
      const head = approvals[0];
      if (head === undefined) {
        clear();
        return;
      }
      if (head.id === currentId && current !== null) {
        // Same request: keep the live card and its countdown rather than
        // rebuilding it, which would restart the clock and steal focus.
        current.refresh();
        return;
      }
      clear();
      const card = buildCard(head, approvals.length - 1, ctx);
      current = card;
      host.replaceChildren(card.node);
      // Insert first, then trap and focus; the rest of the app goes inert so
      // `aria-modal` is not just a claim.
      card.activate();
      restoreBackground = isolateBackground(keepAlive);
      currentId = head.id;
    },
  );

  // The busy flag and the error text live outside `approvals`, so they need
  // their own subscription to reach an already-rendered card.
  const offBusy = ctx.store.select(
    (state) => [state.approvalBusyId, state.approvalError] as const,
    () => {
      current?.refresh();
    },
    (a, b) => a[0] === b[0] && a[1] === b[1],
  );

  return () => {
    offList();
    offBusy();
    clear();
    host.remove();
  };
}
