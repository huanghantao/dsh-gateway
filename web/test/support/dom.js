/**
 * A DOM small enough to catch the renderer doing something it should not.
 *
 * This is not a DOM implementation and does not try to be one. It implements
 * exactly the surface `markdown/render.ts` is allowed to use, and it *throws*
 * from the ones it is not: a future edit that reached for `innerHTML`,
 * `insertAdjacentHTML` or `outerHTML` fails here rather than in a browser
 * months later. The allow-list of element names is enforced the same way, by
 * recording every tag the renderer asks for.
 *
 * What it cannot check is layout or CSS. `make e2e` is the tier for that.
 */

class Node {
  constructor() {
    this.childNodes = [];
  }

  appendChild(child) {
    this.childNodes.push(child);
    return child;
  }

  get textContent() {
    return this.childNodes.map((child) => child.textContent).join("");
  }

  set textContent(value) {
    this.childNodes = [new Text(String(value))];
  }
}

class Text extends Node {
  constructor(data) {
    super();
    this.data = data;
  }

  get textContent() {
    return this.data;
  }
}

class Element extends Node {
  constructor(tagName, created) {
    super();
    this.tagName = tagName;
    this.className = "";
    this.attributes = new Map();
    created.push(tagName);
  }

  setAttribute(name, value) {
    this.attributes.set(name, String(value));
  }

  getAttribute(name) {
    return this.attributes.get(name) ?? null;
  }

  addEventListener() {}

  // The three ways a string could become markup. None is implemented, so any of
  // them is a failed test rather than a silent XSS.
  set innerHTML(_value) {
    throw new Error("markdown/render.ts must not assign innerHTML");
  }

  set outerHTML(_value) {
    throw new Error("markdown/render.ts must not assign outerHTML");
  }

  insertAdjacentHTML() {
    throw new Error("markdown/render.ts must not call insertAdjacentHTML");
  }
}

class Fragment extends Node {}

export const created = [];

globalThis.document = {
  createElement(tagName) {
    return new Element(tagName, created);
  },
  createTextNode(data) {
    return new Text(data);
  },
  createDocumentFragment() {
    return new Fragment();
  },
};

// `copybutton.ts` reads these from inside a click handler, which never runs
// here, so they only have to exist for the module graph to load. `navigator` is
// deliberately not replaced: it is a read-only global on Node 22, and leaving
// `navigator.clipboard` absent is exactly the plain-HTTP case that the fallback
// in `clipboard.ts` exists for.
globalThis.window = { location: { href: "https://gateway.test/m/" }, clearTimeout, setTimeout };

const VOID_ELEMENTS = new Set(["br", "hr", "input"]);

/** Text and attribute values are escaped, so an expectation cannot be misread. */
const escape = (value) => value.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

/** The tree as one line, so a failure prints something readable. */
export function serialize(node) {
  if (node instanceof Text) return escape(node.data);
  if (node instanceof Fragment) return node.childNodes.map(serialize).join("");

  // `className` first, because that is the order `el` applies them in and an
  // expectation reads better when it matches the source.
  const pairs = node.className === "" ? [...node.attributes] : [["class", node.className], ...node.attributes];
  const attributes = pairs.map(([name, value]) => ` ${name}="${escape(value)}"`).join("");
  const open = `<${node.tagName}${attributes}>`;
  if (VOID_ELEMENTS.has(node.tagName)) return open;
  return `${open}${node.childNodes.map(serialize).join("")}</${node.tagName}>`;
}
