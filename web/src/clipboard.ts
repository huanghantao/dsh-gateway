/**
 * Copying text to the clipboard, on a page that may not be allowed to.
 *
 * Two facts about this app make the modern Clipboard API optional rather than
 * assumed. It is reached at the LAN address of the machine it runs on — a phone
 * on the same Wi-Fi is the whole point — and `navigator.clipboard` does not
 * exist outside a secure context, so on `http://<mac-ip>:8787/m/` the property is
 * simply absent. And even in a secure context `writeText` rejects for reasons
 * the page cannot see: a denied permission, a document that lost focus, a
 * browser that answers the API and then refuses the write.
 *
 * Neither case may end in a button that does nothing, so this module never
 * throws and never returns "maybe": it reports whether the text reached the
 * clipboard, and callers are expected to show the text some other way when it
 * did not.
 */

/**
 * Copies `text`, resolving `false` when the browser refused.
 *
 * The rejection is deliberately not re-thrown. There is no caller that can do
 * anything with the exception that this module cannot do better itself: the
 * legacy path below works in several of the cases that land here, and when it
 * does not, "the browser would not copy" is a fact to show the reader rather
 * than an error to log.
 */
export async function copyText(text: string): Promise<boolean> {
  const clipboard: Clipboard | undefined = navigator.clipboard;
  if (clipboard !== undefined && typeof clipboard.writeText === "function") {
    try {
      await clipboard.writeText(text);
      return true;
    } catch {
      // Fall through to the legacy path.
    }
  }
  return legacyCopy(text);
}

/**
 * `document.execCommand("copy")`, which is deprecated and still the only way to
 * copy from a page that is not a secure context.
 *
 * The shape matters as much as the call. The field has to be in the document and
 * inside the viewport, because a `display: none` or off-screen node cannot be
 * selected; it must not be `readonly`, because iOS then refuses to select it at
 * all; and it is 16px with `inputmode="none"` so that focusing it neither zooms
 * the page nor raises the on-screen keyboard, the value not being for editing.
 */
function legacyCopy(text: string): boolean {
  const field = document.createElement("textarea");
  field.value = text;
  field.setAttribute("aria-hidden", "true");
  field.setAttribute("inputmode", "none");
  field.setAttribute("autocapitalize", "off");
  field.setAttribute("autocorrect", "off");
  field.setAttribute("spellcheck", "false");
  field.style.cssText =
    "position:fixed;top:50%;left:50%;width:1px;height:1px;margin:0;padding:0;border:0;opacity:0;font-size:16px;";

  // Whatever the reader had selected before the tap is theirs, not ours.
  const selection = document.getSelection();
  const saved = selection !== null && selection.rangeCount > 0 ? selection.getRangeAt(0).cloneRange() : null;

  document.body.appendChild(field);
  field.focus({ preventScroll: true });
  field.select();
  field.setSelectionRange(0, text.length);

  let copied = false;
  try {
    copied = document.execCommand("copy");
  } catch {
    copied = false;
  }

  field.remove();
  if (saved !== null && selection !== null) {
    selection.removeAllRanges();
    selection.addRange(saved);
  }
  return copied;
}
