/**
 * Whether a command deserves a second look, and why.
 *
 * A heuristic, and deliberately a shallow one: it recognises the handful of
 * shapes where a misplaced keystroke on a phone costs someone their work. It does
 * **not** recognise everything dangerous, and its silence is not a claim that a
 * command is safe — the approval sheet says "looks destructive" about what it
 * matches and says nothing at all about the rest.
 *
 * It lives beside the rest of the tool-reading code rather than in the sheet that
 * uses it today because it describes the call, not the screen: a caller that
 * wants to warn about a command should not have to import a dialog to ask.
 */

import { parseArgs, str } from "./args.js";

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
 * Silence means nothing at all: the caller must not present it as "this is safe".
 */
export function looksDestructive(name: string, input: string | null): string {
  if (name !== "bash") return "";
  const command = str(parseArgs(input), "command");
  if (command === "") return "";
  for (const { pattern, why } of RISKY_COMMANDS) {
    if (pattern.test(command)) return why;
  }
  return "";
}
