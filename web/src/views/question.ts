/**
 * Screen 4b — the question sheet.
 *
 * The agent stops mid-turn to ask something (`ask_user_question`), which over
 * ACP has no representation at all: the gateway brokers it to this screen. It is
 * built beside the approval sheet because the two are the same problem — an
 * agent that cannot proceed until a person answers — so the frame, the clock and
 * the dismissal rules are deliberately the same. Four things differ, and each is
 * a decision:
 *
 * - **It is a form, not an alarm.** `role="dialog"`, not `alertdialog`. An
 *   approval is an interruption with a safe default ("do not run that tool"), and
 *   `alertdialog` is exactly right for it. A question has no default and is
 *   content to read and answer — possibly four answers in one request — so
 *   having a screen reader interrupt everything to read it would be noise. The
 *   countdown still says how long the reader has.
 * - **Several questions are paged, not stacked.** One question at a time with a
 *   `2/4` indicator and arrows, because a phone shows one question's worth of
 *   content at once; the answers are collected locally and sent once, at the end.
 * - **The recommendation badge is presentation only.** The harness writes
 *   " (Recommended)" into the label and the model matches that exact string, so
 *   the suffix is stripped from the label on screen and kept in the answer.
 * - **The clock is real.** The countdown is derived from `expiresAt`, and at zero
 *   the controls are disabled rather than left to fail with a 409.
 *
 * Mounted at the app root beside the approval sheet, so a question asked while
 * the reader is in Settings or on the session list is still answerable — the
 * agent is blocked either way.
 */

import type { Ctx } from "../actions.js";
import { el, isolateBackground, trapFocus } from "../dom.js";
import { formatCountdown } from "../format.js";
import { renderMarkdown } from "../markdown/render.js";
import type { Question, QuestionAnswer, QuestionItem } from "../types.js";

const TICK_MS = 1000;
const URGENT_MS = 15_000;

/** The sheet's own ids; only one card is mounted at a time. */
const TITLE_ID = "question-title";
const NOTE_ID = "question-note";
const FREE_NOTE_ID = "question-free-note";

/**
 * The harness's own recommendation suffix, as it appears inside a label.
 *
 * Anchored to the end and case-insensitive, with the space optional, so it
 * matches whatever the server's derivation of `recommended` matched.
 */
const RECOMMENDED_SUFFIX = /\s*\(recommended\)\s*$/i;

/**
 * A label as it should be read, without the recommendation suffix.
 *
 * The badge beside it says the same thing in one word, and a label that says it
 * twice reads like a machine wrote it. When stripping would leave nothing — the
 * label *is* the suffix, odd but possible — the original stands, because a
 * nameless choice is worse than a repeated word.
 */
export function displayLabel(label: string): string {
  const stripped = label.replace(RECOMMENDED_SUFFIX, "").trim();
  return stripped === "" ? label : stripped;
}

/** One question's answer while it is still being edited. */
export interface QuestionDraft {
  readonly selected: readonly string[];
  readonly custom: string;
}

/**
 * The answer one question will carry.
 *
 * The label sent is the option's own, suffix and all: the model matches its own
 * strings, and a client that "tidied" one would be answered with
 * `400 unknown_option` and would lose the whole answer set with it.
 *
 * A single-select answer carries at most one of the two fields, because the
 * sheet keeps them exclusive — what is on screen is what gets sent. A
 * multi-select one may carry both, since there `custom` supplements the labels.
 * Both empty is a skipped question, which is a real answer rather than a missing
 * one.
 */
export function answerFor(item: QuestionItem, draft: QuestionDraft | undefined): QuestionAnswer {
  const selected = draft?.selected ?? [];
  const custom = (draft?.custom ?? "").trim();
  if (custom !== "") {
    return { id: item.id, selected: item.multiSelect ? [...selected] : [], custom };
  }
  return { id: item.id, selected: [...selected], custom: "" };
}

/**
 * Every item's answer, in the order the model asked them.
 *
 * Complete rather than partial: an item the reader paged past is sent as an
 * explicit skip. Omitting it would mean the same thing to the server, but a
 * complete set is what the model is handed, and a client that dropped items
 * would make "what did the operator not answer" a question about the transport.
 */
export function answersFor(
  items: readonly QuestionItem[],
  drafts: ReadonlyMap<string, QuestionDraft>,
): readonly QuestionAnswer[] {
  return items.map((item) => answerFor(item, drafts.get(item.id)));
}

/* -------------------------------------------------------------------- card */

interface QuestionCard {
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

function buildCard(question: Question, queued: number, ctx: Ctx): QuestionCard {
  const items = question.items;
  const expiresAt = question.expiresAt === "" ? Number.NaN : new Date(question.expiresAt).getTime();
  const requestedAt = question.requestedAt === "" ? Number.NaN : new Date(question.requestedAt).getTime();
  // Without a usable window there is nothing honest to draw, so the clock is
  // hidden rather than faked — the same rule the approval sheet follows.
  const totalWindow = Number.isNaN(expiresAt)
    ? Number.NaN
    : Math.max(1, expiresAt - (Number.isNaN(requestedAt) ? expiresAt - 1 : requestedAt));

  /** What the reader has chosen so far, by item id. Survives paging. */
  const drafts = new Map<string, QuestionDraft>();
  let page = 0;
  /** A submission is in flight; the card must not send a second one. */
  let settled = false;
  /** The question's window has closed; the controls are dead. */
  let expired = false;
  /** The current page's question, which takes focus after a page change. */
  let heading: HTMLElement | null = null;

  const draftOf = (id: string): QuestionDraft => drafts.get(id) ?? { selected: [], custom: "" };

  const countdown = el("span", { class: "countdown-text", text: "" });
  const progressBar = el("progress", { class: "countdown-bar", attrs: { max: "100", value: "100" } });
  const timerRow = el(
    "div",
    { class: "question-timer" },
    el("span", { class: "countdown-label", text: "Expires in " }),
    countdown,
    progressBar,
  );

  const title = el("h2", { class: "question-title", attrs: { id: TITLE_ID }, text: "A question from the agent" });
  const back = el("button", {
    class: "btn btn-ghost btn-icon question-arrow",
    attrs: { type: "button", "aria-label": "Previous question" },
    text: "\u2039",
  });
  const forward = el("button", {
    class: "btn btn-ghost btn-icon question-arrow",
    attrs: { type: "button", "aria-label": "Next question" },
    text: "\u203a",
  });
  const progressText = el("span", { class: "question-progress-text", text: "" });
  const progressRow = el("div", { class: "question-progress" }, back, progressText, forward);

  const body = el("div", { class: "question-body" });
  const errorBox = el("p", { class: "alert alert-error", attrs: { role: "alert", hidden: "" } });
  const skip = el("button", { class: "btn btn-ghost question-skip", attrs: { type: "button" }, text: "Skip" });
  const primary = el("button", { class: "btn btn-primary question-primary", attrs: { type: "button" }, text: "Next" });
  const actions = el("div", { class: "question-actions" }, skip, primary);

  const card = el(
    "div",
    {
      class: "sheet question",
      attrs: {
        role: "dialog",
        "aria-modal": "true",
        "aria-labelledby": TITLE_ID,
        "aria-describedby": NOTE_ID,
        tabindex: "-1",
      },
    },
    el(
      "header",
      { class: "question-head" },
      title,
      el("p", {
        class: "muted",
        attrs: { id: NOTE_ID },
        text: "The agent is paused until you answer. If nobody answers, it carries on without one.",
      }),
      queued > 0 ? el("p", { class: "question-queue", text: `${queued} more waiting after this one` }) : null,
    ),
    timerRow,
    progressRow,
    body,
    errorBox,
    actions,
  );

  const panel = el("div", { class: "sheet-backdrop question-backdrop" }, card);

  /* ------------------------------------------------------------- paging */

  /**
   * Draws the current page: its question, its choices, and the draft as it
   * stands.
   *
   * Rebuilt per page rather than updated in place, because a page is a whole
   * question — different options, a different free-text value, a different
   * heading — and reconciling four kinds of field to save a node or two would be
   * more code than the page is worth. `sync` is the one place control state is
   * applied, so it runs last.
   */
  const renderPage = (): void => {
    const item = items[page];
    if (item === undefined) return;
    const draft = draftOf(item.id);
    const children: HTMLElement[] = [];

    // The model's own heading names the card when it gave one, which is how
    // DSH's own card reads; without it the title says what this is.
    title.textContent = item.header.trim() === "" ? "A question from the agent" : item.header.trim();

    // Focusable so focus can be put on it: opening the sheet and every page
    // change land here, which is how a reader who cannot see the card learns
    // what is being asked before any answer is offered. It sits after the
    // arrows in document order, so Shift+Tab from it reaches the previous
    // control rather than the page behind the dialog — `trapFocus` cannot wrap
    // from a node it does not think is first.
    heading = el("h3", { class: "question-text", attrs: { tabindex: "0" }, text: item.question });
    children.push(heading);

    if (item.detail.trim() !== "") {
      // Supporting text, never a choice: a plan or a diff belongs with the
      // question, and offering it as an option would invent an answer.
      //
      // Rendered as markdown, because that is what the model writes. The field
      // carries a whole plan when the question is a plan review — `exit_plan_mode`
      // asks through this same seam — and a plan flattened into one paragraph
      // loses the structure that makes it reviewable. The renderer builds nodes
      // rather than HTML, so model output cannot inject markup.
      children.push(el("div", { class: "question-detail" }, renderMarkdown(item.detail)));
    }

    const radios: HTMLInputElement[] = [];
    let free: HTMLTextAreaElement | null = null;

    if (item.options.length > 0) {
      children.push(
        el("p", {
          class: "question-hint",
          text: item.multiSelect ? "Select all that apply." : "Select one.",
        }),
      );
      const list = el("div", { class: "question-options" });
      item.options.forEach((option, index) => {
        const chosen = draft.selected.includes(option.label);
        const input = el("input", {
          class: "choice-input",
          attrs: {
            // Native controls rather than a re-implementation of radio
            // semantics: they carry arrow-key navigation, the group's position
            // ("2 of 3") and the platform's own touch targets for free.
            type: item.multiSelect ? "checkbox" : "radio",
            name: `question-option-${question.id}-${item.id}`,
            value: option.label,
            checked: chosen ? "" : null,
          },
          on: {
            change: () => {
              const current = draftOf(item.id);
              if (item.multiSelect) {
                const selected = input.checked
                  ? [...current.selected, option.label]
                  : current.selected.filter((label) => label !== option.label);
                drafts.set(item.id, { selected, custom: current.custom });
                return;
              }
              // A single-select question holds one answer, so choosing a label
              // clears anything typed: the screen and the answer agree.
              drafts.set(item.id, { selected: input.checked ? [option.label] : [], custom: "" });
              if (free !== null) free.value = "";
            },
          },
        });
        if (!item.multiSelect) radios.push(input);
        list.appendChild(
          el(
            "label",
            { class: "choice question-option" },
            el("span", { class: "question-option-number", attrs: { "aria-hidden": "true" }, text: String(index + 1) }),
            input,
            el(
              "span",
              { class: "choice-text" },
              el(
                "span",
                { class: "choice-title" },
                el("span", { text: displayLabel(option.label) }),
                option.recommended ? el("span", { class: "question-recommended", text: "Recommended" }) : null,
              ),
              option.description.trim() === ""
                ? null
                : el("span", { class: "question-option-sub", text: option.description }),
            ),
          ),
        );
      });
      children.push(list);
    }

    free = el("textarea", {
      class: "input question-free",
      attrs: {
        id: "question-free-input",
        rows: "2",
        placeholder: "Type your answer",
        "aria-describedby": FREE_NOTE_ID,
        autocomplete: "off",
        autocapitalize: "sentences",
      },
      on: {
        input: () => {
          const current = draftOf(item.id);
          const custom = free?.value ?? "";
          // For a single-select question the typed answer *is* the answer, so
          // it overrides the list; for a multi-select one it supplements it.
          if (!item.multiSelect && custom.trim() !== "") {
            for (const radio of radios) radio.checked = false;
            drafts.set(item.id, { selected: [], custom });
            return;
          }
          drafts.set(item.id, { selected: current.selected, custom });
        },
      },
    });
    free.value = draft.custom;
    children.push(
      el(
        "div",
        { class: "question-free-field" },
        el("label", {
          class: "question-free-label",
          attrs: { for: "question-free-input" },
          text: item.options.length === 0 ? "Your answer" : "Or type your own",
        }),
        free,
        el("p", {
          class: "question-free-note",
          attrs: { id: FREE_NOTE_ID },
          text:
            item.multiSelect
              ? "Adds to the choices above."
              : item.options.length > 0
                ? "Replaces the choice above."
                : "This question has no choices — type your answer.",
        }),
      ),
    );

    body.replaceChildren(...children);
    progressText.textContent = `${page + 1}/${items.length}`;
    // One question is not a page of them: an indicator and two arrows that
    // cannot move would be furniture the reader has to read past.
    progressRow.hidden = items.length < 2;
    primary.textContent = page === items.length - 1 ? "Submit" : "Next";
    sync();
  };

  /* -------------------------------------------------------------- clock */

  const sync = (): void => {
    const state = ctx.store.state;
    const busy = state.questionBusyId === question.id;
    // A submission that came back as a problem — a label the server no longer
    // offers, a lost race that did not close the card — gives the controls back.
    // Leaving the card disabled would trap the reader in a modal whose only exit
    // is a `Skip` they cannot press; the error text says what the server said.
    if (!busy && state.questionError !== null) settled = false;
    const blocked = settled || expired || busy;
    // The arrows stop at the ends of the request: paging past the last question
    // would submit, which is the primary button's job, not an arrow's.
    back.disabled = blocked || page === 0;
    forward.disabled = blocked || page === items.length - 1;
    skip.disabled = blocked;
    primary.disabled = blocked;
    for (const control of body.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>("input, textarea")) {
      control.disabled = blocked;
    }
    // A class rather than an inline opacity, because the CSP forbids the
    // attribute and the CSSOM write alike.
    card.classList.toggle("is-blocked", blocked);

    const message = state.questionError;
    if (message === null) {
      errorBox.setAttribute("hidden", "");
    } else {
      errorBox.textContent = message;
      errorBox.removeAttribute("hidden");
    }
  };

  const tick = (): void => {
    if (Number.isNaN(expiresAt)) {
      countdown.textContent = "no deadline given";
      timerRow.classList.add("is-unknown");
      return;
    }
    const remaining = expiresAt - Date.now();
    countdown.textContent = formatCountdown(remaining);
    progressBar.setAttribute("value", String(Math.max(0, Math.min(100, Math.round((remaining / totalWindow) * 100)))));
    card.classList.toggle("is-urgent", remaining <= URGENT_MS);

    if (remaining <= 0 && !expired) {
      expired = true;
      countdown.textContent = "expired";
      sync();
      // The server withdraws on its own schedule; refreshing is how the card
      // disappears once the question has actually closed there.
      void ctx.loadQuestions();
    }
  };

  /* ------------------------------------------------------------ actions */

  const submit = (): void => {
    if (settled || expired) return;
    // One answer set per card: a second tap while the first POST is in flight
    // would come back as `409 question_answered`.
    settled = true;
    sync();
    void ctx.answerQuestion(question, answersFor(items, drafts));
  };

  const goTo = (next: number): void => {
    if (next < 0 || next >= items.length) return;
    page = next;
    renderPage();
    // The new question is what a reader has to hear next, and the heading is
    // the one node that carries it.
    heading?.focus({ preventScroll: true });
  };

  back.addEventListener("click", () => {
    goTo(page - 1);
  });
  forward.addEventListener("click", () => {
    goTo(page + 1);
  });
  primary.addEventListener("click", () => {
    if (page === items.length - 1) {
      submit();
      return;
    }
    goTo(page + 1);
  });
  skip.addEventListener("click", () => {
    const item = items[page];
    if (item === undefined) return;
    // Skipping is a decision, not an absence: the draft is cleared so the reader
    // can see that this question will be sent unanswered, and on the last page
    // that cleared answer is submitted with the rest rather than in place of it.
    drafts.set(item.id, { selected: [], custom: "" });
    if (page === items.length - 1) {
      // Redrawn before the POST, so what the reader sees is what is being sent.
      // If the request fails they are left looking at the truth rather than at a
      // choice that is no longer in the answer.
      renderPage();
      submit();
      return;
    }
    goTo(page + 1);
  });

  const timer = window.setInterval(tick, TICK_MS);
  renderPage();
  tick();

  let releaseTrap: (() => void) | null = null;

  return {
    node: panel,
    refresh: sync,
    activate: () => {
      releaseTrap = trapFocus(card);
      // `trapFocus` focuses the first *control*, which here is the progress
      // arrow — a form should open on its question instead, so focus is moved
      // onto the heading. Nothing escapes by it: the heading sits after the
      // arrows in document order, so Shift+Tab from it reaches the arrow rather
      // than the page behind the dialog.
      heading?.focus({ preventScroll: true });
    },
    dispose: () => {
      window.clearInterval(timer);
      releaseTrap?.();
      panel.remove();
    },
  };
}

/* ------------------------------------------------------------------ mount */

export function mountQuestions(root: HTMLElement, ctx: Ctx): () => void {
  // An inner host, so `replaceChildren` swaps the card without touching whatever
  // else lives in `root` (which is `body`, and would be erased).
  const host = el("div", { class: "question-host" });
  root.appendChild(host);

  /**
   * What stays interactive while this sheet is up.
   *
   * The notice region is spared for the same reason the approval sheet spares
   * it: inert also removes a live region from the accessibility tree, and "that
   * question had already expired" is exactly the message that must still be read.
   *
   * The approval host is spared too, and that is this sheet's one departure from
   * its neighbour. Approval.ts mounts first and cannot know about a host added
   * later, so its own isolation already makes this host inert while an approval
   * is pending — which is right, the newest blocking card wins. The reverse must
   * not happen: an approval that arrives while a question is open would be drawn
   * but untouchable, and both block an agent, so neither may make the other
   * unanswerable.
   */
  const keepAlive: HTMLElement[] = [host];
  const noticeHost = document.getElementById("notice-host");
  if (noticeHost !== null) keepAlive.push(noticeHost);
  const approvalHost = document.querySelector<HTMLElement>(".approval-host");
  if (approvalHost !== null) keepAlive.push(approvalHost);

  let currentId: string | null = null;
  let current: QuestionCard | null = null;
  let restoreBackground: (() => void) | null = null;

  const clear = (): void => {
    current?.dispose();
    current = null;
    currentId = null;
    restoreBackground?.();
    restoreBackground = null;
  };

  const offList = ctx.store.select(
    (state) => state.questions,
    (questions) => {
      const head = questions[0];
      if (head === undefined) {
        clear();
        return;
      }
      if (head.id === currentId && current !== null) {
        // Same question: keep the live card, its clock and whatever has been
        // typed into it rather than rebuilding, which would restart the countdown
        // and throw the drafts away.
        current.refresh();
        return;
      }
      clear();
      const card = buildCard(head, questions.length - 1, ctx);
      current = card;
      host.replaceChildren(card.node);
      // Insert first, then trap and focus; the rest of the app goes inert so
      // `aria-modal` is not just a claim.
      card.activate();
      restoreBackground = isolateBackground(keepAlive);
      currentId = head.id;
    },
  );

  // The busy flag and the error text live outside `questions`, so they need
  // their own subscription to reach an already-rendered card.
  const offBusy = ctx.store.select(
    (state) => [state.questionBusyId, state.questionError] as const,
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
