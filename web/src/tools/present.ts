/**
 * What a tool call means, and how it should read.
 *
 * The card in a transcript, the sheet that asks whether to allow a call, and the
 * change screen that collects a session's edits all describe the same event. What
 * they need from it is not a string: it is a *shape* — a headline for the row, the
 * content blocks worth showing, a couple of facts, and the sentences that keep
 * the card honest about what it is not showing.
 *
 * That shape is built here, once, by a registry. Adding a tool means adding one
 * entry; a tool nobody has taught this module about still renders, because the
 * fallback shows its arguments and its output rather than nothing at all.
 *
 * Two rules hold throughout, carried over from the module this one replaces:
 *
 * 1. **Total functions.** An unknown tool, a missing field, malformed JSON or a
 *    future argument shape produce a smaller view rather than an exception. This
 *    runs while someone scrolls a phone; a crash here is a blank screen.
 * 2. **No claims the data does not support.** A whole-file write reports its
 *    additions and no deletions, because the arguments do not carry the previous
 *    contents. A result that says nothing about how it ended yields no facts.
 */

import type { FeedItem } from "../types.js";
import { firstLine, firstString, join, lineCount, parseArgs, shortPath, str, strings, type ToolArgs } from "./args.js";
import { changeOf, diffStat, type ToolChange } from "./diff.js";

/** The tool row of the feed, which is all this module reads. */
export type ToolFeedItem = Extract<FeedItem, { kind: "tool" }>;

/**
 * One tool call, flattened into the facts a view needs.
 *
 * It is deliberately not the feed item: a view should not have to know which of
 * these arrived on a live frame and which were read back out of a session log.
 */
export interface ToolCallView {
  readonly tool: string;
  /** Raw JSON arguments, as the harness recorded them. */
  readonly input: string | null;
  readonly output: string | null;
  readonly isError: boolean;
  readonly running: boolean;
  readonly workspace: string;
  readonly exitCode: number | null;
  readonly errorName: string | null;
  readonly errorCode: string | null;
  readonly notices: readonly string[];
  readonly inputTruncated: boolean;
  readonly outputTruncated: boolean;
  readonly harnessTruncated: boolean;
  readonly spillPath: string | null;
  readonly startedAt: string | null;
  readonly endedAt: string | null;
}

/** How a call ended, in the terms a chip can show. */
export type ToolStatus = "running" | "ok" | "failed" | "warn";

/** One labelled fact, shown under a card's content. */
export interface ToolFact {
  readonly label: string;
  readonly value: string;
}

/** Content worth its own space in a card. */
export type ToolBlock =
  | { readonly kind: "diff"; readonly change: ToolChange }
  | { readonly kind: "code"; readonly label: string; readonly text: string; readonly copy: boolean }
  | { readonly kind: "text"; readonly label: string; readonly text: string };

/** A sentence a card has to carry to stay honest. */
export interface ToolNote {
  readonly text: string;
  readonly tone: "warn" | "muted";
}

/** A tool call as a card should show it. */
export interface ToolView {
  readonly tool: string;
  readonly status: ToolStatus;
  /** What the chip says: "running", "exit 2", "failed", "done". */
  readonly statusLabel: string;
  /** The collapsed row's one line. Empty when the call has nothing worth saying. */
  readonly headline: string;
  readonly blocks: readonly ToolBlock[];
  readonly facts: readonly ToolFact[];
  readonly notes: readonly ToolNote[];
  /** The arguments as they arrived, for the fold. Null when they are already shown. */
  readonly raw: string | null;
}

/* --------------------------------------------------------------- the input */

/** Flattens a feed item into the facts a view reads. */
export function toolCallView(item: ToolFeedItem, workspace: string): ToolCallView {
  return {
    tool: item.tool,
    input: item.input,
    output: item.output,
    isError: item.isError,
    running: item.open,
    workspace,
    exitCode: item.exitCode,
    errorName: item.errorName,
    errorCode: item.errorCode,
    notices: item.notices,
    inputTruncated: item.inputTruncated,
    outputTruncated: item.outputTruncated,
    harnessTruncated: item.harnessTruncated,
    spillPath: item.spillPath,
    startedAt: item.time,
    endedAt: item.endedAt,
  };
}

/* ---------------------------------------------------------------- registry */

/** What a presenter has to produce. Everything else is added by `presentTool`. */
interface Presented {
  readonly headline: string;
  readonly blocks: readonly ToolBlock[];
  readonly facts?: readonly ToolFact[];
  readonly notes?: readonly ToolNote[];
  /**
   * Arguments to offer behind a fold. Defaults to the raw input; a presenter that
   * already shows them inline returns null to avoid saying the same thing twice.
   */
  readonly raw?: string | null;
}

type Presenter = (call: ToolCallView, args: ToolArgs | null) => Presented;

/** A file's contents, or the result of searching them. */
function outputBlock(call: ToolCallView, label = "Output"): ToolBlock | null {
  if (call.output === null || call.output === "") return null;
  return { kind: "code", label, text: call.output, copy: true };
}

/** A command, kept apart from its output because it is what a reader copies. */
function commandBlock(command: string): ToolBlock | null {
  if (command === "") return null;
  return { kind: "code", label: "Command", text: command, copy: true };
}

function pathArgs(call: ToolCallView, args: ToolArgs | null): string {
  const path = str(args, "file_path") || str(args, "path");
  return path === "" ? "" : shortPath(path, call.workspace);
}

/**
 * The presenters, keyed by the tool name DSH reports.
 *
 * Each returns only what is specific to that tool. A tool that is missing falls
 * through to `unfamiliar`, which is a real answer rather than an error: the
 * arguments and the output are still exactly what happened.
 */
const PRESENTERS: Readonly<Record<string, Presenter>> = {
  read: (call, args) => ({
    headline: pathArgs(call, args),
    blocks: [outputBlock(call, "Contents")].filter((block): block is ToolBlock => block !== null),
    facts: countFact(call.output, "Lines"),
  }),

  write: (call) => changeView(call),
  edit: (call) => changeView(call),
  notebook_edit: (call) => changeView(call),
  read_image: (call, args) => ({ headline: pathArgs(call, args), blocks: [] }),

  bash: (call, args) => {
    const command = str(args, "command");
    const description = str(args, "description");
    return {
      // The description is what the agent meant to do; the command is what it
      // did. "Run the end-to-end test" answers "what is happening" better than
      // `node scripts/e2e-browser.mjs` does.
      headline: firstLine(description !== "" ? description : command),
      blocks: [commandBlock(command), outputBlock(call)].filter((block): block is ToolBlock => block !== null),
      raw: null,
    };
  },

  grep: (call, args) => ({
    headline: join([firstLine(str(args, "pattern")), pathArgs(call, args)]),
    blocks: [outputBlock(call, "Matches")].filter((block): block is ToolBlock => block !== null),
  }),

  glob: (call, args) => ({
    headline: join([firstLine(str(args, "pattern")), pathArgs(call, args)]),
    blocks: [outputBlock(call, "Paths")].filter((block): block is ToolBlock => block !== null),
  }),

  web_fetch: (call, args) => ({
    headline: firstLine(str(args, "url")),
    blocks: [outputBlock(call)].filter((block): block is ToolBlock => block !== null),
  }),

  web_search: (call, args) => {
    const queries = strings(args, "query");
    const first = queries[0] ?? firstString(args, "query");
    return {
      headline: firstLine(queries.length > 1 ? `${first} +${queries.length - 1} more` : first),
      blocks: [outputBlock(call, "Results")].filter((block): block is ToolBlock => block !== null),
      facts: queries.map((query, index) => ({ label: queries.length > 1 ? `Query ${index + 1}` : "Query", value: query })),
    };
  },

  subagent: delegate,
  subagent_fork: delegate,
  workflow: delegate,

  send_message: (call, args) => {
    const message = str(args, "message");
    return {
      headline: firstLine(message),
      blocks: [
        message === "" ? null : { kind: "text", label: "Message", text: message },
        outputBlock(call, "Reply"),
      ].filter((block): block is ToolBlock => block !== null),
    };
  },

  ask_user_question: (call, args) => {
    const question = str(args, "question");
    return {
      headline: firstLine(question),
      blocks: [
        question === "" ? null : { kind: "text", label: "Question", text: question },
        outputBlock(call, "Answer"),
      ].filter((block): block is ToolBlock => block !== null),
    };
  },
};

/** The change an edit or a write made, drawn as a diff. */
function changeView(call: ToolCallView): Presented {
  const change = changeOf(call.tool, call.input, call.workspace);
  if (change === null) {
    return { headline: "", blocks: [], raw: null };
  }
  // The change's own line count belongs on the row for a write as much as for an
  // edit: it is the one number that says how much of the file moved.
  const stat = diffStat(change.added, change.deleted);
  return {
    headline: join([change.display, stat]),
    blocks: change.lines.length === 0 ? [] : [{ kind: "diff", change }],
    // The diff is the arguments, restated; the fold would repeat it.
    raw: null,
  };
}

/**
 * A delegated task, which reads differently before and after it answers.
 *
 * While it runs there is no report yet, and the prompt is the only thing that
 * says what was handed over; once it settles, the report is what the reader came
 * for and the prompt goes back behind the fold.
 */
function delegate(call: ToolCallView, args: ToolArgs | null): Presented {
  const description = str(args, "description");
  const prompt = str(args, "prompt");
  return {
    headline: firstLine(description !== "" ? description : prompt),
    blocks: [
      call.running && prompt !== "" ? { kind: "code", label: "Task", text: prompt, copy: false } : null,
      outputBlock(call, "Report"),
    ].filter((block): block is ToolBlock => block !== null),
  };
}

/** A tool this module has not been taught: show what ran and what came back. */
function unfamiliar(call: ToolCallView): Presented {
  const args = call.input === null ? "" : call.input.trim();
  return {
    headline: "",
    blocks: [
      args === "" ? null : { kind: "code", label: "Arguments", text: args, copy: true },
      outputBlock(call),
    ].filter((block): block is ToolBlock => block !== null),
    // Shown above, so there is nothing left for a fold to reveal.
    raw: null,
  };
}

/** A line count, as a fact, when there is a count to speak of. */
function countFact(output: string | null, label: string): readonly ToolFact[] {
  if (output === null) return [];
  const lines = lineCount(output);
  return lines === 0 ? [] : [{ label, value: String(lines) }];
}

/* ---------------------------------------------------------------- the view */

/** Reads a call's argument JSON once and hands it to the presenter. */
export function presentTool(call: ToolCallView): ToolView {
  const args = parseArgs(call.input);
  const presenter = PRESENTERS[call.tool] ?? unfamiliar;
  let presented: Presented;
  try {
    presented = presenter(call, args);
  } catch {
    // A presenter is a pure function of parsed JSON and should not throw; if one
    // ever does, the card still has to render, so the fallback takes over.
    presented = unfamiliar(call);
  }

  const facts = [...(presented.facts ?? []), ...resultFacts(call)];
  const notes = [...(presented.notes ?? []), ...resultNotes(call)];
  const status = statusOf(call);

  return {
    tool: call.tool,
    status,
    statusLabel: statusLabel(call, status),
    headline: presented.headline,
    blocks: presented.blocks,
    facts,
    notes,
    // `undefined` means "offer the arguments"; null means the presenter already
    // showed them, and an empty input has nothing to offer.
    raw:
      presented.raw === null
        ? null
        : (call.input ?? "").trim() === ""
          ? null
          : prettyArgs(call.input ?? ""),
  };
}

/** The headline alone, for a caller that only has a line to fill. */
export function toolHeadline(tool: string, input: string | null, workspace: string): string {
  return presentTool({
    tool,
    input,
    output: null,
    isError: false,
    running: true,
    workspace,
    exitCode: null,
    errorName: null,
    errorCode: null,
    notices: [],
    inputTruncated: false,
    outputTruncated: false,
    harnessTruncated: false,
    spillPath: null,
    startedAt: null,
    endedAt: null,
  }).headline;
}

/** Arguments are shown as they arrived, only re-indented when they are JSON. */
function prettyArgs(raw: string): string {
  const trimmed = raw.trim();
  if (trimmed[0] !== "{" && trimmed[0] !== "[") return raw;
  try {
    return JSON.stringify(JSON.parse(trimmed) as unknown, null, 2);
  } catch {
    return raw;
  }
}

/**
 * How a call ended.
 *
 * `failed` is the harness saying the call itself broke. A non-zero exit is *not*
 * that: DSH reports a command's exit status rather than erroring, so a command
 * that failed is recorded as a success, and a card that called it "done" would be
 * wrong about the one thing the reader wanted to know. It gets its own state —
 * "warn", labelled with the status — rather than being promoted to a failure the
 * harness never claimed.
 */
function statusOf(call: ToolCallView): ToolStatus {
  if (call.running) return "running";
  if (call.isError) return "failed";
  if ((call.exitCode !== null && call.exitCode !== 0) || call.errorCode !== null || call.notices.length > 0) {
    return "warn";
  }
  return "ok";
}

/** What the status chip says. */
function statusLabel(call: ToolCallView, status: ToolStatus): string {
  switch (status) {
    case "running":
      return "running";
    case "failed":
      return "failed";
    case "warn":
      if (call.exitCode !== null && call.exitCode !== 0) return `exit ${call.exitCode}`;
      if (call.errorCode !== null) return "error";
      return "stopped";
    case "ok":
      return "done";
  }
}

/** Facts the result itself contributed. */
function resultFacts(call: ToolCallView): readonly ToolFact[] {
  const facts: ToolFact[] = [];
  if (call.exitCode !== null) facts.push({ label: "Exit code", value: String(call.exitCode) });
  if (call.errorCode !== null || call.errorName !== null) {
    facts.push({ label: "Harness error", value: join([call.errorName ?? "", call.errorCode ?? ""]) });
  }
  return facts;
}

/**
 * What the card is not showing, said out loud.
 *
 * Three different things can cut a payload and a reader deserves to know which:
 * the harness kept only part of what the command printed, this deployment sent
 * only part of what the harness kept, or the arguments were trimmed to fit — in
 * which case any diff drawn from them is partial.
 */
function resultNotes(call: ToolCallView): readonly ToolNote[] {
  const notes: ToolNote[] = [];
  for (const notice of call.notices) notes.push({ text: notice, tone: "warn" });
  if (call.harnessTruncated) {
    notes.push({
      text:
        call.spillPath === null
          ? "The harness cut this output short and kept no copy of the rest."
          : `The harness cut this output short; the full text is in ${call.spillPath}.`,
      tone: "warn",
    });
  }
  if (call.outputTruncated) {
    notes.push({ text: "Only the beginning and the end of this output are shown; the rest was left on the desktop.", tone: "muted" });
  }
  if (call.inputTruncated) {
    notes.push({ text: "These arguments were trimmed to fit; anything drawn from them may be partial.", tone: "warn" });
  }
  return notes;
}
