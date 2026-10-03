/**
 * The copy control, in one place.
 *
 * Two surfaces offer one — a fenced code block in a model's answer, and a
 * command or a result inside a tool card — and both want the same three things:
 * a tap that works over plain HTTP as well as HTTPS, an answer that says whether
 * it worked, and a label that goes back to normal by itself. Writing that twice
 * is how the two drift: one of them ends up reporting a refused clipboard write
 * as success, which is exactly the bug `copyText` exists to prevent.
 */

import { copyText } from "./clipboard.js";
import { el } from "./dom.js";

/** How long the button says what happened before it says "Copy" again. */
const COPIED_LABEL_MS = 1200;

/** A button that copies `text` and reports what happened. */
export function copyButton(text: string, className = ""): HTMLButtonElement {
  const button = el("button", {
    class: className === "" ? "copy-btn" : `copy-btn ${className}`,
    attrs: { type: "button" },
    text: "Copy",
  });
  let reset = 0;
  button.addEventListener("click", () => {
    void copyText(text).then((copied) => {
      button.textContent = copied ? "Copied" : "Copy failed";
      window.clearTimeout(reset);
      reset = window.setTimeout(() => {
        button.textContent = "Copy";
      }, COPIED_LABEL_MS);
    });
  });
  return button;
}
