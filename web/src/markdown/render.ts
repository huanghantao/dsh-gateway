/**
 * A Markdown tree, drawn as DOM nodes.
 *
 * Everything here is a translation: `parse.ts` has already decided what each
 * block and span *is*, and this file only decides which element it becomes. That
 * is the whole reason it can be read in one sitting, and the reason the safety
 * property is checkable rather than asserted:
 *
 *   Safety rests on construction rather than sanitising. Only `el` and
 *   `document.createTextNode` are called, and `el` is a `createElement` wrapper
 *   that takes text, never markup — so there is no `innerHTML` call to get
 *   wrong, and a future edit that wanted one would have to add it here. A test
 *   holds the element set below to a fixed list, and fails on `innerHTML`.
 *
 * Two things are left undrawn for that reason rather than for budget, and they
 * are the two the grammar does not already refuse:
 *
 * - **Raw HTML.** Drawing it would be the injection point this file exists to
 *   not have, and there is no sanitizer to get wrong because there is nothing to
 *   sanitize. `<script>` reaches the screen as those eight characters.
 * - **Images.** A remote image is a beacon: the host learns the reader's address
 *   every time the transcript is opened, and `PRIVACY.md` undertakes to list
 *   every third party a deployment involves. `![alt](url)` falls out of the
 *   grammar as a literal `!` followed by a link, which is honest about what was
 *   there.
 *
 * The grammar's own omissions are listed in `parse.ts`.
 */

import { copyButton } from "../copybutton.js";
import { el } from "../dom.js";
import { parseMarkdown, type Alignment, type Block, type ListBlock, type Span, type TableBlock } from "./parse.js";

/* ---------------------------------------------------------------- the spans */

/**
 * The rendered heading level is one below the written one: the conversation view
 * already owns the page's `h1` (the session title), so a message's `#` opens at
 * `h2` rather than starting a second top level. The stylesheet's `:first-child`
 * rule keeps the first one from pushing the message down.
 */
const HEADING_TAGS = ["h2", "h3", "h4", "h5", "h6", "h6"] as const;

const EMPHASIS_TAGS = { em: "em", strong: "strong", strike: "s" } as const;

function appendSpans(target: Node, spans: readonly Span[]): void {
  for (const span of spans) {
    switch (span.kind) {
      case "text":
        target.appendChild(document.createTextNode(span.text));
        break;
      case "break":
        target.appendChild(el("br"));
        break;
      case "code":
        target.appendChild(el("code", { class: "md-inline-code", text: span.text }));
        break;
      case "emphasis": {
        const node = el(EMPHASIS_TAGS[span.style]);
        appendSpans(node, span.spans);
        target.appendChild(node);
        break;
      }
      case "link": {
        const anchor = el("a", {
          class: "md-link",
          attrs: { href: span.target, target: "_blank", rel: "noopener noreferrer" },
        });
        appendSpans(anchor, span.spans);
        target.appendChild(anchor);
        break;
      }
    }
  }
}

/* --------------------------------------------------------------- the blocks */

/**
 * A column's alignment becomes a class rather than a `style`, because the CSP
 * grants `style-src` a nonce and no `'unsafe-inline'`: an inline style would be
 * refused by the browser, and the app has no dynamic styling anywhere.
 */
function alignmentClass(alignment: Alignment): string {
  return alignment === null ? "" : ` is-${alignment}`;
}

function appendHeading(target: Node, depth: number, spans: readonly Span[]): void {
  const node = el(HEADING_TAGS[depth - 1] ?? "h6", { class: `md-h md-h${depth}` });
  appendSpans(node, spans);
  target.appendChild(node);
}

function appendTable(target: Node, block: TableBlock): void {
  const table = el("table", { class: "md-table" });

  const head = el("thead");
  appendRow(head, block.header, block.alignments, "th");
  table.appendChild(head);

  const body = el("tbody");
  for (const row of block.rows) appendRow(body, row, block.alignments, "td");
  table.appendChild(body);

  // A four-column table cannot fit 390px, and the page must not scroll sideways
  // to show it: the wrapper takes the overflow so the transcript keeps its width.
  target.appendChild(el("div", { class: "md-table-wrap" }, table));
}

/** Appends one `<tr>` of `th` or `td`, aligned by column. */
function appendRow(target: Node, cells: readonly (readonly Span[])[], alignments: readonly Alignment[], tag: "th" | "td"): void {
  const tr = el("tr");
  cells.forEach((cell, column) => {
    const node = el(tag, { class: `md-${tag}${alignmentClass(alignments[column] ?? null)}` });
    appendSpans(node, cell);
    tr.appendChild(node);
  });
  target.appendChild(tr);
}

function appendList(target: Node, block: ListBlock): void {
  const list = el(block.ordered ? "ol" : "ul", { class: "md-list" });
  if (block.ordered && block.start !== 1) list.setAttribute("start", String(block.start));

  // Tight when no item holds more than one paragraph, which is CommonMark's rule
  // stated the short way. A nested list does not make a list loose.
  const tight = block.items.every((item) => item.blocks.filter((child) => child.kind === "paragraph").length <= 1);

  for (const item of block.items) {
    const node = el("li", { class: item.task ? `md-li md-task${item.checked ? " is-done" : ""}` : "md-li" });
    if (item.task) {
      node.appendChild(
        el("input", { class: "md-check", attrs: { type: "checkbox", disabled: true, checked: item.checked } }),
      );
    }
    for (const child of item.blocks) {
      // A tight item's prose is drawn inline, so the list has no paragraph
      // margins between its lines; a loose one keeps them.
      if (tight && child.kind === "paragraph") appendSpans(node, child.spans);
      else appendBlock(node, child);
    }
    list.appendChild(node);
  }

  target.appendChild(list);
}

function appendCode(target: Node, language: string, code: string): void {
  const bar = el(
    "div",
    { class: "md-code-bar" },
    el("span", { class: "md-lang", text: language === "" ? "code" : language }),
  );
  // Always offered: the fallback in `copyText` is what makes this work over
  // plain HTTP, where `navigator.clipboard` does not exist at all.
  bar.appendChild(copyButton(code, "md-copy"));
  target.appendChild(el("div", { class: "md-block" }, bar, el("pre", { class: "md-pre" }, el("code", { text: code }))));
}

function appendBlock(target: Node, block: Block): void {
  switch (block.kind) {
    case "heading":
      appendHeading(target, block.depth, block.spans);
      return;
    case "rule":
      target.appendChild(el("hr", { class: "md-rule" }));
      return;
    case "code":
      appendCode(target, block.language, block.code);
      return;
    case "quote": {
      const quote = el("blockquote", { class: "md-quote" });
      appendBlocks(quote, block.blocks);
      target.appendChild(quote);
      return;
    }
    case "list":
      appendList(target, block);
      return;
    case "table":
      appendTable(target, block);
      return;
    case "paragraph": {
      const node = el("p", { class: "md-p" });
      appendSpans(node, block.spans);
      target.appendChild(node);
      return;
    }
  }
}

function appendBlocks(target: Node, blocks: readonly Block[]): void {
  for (const block of blocks) appendBlock(target, block);
}

/** Renders `text` as a fragment of safe nodes. */
export function renderMarkdown(text: string): DocumentFragment {
  const fragment = document.createDocumentFragment();
  appendBlocks(fragment, parseMarkdown(text));
  return fragment;
}
