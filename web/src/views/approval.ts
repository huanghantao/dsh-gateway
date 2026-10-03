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
 * - **Readable before it is answerable.** A file edit asks about two long JSON
 *   strings; deciding on `{"file_path":…,"old_string":"…"}` from a phone is
 *   guessing. A recorded file change is rendered as a diff, a destructive
 *   command is named as one, and the raw arguments stay one tap away — the diff
 *   is a convenience, the input is the truth.
 * - **Clear about what each choice does.** The harness's allow-once/reject-once
 *   answer *this* request and stay the dominant pair; a scoped grant answers it
 *   for a while, and says for how long.
 *
 * Requests are shown one at a time: `alertdialog` semantics only hold for a
 * single decision, and stacking them invites a mis-tap on the wrong one.
 */

import type { Ctx } from "../actions.js";
import { el, isolateBackground, trapFocus } from "../dom.js";
import { formatCountdown } from "../format.js";
import { changeOf, diffBlock } from "../tools/diff.js";
import { looksDestructive } from "../tools/risk.js";
import type { Approval, ApprovalOption } from "../types.js";

/** Mirrors the contract's documented options; used only if the server omits them. */
const FALLBACK_OPTIONS: readonly ApprovalOption[] = [
  { id: "allow-once", name: "Allow once", grant: false },
  { id: "reject-once", name: "Reject", grant: false },
];

const TICK_MS = 1000;
const URGENT_MS = 15_000;

/** The sheet's notes are announced by id; one card is open at a time. */
const RISK_NOTE_ID = "approval-risk-note";
const GRANT_NOTE_ID = "approval-grant-note";

/** Emphasis is derived from the id, so an unknown option still renders sanely. */
function optionClass(option: ApprovalOption): string {
  // A scoped choice is deliberately not the primary or danger treatment: it is
  // a convenience with a lifetime, not the answer to this request.
  if (option.grant) return "btn btn-grant";
  if (option.id.startsWith("allow")) return "btn btn-primary";
  if (option.id.startsWith("reject") || option.id.startsWith("deny")) return "btn btn-danger";
  return "btn btn-ghost";
}

/** The arguments as they arrived. Never reformatted — this is the bytes being judged. */
function rawInput(input: string): HTMLElement {
  return el("pre", { class: "approval-input", text: input === "" ? "(no input)" : input });
}

/* ------------------------------------------------------------ grant choices */

/** A lifetime in words: "45 seconds", "30 minutes", "2 hours", "1 day". */
function spellSeconds(seconds: number): string {
  const units: readonly (readonly [number, string])[] = [
    [86_400, "day"],
    [3_600, "hour"],
    [60, "minute"],
  ];
  for (const [size, name] of units) {
    // Only an exact multiple reads honestly: 5400 seconds is "90 minutes", not
    // "1.5 hours".
    if (seconds >= size && seconds % size === 0) {
      const count = seconds / size;
      return `${count} ${name}${count === 1 ? "" : "s"}`;
    }
  }
  return `${Math.round(seconds)} second${seconds === 1 ? "" : "s"}`;
}

/**
 * One sentence over the scoped choices, saying what they are not.
 *
 * The harness's own pair answers the request in front of the reader; these
 * answer it for a while, and the whole reason a lifetime is on screen is that
 * agreeing to one is a different decision from agreeing to a single call.
 */
function grantNote(ttlSecs: number): string {
  if (ttlSecs > 0) {
    return `Or answer it for a while: the choices below stay in force for the next ${spellSeconds(ttlSecs)}, in this session.`;
  }
  return "Or answer it for a while: the choices below stay in force beyond this one call, in this session.";
}

/**
 * The session root a path may be shortened against, or "" when it is not known.
 *
 * An approval can belong to a session that is not the one on screen, and a path
 * shortened against the wrong root is worse than a long one: it names a file in
 * a checkout the reader is not looking at.
 */
function workspaceFor(approval: Approval, ctx: Ctx): string {
  const active = ctx.store.state.active;
  if (active === null || active.sessionId !== approval.sessionId) return "";
  return active.session?.workspace ?? "";
}

/* -------------------------------------------------------------------- card */

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

  const change = changeOf(approval.tool, approval.input, workspaceFor(approval, ctx));
  const risk = looksDestructive(approval.tool, approval.input);
  // A change with no lines to draw is not a diff — an empty whole-file write
  // would be a blank box that says nothing — so it falls back to the bytes.
  const shown = change !== null && change.lines.length > 0 ? change : null;

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
  const actions = el("div", { class: "approval-actions" });

  let settled = false;

  const decide = (optionId: string): void => {
    if (settled) return;
    // One decision per card: a double tap must not send two POSTs, the second
    // of which would 409.
    settled = true;
    sync();
    void ctx.decideApproval(approval, optionId);
  };

  const harnessChoices: HTMLButtonElement[] = [];
  const scopedChoices: HTMLButtonElement[] = [];
  for (const option of options) {
    const control = el("button", {
      class: optionClass(option),
      attrs: {
        type: "button",
        // The lifetime belongs to the choice rather than to the region above it:
        // a reader who reaches a grant button hears how long it lasts.
        "aria-describedby": option.grant ? GRANT_NOTE_ID : null,
      },
      text: option.name,
      on: { click: () => decide(option.id) },
    });
    (option.grant ? scopedChoices : harnessChoices).push(control);
  }
  const controls: readonly HTMLButtonElement[] = [...harnessChoices, ...scopedChoices];
  actions.append(...harnessChoices);
  if (scopedChoices.length > 0) {
    actions.append(
      el("p", {
        class: "approval-grant-note",
        attrs: { id: GRANT_NOTE_ID },
        text: grantNote(ctx.store.state.features.approvalGrantTTLSecs),
      }),
      ...scopedChoices,
    );
  }

  // The warning hangs off the dialog rather than off a button, because
  // `activate` puts focus on the dialog: this is the one place it is read out
  // with the title instead of waiting to be tabbed into.
  const card = el(
    "div",
    {
      class: "sheet approval",
      attrs: {
        role: "alertdialog",
        "aria-modal": "true",
        "aria-labelledby": "approval-title",
        "aria-describedby": risk === "" ? null : RISK_NOTE_ID,
        tabindex: "-1",
      },
    },
    el(
      "header",
      { class: "approval-head" },
      el("h2", { class: "approval-title", attrs: { id: "approval-title" }, text: `Allow “${approval.tool}”?` }),
      el("p", { class: "muted", text: "The agent is paused until you decide. If nobody answers, the request is rejected." }),
      queued > 0 ? el("p", { class: "approval-queue", text: `${queued} more waiting after this one` }) : null,
    ),
    timerRow,
    risk === "" ? null : el("p", { class: "risk-chip", attrs: { id: RISK_NOTE_ID }, text: `Look twice: ${risk}.` }),
    el("h3", { class: "tool-label", text: shown === null ? "Requested input" : "Proposed change" }),
    ...(shown === null ? [rawInput(approval.input)] : diffBlock(shown)),
    shown === null
      ? null
      : el("details", { class: "approval-raw" }, el("summary", { text: "Raw arguments" }), rawInput(approval.input)),
    errorBox,
    actions,
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
