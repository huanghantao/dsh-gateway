/**
 * The trash: what was deleted, and the way back.
 *
 * A delete button that only deletes is a button people are right to be afraid
 * of. This screen is the other half of the promise — every session moved out of
 * the list is still here, with its own title and the day it went, one tap from
 * being put back. The gateway purges after a month, which is the only place
 * anything is ever really removed.
 */

import type { Ctx } from "../actions.js";
import { el, on } from "../dom.js";
import { relativeTime } from "../format.js";
import type { TrashEntry } from "../types.js";
import { emptyState, openSheet, spinner } from "./ui.js";

export function openTrashSheet(ctx: Ctx, onRestored?: () => void): void {
  const body = el("div", { class: "stack" }, spinner("Looking in the trash…"));
  openSheet({ title: "Trash", body });

  const load = (): void => {
    body.replaceChildren(spinner("Looking in the trash…"));
    void ctx
      .trash()
      .then((entries) => {
        body.replaceChildren(...render(entries));
      })
      .catch((cause: unknown) => {
        body.replaceChildren(
          el("p", {
            class: "alert alert-error",
            attrs: { role: "alert" },
            text: cause instanceof Error ? cause.message : "The trash could not be read.",
          }),
        );
      });
  };

  const render = (entries: readonly TrashEntry[]): readonly Node[] => {
    if (entries.length === 0) {
      return [
        emptyState("Nothing in the trash", "Sessions you delete land here, and stay recoverable for 30 days."),
      ];
    }

    const list = el("div", { class: "tidy-list", attrs: { role: "list" } });
    const status = el("p", { class: "muted", attrs: { role: "status" }, text: "" });

    for (const entry of entries) {
      const restore = el("button", {
        class: "btn btn-ghost",
        attrs: { type: "button" },
        text: "Restore",
      });
      on(restore, "click", () => {
        restore.disabled = true;
        status.textContent = "Restoring…";
        void ctx
          .restoreSession(entry.id)
          .then(() => {
            onRestored?.();
            load();
          })
          .catch((cause: unknown) => {
            status.textContent = cause instanceof Error ? cause.message : "That session could not come back.";
            restore.disabled = false;
          });
      });

      const title = entry.title.trim() || entry.preview.trim() || entry.id;
      list.appendChild(
        el(
          "div",
          { class: "tidy-item", attrs: { role: "listitem" } },
          el(
            "span",
            { class: "tidy-text" },
            el("span", { class: "tidy-title", text: title }),
            el("span", {
              class: "tidy-sub",
              text: entry.deletedAt === "" ? "deleted" : `deleted ${relativeTime(entry.deletedAt)}`,
            }),
          ),
          restore,
        ),
      );
    }

    return [
      list,
      status,
      el("p", {
        class: "muted tidy-note",
        text: "The gateway removes trashed sessions for good after 30 days. Until then they are here, whole.",
      }),
    ];
  };

  load();
}
