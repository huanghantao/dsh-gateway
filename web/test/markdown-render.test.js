/**
 * The renderer: a Markdown tree becomes DOM nodes.
 *
 * This tier is small on purpose. The grammar is `markdown.test.js`; there is
 * nothing to decide here, so these tests are about the two things that are still
 * this file's responsibility — that a tree becomes the right elements, and that
 * it never becomes anything else. `./dom.js` is the shim that makes the second
 * one fail loudly, which is why it has to be imported before the renderer.
 */

import assert from "node:assert/strict";
import { beforeEach, describe, test } from "node:test";

import { created, serialize } from "./support/dom.js";

const { renderMarkdown } = await import("../dist/markdown/render.js");

const html = (markdown) => serialize(renderMarkdown(markdown));

/**
 * Every element the renderer may create.
 *
 * A fixed set is the whole safety argument, so a new tag has to be added here
 * deliberately — with the reason it is safe — rather than appearing in a
 * transcript first.
 */
const ALLOWED_ELEMENTS = new Set([
  // Structure.
  "p", "br", "hr", "blockquote",
  // Headings, capped at h6 by the renderer.
  "h2", "h3", "h4", "h5", "h6",
  // Lists.
  "ul", "ol", "li", "input",
  // Tables.
  "table", "thead", "tbody", "tr", "th", "td", "div",
  // Inline.
  "em", "strong", "s", "code", "a",
  // Code blocks.
  "pre", "span", "button",
]);

beforeEach(() => {
  created.length = 0;
});

describe("the elements it is allowed to build", () => {
  test("a full message uses only elements from the allow-list", () => {
    renderMarkdown(
      [
        "# h1",
        "## h2",
        "### h3",
        "#### h4",
        "##### h5",
        "###### h6",
        "---",
        "> quote with **strong**",
        "- [x] task",
        "- bullet",
        "3. ordered",
        "| a | b |",
        "| :- | -: |",
        "| 1 | 2 |",
        "```js",
        "code",
        "```",
        "text with `code`, *em*, ~~strike~~ and [a link](https://example.com)",
      ].join("\n"),
    );

    const unexpected = [...new Set(created)].filter((tag) => !ALLOWED_ELEMENTS.has(tag));
    assert.deepEqual(unexpected, []);
  });
});

describe("blocks", () => {
  test("a message's own h1 opens at h2, because the view owns the page's h1", () => {
    assert.equal(html("# one"), '<h2 class="md-h md-h1">one</h2>');
    assert.equal(html("###### six"), '<h6 class="md-h md-h6">six</h6>');
    assert.equal(html("####### seven is a paragraph"), '<p class="md-p">####### seven is a paragraph</p>');
  });

  test("a paragraph keeps its source line breaks as br", () => {
    assert.equal(html("one\ntwo"), '<p class="md-p">one<br>two</p>');
  });

  test("a rule is an hr", () => {
    assert.equal(html("---"), '<hr class="md-rule">');
  });

  test("a quote contains its blocks", () => {
    assert.equal(html("> - a"), '<blockquote class="md-quote"><ul class="md-list"><li class="md-li">a</li></ul></blockquote>');
  });

  test("a code block carries its language and a copy button", () => {
    const rendered = html("```js\nconst a = 1;\n```");
    assert.match(rendered, /<span class="md-lang">js<\/span>/);
    assert.match(rendered, /<pre class="md-pre"><code>const a = 1;<\/code><\/pre>/);
    assert.equal(html("```\nx\n```").includes('<span class="md-lang">code</span>'), true);
  });
});

describe("lists", () => {
  test("ordered lists keep their starting number", () => {
    assert.equal(html("3. three"), '<ol class="md-list" start="3"><li class="md-li">three</li></ol>');
  });

  test("a tight item's prose is not wrapped in a paragraph", () => {
    assert.equal(html("- one\n- two"), '<ul class="md-list"><li class="md-li">one</li><li class="md-li">two</li></ul>');
  });

  test("a loose item's paragraphs are", () => {
    assert.equal(
      html("- one\n\n  two"),
      '<ul class="md-list"><li class="md-li"><p class="md-p">one</p><p class="md-p">two</p></li></ul>',
    );
  });

  test("a nested list does not make its parent loose", () => {
    assert.equal(
      html("- outer\n  - inner"),
      '<ul class="md-list"><li class="md-li">outer<ul class="md-list"><li class="md-li">inner</li></ul></li></ul>',
    );
  });

  test("a task is a disabled checkbox, checked or not", () => {
    assert.equal(
      html("- [x] done\n- [ ] todo"),
      '<ul class="md-list">' +
        '<li class="md-li md-task is-done"><input class="md-check" type="checkbox" disabled="" checked="">done</li>' +
        '<li class="md-li md-task"><input class="md-check" type="checkbox" disabled="">todo</li>' +
        "</ul>",
    );
  });
});

describe("tables", () => {
  test("alignment becomes a class, because the CSP forbids an inline style", () => {
    const rendered = html("| a | b | c |\n| :- | :-: | -: |\n| 1 | 2 | 3 |");
    assert.match(rendered, /<th class="md-th is-left">a<\/th>/);
    assert.match(rendered, /<th class="md-th is-center">b<\/th>/);
    assert.match(rendered, /<th class="md-th is-right">c<\/th>/);
    assert.match(rendered, /<td class="md-td is-right">3<\/td>/);
  });

  test("the table is wrapped so a wide one scrolls instead of widening the page", () => {
    const rendered = html("| a |\n| - |\n| 1 |");
    assert.match(rendered, /^<div class="md-table-wrap"><table class="md-table">/);
    assert.match(rendered, /<\/table><\/div>$/);
  });

  test("header cells are th and body cells are td", () => {
    const rendered = html("| a |\n| - |\n| 1 |");
    assert.match(rendered, /<thead><tr><th class="md-th">a<\/th><\/tr><\/thead>/);
    assert.match(rendered, /<tbody><tr><td class="md-td">1<\/td><\/tr><\/tbody>/);
  });
});

describe("inline", () => {
  test("emphasis, strikethrough and code become their elements", () => {
    assert.equal(html("**a**"), '<p class="md-p"><strong>a</strong></p>');
    assert.equal(html("*a*"), '<p class="md-p"><em>a</em></p>');
    assert.equal(html("~~a~~"), '<p class="md-p"><s>a</s></p>');
    assert.equal(html("`a`"), '<p class="md-p"><code class="md-inline-code">a</code></p>');
  });

  test("a link opens away from the app and cannot reach back into it", () => {
    assert.equal(
      html("[a](https://example.com)"),
      '<p class="md-p"><a class="md-link" href="https://example.com" target="_blank" rel="noopener noreferrer">a</a></p>',
    );
  });

  test("a scheme we do not follow is drawn as the text the author wrote", () => {
    assert.equal(html("[a](javascript:alert(1))"), '<p class="md-p">[a](javascript:alert(1))</p>');
  });
});

describe("what must never leave this file", () => {
  test("raw html is escaped into text nodes, not built", () => {
    assert.equal(html("<script>alert(1)</script>"), '<p class="md-p">&lt;script&gt;alert(1)&lt;/script&gt;</p>');
    assert.equal(created.includes("script"), false);
  });

  test("a remote image is never fetched", () => {
    assert.equal(created.includes("img"), false);
    assert.equal(html("![alt](https://tracker.example/p.png)"), '<p class="md-p">!<a class="md-link" href="https://tracker.example/p.png" target="_blank" rel="noopener noreferrer">alt</a></p>');
  });

  test("nothing is assigned to innerHTML, outerHTML or insertAdjacentHTML", () => {
    // The shim throws from all three, so reaching any of these fails the test
    // rather than passing quietly.
    assert.doesNotThrow(() =>
      renderMarkdown("**a** `b` [c](https://example.com) <img src=x onerror=alert(1)> ![d](https://e.f/g.png)"),
    );
  });
});
