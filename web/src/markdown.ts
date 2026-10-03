/**
 * A deliberately small Markdown subset, rendered straight to DOM nodes.
 *
 * Scope is exactly what the contract says model output contains: fenced code
 * blocks, inline code, bold and links. Anything else is literal text, which is
 * the correct failure mode for a chat transcript.
 *
 * Safety rests on construction rather than sanitising: the renderer only ever
 * creates text nodes and a fixed set of elements, so there is no `innerHTML`
 * call to get wrong. Link targets are additionally re-parsed and restricted to
 * `http`/`https`/`mailto`, which rejects `javascript:` and `data:` URLs even if
 * a future edit forgets why that check is there.
 */

import { copyButton } from "./copybutton.js";
import { el } from "./dom.js";

const FENCE = /^```(.*)$/;
const CLOSING_FENCE = /^```\s*$/;

type Block = { readonly kind: "code"; readonly lang: string; readonly code: string } | { readonly kind: "paragraph"; readonly lines: readonly string[] };

function splitBlocks(text: string): readonly Block[] {
  const lines = text.split("\n");
  // `noUncheckedIndexedAccess` makes `lines[i]` optional; this collapses the
  // bounds check into one place instead of at every use.
  const at = (index: number): string => lines[index] ?? "";
  const blocks: Block[] = [];
  let paragraph: string[] = [];

  const flush = (): void => {
    if (paragraph.length > 0) {
      blocks.push({ kind: "paragraph", lines: paragraph });
      paragraph = [];
    }
  };

  for (let index = 0; index < lines.length; index += 1) {
    const line = at(index);
    const fence = FENCE.exec(line);
    if (fence !== null) {
      flush();
      const lang = (fence[1] ?? "").trim();
      const body: string[] = [];
      index += 1;
      while (index < lines.length && !CLOSING_FENCE.test(at(index))) {
        body.push(at(index));
        index += 1;
      }
      // An unterminated fence still renders as code: a streaming response
      // routinely ends mid-block, and swallowing the text would be worse.
      blocks.push({ kind: "code", lang, code: body.join("\n") });
      continue;
    }
    if (line.trim() === "") {
      flush();
      continue;
    }
    paragraph.push(line);
  }
  flush();
  return blocks;
}

/** Returns an absolute URL only for schemes that cannot execute script. */
function safeUrl(raw: string): string | null {
  let url: URL;
  try {
    url = new URL(raw, window.location.href);
  } catch {
    return null;
  }
  if (url.protocol === "http:" || url.protocol === "https:" || url.protocol === "mailto:") {
    return url.href;
  }
  return null;
}

/** Appends the inline spans of `text` to `target` in a single left-to-right pass. */
function renderInline(target: Node, text: string): void {
  let literal = "";
  let index = 0;

  const flushLiteral = (): void => {
    if (literal !== "") {
      target.appendChild(document.createTextNode(literal));
      literal = "";
    }
  };

  while (index < text.length) {
    const char = text.charAt(index);

    if (char === "`") {
      const close = text.indexOf("`", index + 1);
      if (close > index + 1) {
        flushLiteral();
        target.appendChild(el("code", { class: "md-inline-code", text: text.slice(index + 1, close) }));
        index = close + 1;
        continue;
      }
    }

    if (char === "*" && text.charAt(index + 1) === "*") {
      const close = text.indexOf("**", index + 2);
      if (close > index + 2) {
        flushLiteral();
        const strong = el("strong");
        renderInline(strong, text.slice(index + 2, close));
        target.appendChild(strong);
        index = close + 2;
        continue;
      }
    }

    if (char === "[") {
      const labelEnd = text.indexOf("]", index + 1);
      if (labelEnd > index + 1 && text.charAt(labelEnd + 1) === "(") {
        const urlEnd = text.indexOf(")", labelEnd + 2);
        if (urlEnd > labelEnd + 2) {
          const href = safeUrl(text.slice(labelEnd + 2, urlEnd).trim());
          if (href !== null) {
            flushLiteral();
            const anchor = el("a", {
              class: "md-link",
              attrs: { href, target: "_blank", rel: "noopener noreferrer" },
            });
            renderInline(anchor, text.slice(index + 1, labelEnd));
            target.appendChild(anchor);
            index = urlEnd + 1;
            continue;
          }
        }
      }
    }

    literal += char;
    index += 1;
  }
  flushLiteral();
}

function codeBlock(lang: string, code: string): HTMLElement {
  const bar = el("div", { class: "md-code-bar" }, el("span", { class: "md-lang", text: lang === "" ? "code" : lang }));
  // Always offered: the fallback in `copyText` is what makes this work over
  // plain HTTP, where `navigator.clipboard` does not exist at all.
  bar.appendChild(copyButton(code, "md-copy"));
  return el("div", { class: "md-block" }, bar, el("pre", { class: "md-pre" }, el("code", { text: code })));
}

function paragraph(lines: readonly string[]): HTMLElement {
  const node = el("p", { class: "md-p" });
  lines.forEach((line, position) => {
    if (position > 0) node.appendChild(el("br"));
    renderInline(node, line);
  });
  return node;
}

/** Renders `text` as a fragment of safe nodes. */
export function renderMarkdown(text: string): DocumentFragment {
  const fragment = document.createDocumentFragment();
  for (const block of splitBlocks(text)) {
    fragment.appendChild(block.kind === "code" ? codeBlock(block.lang, block.code) : paragraph(block.lines));
  }
  return fragment;
}
