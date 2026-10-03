/**
 * What a tool call did to a file, and how to draw it.
 *
 * This module is the single source of a change: the transcript's tool card, the
 * approval sheet that asks whether to allow an edit, and the change screen that
 * collects a session's edits all describe the same two strings — what was
 * replaced, and what replaced it. Each of them used to compute that for itself,
 * and one of them — the transcript — did not compute it at all, so the card with
 * the least room showed the raw JSON while the two with the most showed a diff.
 *
 * The rules the computation holds to:
 *
 * 1. **Nothing is invented.** Only `edit` and `write` are read, because those are
 *    the shapes DSH records. A `bash` command that ran `sed -i` changed a file
 *    too, and claiming to know that would be worse than saying nothing.
 * 2. **A whole-file write has no other side.** The arguments carry the new
 *    contents and not the old, so `deleted` is zero rather than a guess.
 * 3. **It cannot stall a phone.** The difference is trimmed at the common prefix
 *    and suffix, which is linear; a full LCS on a multi-megabyte write is not.
 */

import { el } from "../dom.js";
import { parseArgs, str, type ToolArgs } from "./args.js";

/** Context lines kept on each side of a change, matching a unified diff. */
const DIFF_CONTEXT = 3;

/** Beyond this, a rendered diff stops being something a phone should hold. */
const MAX_DIFF_LINES = 400;

/** One file mutation a tool call recorded. */
export interface ToolChange {
  /** The path as the tool named it. */
  readonly path: string;
  /** The path relative to the workspace, which is how a reader knows it. */
  readonly display: string;
  /** A whole-file write, whose previous contents the arguments do not carry. */
  readonly wholeFile: boolean;
  readonly added: number;
  readonly deleted: number;
  /** Change lines, each prefixed with "+", "-" or a space. */
  readonly lines: readonly string[];
  /** The change was cut to keep it renderable. */
  readonly truncated: boolean;
}

/** Splits content into lines, tolerating CRLF and a trailing newline. */
function splitLines(value: string): readonly string[] {
  if (value === "") return [];
  const trimmed = value.endsWith("\n") ? value.slice(0, -1) : value;
  return trimmed.split("\n").map((line) => (line.endsWith("\r") ? line.slice(0, -1) : line));
}

/** The difference between two strings, as change lines and their counts. */
function diffLines(before: string, after: string): { lines: string[]; added: number; deleted: number } {
  const oldLines = splitLines(before);
  const newLines = splitLines(after);

  let prefix = 0;
  while (prefix < oldLines.length && prefix < newLines.length && oldLines[prefix] === newLines[prefix]) prefix += 1;
  let suffix = 0;
  while (
    suffix < oldLines.length - prefix &&
    suffix < newLines.length - prefix &&
    oldLines[oldLines.length - 1 - suffix] === newLines[newLines.length - 1 - suffix]
  ) {
    suffix += 1;
  }

  const contextBefore = Math.min(prefix, DIFF_CONTEXT);
  const contextAfter = Math.min(suffix, DIFF_CONTEXT);
  const removed = oldLines.slice(prefix, oldLines.length - suffix);
  const addedLines = newLines.slice(prefix, newLines.length - suffix);

  const lines: string[] = [];
  for (const line of oldLines.slice(prefix - contextBefore, prefix)) lines.push(` ${line}`);
  for (const line of removed) lines.push(`-${line}`);
  for (const line of addedLines) lines.push(`+${line}`);
  for (const line of oldLines.slice(oldLines.length - suffix, oldLines.length - suffix + contextAfter)) {
    lines.push(` ${line}`);
  }
  return { lines, added: addedLines.length, deleted: removed.length };
}

/**
 * Reads a file mutation out of a tool call's arguments.
 *
 * `workspace` is the session root, used to shorten the display path; "" leaves
 * the path as the tool named it.
 */
function toolChange(name: string, args: ToolArgs | null, workspace: string): ToolChange | null {
  if (args === null) return null;
  const path = str(args, "file_path") || str(args, "path");
  if (path === "") return null;

  const display = workspace !== "" && path.startsWith(`${workspace}/`) ? path.slice(workspace.length + 1) : path;
  const base = { path, display };

  if (name === "write") {
    const lines = splitLines(str(args, "content")).map((line) => `+${line}`);
    return {
      ...base,
      wholeFile: true,
      added: lines.length,
      // Nothing to count on the other side: the arguments do not carry what the
      // file held before, and inventing a number would misreport the change.
      deleted: 0,
      lines: lines.slice(0, MAX_DIFF_LINES),
      truncated: lines.length > MAX_DIFF_LINES,
    };
  }

  if (name === "edit" || name === "notebook_edit") {
    const diff = diffLines(str(args, "old_string") || str(args, "old_source"), str(args, "new_string") || str(args, "new_source"));
    return {
      ...base,
      wholeFile: false,
      added: diff.added,
      deleted: diff.deleted,
      lines: diff.lines.slice(0, MAX_DIFF_LINES),
      truncated: diff.lines.length > MAX_DIFF_LINES,
    };
  }

  return null;
}

/** The change for a call whose input is still the raw JSON string. */
export function changeOf(name: string, input: string | null, workspace: string): ToolChange | null {
  return toolChange(name, parseArgs(input), workspace);
}

/* --------------------------------------------------------------- rendering */

/** `+14 −2`, or "" when the change added and removed nothing. */
export function diffStat(added: number, deleted: number): string {
  const parts: string[] = [];
  if (added > 0) parts.push(`+${added}`);
  if (deleted > 0) parts.push(`−${deleted}`);
  return parts.join(" ");
}

/**
 * One line of a change.
 *
 * The leading sign is markup rather than the line's text: the class carries it
 * for the stylesheet, and an aria-hidden span carries it for the eye, so the
 * spoken text does not begin with a sign on every single line. What the change
 * amounts to is said once, on the block.
 */
function diffLine(line: string): HTMLElement {
  const sign = line.slice(0, 1);
  const kind = sign === "+" ? "diff-add" : sign === "-" ? "diff-del" : "diff-ctx";
  return el(
    "div",
    { class: `diff-line ${kind}` },
    el("span", { attrs: { "aria-hidden": "true" }, text: sign === "" ? " " : sign }),
    line.slice(1),
  );
}

/** The change in words, for a reader the colours do not reach. */
function diffLabel(change: ToolChange): string {
  const parts = [`Change to ${change.display}`];
  if (change.added > 0) parts.push(`${change.added} added`);
  if (change.deleted > 0) parts.push(`${change.deleted} removed`);
  if (change.wholeFile) parts.push("the previous contents are not recorded");
  if (change.truncated) parts.push("it is shown cut short");
  return `${parts.join(", ")}.`;
}

/** What the diff does not show, said plainly rather than left to be assumed. */
function diffNote(change: ToolChange): string {
  const notes: string[] = [];
  if (change.wholeFile) {
    notes.push("The log does not record what this file held before, so only the new contents are shown.");
  }
  if (change.truncated) {
    notes.push("This change was cut short to keep it renderable; more of the file changed than is shown here.");
  }
  return notes.join(" ");
}

/** The changed file: a caption, its lines, and a note when the diff is partial. */
export function diffBlock(change: ToolChange): readonly Node[] {
  const stat = diffStat(change.added, change.deleted);
  const head = el("p", {
    class: "diff-head",
    text: stat === "" ? change.display : `${change.display} · ${stat}`,
  });
  const lines = el("div", { class: "diff", attrs: { role: "group", "aria-label": diffLabel(change) } });
  for (const line of change.lines) lines.appendChild(diffLine(line));

  const nodes: Node[] = [head, lines];
  const note = diffNote(change);
  // Beside the block, not inside it: a note below a scrolling diff is a note a
  // reader who never scrolls to the end never sees.
  if (note !== "") nodes.push(el("p", { class: "diff-note", text: note }));
  return nodes;
}
