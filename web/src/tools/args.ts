/**
 * Reading a tool call's arguments.
 *
 * A tool's input arrives as the JSON string DSH recorded, and every reader of it
 * — the collapsed row, the card body, the approval sheet — needs the same few
 * things from it: a path, a command, a pattern. They are read here, once, under
 * one rule: **total functions**. An unknown tool, a missing field, malformed
 * JSON, or an argument shape a future DSH invents all produce an empty answer
 * rather than an exception. This runs while somebody scrolls a phone, and a
 * crash here is a blank screen.
 *
 * The second rule is that a missing answer stays missing. A field that is not
 * there yields "", never a guess: a card that says "no description" is honest,
 * and a card that invents one is not.
 */

import { isRecord } from "../decode.js";

/** A parsed argument object, or null when the payload is not one. */
export type ToolArgs = Readonly<Record<string, unknown>>;

/**
 * How much of a value fits on a phone line before the stylesheet clips it.
 *
 * The stylesheet ellipsises a row rather than wrapping it — one line per tool
 * call is what keeps a long turn scannable — so a summary longer than this is
 * text the reader will never see, and it pushes the status chip off the row.
 */
export const MAX_DETAIL = 120;

export function parseArgs(input: string | null): ToolArgs | null {
  if (input === null || input.trim() === "") return null;
  try {
    const parsed: unknown = JSON.parse(input);
    return isRecord(parsed) ? parsed : null;
  } catch {
    return null;
  }
}

/** A string field, or "" — never a coerced number, never "undefined". */
export function str(source: ToolArgs | null, key: string): string {
  if (source === null) return "";
  const value = source[key];
  return typeof value === "string" ? value : "";
}

/** The first string in an array-valued field, for `query: string[]`. */
export function firstString(source: ToolArgs | null, key: string): string {
  if (source === null) return "";
  const value = source[key];
  if (typeof value === "string") return value;
  if (!Array.isArray(value)) return "";
  const found = value.find((item): item is string => typeof item === "string");
  return found ?? "";
}

/** Every string in an array-valued field, in order. */
export function strings(source: ToolArgs | null, key: string): readonly string[] {
  if (source === null) return [];
  const value = source[key];
  if (typeof value === "string") return value === "" ? [] : [value];
  if (!Array.isArray(value)) return [];
  return value.filter((item): item is string => typeof item === "string" && item !== "");
}

/** The first line of a value, clipped to what a row can show. */
export function firstLine(value: string, max = MAX_DETAIL): string {
  const line = value.split("\n")[0] ?? "";
  return line.length > max ? `${line.slice(0, max - 1)}…` : line;
}

/** A count of lines in a value, tolerating CRLF and a trailing newline. */
export function lineCount(value: string): number {
  if (value.trim() === "") return 0;
  const trimmed = value.endsWith("\n") ? value.slice(0, -1) : value;
  return trimmed.split("\n").length;
}

/**
 * A path as the reader thinks of it: relative to the workspace they opened.
 *
 * An absolute path on a 390px line spends most of its width on directories the
 * reader already knows, and pushes the file name out of sight exactly when it is
 * the part that matters.
 */
export function shortPath(path: string, workspace: string): string {
  if (workspace !== "" && path.startsWith(`${workspace}/`)) return path.slice(workspace.length + 1);
  return path;
}

/** Joins the parts of a summary, dropping the ones with nothing to say. */
export function join(parts: readonly string[]): string {
  return parts.filter((part) => part !== "").join(" ");
}
