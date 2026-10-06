/**
 * The Markdown grammar, asserted against the tree rather than the DOM.
 *
 * These run against `dist/`, which is what the browser is actually served, and
 * they need no DOM at all: `markdown/parse.ts` has no imports. That is the point
 * of the split — the interesting half of the renderer is reachable from a plain
 * `node --test`, so a change to the grammar fails here rather than silently in
 * whoever's transcript next contains a table.
 *
 * `markdown-render.test.js` covers the other half, including the one property
 * that must never regress: that nothing is built from a string.
 */

import assert from "node:assert/strict";
import { describe, test } from "node:test";

import { parseInline, parseMarkdown } from "../dist/markdown/parse.js";

/* ------------------------------------------------------------------ helpers */

/** The block kinds of a message, which is what most of these assert on. */
const kinds = (markdown) => parseMarkdown(markdown).map((block) => block.kind);

/** The single top-level block of a message, for tests that parse one thing. */
const only = (markdown) => {
  const blocks = parseMarkdown(markdown);
  assert.equal(blocks.length, 1, `expected one block, got ${JSON.stringify(kinds(markdown))}`);
  return blocks[0];
};

/** A span list as text, with the markup visible so it can be asserted on. */
function outline(spans) {
  return spans
    .map((span) => {
      switch (span.kind) {
        case "text":
          return span.text;
        case "break":
          return "<br>";
        case "code":
          return `\`${span.text}\``;
        case "emphasis":
          return `<${span.style}>${outline(span.spans)}</${span.style}>`;
        case "link":
          return `<a ${span.target}>${outline(span.spans)}</a>`;
      }
    })
    .join("");
}

/** The same, for a whole message. */
const text = (markdown) => parseMarkdown(markdown).map((block) => outline(block.spans ?? [])).join("\n");

/* ------------------------------------------------------------------ headings */

describe("headings", () => {
  test("one through six become a heading at that depth", () => {
    for (const depth of [1, 2, 3, 4, 5, 6]) {
      const block = only(`${"#".repeat(depth)} title`);
      assert.equal(block.kind, "heading");
      assert.equal(block.depth, depth);
      assert.equal(outline(block.spans), "title");
    }
  });

  test("seven hashes is not a heading, because CommonMark caps at six", () => {
    assert.deepEqual(kinds("####### title"), ["paragraph"]);
  });

  test("a hash with no space after it is not a heading", () => {
    assert.deepEqual(kinds("##title"), ["paragraph"]);
    assert.deepEqual(kinds("#nospace"), ["paragraph"]);
  });

  test("a trailing run of hashes closes the heading only after a space", () => {
    assert.equal(outline(only("## 标题 ##").spans), "标题");
    assert.equal(outline(only("## C#").spans), "C#");
  });

  test("a lone hash is a heading with nothing in it, as CommonMark has it", () => {
    const block = only("#");
    assert.equal(block.kind, "heading");
    assert.equal(block.depth, 1);
    assert.equal(outline(block.spans), "");
  });

  test("an empty heading is still a heading", () => {
    const block = only("##");
    assert.equal(block.kind, "heading");
    assert.equal(outline(block.spans), "");
  });

  test("headings carry inline markup", () => {
    assert.equal(outline(only("## a **b** c").spans), "a <strong>b</strong> c");
  });
});

/* ------------------------------------------------------------ rules and code */

describe("thematic breaks", () => {
  test("every marker run is a rule", () => {
    for (const line of ["---", "***", "___", "- - -", "* * *", "-----"]) {
      assert.deepEqual(kinds(line), ["rule"], line);
    }
  });

  test("fewer than three markers is not a rule", () => {
    assert.deepEqual(kinds("--"), ["paragraph"]);
    assert.deepEqual(kinds("**"), ["paragraph"]);
  });

  test("a rule inside a fence stays code", () => {
    assert.deepEqual(kinds("```\n---\n```"), ["code"]);
  });
});

describe("fenced code", () => {
  test("the info string becomes the language and the body is verbatim", () => {
    const block = only("```js\nconst a = 1;\n```");
    assert.equal(block.kind, "code");
    assert.equal(block.language, "js");
    assert.equal(block.code, "const a = 1;");
  });

  test("a fence with no info string has an empty language", () => {
    assert.equal(only("```\nx\n```").language, "");
  });

  test("an unterminated fence still renders, because a stream ends mid-block", () => {
    const block = only("```py\none\ntwo");
    assert.equal(block.code, "one\ntwo");
  });

  test("markup inside a fence is not parsed", () => {
    assert.equal(only("```\n**not bold**\n```").code, "**not bold**");
  });
});

/* -------------------------------------------------------------- the literals */

describe("what stays literal", () => {
  test("raw HTML is text, not markup", () => {
    assert.deepEqual(kinds("<script>alert(1)</script>"), ["paragraph"]);
    assert.equal(text("<script>alert(1)</script>"), "<script>alert(1)</script>");
    assert.equal(text("<img src=x onerror=alert(1)>"), "<img src=x onerror=alert(1)>");
  });

  test("an image degrades to a literal bang and a link", () => {
    assert.equal(text("![alt](https://example.com/a.png)"), "!<a https://example.com/a.png>alt</a>");
  });

  test("identifiers and arithmetic are not emphasis", () => {
    assert.equal(text("snake_case_name"), "snake_case_name");
    assert.equal(text("SCREAMING_SNAKE"), "SCREAMING_SNAKE");
    assert.equal(text("2 * 3 * 4"), "2 * 3 * 4");
    assert.equal(text("** not bold **"), "** not bold **");
  });
});

/* -------------------------------------------------------------- block nesting */

describe("blockquotes", () => {
  test("consecutive quoted lines are one quote", () => {
    const block = only("> one\n> two");
    assert.equal(block.kind, "quote");
    assert.deepEqual(block.blocks.map((child) => child.kind), ["paragraph"]);
    assert.equal(outline(block.blocks[0].spans), "one<br>two");
  });

  test("a quote contains blocks, not lines", () => {
    const block = only("> - a\n> - b");
    assert.deepEqual(block.blocks.map((child) => child.kind), ["list"]);
  });

  test("quotes nest", () => {
    const block = only("> > deep");
    assert.deepEqual(block.blocks.map((child) => child.kind), ["quote"]);
  });
});

describe("lists", () => {
  test("a bullet run is one list", () => {
    const block = only("- one\n- two");
    assert.equal(block.kind, "list");
    assert.equal(block.ordered, false);
    assert.equal(block.items.length, 2);
    assert.equal(outline(block.items[0].blocks[0].spans), "one");
  });

  test("an ordered list keeps the number it starts at", () => {
    const block = only("3. three\n4. four");
    assert.equal(block.ordered, true);
    assert.equal(block.start, 3);
  });

  test("an indented marker nests", () => {
    const block = only("- outer\n  - inner");
    assert.deepEqual(block.items[0].blocks.map((child) => child.kind), ["paragraph", "list"]);
  });

  test("a task item is a checkbox, checked or not", () => {
    const block = only("- [x] done\n- [ ] todo\n- plain");
    assert.deepEqual(
      block.items.map((item) => [item.task, item.checked]),
      [
        [true, true],
        [true, false],
        [false, false],
      ],
    );
  });

  test("a blank line inside an item makes the list loose", () => {
    const block = only("- one\n\n  two");
    assert.equal(block.kind, "list");
    assert.deepEqual(block.items[0].blocks.map((child) => child.kind), ["paragraph", "paragraph"]);
  });

  test("a sibling marker starts a new item rather than a nested list", () => {
    const block = only("- one\n- two\n- three");
    assert.equal(block.items.length, 3);
  });

  test("an unindented line after a list is a paragraph, not a lazy item", () => {
    assert.deepEqual(kinds("- one\nafter"), ["list", "paragraph"]);
  });

  test("a bullet and an ordered marker do not merge into one list", () => {
    assert.deepEqual(kinds("- one\n1. two"), ["list", "list"]);
  });
});

describe("tables", () => {
  test("alignment comes from the delimiter row", () => {
    const block = only("| a | b | c | d |\n|:--|:-:|--:|---|\n| 1 | 2 | 3 | 4 |");
    assert.equal(block.kind, "table");
    assert.deepEqual(block.alignments, ["left", "center", "right", null]);
  });

  test("cells carry inline markup", () => {
    const block = only("| a |\n| --- |\n| **b** |");
    assert.equal(outline(block.header[0]), "a");
    assert.equal(outline(block.rows[0][0]), "<strong>b</strong>");
  });

  test("an escaped pipe is a cell's content, not a delimiter", () => {
    const block = only("| a | b |\n| --- | --- |\n| `x \\| y` | z |");
    assert.equal(block.rows[0].length, 2);
    assert.equal(outline(block.rows[0][0]), "`x | y`");
  });

  test("short rows are padded and long ones truncated to the delimiter width", () => {
    const block = only("| a | b |\n| --- | --- |\n| 1 |\n| 1 | 2 | 3 |");
    assert.deepEqual(block.rows.map((row) => row.length), [2, 2]);
    assert.equal(outline(block.rows[0][1]), "");
  });

  test("a paragraph containing a pipe is not a table", () => {
    assert.deepEqual(kinds("a | b"), ["paragraph"]);
    assert.deepEqual(kinds("a | b\nnot a delimiter"), ["paragraph"]);
  });

  test("the table ends at a line without a pipe", () => {
    assert.deepEqual(kinds("| a |\n| --- |\n| 1 |\nafter"), ["table", "paragraph"]);
  });
});

/* --------------------------------------------------------------- inline text */

describe("inline spans", () => {
  test("the four emphasis forms, and their nesting", () => {
    assert.equal(outline(parseInline("*a*")), "<em>a</em>");
    assert.equal(outline(parseInline("_a_")), "<em>a</em>");
    assert.equal(outline(parseInline("**a**")), "<strong>a</strong>");
    assert.equal(outline(parseInline("__a__")), "<strong>a</strong>");
    assert.equal(outline(parseInline("~~a~~")), "<strike>a</strike>");
    assert.equal(outline(parseInline("**a *b* c**")), "<strong>a <em>b</em> c</strong>");
  });

  test("a backslash escapes the character after it", () => {
    assert.equal(outline(parseInline("\\*not emphasis\\*")), "*not emphasis*");
    assert.equal(outline(parseInline("\\`not code\\`")), "`not code`");
    assert.equal(outline(parseInline("a \\\\ b")), "a \\ b");
  });

  test("inline code is verbatim", () => {
    assert.equal(outline(parseInline("`**a**`")), "`**a**`");
    assert.equal(outline(parseInline("``")), "``");
  });

  test("links keep the target the author wrote", () => {
    assert.equal(outline(parseInline("[a](https://example.com/x?y=1)")), "<a https://example.com/x?y=1>a</a>");
    assert.equal(outline(parseInline("[a](./relative)")), "<a ./relative>a</a>");
    assert.equal(outline(parseInline("[**a**](mailto:x@y.z)")), "<a mailto:x@y.z><strong>a</strong></a>");
  });

  test("a scheme we do not follow leaves the whole thing literal", () => {
    for (const target of [
      "javascript:alert(1)",
      "JavaScript:alert(1)",
      "data:text/html,<script>alert(1)</script>",
      "vbscript:msgbox",
      "file:///etc/passwd",
      // The URL parser strips tabs and newlines before it looks for the colon,
      // so these would otherwise reach `javascript:` in a browser.
      "java\tscript:alert(1)",
      "java\nscript:alert(1)",
    ]) {
      assert.equal(parseInline(`[a](${target})`).length, 1, target);
      assert.equal(parseInline(`[a](${target})`)[0].kind, "text", target);
    }
  });
});

/* ----------------------------------------------------------------- the limits */

describe("the nesting caps", () => {
  test("a deeply nested quote stops nesting instead of overflowing the stack", () => {
    const block = only(`${">".repeat(200)} deep`);
    assert.equal(block.kind, "quote");

    let depth = 0;
    for (let node = block; node.kind === "quote"; depth += 1) node = node.blocks[0];
    assert.ok(depth <= 6, `nested ${depth} deep, past the cap`);
  });

  test("a deeply repeated marker stops nesting too", () => {
    assert.doesNotThrow(() => parseMarkdown("- ".repeat(300)));
  });

  test("a long run of emphasis delimiters is parsed, not recursed forever", () => {
    assert.doesNotThrow(() => parseMarkdown("*".repeat(2000)));
    assert.doesNotThrow(() => parseMarkdown(`${"*".repeat(200)}deep`));
  });
});

/* --------------------------------------------------------------- the whole thing */

describe("a message", () => {
  test("the answer from the screenshot parses to the shapes it was written as", () => {
    const markdown = [
      "12 周课程已全部建成并通过验收。",
      "",
      "## 📖 交付物",
      "",
      "**项目位置：** `/tmp/study`",
      "",
      "| | |",
      "|---|---|",
      "| 正文 | 86 个章节文件、**23,827 行** |",
      "",
      "---",
      "",
      "1. 第一步",
      "2. 第二步",
    ].join("\n");

    assert.deepEqual(kinds(markdown), ["paragraph", "heading", "paragraph", "table", "rule", "list"]);
  });

  test("line breaks inside a paragraph survive as break spans", () => {
    assert.equal(text("one\ntwo"), "one<br>two");
  });

  test("a blank line separates paragraphs", () => {
    assert.deepEqual(kinds("one\n\ntwo"), ["paragraph", "paragraph"]);
  });

  test("windows line endings do not leave a carriage return in the text", () => {
    assert.equal(text("one\r\ntwo"), "one<br>two");
    assert.equal(text("one\rtwo"), "one<br>two");
  });
});
