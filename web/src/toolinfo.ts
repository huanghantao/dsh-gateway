/**
 * What a tool call means, in the terms a reader needs.
 *
 * A collapsed tool card is a row in a conversation, not a JSON viewer: "edit"
 * says nothing, while "edit · web/src/views/conversation.ts +14" says what
 * happened and where. The same interpretation is needed in three places — the
 * transcript's tool card, the approval sheet that asks whether to allow the
 * call, and the change screen that collects a session's edits — so it lives
 * here, once.
 *
 * Two rules hold throughout:
 *
 * 1. **Total functions.** An unknown tool, a missing field, malformed JSON or a
 *    future argument shape produce an empty answer rather than an exception.
 *    This runs while someone is scrolling a phone; a crash here is a blank
 *    screen.
 * 2. **No safety claims.** `looksDestructive` names the reason a command
 *    deserves a second look. Its silence means nothing at all, and the UI must
 *    never present it as "this is safe".
 */

import { isRecord } from "./decode.js";

/** Tools whose only interesting argument is a path. */
const PATH_TOOLS = new Set(["read", "write", "notebook_edit", "read_image"]);

/** How much of a command or description fits on a phone line before the CSS clips it. */
const MAX_DETAIL = 120;

/** Context lines kept on each side of a change, matching a unified diff. */
const DIFF_CONTEXT = 3;

/** Beyond this, a rendered diff stops being something a phone should hold. */
const MAX_DIFF_LINES = 400;

function args(input: string | null): Record<string, unknown> | null {
  if (input === null || input.trim() === "") return null;
  try {
    const parsed: unknown = JSON.parse(input);
    return isRecord(parsed) ? parsed : null;
  } catch {
    return null;
  }
}

function text(source: Record<string, unknown>, key: string): string {
  const value = source[key];
  return typeof value === "string" ? value : "";
}

function firstLine(value: string): string {
  const line = value.split("\n")[0] ?? "";
  return line.length > MAX_DETAIL ? `${line.slice(0, MAX_DETAIL - 1)}…` : line;
}

/**
 * A path as the reader thinks of it: relative to the workspace they opened.
 *
 * An absolute path on a 390px line spends most of its width on directories the
 * reader already knows, and pushes the file name out of sight exactly when it
 * is the part that matters.
 */
function shortPath(path: string, workspace: string): string {
  if (workspace !== "" && path.startsWith(`${workspace}/`)) return path.slice(workspace.length + 1);
  return path;
}

function join(parts: readonly string[]): string {
  return parts.filter((part) => part !== "").join(" ");
}

/* ------------------------------------------------------------------ changes */

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

/** A tool call, described for a reader. */
export interface ToolSummary {
  /** One line: "edit · internal/cache/store.go +14 -2". */
  readonly headline: string;
  /** The file change this call made, when it made one. */
  readonly change: ToolChange | null;
  /**
   * Why this call deserves a second look, or "" when nothing about it is
   * recognisable as destructive. Silence is not a safety claim.
   */
  readonly risk: string;
}

/**
 * Splits content into lines, tolerating CRLF and a trailing newline.
 *
 * Paired with `splitLines` in `internal/sessionlog/changes.go`: the server
 * builds the same hunks for the session-wide view, and a reader comparing the
 * two would notice at once if they disagreed. Each is tested on its own side.
 */
function splitLines(value: string): readonly string[] {
  if (value === "") return [];
  const trimmed = value.endsWith("\n") ? value.slice(0, -1) : value;
  return trimmed.split("\n").map((line) => (line.endsWith("\r") ? line.slice(0, -1) : line));
}

/**
 * Renders the difference between two strings as change lines.
 *
 * The common prefix and suffix are trimmed and everything between them is marked
 * as replaced. That is not a *minimal* diff — a block that moved shows as a
 * replacement rather than as two small edits — but it is always correct, it is
 * linear in the size of the edit, and it cannot stall a phone on the
 * multi-megabyte write that a full LCS would have to build a matrix for.
 */
export function diffLines(before: string, after: string): { lines: string[]; added: number; deleted: number } {
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
 * The two shapes here are the ones the harness's own file tools record: `edit`
 * carries the text it replaced and the text that took its place, and `write`
 * carries a whole file. Anything else returns null rather than a guess — a bash
 * command that ran `sed -i` changed a file too, and claiming to know that would
 * be worse than saying nothing.
 */
export function toolChange(name: string, input: string | null, workspace: string): ToolChange | null {
  const parsed = args(input);
  if (parsed === null) return null;
  const path = text(parsed, "file_path") || text(parsed, "path");
  if (path === "") return null;

  const base = { path, display: shortPath(path, workspace) };

  if (name === "write") {
    const lines = splitLines(text(parsed, "content")).map((line) => `+${line}`);
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

  if (name === "edit") {
    const diff = diffLines(text(parsed, "old_string"), text(parsed, "new_string"));
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

/* --------------------------------------------------------------------- risk */

/** Patterns that make a shell command worth reading twice, and why. */
const RISKY_COMMANDS: readonly { readonly pattern: RegExp; readonly why: string }[] = [
  { pattern: /\brm\s+(-[a-zA-Z]*[rR][a-zA-Z]*|--recursive)\b/, why: "deletes files recursively" },
  { pattern: /\bgit\s+push\b[^\n]*(--force\b|\s-f\b)/, why: "rewrites published history" },
  { pattern: /\bgit\s+reset\s+--hard\b/, why: "discards uncommitted work" },
  { pattern: /\bgit\s+clean\b[^\n]*\s-[a-zA-Z]*[fdx]/, why: "deletes untracked files" },
  { pattern: /\b(mkfs|fdisk|dd)\b/, why: "writes to a raw device" },
  { pattern: /\bchmod\s+(-R\s+)?0?777\b/, why: "makes files world-writable" },
  { pattern: /(curl|wget)\b[^\n|]*\|\s*(sudo\s+)?(ba|z|k)?sh\b/, why: "pipes a download into a shell" },
  { pattern: /\bsudo\b/, why: "runs as root" },
  { pattern: />\s*\/dev\/(sd|disk|nvme)/, why: "writes to a raw device" },
  { pattern: /\btruncate\b[^\n]*\s-s\s*0/, why: "empties a file" },
  { pattern: /\bshutdown\b|\breboot\b/, why: "restarts the machine" },
];

/**
 * Names why a command deserves a second look, or returns "".
 *
 * A heuristic, and deliberately a shallow one: it recognises the handful of
 * shapes where a misplaced keystroke on a phone costs someone their work. It
 * does **not** recognise everything dangerous, and its silence is not a claim
 * that a command is safe — the approval sheet says "looks destructive" about
 * what it matches, and says nothing at all about the rest.
 */
export function looksDestructive(name: string, input: string | null): string {
  if (name !== "bash") return "";
  const parsed = args(input);
  if (parsed === null) return "";
  const command = text(parsed, "command");
  if (command === "") return "";
  for (const { pattern, why } of RISKY_COMMANDS) {
    if (pattern.test(command)) return why;
  }
  return "";
}

/* ----------------------------------------------------------------- headline */

/** `+added -removed` for a change, or "" when neither side has content. */
function diffStat(added: number, deleted: number): string {
  const parts: string[] = [];
  if (added > 0) parts.push(`+${added}`);
  if (deleted > 0) parts.push(`-${deleted}`);
  return parts.join(" ");
}

/**
 * The summary for one tool call.
 *
 * `workspace` is the session's root, used to shorten paths; pass "" when it is
 * not known yet, and absolute paths are shown instead.
 */
export function describeTool(name: string, input: string | null, workspace: string): ToolSummary {
  const parsed = args(input);
  const change = toolChange(name, input, workspace);
  const risk = looksDestructive(name, input);

  if (parsed === null) return { headline: "", change, risk };

  if (PATH_TOOLS.has(name)) {
    const path = text(parsed, "path") || text(parsed, "file_path");
    return { headline: path === "" ? "" : shortPath(path, workspace), change, risk };
  }

  switch (name) {
    case "bash": {
      // The description is what the agent meant to do; the command is what it
      // did. DSH's own timeline leads with the description for the same reason:
      // `node scripts/e2e-browser.mjs` is a worse answer to "what is happening"
      // than "Run the end-to-end test".
      const description = text(parsed, "description");
      return { headline: firstLine(description !== "" ? description : text(parsed, "command")), change, risk };
    }

    case "edit":
    case "write": {
      const shown = change === null ? "" : change.display;
      const stat = change === null ? "" : diffStat(change.added, change.deleted);
      return { headline: join([shown, stat]), change, risk };
    }

    case "grep":
    case "glob": {
      const pattern = text(parsed, "pattern");
      const path = text(parsed, "path");
      return { headline: join([pattern, path === "" ? "" : shortPath(path, workspace)]), change, risk };
    }

    case "web_fetch":
      return { headline: firstLine(text(parsed, "url")), change, risk };

    case "web_search": {
      const query = parsed["query"];
      if (typeof query === "string") return { headline: firstLine(query), change, risk };
      if (Array.isArray(query)) {
        const first = query.find((item): item is string => typeof item === "string");
        return { headline: first === undefined ? "" : firstLine(first), change, risk };
      }
      return { headline: "", change, risk };
    }

    case "subagent":
    case "subagent_fork":
    case "workflow":
      return { headline: firstLine(text(parsed, "description") || text(parsed, "prompt")), change, risk };

    case "send_message":
    case "ask_user_question":
      return { headline: firstLine(text(parsed, "message") || text(parsed, "question")), change, risk };

    default:
      return { headline: "", change, risk };
  }
}

/** The one-line summary alone, for a caller that wants nothing else. */
export function toolDetail(name: string, input: string | null, workspace: string): string {
  return describeTool(name, input, workspace).headline;
}
