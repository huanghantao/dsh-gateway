/**
 * Small shared pieces of UI: badges, a bottom sheet and a labelled `<select>`.
 *
 * These three earn their place by being used by more than one screen and by
 * encoding an accessibility decision once: the sheet is a real modal dialog
 * with a focus trap, and every select carries a visible label rather than a
 * placeholder (a placeholder disappears exactly when it is needed).
 */

import { el, on, trapFocus } from "../dom.js";

export type BadgeVariant = "leased" | "busy" | "muted" | "error" | "ok";

export function badge(label: string, variant: BadgeVariant = "muted"): HTMLElement {
  return el("span", { class: `badge badge-${variant}`, text: label });
}

export interface SheetOptions {
  readonly title: string;
  readonly body: Node;
  readonly onClose?: () => void;
}

export interface Sheet {
  close(): void;
}

/**
 * Opens a modal sheet in `#sheet-host`.
 *
 * The sheet is removed from the DOM on close rather than hidden, so a trap or a
 * timer inside it cannot keep running behind the user's back.
 */
export function openSheet(options: SheetOptions): Sheet {
  const host = document.getElementById("sheet-host");
  if (host === null) throw new Error("ui: #sheet-host is missing from the shell");

  const panel = el("div", {
    class: "sheet",
    attrs: { role: "dialog", "aria-modal": "true", "aria-label": options.title, tabindex: "-1" },
  });

  const closeButton = el("button", {
    class: "btn btn-ghost btn-icon",
    attrs: { type: "button", "aria-label": "Close" },
    text: "\u00d7",
  });

  panel.appendChild(
    el(
      "header",
      { class: "sheet-head" },
      el("h2", { class: "sheet-title", text: options.title }),
      closeButton,
    ),
  );
  panel.appendChild(el("div", { class: "sheet-body" }, options.body));

  const backdrop = el("div", { class: "sheet-backdrop" }, panel);
  host.appendChild(backdrop);

  // After insertion, never before: `trapFocus` moves focus, and a detached node
  // cannot take it.
  const releaseTrap = trapFocus(panel);
  let closed = false;
  const close = (): void => {
    if (closed) return;
    closed = true;
    offEscape();
    offBackdrop();
    offClose();
    releaseTrap();
    backdrop.remove();
    options.onClose?.();
  };

  const offEscape = on(backdrop, "keydown", (event) => {
    if (event.key === "Escape") {
      event.stopPropagation();
      close();
    }
  });
  const offBackdrop = on(backdrop, "click", (event) => {
    // Only a click that lands on the backdrop itself dismisses; a click inside
    // the panel has a different target.
    if (event.target === backdrop) close();
  });
  const offClose = on(closeButton, "click", close);

  return { close };
}

export interface SelectOption {
  readonly value: string;
  readonly label: string;
}

/** A labelled `<select>`; the label is a real element, not a placeholder. */
export function selectField(
  id: string,
  label: string,
  options: readonly SelectOption[],
  value: string,
  onChange: (value: string) => void,
): HTMLElement {
  const select = el("select", { class: "input", attrs: { id, name: id } });
  for (const option of options) {
    const node = el("option", { attrs: { value: option.value }, text: option.label });
    // `selected` as a property survives the option being appended later.
    if (option.value === value) node.selected = true;
    select.appendChild(node);
  }
  // The listener is garbage-collected with the node, so there is nothing to
  // return; callers that need teardown remove the whole field from the DOM.
  on(select, "change", () => onChange(select.value));

  return el("label", { class: "field", attrs: { for: id } }, el("span", { class: "field-label", text: label }), select);
}

/**
 * The "nothing here" state, optionally with the one thing worth doing next.
 *
 * The action matters: most empty states in this product appear right after a
 * search that was too narrow, and a dead end with a suggestion in it is worse
 * than a button that acts on the suggestion.
 */
export function emptyState(title: string, body: string, action?: { label: string; run: () => void }): HTMLElement {
  const nodes: Node[] = [
    el("p", { class: "empty-title", text: title }),
    el("p", { class: "muted", text: body }),
  ];
  if (action !== undefined) {
    nodes.push(
      el("button", {
        class: "btn btn-small",
        attrs: { type: "button" },
        text: action.label,
        on: { click: action.run },
      }),
    );
  }
  return el("div", { class: "empty" }, ...nodes);
}

export function spinner(label: string): HTMLElement {
  return el("p", { class: "loading", attrs: { role: "status" }, text: label });
}
