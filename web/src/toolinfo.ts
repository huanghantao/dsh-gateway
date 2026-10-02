/**
 * One line about what a tool call is doing.
 *
 * A collapsed tool card is a row in a conversation, not a JSON viewer: "edit"
 * says nothing, while "edit · web/src/views/conversation.ts +14" says what
 * happened and where. DSH's own desktop timeline reads the same way, and the
 * phone should not be the place where a reader has to expand a card to find out
 * which file was touched.
 *
 * The arguments are a JSON string — that is what both routes carry: the ACP
 * bridge's `rawInput` and the session log's `arguments`. Parsing them here, once,
 * keeps the view free of tool-specific knowledge, and every function is total:
 * an unknown tool, a missing field, malformed JSON or a future argument shape
 * all produce an empty summary rather than an exception on a phone.
 */

import { isRecord } from "./decode.js";

/** Tools whose only interesting argument is a path. */
const PATH_TOOLS = new Set(["read", "write", "notebook_edit", "read_image"]);

/** How much of a command or description fits on a phone line before the CSS clips it. */
const MAX_DETAIL = 120;

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

/** `+added -removed` for an edit, or "" when neither side has content. */
function diffStat(before: string, after: string): string {
  const added = after === "" ? 0 : after.split("\n").length;
  const removed = before === "" ? 0 : before.split("\n").length;
  if (added === 0 && removed === 0) return "";
  return `+${added} -${removed}`;
}

function join(parts: readonly string[]): string {
  return parts.filter((part) => part !== "").join(" ");
}

/**
 * The summary for one tool call, or "" when there is nothing worth saying.
 *
 * `workspace` is the session's root, used to shorten paths; pass "" when it is
 * not known yet, and absolute paths are shown instead.
 */
export function toolDetail(name: string, input: string | null, workspace: string): string {
  const parsed = args(input);
  if (parsed === null) return "";

  if (PATH_TOOLS.has(name)) {
    const path = text(parsed, "path") || text(parsed, "file_path");
    return path === "" ? "" : shortPath(path, workspace);
  }

  switch (name) {
    case "bash": {
      // The description is what the agent meant to do; the command is what it
      // did. DSH's own timeline leads with the description for the same reason:
      // `node scripts/e2e-browser.mjs` is a worse answer to "what is happening"
      // than "Run the end-to-end test".
      const description = text(parsed, "description");
      return firstLine(description !== "" ? description : text(parsed, "command"));
    }

    case "edit": {
      const path = text(parsed, "file_path");
      const stat = diffStat(text(parsed, "old_string"), text(parsed, "new_string"));
      const shown = path === "" ? "" : shortPath(path, workspace);
      return join([shown, stat]);
    }

    case "grep":
    case "glob": {
      const pattern = text(parsed, "pattern");
      const path = text(parsed, "path");
      return join([pattern, path === "" ? "" : shortPath(path, workspace)]);
    }

    case "web_fetch":
      return firstLine(text(parsed, "url"));

    case "web_search": {
      const query = parsed["query"];
      if (typeof query === "string") return firstLine(query);
      if (Array.isArray(query)) {
        const first = query.find((item): item is string => typeof item === "string");
        return first === undefined ? "" : firstLine(first);
      }
      return "";
    }

    case "subagent":
    case "subagent_fork":
    case "workflow":
      return firstLine(text(parsed, "description") || text(parsed, "prompt"));

    case "send_message":
    case "ask_user_question":
      return firstLine(text(parsed, "message") || text(parsed, "question"));

    default:
      return "";
  }
}
