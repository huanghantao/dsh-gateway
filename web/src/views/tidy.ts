/**
 * The tidy-up sheet: the gateway's suggestion about what can be put away, shown
 * before anything happens.
 *
 * Why a sheet and not a button that just does it: the rule that decides what
 * leaves a list is a heuristic, and a heuristic that acts without being read is
 * indistinguishable from a bug. The operator sees every candidate, the sentence
 * that explains why each one is there, and can uncheck any of them — and what
 * they do not see, because it is not a candidate, is their own work. Archive,
 * never delete: the sheet's promise is that everything here can come back.
 */

import type { Ctx } from "../actions.js";
import { el, on } from "../dom.js";
import { basename, pluralize, shortWorkspace } from "../format.js";
import type { TriageCandidate, TriageReport } from "../types.js";
import { openSheet, spinner } from "./ui.js";

/** How many candidates are drawn before the list is summarised instead. */
const MAX_ROWS = 80;

export interface TidyOptions {
  /** Called after a successful archive, so the screen can offer an undo. */
  readonly onArchived: (count: number, ids: readonly string[]) => void;
}

export function openTidySheet(ctx: Ctx, options: TidyOptions): void {
  const body = el("div", { class: "stack" }, spinner("Looking for test runs and drafts…"));
  const sheet = openSheet({ title: "Tidy up", body });

  void ctx
    .triage()
    .then((report) => {
      body.replaceChildren(...renderReport(report));
    })
    .catch((cause: unknown) => {
      body.replaceChildren(
        el("p", {
          class: "alert alert-error",
          attrs: { role: "alert" },
          text: cause instanceof Error ? cause.message : "The sessions could not be examined.",
        }),
      );
    });

  function renderReport(report: TriageReport): readonly Node[] {
    const { summary, candidates } = report;
    if (summary.candidates === 0 || candidates.length === 0) {
      return [
        el("p", { class: "tidy-lead", text: "Nothing to tidy." }),
        el("p", {
          class: "muted",
          text: `${summary.scanned} ${pluralize(summary.scanned, "session")} examined; none of them reads like a test run, lives in a scratch directory, or was never used.`,
        }),
      ];
    }

    const selected = new Set(candidates.map((candidate) => candidate.id));
    const nodes: Node[] = [];

    nodes.push(
      el("p", {
        class: "tidy-lead",
        text: `${summary.candidates} of ${summary.scanned} sessions look like test runs or drafts.`,
      }),
    );
    nodes.push(
      el(
        "p",
        { class: "muted tidy-note" },
        summary.test > 0 ? `${summary.test} with a test-like title` : null,
        summary.test > 0 && (summary.temp > 0 || summary.draft > 0) ? " · " : null,
        summary.temp > 0 ? `${summary.temp} in a scratch directory` : null,
        summary.temp > 0 && summary.draft > 0 ? " · " : null,
        summary.draft > 0 ? `${summary.draft} never used` : null,
        summary.archived > 0 ? ` · ${summary.archived} already archived` : null,
      ),
    );

    const count = el("span", { class: "tidy-count" });
    const archive = el("button", { class: "btn btn-primary btn-block", attrs: { type: "button" } });
    const status = el("p", { class: "muted", attrs: { role: "status" }, text: "" });
    const boxes: HTMLInputElement[] = [];

    const syncCount = (): void => {
      const n = selected.size;
      archive.textContent = n === 0 ? "Nothing selected" : `Archive ${n} ${pluralize(n, "session")}`;
      archive.disabled = n === 0;
      count.textContent =
        n === candidates.length ? "All selected" : `${n} of ${candidates.length} selected`;
    };

    const list = el("div", { class: "tidy-list", attrs: { role: "group", "aria-label": "Sessions to archive" } });
    const drawn = candidates.slice(0, MAX_ROWS);
    for (const candidate of drawn) {
      boxes.push(appendCandidate(list, candidate, selected, syncCount));
    }
    nodes.push(list);
    if (candidates.length > drawn.length) {
      nodes.push(
        el("p", {
          class: "muted",
          text: `${candidates.length - drawn.length} more are included but not listed here.`,
        }),
      );
    }

    const all = el("button", {
      class: "btn btn-ghost",
      attrs: { type: "button" },
      text: "Select all",
      on: {
        click: () => {
          for (const candidate of candidates) selected.add(candidate.id);
          for (const box of boxes) box.checked = true;
          syncCount();
        },
      },
    });
    const none = el("button", {
      class: "btn btn-ghost",
      attrs: { type: "button" },
      text: "Select none",
      on: {
        click: () => {
          selected.clear();
          for (const box of boxes) box.checked = false;
          syncCount();
        },
      },
    });
    nodes.push(el("div", { class: "tidy-controls" }, count, all, none));

    on(archive, "click", () => {
      const ids = [...selected];
      if (ids.length === 0) return;
      archive.disabled = true;
      status.textContent = "Archiving…";
      void ctx
        .curate(ids, { archived: true })
        .then(() => {
          sheet.close();
          options.onArchived(ids.length, ids);
        })
        .catch((cause: unknown) => {
          status.textContent = cause instanceof Error ? cause.message : "The sessions could not be archived.";
          archive.disabled = false;
        });
    });

    nodes.push(archive, status);
    nodes.push(
      el("p", {
        class: "muted tidy-note",
        text: "Archived sessions leave this list, stay searchable, and can be restored from the Archived view.",
      }),
    );
    syncCount();
    return nodes;
  }

  function appendCandidate(
    list: HTMLElement,
    candidate: TriageCandidate,
    selected: Set<string>,
    onToggle: () => void,
  ): HTMLInputElement {
    const box = el("input", {
      class: "choice-input",
      attrs: { type: "checkbox", ...(selected.has(candidate.id) ? { checked: "" } : {}) },
      on: {
        change: () => {
          if (box.checked) selected.add(candidate.id);
          else selected.delete(candidate.id);
          onToggle();
        },
      },
    });
    const title = candidate.title.trim() || candidate.preview.trim() || basename(candidate.workspace);
    list.appendChild(
      el(
        "label",
        { class: "tidy-item" },
        box,
        el(
          "span",
          { class: "tidy-text" },
          el("span", { class: "tidy-title", text: title }),
          el("span", {
            class: "tidy-sub",
            text: `${shortWorkspace(candidate.workspace)} · ${candidate.reason}${
              candidate.messages > 0 ? ` · ${candidate.messages} ${pluralize(candidate.messages, "message")}` : ""
            }`,
          }),
        ),
      ),
    );
    return box;
  }
}
