/**
 * A very small DOM builder.
 *
 * Two of its constraints come straight from the server's Content-Security
 * Policy, which grants `style-src` a nonce and no `'unsafe-inline'`:
 *
 * - There is no `html` option. Untrusted text can only become a text node, so
 *   an XSS in a model response is structurally impossible rather than
 *   "carefully avoided". `markdown.ts` obeys the same rule.
 * - Nothing here writes to `element.style`. Setting a CSS property through the
 *   CSSOM is blocked by a nonce-based `style-src` in every current browser, so
 *   dynamic geometry has to be expressed with classes, attributes (`<progress>`
 *   `value`, `hidden`, `open`) or scrolling — never with a style attribute.
 */

type Child = Node | string | null | undefined | false;

type EventMapOf<T extends Element> = T extends HTMLElement
  ? HTMLElementEventMap
  : T extends SVGElement
    ? SVGElementEventMap
    : ElementEventMap;

export interface ElementOptions<T extends Element = HTMLElement> {
  readonly class?: string;
  /** Convenience for the single-text-node case; never parsed as markup. */
  readonly text?: string;
  readonly attrs?: Readonly<Record<string, string | number | boolean | null | undefined>>;
  readonly on?: Partial<{ [K in keyof EventMapOf<T>]: (event: EventMapOf<T>[K]) => void }>;
}

function applyAttrs(node: Element, attrs: ElementOptions["attrs"]): void {
  if (attrs === undefined) return;
  for (const [name, value] of Object.entries(attrs)) {
    if (value === null || value === undefined || value === false) continue;
    node.setAttribute(name, value === true ? "" : String(value));
  }
}

function applyEvents(node: Element, on: ElementOptions["on"]): void {
  if (on === undefined) return;
  for (const [type, handler] of Object.entries(on)) {
    if (typeof handler !== "function") continue;
    // `Object.entries` erases the per-key event type; the public signature
    // already constrains it, so this is the one place a cast is warranted.
    node.addEventListener(type, handler as EventListener);
  }
}

function appendChildren(node: Node, children: readonly Child[]): void {
  for (const child of children) {
    if (child === null || child === undefined || child === false) continue;
    node.appendChild(typeof child === "string" ? document.createTextNode(child) : child);
  }
}

/** The single constructor used by every view. */
export function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  options?: ElementOptions<HTMLElementTagNameMap[K]>,
  ...children: readonly Child[]
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  if (options !== undefined) {
    if (options.class !== undefined) node.className = options.class;
    applyAttrs(node, options.attrs);
    applyEvents(node, options.on as ElementOptions["on"]);
  }
  appendChildren(node, children);
  if (options?.text !== undefined) node.textContent = options.text;
  return node;
}

export function mustFind<T extends Element>(root: ParentNode, selector: string): T {
  const found = root.querySelector<T>(selector);
  // A missing node here is a programming error in a template we own, and
  // failing loudly beats a null-dereference three frames later.
  if (found === null) throw new Error(`dom: no element matches ${selector}`);
  return found;
}

/** Adds a listener and returns its remover, so views can clean up in one call. */
export function on<K extends keyof HTMLElementEventMap>(
  target: EventTarget,
  type: K | string,
  handler: (event: HTMLElementEventMap[K]) => void,
  options?: AddEventListenerOptions,
): () => void {
  const listener = handler as EventListener;
  target.addEventListener(type, listener, options);
  return () => target.removeEventListener(type, listener, options);
}

/* --------------------------------------------------------------- scrolling */

/** Pins a scroll container to its bottom; used by the message log. */
export function scrollToEnd(node: HTMLElement): void {
  node.scrollTop = node.scrollHeight;
}

/**
 * Keeps `node` inside the *visual* viewport when the software keyboard opens.
 *
 * On iOS the layout viewport does not shrink for the keyboard — only
 * `visualViewport` does — so `100dvh` alone leaves the composer underneath it.
 * The conventional fix is to scroll the page by the overshoot; that mutates
 * scroll position, not style, so the CSP does not block it.
 */
export function pinAboveKeyboard(node: HTMLElement): () => void {
  const viewport = window.visualViewport;
  if (viewport === null || viewport === undefined) return () => {};

  let frame = 0;
  const settle = (): void => {
    frame = 0;
    const rect = node.getBoundingClientRect();
    const overshoot = rect.bottom - (viewport.height + viewport.offsetTop);
    if (overshoot > 1) window.scrollBy({ top: overshoot, behavior: "auto" });
  };
  const schedule = (): void => {
    if (frame === 0) frame = window.requestAnimationFrame(settle);
  };

  viewport.addEventListener("resize", schedule);
  viewport.addEventListener("scroll", schedule);
  return () => {
    if (frame !== 0) window.cancelAnimationFrame(frame);
    viewport.removeEventListener("resize", schedule);
    viewport.removeEventListener("scroll", schedule);
  };
}

/* ------------------------------------------------------------ focus trap */

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), ' +
  'textarea:not([disabled]), summary, [tabindex]:not([tabindex="-1"])';

/**
 * Confines Tab to `container` while it is open. Returns a release function that
 * also restores focus to whatever was focused before, which is what makes an
 * interrupting modal tolerable to keyboard and screen-reader users.
 *
 * Call this only once `container` is in the document: focusing a detached node
 * is a silent no-op, which is exactly how a modal ends up leaving focus on the
 * page behind it.
 */
export function trapFocus(container: HTMLElement): () => void {
  const previous = document.activeElement;
  const focusables = (): readonly HTMLElement[] =>
    Array.from(container.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
      (node) => node.offsetParent !== null || node === document.activeElement,
    );

  const first = focusables()[0];
  // Prefer the first control; fall back to the container itself (which carries
  // `tabindex="-1"`), so focus always lands inside the dialog.
  if (first === undefined) container.focus({ preventScroll: true });
  else first.focus({ preventScroll: true });

  const remove = on(container, "keydown", (event) => {
    if (event.key !== "Tab") return;
    const items = focusables();
    const head = items[0];
    const tail = items[items.length - 1];
    if (head === undefined || tail === undefined) return;
    // Wrap manually: without this, Tab escapes into the page behind the modal.
    if (event.shiftKey && document.activeElement === head) {
      event.preventDefault();
      tail.focus();
    } else if (!event.shiftKey && document.activeElement === tail) {
      event.preventDefault();
      head.focus();
    }
  });

  return () => {
    remove();
    if (previous instanceof HTMLElement && document.contains(previous)) previous.focus();
  };
}

/**
 * Marks everything outside `keep` as inert while a blocking modal is open.
 * `aria-modal` alone is a promise the rest of the page cannot be reached; this
 * makes it true for assistive technology and for Tab alike. Returns a function
 * that restores the previous state.
 */
export function isolateBackground(keep: readonly Element[]): () => void {
  const touched: { element: HTMLElement; wasInert: boolean }[] = [];
  for (const candidate of document.querySelectorAll<HTMLElement>("body > *")) {
    if (keep.includes(candidate)) continue;
    if (!("inert" in candidate)) continue;
    touched.push({ element: candidate, wasInert: candidate.inert });
    candidate.inert = true;
  }
  return () => {
    for (const { element, wasInert } of touched) element.inert = wasInert;
  };
}

