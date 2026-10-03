/**
 * Reading the harness's own account of a delegated task.
 *
 * When a background subagent settles, DSH writes a *plain-text, user-role*
 * message into the parent session — there is no envelope, no typed field, and
 * ACP has no subagent scope at all (it is an open RFD, not a stable one). The
 * literal text, from DSH's own source:
 *
 *     Background subagent <child-id> finished and will do no further work unless
 *     you send it more.
 *     Its closing message:
 *     <the child's final text>
 *
 * The consequences of that shape are what this module exists to undo:
 *
 *   - The transcript renders it as a message from **You**. The reader asked for a
 *     review of their branch, and the answer arrives looking like something they
 *     said themselves.
 *   - It identifies the child by session id and never by what it was asked to
 *     do, so three delegations finishing in one session produce three rows that
 *     differ only in a hex string.
 *   - It cannot be answered by a client that does not parse it, because the
 *     meaning is only in the prose.
 *
 * Parsing prose is a real cost and it is paid deliberately: the alternative is
 * leaving the operator unable to tell their own words from their agent's, and no
 * other channel carries the fact. Every pattern here is copied from the
 * harness's source, and a message that does not match is left exactly as it
 * arrived — the failure mode of a parser like this must be "shows the original
 * text", never "shows nothing".
 *
 * The sibling vocabulary matches Claude Code's task notifications too, because a
 * gateway in front of a different harness should not be a gateway that cannot
 * read it.
 */

/** What a settlement message says, once it has been read. */
export interface Settlement {
  /** The subject as the harness wrote it: "Background subagent abc123". */
  readonly subject: string;
  /** The child's own identifier, when one can be read out of the subject. */
  readonly childId: string;
  /** How it ended, in the gateway's vocabulary. */
  readonly outcome: "completed" | "failed" | "cancelled";
  /** The child's closing message, or "" when it left none. */
  readonly report: string;
}

/** The completion sentence DSH writes, and the five ways it can end instead. */
const SETTLEMENTS: readonly { readonly pattern: RegExp; readonly outcome: Settlement["outcome"] }[] = [
  { pattern: /^(.+?) finished and will do no further work unless you send it more\.?$/, outcome: "completed" },
  { pattern: /^(.+?) finished and left no closing message\.?$/, outcome: "completed" },
  { pattern: /^(.+?) was stopped before it finished\.?$/, outcome: "cancelled" },
  { pattern: /^(.+?) ran out of room before it finished\.?$/, outcome: "failed" },
  { pattern: /^(.+?) declined the task\.?$/, outcome: "failed" },
  { pattern: /^(.+?) failed before it finished\.?$/, outcome: "failed" },
  { pattern: /^(.+?) ended abnormally \((.+)\) before it finished\.?$/, outcome: "failed" },
  { pattern: /^(.+?) ended abnormally before it finished\.?$/, outcome: "failed" },
  // The one-shot background job form, which is the same event for a child that
  // cannot be messaged again.
  { pattern: /^background job (\S+) \((.+)\) finished(?: \[status: (.+)\])?\.?$/, outcome: "completed" },
];

/**
 * The markers DSH writes after the settlement sentence, in the order they can
 * appear on a line the harness concatenated:
 *
 *   "<sentence>Its closing message:<report>"
 *   "<sentence>It left no closing message."
 *
 * They are matched by name rather than by line, because a block boundary is not
 * something the recorded text preserves.
 */
const MARKERS: readonly string[] = ["Its closing message", "It left no closing message"];

/** The longest closing message worth keeping for a row. */
const REPORT_LIMIT = 400;

/**
 * Reads a settlement out of one committed message, or null when the text is
 * anything else — which is the case for every prompt a person typed.
 */
export function parseSettlement(text: string): Settlement | null {
  const trimmed = text.trim();
  if (trimmed === "") return null;

  const lines = trimmed.split(/\r?\n/);
  const first = lines[0]?.trim() ?? "";
  // The harness concatenates its own blocks, so the sentence and the marker
  // arrive as one line: the recorded text is literally
  // "…send it more.Its closing message:Report delivered…". Splitting on the
  // marker first is what makes the sentence readable, and it splits on a literal
  // the harness wrote rather than on a guess.
  const marker = firstMarker(first);
  const sentence = (marker.at === -1 ? first : first.slice(0, marker.at)).trim();
  const matched = matchSettlement(sentence);
  if (matched === null) return null;

  const tail = marker.at === -1 ? lines.slice(1) : [first.slice(marker.at), ...lines.slice(1)];
  return {
    subject: matched.subject,
    childId: childIdOf(matched.subject),
    outcome: matched.outcome,
    report: closingMessage(tail),
  };
}

/** Where the first marker after the sentence begins, and which one it is. */
function firstMarker(line: string): { at: number; which: string } {
  let best = { at: -1, which: "" };
  for (const which of MARKERS) {
    const at = line.indexOf(which);
    if (at !== -1 && (best.at === -1 || at < best.at)) best = { at, which };
  }
  return best;
}

/** Matches the opening sentence against the harness's own vocabulary. */
function matchSettlement(line: string): { subject: string; outcome: Settlement["outcome"] } | null {
  for (const candidate of SETTLEMENTS) {
    const match = candidate.pattern.exec(line);
    const subject = match?.[1]?.trim() ?? "";
    if (match !== null && subject !== "") {
      // A one-shot job that reports a failure status did fail, whatever verb the
      // sentence used to say it finished.
      const status = (match[3] ?? "").toLowerCase();
      const outcome: Settlement["outcome"] =
        status === "failed" || status === "error" ? "failed" : candidate.outcome;
      return { subject, outcome };
    }
  }
  return null;
}

/**
 * The child's identifier, read out of the subject.
 *
 * The id is the one thing a reader can match against `list_agents`, so it is
 * worth keeping separate from the prose. It is not the *name* of the task —
 * nothing in this message is — which is why the activity row prefers the name
 * the delegation tool call carried.
 */
function childIdOf(subject: string): string {
  const match = /(\S+)$/.exec(subject.trim());
  return match?.[1] ?? "";
}

/**
 * The child's final text, as the lines that follow the marker.
 *
 * Everything after the marker is the report, including blank lines and anything
 * that looks like markup — a closing message is arbitrary model output and this
 * is not the place to interpret it.
 */
function closingMessage(lines: readonly string[]): string {
  const first = lines[0] ?? "";
  const which = MARKERS.find((marker) => first.includes(marker));
  // No marker at all: the text after the sentence is whatever the child left.
  // That is still a report, and dropping it would lose the only thing the reader
  // came for.
  if (which === undefined) return bounded(lines.join("\n"));
  if (which !== "Its closing message") return "";
  // Whatever follows the marker, on this line and the rest of them.
  const rest = [first.slice(first.indexOf(which) + which.length), ...lines.slice(1)];
  return bounded(rest.join("\n").replace(/^:\s*/, ""));
}

/** Keeps a report to something a row can hold. */
function bounded(report: string): string {
  const trimmed = report.trim();
  if (trimmed === "") return "";
  return trimmed.length > REPORT_LIMIT ? `${trimmed.slice(0, REPORT_LIMIT - 1)}…` : trimmed;
}
