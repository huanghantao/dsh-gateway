/**
 * Folding protocol into the conversation feed.
 *
 * History (`GET /transcript`) and live frames describe the same turn stream but
 * arrive by different routes, and a client that opened mid-turn sees both. This
 * module is the single place that reconciles them, as pure functions over an
 * immutable array so the view can diff by reference.
 *
 * Keys are the mechanism: every row gets a stable key, and tool lifecycle pairs
 * up on `callId`. Appending is O(1) for the common case (a new row) and O(n)
 * only for the rarity (a `session.tool` end frame closing a card that may be
 * hundreds of rows back).
 *
 * A row's key is the *same* whether it came from history or from the stream —
 * `msg:<message id>` and `tool:<call id>` — because the two routes overlap.
 * DSH's ids are durable and appear in both: the transcript item for an assistant
 * message carries the id the live frame carried. When a refetched history (after
 * a reconnect or a resync) lands on top of frames that are still arriving, the
 * rows collapse instead of doubling.
 */

import { parseSettlement } from "./settlement.js";
import type {
  FeedItem,
  FeedMessageItem,
  FeedNoticeItem,
  FeedToolItem,
  MessageData,
  NoticeOutcome,
  Session,
  SessionPatch,
  TokenUsage,
  ToolData,
  ToolResultFacts,
  ToolTrim,
  TranscriptItem,
} from "./types.js";

/** How many trailing rows to scan when pairing a tool end with its start. */
const TOOL_LOOKBACK = 400;

export function feedFromTranscript(items: readonly TranscriptItem[]): readonly FeedItem[] {
  const feed: FeedItem[] = [];
  for (const item of items) {
    switch (item.role) {
      case "user": {
        // A background subagent's settlement is committed as a *user* message by
        // the harness, because it is delivered to the model as one. Rendering it
        // as one would show the reader a report from their child agent wearing
        // their own name, so it is read here and folded as the notice it is.
        const report = settlementNotice(`msg:${item.id}`, item.text ?? "", item.time);
        if (report !== null) {
          feed.push(report);
          break;
        }
        feed.push({
          key: `msg:${item.id}`,
          kind: "message",
          role: "user",
          time: item.time,
          text: item.text ?? "",
          thinking: null,
          model: null,
          usage: null,
          attachments: item.attachments,
        });
        break;
      }
      case "assistant":
        feed.push({
          key: `msg:${item.id}`,
          kind: "message",
          role: "assistant",
          time: item.time,
          text: item.text ?? "",
          thinking: item.thinking,
          model: item.model,
          usage: item.usage,
          attachments: 0,
        });
        break;
      case "tool":
        feed.push({
          key: `tool:${item.id}`,
          kind: "tool",
          time: item.time,
          endedAt: item.endedAt,
          tool: item.tool ?? "tool",
          callId: item.id,
          input: item.toolInput,
          output: item.toolOutput,
          // A call the log holds without a result is running right now: a phone
          // opening the session mid-turn has to see that, not a finished card
          // with nothing in it.
          open: item.pending,
          isError: item.isError,
          ...factsOf(item),
        });
        break;
      case "notice":
        // A settlement is keyed the way the live stream keys it, not the way
        // this route keys its own annotations: both describe the same row, and
        // the whole point of the key is that the fold collapses them.
        feed.push(noticeItem(item.actor === null ? `hist-notice:${item.id}` : `msg:${item.id}`, item));
        break;
    }
  }
  return feed;
}

function findToolIndex(feed: readonly FeedItem[], callId: string): number {
  const floor = Math.max(0, feed.length - TOOL_LOOKBACK);
  for (let index = feed.length - 1; index >= floor; index -= 1) {
    const item = feed[index];
    if (item !== undefined && item.kind === "tool" && item.callId === callId) return index;
  }
  return -1;
}

export function applyToolStart(feed: readonly FeedItem[], data: ToolData, time: string, seq: number): readonly FeedItem[] {
  const existing = findToolIndex(feed, data.callId);
  if (existing >= 0) {
    const current = feed[existing];
    if (current === undefined || current.kind !== "tool") return feed;
    const next = [...feed];
    next[existing] = { ...current, input: keepInput(current.input, data.input), tool: data.tool, open: true };
    return next;
  }
  return [
    ...feed,
    {
      key: `tool:${data.callId === "" ? seq : data.callId}`,
      kind: "tool",
      time,
      endedAt: null,
      tool: data.tool,
      callId: data.callId === "" ? `seq-${seq}` : data.callId,
      input: data.input,
      output: null,
      open: true,
      isError: false,
      ...emptyFacts(),
    },
  ];
}

export function applyToolEnd(feed: readonly FeedItem[], data: ToolData, time: string, seq: number): readonly FeedItem[] {
  const existing = findToolIndex(feed, data.callId);
  if (existing >= 0) {
    const current = feed[existing];
    if (current === undefined || current.kind !== "tool") return feed;
    const next = [...feed];
    next[existing] = {
      ...current,
      endedAt: time,
      output: data.output ?? current.output,
      isError: data.isError,
      open: false,
      input: keepInput(current.input, data.input),
      ...factsOf(data),
    };
    return next;
  }
  // An end frame with no start means we attached mid-tool; the card is still
  // worth showing, just already closed.
  return [
    ...feed,
    {
      key: `tool:${data.callId === "" ? seq : data.callId}`,
      kind: "tool",
      time,
      endedAt: time,
      tool: data.tool,
      callId: data.callId === "" ? `seq-${seq}` : data.callId,
      input: data.input,
      output: data.output,
      open: false,
      isError: data.isError,
      ...factsOf(data),
    },
  ];
}

/**
 * The arguments to keep when a frame arrives.
 *
 * A completion frame carries no arguments — DSH's own projection sends the call
 * id, the status and the result and nothing else — and the gateway fills them
 * back in from what it remembered. A client that took the empty string at face
 * value would erase the one line that says what the call was, so the rule is
 * stated here too: an empty payload never overwrites a known one.
 */
function keepInput(current: string | null, incoming: string | null): string | null {
  if (incoming === null || incoming === "") return current;
  return incoming;
}

/** The result facts of a frame, or of a transcript item, in the feed's shape. */
function factsOf(source: ToolResultFacts & ToolTrim): ToolResultFacts & ToolTrim {
  return {
    exitCode: source.exitCode,
    errorName: source.errorName,
    errorCode: source.errorCode,
    notices: source.notices,
    harnessTruncated: source.harnessTruncated,
    spillPath: source.spillPath,
    inputTruncated: source.inputTruncated,
    outputTruncated: source.outputTruncated,
  };
}

/** A call that has not settled yet has said nothing about how it ended. */
function emptyFacts(): ToolResultFacts & ToolTrim {
  return {
    exitCode: null,
    errorName: null,
    errorCode: null,
    notices: [],
    harnessTruncated: false,
    spillPath: null,
    inputTruncated: false,
    outputTruncated: false,
  };
}

/**
 * Appends the operator's own prompt.
 *
 * `attachments` is the count of images sent with it. The app has the images in
 * hand and could render them, but it deliberately does not: the transcript
 * projection carries the count and not the bytes, so a locally rendered image
 * would vanish on the next reload and the row would change shape under the
 * reader. Showing the same "2 images" marker both ways keeps a prompt looking
 * like itself.
 *
 * The gateway echoes an admitted prompt back to every client, including this
 * one, so the echo and this row describe the same message and only one of them
 * may be drawn. Whichever arrives second is dropped — see `samePrompt`.
 */
export function appendUserMessage(
  feed: readonly FeedItem[],
  text: string,
  time: string,
  attachments = 0,
): readonly FeedItem[] {
  if (samePrompt(feed[feed.length - 1], text)) return feed;
  return [
    ...feed,
    {
      key: `local:${time}:${feed.length}`,
      kind: "message",
      role: "user",
      time,
      text,
      thinking: null,
      model: null,
      usage: null,
      attachments,
    },
  ];
}

/**
 * Whether the row already on the end of the feed is the prompt about to be
 * added.
 *
 * This is the whole of the local-echo reconciliation, and it is symmetric on
 * purpose: the HTTP response and the gateway's echo race, so either can land
 * first, and in both orders the second one has to be recognised as the same
 * message. Two identical prompts typed back to back are the case it cannot tell
 * apart; it would fold them into one row until the next history fetch, which is
 * a smaller wrong than showing every prompt twice.
 */
function samePrompt(row: FeedItem | undefined, text: string): boolean {
  return row !== undefined && row.kind === "message" && row.role === "user" && row.text === text;
}

/**
 * Appends a committed message — the assistant's answer, or a prompt someone
 * typed somewhere else (the desk, a headless run) that this client is watching.
 *
 * History and the live stream overlap for a moment after opening a session, so
 * a message that repeats the previous row verbatim is dropped. `data.id` is used
 * as the key when the server provides one, which makes the same guard exact
 * rather than heuristic for servers that send it.
 */
export function appendMessage(feed: readonly FeedItem[], data: MessageData, time: string, seq: number): readonly FeedItem[] {
  const key = data.id === null ? `msg:${data.role}:${seq}` : `msg:${data.id}`;
  const previous = feed[feed.length - 1];
  if (data.id !== null && feed.some((item) => item.key === key)) return feed;
  // The echo of a prompt this client typed itself: it has already drawn that
  // row, and drawing it again would show the reader their own message twice.
  if (data.role === "user" && data.id === null && samePrompt(previous, data.text)) return feed;
  if (
    data.id === null &&
    previous !== undefined &&
    previous.kind === "message" &&
    previous.role === data.role &&
    previous.text === data.text
  ) {
    return feed;
  }
  // A settlement that arrives live takes the same route as one read back from
  // history: the two must produce the same row, or a reconnect would redraw it
  // differently.
  if (data.role === "user") {
    const report = settlementNotice(key, data.text, time);
    if (report !== null) return [...feed, report];
  }
  return [
    ...feed,
    {
      key,
      kind: "message",
      role: data.role,
      time,
      text: data.text,
      thinking: data.thinking,
      model: data.model,
      usage: data.usage,
      attachments: data.attachments,
    },
  ];
}

/**
 * Reads one committed user message as a delegated task's settlement, or null
 * when it is a prompt a person typed.
 *
 * The actor is deliberately unnamed here. The harness's settlement names the
 * child by id and says nothing about the task it was given — that is the whole
 * complaint about this message shape — so the name has to come from the
 * delegation tool call, which the activity fold has and a pure transcript
 * projection does not. A row built from history therefore reads
 * "Subagent · completed" and one built from live events reads
 * "Subagent · Audit the handlers · completed"; both are honest, and the second
 * is the one the reader gets while it happens.
 */
export function settlementNotice(key: string, text: string, time: string | null): FeedNoticeItem | null {
  const settlement = parseSettlement(text);
  if (settlement === null) return null;
  return {
    key: `${key}:settled`,
    kind: "notice",
    time,
    text: settlement.report,
    actor: { kind: "subagent", name: "" },
    outcome: settlement.outcome,
    summary: settlement.subject,
    detail: settlement.report,
  };
}

/**
 * Builds a notice row from a transcript item.
 *
 * Two sources of truth, in order of authority. A settlement the *log* recorded
 * carries the harness's own typed account — who settled, its summary sentence,
 * how it went — and that is read verbatim. A settlement that reached this client
 * as a live user-role message has no such envelope (ACP carries no subagent
 * scope at all; it is an open RFD), so it falls back to reading the prose. Both
 * produce the same row shape, which is what keeps history and the live stream
 * from disagreeing about the same event.
 */
function noticeItem(key: string, item: TranscriptItem): FeedNoticeItem {
  if (item.actor === "subagent") {
    return {
      // The same key the live stream gives this settlement. The two routes
      // overlap for a moment whenever a session is opened or refetched, and a
      // key that differed would show the child's report twice.
      key: `${key}:settled`,
      kind: "notice",
      time: item.time,
      text: item.text ?? "",
      actor: { kind: "subagent", name: "" },
      outcome: noticeOutcome(item.outcome),
      summary: item.summary ?? "",
      detail: item.text ?? "",
    };
  }
  return {
    key,
    kind: "notice",
    time: item.time,
    text: item.text ?? "",
    actor: null,
    outcome: null,
    summary: "",
    detail: "",
  };
}

/** Narrows an outcome word off the wire, defaulting to "no claim". */
function noticeOutcome(value: string | null): NoticeOutcome | null {
  switch (value) {
    case "completed":
    case "failed":
    case "cancelled":
    case "expired":
    case "waiting":
      return value;
    default:
      return null;
  }
}

export function appendNotice(feed: readonly FeedItem[], text: string, time: string, seq: number): readonly FeedItem[] {
  return [
    ...feed,
    { key: `notice:${seq}`, kind: "notice", time, text, actor: null, outcome: null, summary: "", detail: "" },
  ];
}

/** Usage for the header: the authoritative total, else the newest reported. */
export function latestUsage(feed: readonly FeedItem[], reported: TokenUsage | null): TokenUsage | null {
  if (reported !== null) return reported;
  for (let index = feed.length - 1; index >= 0; index -= 1) {
    const item = feed[index];
    if (item !== undefined && item.kind === "message" && item.usage !== null) return item.usage;
  }
  return null;
}

/** Overlays a partial `session.state` onto the metadata we already hold. */
export function mergeSessionPatch(session: Session, patch: SessionPatch): Session {
  return {
    id: session.id,
    title: patch.title ?? session.title,
    workspace: session.workspace,
    createdAt: session.createdAt,
    updatedAt: patch.updatedAt ?? session.updatedAt,
    leased: patch.leased ?? session.leased,
    busy: patch.busy ?? session.busy,
    model: patch.model ?? session.model,
    reasoningEffort: patch.reasoningEffort ?? session.reasoningEffort,
    // Curation is not part of a session patch: it moves by its own request, and
    // a frame that says nothing about it must not clear it.
    preview: session.preview,
    messageCount: session.messageCount,
    archived: session.archived,
    archivedOnDesk: session.archivedOnDesk,
    pinned: session.pinned,
    turn: session.turn,
    queue: session.queue,
  };
}

/* -------------------------------------------------------------------- rows */

/**
 * How many consecutive tool calls make a run worth a header.
 *
 * Two calls are a pair a reader scans without help; a dozen are a wall they have
 * to scroll past to reach the answer, and the header is what turns that wall into
 * one line they can fold.
 */
export const TOOL_RUN_MIN = 3;

/**
 * One row of the rendered conversation.
 *
 * Rows are the view's model, not a DOM description: which items exist, which of
 * them are folded away, and what a group of them amounts to. The data objects are
 * reused across calls whenever nothing about them changed, and that identity is
 * the whole update mechanism — see `rows.ts`.
 */
export type FeedRow = { readonly key: string } & (
  | { readonly kind: "message"; readonly item: FeedMessageItem }
  | {
      readonly kind: "tool";
      readonly item: FeedToolItem;
      /** The run this call belongs to is folded shut. */
      readonly collapsed: boolean;
      /** Session root, for the paths in the card's headline. */
      readonly workspace: string;
    }
  | {
      readonly kind: "toolrun";
      readonly runKey: string;
      readonly count: number;
      /** Calls the harness reported as failed. */
      readonly failed: number;
      /** Calls that exited non-zero, which DSH reports rather than erroring. */
      readonly nonzero: number;
      readonly collapsed: boolean;
      readonly running: boolean;
      readonly startedAt: string | null;
      readonly endedAt: string | null;
    }
  | { readonly kind: "notice"; readonly item: FeedNoticeItem }
);

export interface FeedRowOptions {
  /** Session root, for shortening the paths in tool headlines. */
  readonly workspace: string;
  /** Run keys the reader has folded shut. */
  readonly collapsedRuns: ReadonlySet<string>;
}

/**
 * Turns the folded feed into rows, grouping consecutive tool calls.
 *
 * Pure, and cheap to call on every frame: it walks the feed once and reuses the
 * previous row object wherever the inputs are unchanged, which is what lets the
 * view update exactly the rows that moved. Passing the previous rows back in is
 * therefore not an optimisation the caller may skip — it is what keeps a running
 * card's reader-chosen state and a long turn's scroll position intact.
 */
export function feedRows(
  feed: readonly FeedItem[],
  previous: readonly FeedRow[],
  options: FeedRowOptions,
): readonly FeedRow[] {
  const known = new Map<string, FeedRow>();
  for (const row of previous) known.set(row.key, row);

  const rows: FeedRow[] = [];
  let index = 0;
  while (index < feed.length) {
    const item = feed[index];
    if (item === undefined) {
      index += 1;
      continue;
    }
    if (item.kind !== "tool") {
      rows.push(item.kind === "message" ? messageRow(item, known) : noticeRow(item, known));
      index += 1;
      continue;
    }

    // A run: every consecutive tool call, which is how the agent works — several
    // calls between two things it has to say.
    let end = index;
    while (end < feed.length && feed[end]?.kind === "tool") end += 1;
    const run: FeedToolItem[] = [];
    for (let at = index; at < end; at += 1) {
      const tool = feed[at];
      if (tool !== undefined && tool.kind === "tool") run.push(tool);
    }

    const first = run[0];
    const runKey = first === undefined ? "run:" : `run:${first.key}`;
    const collapsed = options.collapsedRuns.has(runKey);
    if (run.length >= TOOL_RUN_MIN) rows.push(runRow(runKey, run, collapsed, known));
    for (const tool of run) rows.push(toolRow(tool, collapsed, options.workspace, known));
    index = end;
  }
  return rows;
}

function messageRow(item: FeedMessageItem, previous: ReadonlyMap<string, FeedRow>): FeedRow {
  const cached = previous.get(item.key);
  if (cached !== undefined && cached.kind === "message" && cached.item === item) return cached;
  return { key: item.key, kind: "message", item };
}

function noticeRow(item: FeedNoticeItem, previous: ReadonlyMap<string, FeedRow>): FeedRow {
  const cached = previous.get(item.key);
  if (cached !== undefined && cached.kind === "notice" && cached.item === item) return cached;
  return { key: item.key, kind: "notice", item };
}

function toolRow(
  item: FeedToolItem,
  collapsed: boolean,
  workspace: string,
  previous: ReadonlyMap<string, FeedRow>,
): FeedRow {
  const cached = previous.get(item.key);
  if (
    cached !== undefined &&
    cached.kind === "tool" &&
    cached.item === item &&
    cached.collapsed === collapsed &&
    cached.workspace === workspace
  ) {
    return cached;
  }
  return { key: item.key, kind: "tool", item, collapsed, workspace };
}

function runRow(
  runKey: string,
  run: readonly FeedToolItem[],
  collapsed: boolean,
  previous: ReadonlyMap<string, FeedRow>,
): FeedRow {
  let failed = 0;
  let nonzero = 0;
  let running = false;
  let endedAt: string | null = null;
  for (const item of run) {
    if (item.isError) failed += 1;
    else if (item.exitCode !== null && item.exitCode !== 0) nonzero += 1;
    if (item.open) running = true;
    // The run is over only when its last call is: a missing end means it is
    // still going, and a duration counted to "now" would be a different number
    // every time the row was rebuilt.
    endedAt = item.endedAt;
  }
  const first = run[0];
  const startedAt = first?.time ?? null;

  const key = runKey;
  const cached = previous.get(key);
  if (
    cached !== undefined &&
    cached.kind === "toolrun" &&
    cached.count === run.length &&
    cached.failed === failed &&
    cached.nonzero === nonzero &&
    cached.collapsed === collapsed &&
    cached.running === running &&
    cached.startedAt === startedAt &&
    cached.endedAt === endedAt
  ) {
    return cached;
  }
  return {
    key,
    kind: "toolrun",
    runKey,
    count: run.length,
    failed,
    nonzero,
    collapsed,
    running,
    startedAt,
    endedAt: running ? null : endedAt,
  };
}
