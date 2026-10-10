/**
 * The question sheet's two wire-critical rules, asserted without a DOM.
 *
 * `views/question.ts` builds the card, but the two things that can be *wrong*
 * about an answer are pure functions, so they are exported and held still here:
 * whether the label sent back is the model's own string, and whether a question
 * the reader passed over is sent as a skip rather than dropped.
 *
 * Both failures are silent from the phone's side and expensive on the other:
 * a "tidied" label comes back as `400 unknown_option` and loses the *whole*
 * answer set, and a dropped question is one the model never learns was skipped.
 * The importer pulls in `dom.js` and `format.js`, which touch nothing at module
 * scope — that is why these run in plain node.
 */

import assert from "node:assert/strict";
import { describe, test } from "node:test";

import { decodeQuestion, decodeServerEvent } from "../dist/decode.js";
import { answerFor, answersFor, displayLabel } from "../dist/views/question.js";

/** One wire question, as `GET /questions` returns it. */
const wireQuestion = {
  id: "3f2a",
  sessionId: "50c7",
  items: [
    {
      id: "style",
      header: "Answer style",
      question: "How should I answer from now on?",
      detail: "This is a preference, not a rule.",
      options: [
        { label: "Be concise (Recommended)", description: "Lead with the answer.", recommended: true },
        { label: "Explain the reasoning", description: "Show the working." },
      ],
      multiSelect: false,
    },
  ],
  requestedAt: "2026-10-10T15:04:05Z",
  expiresAt: "2026-10-10T15:14:05Z",
};

/** A question item with the fields these tests do not care about filled in. */
const item = (over = {}) => ({
  id: "style",
  header: "",
  question: "Which style?",
  detail: "",
  options: [],
  multiSelect: false,
  ...over,
});

describe("displayLabel", () => {
  test("strips the harness's recommendation suffix", () => {
    assert.equal(displayLabel("Be concise, lead with the answer (Recommended)"), "Be concise, lead with the answer");
  });

  test("matches the suffix case-insensitively", () => {
    assert.equal(displayLabel("Explain the reasoning (recommended)"), "Explain the reasoning");
    assert.equal(displayLabel("Explain the reasoning (RECOMMENDED)"), "Explain the reasoning");
  });

  test("keeps a label whose whole text is the suffix", () => {
    // Odd, but possible: stripping it would leave a choice with no name.
    assert.equal(displayLabel("(Recommended)"), "(Recommended)");
  });

  test("leaves the words anywhere else alone", () => {
    assert.equal(displayLabel("Recommended reading"), "Recommended reading");
    assert.equal(displayLabel("Mention (Recommended) only sometimes"), "Mention (Recommended) only sometimes");
  });
});

describe("answerFor", () => {
  test("quotes the original label, suffix and all", () => {
    const label = "Be concise (Recommended)";
    const answer = answerFor(item(), { selected: [label], custom: "" });
    // The model matches its own strings; the badge is drawn from `recommended`,
    // not by rewriting the label on the way back.
    assert.deepEqual(answer, { id: "style", selected: [label], custom: "" });
  });

  test("sends a skipped question as an empty answer rather than omitting it", () => {
    assert.deepEqual(answerFor(item(), undefined), { id: "style", selected: [], custom: "" });
    assert.deepEqual(answerFor(item(), { selected: [], custom: "   " }), { id: "style", selected: [], custom: "" });
  });

  test("lets free text override a single-select choice", () => {
    const answer = answerFor(item(), { selected: ["Be concise"], custom: "  Something else  " });
    assert.equal(answer.custom, "Something else");
    assert.deepEqual(answer.selected, []);
  });

  test("lets free text supplement a multi-select choice", () => {
    const answer = answerFor(item({ multiSelect: true }), { selected: ["A", "B"], custom: "And C" });
    assert.deepEqual(answer.selected, ["A", "B"]);
    assert.equal(answer.custom, "And C");
  });
});

describe("answersFor", () => {
  test("answers every item, in the model's order", () => {
    const items = [item({ id: "a" }), item({ id: "b", multiSelect: true }), item({ id: "c" })];
    const drafts = new Map([
      ["a", { selected: ["One"], custom: "" }],
      ["b", { selected: [], custom: "typed" }],
    ]);
    assert.deepEqual(answersFor(items, drafts), [
      { id: "a", selected: ["One"], custom: "" },
      { id: "b", selected: [], custom: "typed" },
      // Paged past without an answer: still an entry, and still a skip.
      { id: "c", selected: [], custom: "" },
    ]);
  });
});

describe("decodeQuestion", () => {
  test("fills in every field the model may omit", () => {
    const question = decodeQuestion({
      id: "3f2a",
      sessionId: "50c7",
      items: [{ id: "style", question: "Which style?" }],
      requestedAt: "2026-10-10T15:04:05Z",
      expiresAt: "2026-10-10T15:14:05Z",
    });
    assert.deepEqual(question.items, [
      { id: "style", header: "", question: "Which style?", detail: "", options: [], multiSelect: false },
    ]);
    assert.equal(question.id, "3f2a");
  });

  test("keeps the recommended flag beside the untouched label", () => {
    const option = decodeQuestion(wireQuestion).items[0].options[0];
    assert.deepEqual(option, {
      label: "Be concise (Recommended)",
      description: "Lead with the answer.",
      recommended: true,
    });
    // The default for an option the model wrote without one.
    assert.equal(decodeQuestion(wireQuestion).items[0].options[1].recommended, false);
  });

  test("drops an option nothing could be answered with", () => {
    const question = decodeQuestion({
      id: "q",
      items: [{ id: "i", question: "?", options: [{ description: "no label" }, { label: "fine" }] }],
    });
    assert.deepEqual(question.items[0].options, [
      { label: "fine", description: "", recommended: false },
    ]);
  });

  test("refuses a question with nothing answerable in it", () => {
    // An empty card would hold the screen for a question that cannot be
    // answered; the broker's timeout still resolves it on the server.
    assert.equal(decodeQuestion({ id: "q", items: [] }), null);
    assert.equal(decodeQuestion({ id: "q", items: [{ question: "no id" }] }), null);
    assert.equal(decodeQuestion({ id: "q", items: [{ id: "i" }] }), null);
    assert.equal(decodeQuestion({ items: [{ id: "i", question: "?" }] }), null);
    assert.equal(decodeQuestion("nonsense"), null);
  });
});

describe("question frames", () => {
  test("a requested frame decodes into the same shape the list returns", () => {
    const event = decodeServerEvent({
      type: "question.requested",
      seq: 12,
      time: "2026-10-10T15:04:05Z",
      sessionId: "50c7",
      data: wireQuestion,
    });
    assert.equal(event.type, "question.requested");
    assert.equal(event.data.id, "3f2a");
    assert.equal(event.data.items.length, 1);
  });

  test("a frame without a sequence number is not trusted", () => {
    // `seq` drives replay, so a frame without one is not authoritative.
    assert.equal(decodeServerEvent({ type: "question.requested", data: wireQuestion }), null);
  });

  test("a resolved frame keeps the reason nobody answered", () => {
    const event = decodeServerEvent({
      type: "question.resolved",
      seq: 13,
      time: "2026-10-10T15:14:05Z",
      sessionId: "50c7",
      data: { id: "3f2a", sessionId: "50c7", answeredBy: "timeout" },
    });
    assert.deepEqual(event.data, { id: "3f2a", sessionId: "50c7", answeredBy: "timeout", answers: [] });
  });

  test("a resolved frame carries the answers a person gave", () => {
    const event = decodeServerEvent({
      type: "question.resolved",
      seq: 13,
      time: "2026-10-10T15:06:00Z",
      sessionId: "50c7",
      data: {
        id: "3f2a",
        sessionId: "50c7",
        answeredBy: "device-1",
        answers: [{ id: "style", selected: ["Be concise (Recommended)"], custom: "Mostly" }],
      },
    });
    assert.deepEqual(event.data.answers, [
      { id: "style", selected: ["Be concise (Recommended)"], custom: "Mostly" },
    ]);
  });

  test("a snapshot carries the pending questions beside the approvals", () => {
    const event = decodeServerEvent({
      type: "snapshot",
      seq: 20,
      time: "2026-10-10T15:04:06Z",
      data: {
        generation: "g",
        seq: 20,
        time: "2026-10-10T15:04:06Z",
        harness: { state: "ready" },
        turns: [],
        approvals: [],
        questions: [wireQuestion],
      },
    });
    assert.equal(event.data.approvals.length, 0);
    assert.equal(event.data.questions.length, 1);
    assert.equal(event.data.questions[0].id, "3f2a");
  });

  test("a snapshot from a server that predates questions still decodes", () => {
    const event = decodeServerEvent({
      type: "snapshot",
      seq: 20,
      time: "2026-10-10T15:04:06Z",
      data: { generation: "g", seq: 20, time: "", harness: { state: "ready" }, turns: [], approvals: [] },
    });
    assert.deepEqual(event.data.questions, []);
  });
});
