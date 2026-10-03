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

import type { FeedItem, MessageData, Session, SessionPatch, TokenUsage, ToolData, TranscriptItem } from "./types.js";

/** How many trailing rows to scan when pairing a tool end with its start. */
const TOOL_LOOKBACK = 400;

export function feedFromTranscript(items: readonly TranscriptItem[]): readonly FeedItem[] {
  const feed: FeedItem[] = [];
  for (const item of items) {
    switch (item.role) {
      case "user":
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
          tool: item.tool ?? "tool",
          callId: item.id,
          input: item.toolInput,
          output: item.toolOutput,
          open: false,
          isError: item.isError,
        });
        break;
      case "notice":
        feed.push({
          key: `hist-notice:${item.id}`,
          kind: "notice",
          time: item.time,
          text: item.text ?? "",
        });
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
    next[existing] = { ...current, input: data.input ?? current.input, tool: data.tool, open: true };
    return next;
  }
  return [
    ...feed,
    {
      key: `tool:${data.callId === "" ? seq : data.callId}`,
      kind: "tool",
      time,
      tool: data.tool,
      callId: data.callId === "" ? `seq-${seq}` : data.callId,
      input: data.input,
      output: null,
      open: true,
      isError: false,
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
      output: data.output ?? current.output,
      isError: data.isError,
      open: false,
      input: data.input ?? current.input,
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
      tool: data.tool,
      callId: data.callId === "" ? `seq-${seq}` : data.callId,
      input: data.input,
      output: data.output,
      open: false,
      isError: data.isError,
    },
  ];
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
 */
export function appendUserMessage(
  feed: readonly FeedItem[],
  text: string,
  time: string,
  attachments = 0,
): readonly FeedItem[] {
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
  if (
    data.id === null &&
    previous !== undefined &&
    previous.kind === "message" &&
    previous.role === data.role &&
    previous.text === data.text
  ) {
    return feed;
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
      attachments: 0,
    },
  ];
}

export function appendNotice(feed: readonly FeedItem[], text: string, time: string, seq: number): readonly FeedItem[] {
  return [...feed, { key: `notice:${seq}`, kind: "notice", time, text }];
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
