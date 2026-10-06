/**
 * The Markdown subset this app draws, parsed into a tree.
 *
 * This is the *decision* half: what is a heading, what is a link, what stays
 * literal text. It has no imports and touches no global, which buys two things.
 * The grammar is testable in Node with no DOM (`web/test/markdown.test.js`), and
 * the renderer's one non-negotiable property — that it builds elements from a
 * fixed set and never from a string — can be checked by reading `render.ts`,
 * which is a third of this file and contains no parsing.
 *
 * It is a subset of CommonMark rather than an approximation of it: `#` needs a
 * space, a table needs a delimiter row, a fenced block needs a line of its own.
 * Five constructs are left out on purpose — setext headings, indented code
 * blocks, reference links, footnotes and lazy continuation lines — because each
 * is rare in what a model writes and each costs a branch that has to stay right
 * forever.
 *
 * The one safety decision that lives here rather than in `render.ts` is the link
 * allow-list. Whether `[a](javascript:…)` is a link at all is a question about
 * the grammar, and it has to be answered before there is anything to draw; the
 * two things left undrawn are listed in `render.ts`.
 */

/* --------------------------------------------------------------------- tree */

/** A table column's alignment; null draws no rule. */
export type Alignment = "left" | "center" | "right" | null;

export type Span = TextSpan | CodeSpan | EmphasisSpan | LinkSpan | BreakSpan;

export interface TextSpan {
  readonly kind: "text";
  readonly text: string;
}

export interface CodeSpan {
  readonly kind: "code";
  readonly text: string;
}

/** `*x*`, `**x**` or `~~x~~`. */
export interface EmphasisSpan {
  readonly kind: "emphasis";
  readonly style: "em" | "strong" | "strike";
  readonly spans: readonly Span[];
}

export interface LinkSpan {
  readonly kind: "link";
  /** Already checked against the allow-list; a relative target stays relative. */
  readonly target: string;
  readonly spans: readonly Span[];
}

/** A source line break inside one paragraph of prose. */
export interface BreakSpan {
  readonly kind: "break";
}

/** One table row: the inline content of each column. */
export type Row = readonly (readonly Span[])[];

export type Block =
  | HeadingBlock
  | RuleBlock
  | CodeBlock
  | QuoteBlock
  | ListBlock
  | TableBlock
  | ParagraphBlock;

export interface HeadingBlock {
  readonly kind: "heading";
  /** As written, 1–6. Which element that becomes is the renderer's business. */
  readonly depth: number;
  readonly spans: readonly Span[];
}

export interface RuleBlock {
  readonly kind: "rule";
}

export interface CodeBlock {
  readonly kind: "code";
  /** The fence's info string, or "" when it had none. */
  readonly language: string;
  readonly code: string;
}

export interface QuoteBlock {
  readonly kind: "quote";
  readonly blocks: readonly Block[];
}

export interface ListBlock {
  readonly kind: "list";
  readonly ordered: boolean;
  /** The first item's number, so `3.` starts a list at three. Always 1 for bullets. */
  readonly start: number;
  readonly items: readonly ListItem[];
}

export interface ListItem {
  /** `- [x]` / `- [ ]`: a checkbox rather than a bullet. */
  readonly task: boolean;
  readonly checked: boolean;
  readonly blocks: readonly Block[];
}

export interface TableBlock {
  readonly kind: "table";
  readonly alignments: readonly Alignment[];
  readonly header: Row;
  readonly rows: readonly Row[];
}

export interface ParagraphBlock {
  readonly kind: "paragraph";
  readonly spans: readonly Span[];
}

/* --------------------------------------------------------------- the limits */

/**
 * How deep block nesting may go before the rest is taken literally.
 *
 * A transcript is the model's output, not the operator's, so a pathological
 * `>>>>…` or `- - - -…` has to cost a bad render rather than the stack. Six is
 * past anything a real answer nests a quote inside a list inside a quote.
 */
const MAX_BLOCK_DEPTH = 6;

/** The same guard for inline nesting, which is one recursion per span. */
const MAX_INLINE_DEPTH = 8;

/* ---------------------------------------------------------------- the lines */

/** Leading width in columns, counting a tab as the four it usually renders as. */
function indentOf(line: string): number {
  let width = 0;
  for (const char of line) {
    if (char === " ") width += 1;
    else if (char === "\t") width += 4;
    else break;
  }
  return width;
}

const FENCE = /^```(.*)$/;
const CLOSING_FENCE = /^```\s*$/;
const HEADING = /^ {0,3}(#{1,6})([ \t]+.*)?$/;
const RULE = /^ {0,3}([-*_])[ \t]*(?:\1[ \t]*){2,}$/;
const QUOTE = /^ {0,3}>[ \t]?(.*)$/;
const BULLET = /^([ \t]*)([-*+])([ \t]+)(.*)$/;
const ORDERED = /^([ \t]*)(\d{1,9})([.)])([ \t]+)(.*)$/;
const TASK = /^\[([ xX])\][ \t]+(.*)$/;
const DELIMITER_CELL = /^:?-+:?$/;

/* -------------------------------------------------------------- inline text */

const EMPHASIS_MARKERS = [
  { marker: "**", style: "strong" },
  { marker: "__", style: "strong" },
  { marker: "~~", style: "strike" },
  { marker: "*", style: "em" },
  { marker: "_", style: "em" },
] as const satisfies readonly { readonly marker: string; readonly style: EmphasisSpan["style"] }[];

const WORD_CHARACTER = /[\p{L}\p{N}]/u;
const ESCAPABLE_CHARACTER = /[\\`*_[\]()~#>+\-.!|]/;
const CONTROL_CHARACTER = /[\u0000-\u001f\u007f]/;
const SCHEME = /^([a-z][a-z0-9+.-]*):/i;

const isSpace = (char: string): boolean => char === "" || char === " " || char === "\t";

/**
 * Whether a link target is one this app will follow.
 *
 * The test is on the scheme rather than on `new URL`, because this module must
 * not reach for `window` — and a scheme test is only as good as its agreement
 * with the URL parser, which strips tabs and newlines *before* it looks for the
 * colon. `java\tscript:alert(1)` would therefore reach `javascript:`, so any
 * control character is refused outright rather than normalised away. Nothing
 * legitimate needs one.
 */
function isFollowableTarget(target: string): boolean {
  const trimmed = target.trim();
  if (trimmed === "" || CONTROL_CHARACTER.test(trimmed)) return false;

  const scheme = SCHEME.exec(trimmed);
  // No scheme at all is a relative reference, which resolves on our own origin.
  if (scheme === null) return true;
  const name = scheme[1]?.toLowerCase();
  return name === "http" || name === "https" || name === "mailto";
}

/**
 * Whether the delimiter run at `index` may open or close emphasis.
 *
 * These are CommonMark's flanking rules, kept because a coding agent's
 * transcript is full of `snake_case_name` and `2 * 3 * 4`: without them `_`
 * inside an identifier and `*` inside arithmetic both become emphasis, which is
 * worse than not supporting emphasis at all.
 */
function opensEmphasis(text: string, index: number, marker: string): boolean {
  if (isSpace(text.charAt(index + marker.length))) return false;
  // `_` is the intra-word-sensitive one; `*` is not, by the spec.
  return marker.charAt(0) !== "_" || !WORD_CHARACTER.test(text.charAt(index - 1));
}

function closesEmphasis(text: string, index: number, marker: string): boolean {
  if (isSpace(text.charAt(index - 1))) return false;
  return marker.charAt(0) !== "_" || !WORD_CHARACTER.test(text.charAt(index + marker.length));
}

/** Parses the inline content of one line of prose. */
export function parseInline(text: string, depth = 0): readonly Span[] {
  const spans: Span[] = [];
  const canNest = depth < MAX_INLINE_DEPTH;
  let literal = "";
  let index = 0;

  const flush = (): void => {
    if (literal !== "") {
      spans.push({ kind: "text", text: literal });
      literal = "";
    }
  };

  while (index < text.length) {
    const char = text.charAt(index);

    if (char === "\\" && ESCAPABLE_CHARACTER.test(text.charAt(index + 1))) {
      literal += text.charAt(index + 1);
      index += 2;
      continue;
    }

    if (char === "`") {
      const close = text.indexOf("`", index + 1);
      if (close > index + 1) {
        flush();
        spans.push({ kind: "code", text: text.slice(index + 1, close) });
        index = close + 1;
        continue;
      }
    }

    if (char === "[" && canNest) {
      const link = parseLink(text, index, depth);
      if (link !== null) {
        flush();
        spans.push(link.span);
        index = link.next;
        continue;
      }
    }

    if (canNest) {
      const emphasis = parseEmphasis(text, index, depth);
      if (emphasis !== null) {
        flush();
        spans.push(emphasis.span);
        index = emphasis.next;
        continue;
      }
    }

    literal += char;
    index += 1;
  }

  flush();
  return spans;
}

interface Parsed {
  readonly span: Span;
  readonly next: number;
}

/** Reads `[label](target)` at `index`, or null when it is not a link we follow. */
function parseLink(text: string, index: number, depth: number): Parsed | null {
  const labelEnd = text.indexOf("]", index + 1);
  if (labelEnd <= index + 1 || text.charAt(labelEnd + 1) !== "(") return null;

  const targetEnd = text.indexOf(")", labelEnd + 2);
  if (targetEnd <= labelEnd + 2) return null;

  const target = text.slice(labelEnd + 2, targetEnd).trim();
  if (!isFollowableTarget(target)) return null;

  return {
    span: { kind: "link", target, spans: parseInline(text.slice(index + 1, labelEnd), depth + 1) },
    next: targetEnd + 1,
  };
}

/** Reads an emphasis run at `index`, or null when no delimiter closes it. */
function parseEmphasis(text: string, index: number, depth: number): Parsed | null {
  for (const { marker, style } of EMPHASIS_MARKERS) {
    if (!text.startsWith(marker, index)) continue;
    // A run of one character is one delimiter. Retrying `**` as `*` after it
    // fails to open is how `** not bold **` would end up emphasising its own
    // inner asterisks instead of staying literal.
    if (marker.length === 1 && text.charAt(index + 1) === marker) continue;
    if (!opensEmphasis(text, index, marker)) continue;

    const from = index + marker.length;
    for (let search = from; ; ) {
      const close = text.indexOf(marker, search);
      if (close < 0) break;
      // An empty span is not emphasis: `****` is a run of asterisks.
      if (close > from && closesEmphasis(text, close, marker)) {
        return {
          span: { kind: "emphasis", style, spans: parseInline(text.slice(from, close), depth + 1) },
          next: close + marker.length,
        };
      }
      search = close + 1;
    }
  }
  return null;
}

/* --------------------------------------------------------------- block text */

/** One table row split into trimmed cells. */
function splitRow(line: string): readonly string[] {
  let text = line.trim();
  if (text.startsWith("|")) text = text.slice(1);
  // `\|` is a literal pipe inside a cell, which is the only escape GFM defines
  // here and the one a table about shell pipelines always needs.
  if (text.endsWith("|") && !text.endsWith("\\|")) text = text.slice(0, -1);

  const cells: string[] = [];
  let cell = "";
  for (let index = 0; index < text.length; index += 1) {
    const char = text.charAt(index);
    if (char === "\\" && text.charAt(index + 1) === "|") {
      cell += "|";
      index += 1;
      continue;
    }
    if (char === "|") {
      cells.push(cell);
      cell = "";
      continue;
    }
    cell += char;
  }
  cells.push(cell);
  return cells.map((value) => value.trim());
}

/**
 * Reads a delimiter row (`|:--|--:|`) as per-column alignment.
 *
 * Null when the line is not one, which is what tells the caller that the line
 * above it was a paragraph that happened to contain a pipe.
 */
function parseDelimiterRow(line: string): readonly Alignment[] | null {
  if (!line.includes("-")) return null;
  const cells = splitRow(line);
  if (cells.length === 0) return null;

  const alignments: Alignment[] = [];
  for (const cell of cells) {
    if (!DELIMITER_CELL.test(cell)) return null;
    const left = cell.startsWith(":");
    const right = cell.endsWith(":");
    alignments.push(left && right ? "center" : right ? "right" : left ? "left" : null);
  }
  return alignments;
}

/** A list marker as read off one line, before its item has been collected. */
interface ListMarker {
  readonly indent: number;
  readonly ordered: boolean;
  readonly start: number;
  readonly text: string;
  /** Column the item's own content starts at, used to strip continuation lines. */
  readonly contentIndent: number;
}

function parseListMarker(line: string): ListMarker | null {
  const bullet = BULLET.exec(line);
  if (bullet !== null) {
    const indent = indentOf(bullet[1] ?? "");
    const marker = (bullet[2] ?? "-").length;
    return {
      indent,
      ordered: false,
      start: 1,
      text: bullet[4] ?? "",
      contentIndent: indent + marker + (bullet[3] ?? " ").length,
    };
  }
  const ordered = ORDERED.exec(line);
  if (ordered !== null) {
    const indent = indentOf(ordered[1] ?? "");
    const marker = (ordered[2] ?? "1").length + (ordered[3] ?? ".").length;
    return {
      indent,
      ordered: true,
      start: Number(ordered[2] ?? "1"),
      text: ordered[5] ?? "",
      contentIndent: indent + marker + (ordered[4] ?? " ").length,
    };
  }
  return null;
}

interface ParsedBlock {
  readonly block: Block;
  readonly next: number;
}

function parseFence(lines: readonly string[], start: number): ParsedBlock {
  const language = (FENCE.exec(lines[start] ?? "")?.[1] ?? "").trim();
  const body: string[] = [];
  let index = start + 1;
  while (index < lines.length && !CLOSING_FENCE.test(lines[index] ?? "")) {
    body.push(lines[index] ?? "");
    index += 1;
  }
  // An unterminated fence still renders as code: a streaming response routinely
  // ends mid-block, and swallowing the text would be worse. `index` is left on
  // the closing fence — or at the end of the input, where the `+ 1` below is
  // simply past it.
  return { block: { kind: "code", language, code: body.join("\n") }, next: index + 1 };
}

function parseTable(lines: readonly string[], start: number, alignments: readonly Alignment[]): ParsedBlock {
  const rows: Row[] = [];
  let index = start + 2;

  while (index < lines.length) {
    const line = lines[index] ?? "";
    // A row has to contain a pipe. Without that, the paragraph after a table
    // with no blank line between them would be swallowed as a one-cell row.
    if (line.trim() === "" || !line.includes("|")) break;
    rows.push(splitRow(line).map((cell) => parseInline(cell)));
    index += 1;
  }

  const width = alignments.length;
  const fit = (cells: readonly (readonly Span[])[]): Row =>
    Array.from({ length: width }, (_, column) => cells[column] ?? []);

  return {
    block: {
      kind: "table",
      alignments,
      header: fit(splitRow(lines[start] ?? "").map((cell) => parseInline(cell))),
      rows: rows.map(fit),
    },
    next: index,
  };
}

function parseList(lines: readonly string[], start: number, first: ListMarker, depth: number): ParsedBlock {
  const items: ListItem[] = [];
  const { indent, ordered } = first;
  let index = start;

  while (index < lines.length) {
    const marker = parseListMarker(lines[index] ?? "");
    if (marker === null || marker.indent !== indent || marker.ordered !== ordered) break;

    const body: string[] = [marker.text];
    index += 1;

    while (index < lines.length) {
      const line = lines[index] ?? "";
      const sibling = parseListMarker(line);

      // A sibling marker ends the item; anything less indented than the content
      // ends the list, so a following paragraph stays a paragraph.
      if (sibling !== null && sibling.indent === indent && sibling.ordered === ordered) break;
      if (line.trim() === "") {
        const after = lines[index + 1] ?? "";
        // A blank line only continues the item when what follows is indented
        // into it; otherwise it separates the list from the next block.
        if (after.trim() !== "" && indentOf(after) > indent) {
          body.push("");
          index += 1;
          continue;
        }
        break;
      }
      if (indentOf(line) > indent) {
        // The item's own indentation is stripped so that a nested list arrives
        // at `parseBlocks` as a list rather than as indented text. `slice`
        // counts characters where `indentOf` counts columns, so a tab-indented
        // continuation keeps part of its leading whitespace — the harmless
        // direction to be wrong in, since the text is drawn as written.
        body.push(line.slice(Math.min(marker.contentIndent, indentOf(line))));
        index += 1;
        continue;
      }
      break;
    }

    const task = TASK.exec(body[0] ?? "");
    items.push({
      task: task !== null,
      checked: (task?.[1] ?? " ").toLowerCase() === "x",
      blocks: parseBlocks(task === null ? body : [(task[2] ?? ""), ...body.slice(1)], depth + 1),
    });
  }

  return { block: { kind: "list", ordered, start: first.start, items }, next: index };
}

/**
 * Splits `lines` into blocks.
 *
 * Recursive: a blockquote and a list item both hand their own contents back to
 * this function, which is what makes `> - a` and a nested list work without a
 * second parser. `depth` is the guard against a transcript that nests forever.
 */
function parseBlocks(lines: readonly string[], depth = 0): readonly Block[] {
  const blocks: Block[] = [];
  let paragraph: string[] = [];
  let index = 0;

  const at = (position: number): string => lines[position] ?? "";
  const flush = (): void => {
    if (paragraph.length > 0) {
      blocks.push({ kind: "paragraph", spans: joinLines(paragraph) });
      paragraph = [];
    }
  };
  const nested = depth >= MAX_BLOCK_DEPTH;

  while (index < lines.length) {
    const line = at(index);

    if (FENCE.test(line)) {
      flush();
      const { block, next } = parseFence(lines, index);
      blocks.push(block);
      index = next;
      continue;
    }

    if (line.trim() === "") {
      flush();
      index += 1;
      continue;
    }

    // Past the nesting cap nothing new opens, but a paragraph still accumulates
    // and a fenced block still renders: the input is taken as written rather
    // than refused.
    if (nested) {
      paragraph.push(line);
      index += 1;
      continue;
    }

    if (RULE.test(line)) {
      flush();
      blocks.push({ kind: "rule" });
      index += 1;
      continue;
    }

    const heading = HEADING.exec(line);
    if (heading !== null) {
      flush();
      blocks.push({
        kind: "heading",
        depth: (heading[1] ?? "#").length,
        spans: parseInline(headingText((heading[2] ?? "").replace(/^[ \t]+/, ""))),
      });
      index += 1;
      continue;
    }

    const quote = QUOTE.exec(line);
    if (quote !== null) {
      flush();
      const inner: string[] = [];
      while (index < lines.length) {
        const quoted = QUOTE.exec(at(index));
        if (quoted === null) break;
        inner.push(quoted[1] ?? "");
        index += 1;
      }
      blocks.push({ kind: "quote", blocks: parseBlocks(inner, depth + 1) });
      continue;
    }

    // A table needs the line below to be a delimiter row, which is also what
    // keeps a paragraph that merely contains a pipe out of this branch.
    if (line.includes("|")) {
      const alignments = parseDelimiterRow(at(index + 1));
      if (alignments !== null) {
        flush();
        const { block, next } = parseTable(lines, index, alignments);
        blocks.push(block);
        index = next;
        continue;
      }
    }

    const marker = parseListMarker(line);
    if (marker !== null) {
      flush();
      const { block, next } = parseList(lines, index, marker, depth);
      blocks.push(block);
      index = next;
      continue;
    }

    paragraph.push(line);
    index += 1;
  }

  flush();
  return blocks;
}

/** A run of paragraph lines, with each source line break kept as a span. */
function joinLines(lines: readonly string[]): readonly Span[] {
  const spans: Span[] = [];
  lines.forEach((line, position) => {
    if (position > 0) spans.push({ kind: "break" });
    spans.push(...parseInline(line));
  });
  return spans;
}

/**
 * A run of trailing `#` only closes a heading when whitespace precedes it, so
 * `## C#` keeps its hash and `## 标题 ##` does not.
 */
function headingText(rest: string): string {
  return rest.replace(/[ \t]+#+$/, "").replace(/[ \t]+$/, "");
}

/* ------------------------------------------------------------------ entries */

/** Parses a whole message into blocks. */
export function parseMarkdown(text: string): readonly Block[] {
  // Every line ending, so a message that arrived with CRLF does not carry a
  // stray carriage return into a text node — which `white-space: pre-wrap`
  // would then draw as a blank line.
  return parseBlocks(text.split(/\r\n|\r|\n/));
}
