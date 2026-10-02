/**
 * Runtime narrowing for everything that crosses the network boundary.
 *
 * The API contract is frozen but the wire is still untrusted input: a proxy
 * could inject a body, an old server could omit a field, a future one could add
 * a frame type. Rather than sprinkle `as Foo` casts (which would make every
 * downstream `?.` a lie), every response and every WebSocket frame passes
 * through a decoder that returns a *fully populated* domain object or `null`.
 *
 * Decoders never throw: a malformed payload is a value the caller handles.
 */

import type {
  Approval,
  ApprovalOption,
  ApprovalResolved,
  Device,
  HarnessStateData,
  HarnessStatus,
  HelloData,
  MessageData,
  ModelOption,
  ModelsResponse,
  Problem,
  ReasoningEffortOption,
  ServerEvent,
  Session,
  SessionListResponse,
  SessionPatch,
  TokenUsage,
  ToolData,
  ToolPhase,
  TranscriptItem,
  TranscriptResponse,
  ReceiptTool,
  SessionReceipt,
  TranscriptRole,
  TrashEntry,
  TranscriptMatch,
  TranscriptSearch,
  TriageCandidate,
  TriageReport,
  TriageVerdict,
  TurnStateData,
  TurnStatus,
  Workspace,
} from "./types.js";

/* ------------------------------------------------------------- primitives */

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** Arrays only; a string is iterable but is never a list on this wire. */
function asArray(value: unknown): readonly unknown[] {
  return Array.isArray(value) ? (value as readonly unknown[]) : [];
}

function asString(value: unknown, fallback: string): string {
  return typeof value === "string" ? value : fallback;
}

function asStringOrNull(value: unknown): string | null {
  return typeof value === "string" ? value : null;
}

function asNumber(value: unknown, fallback: number): number {
  return typeof value === "number" && Number.isFinite(value) ? value : fallback;
}

function asNumberOrNull(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

function asBoolean(value: unknown, fallback: boolean): boolean {
  return typeof value === "boolean" ? value : fallback;
}

/** Narrows to a member of `allowed`, else `fallback`. */
function asEnum<T extends string>(value: unknown, allowed: readonly T[], fallback: T): T {
  return typeof value === "string" && (allowed as readonly string[]).includes(value)
    ? (value as T)
    : fallback;
}

function field(source: Record<string, unknown>, key: string): unknown {
  return source[key];
}

/* ------------------------------------------------------------- decoders */

export function decodeProblem(value: unknown): Problem | null {
  if (!isRecord(value)) return null;
  const code = asString(field(value, "code"), "");
  // `code` is what the client branches on; a problem document without one is
  // not a problem document we can act on.
  if (code === "") return null;
  return {
    type: asString(field(value, "type"), `urn:dsh-gateway:problem:${code}`),
    title: asString(field(value, "title"), "Request failed"),
    status: asNumber(field(value, "status"), 0),
    detail: asStringOrNull(field(value, "detail")),
    instance: asStringOrNull(field(value, "instance")),
    code,
    retryable: asBoolean(field(value, "retryable"), false),
  };
}

export function decodeDevice(value: unknown): Device | null {
  if (!isRecord(value)) return null;
  const id = asStringOrNull(field(value, "id"));
  if (id === null) return null;
  return {
    id,
    name: asString(field(value, "name"), "Unknown device"),
    createdAt: asString(field(value, "createdAt"), ""),
    expiresAt: asString(field(value, "expiresAt"), ""),
  };
}

export function decodeDeviceList(value: unknown): readonly Device[] {
  if (!isRecord(value)) return [];
  return asArray(field(value, "devices")).map(decodeDevice).filter((d): d is Device => d !== null);
}

function decodeWorkspace(value: unknown): Workspace | null {
  if (!isRecord(value)) return null;
  const path = asStringOrNull(field(value, "path"));
  if (path === null) return null;
  return {
    path,
    name: asString(field(value, "name"), path),
    exists: asBoolean(field(value, "exists"), true),
  };
}

export function decodeWorkspaceList(value: unknown): readonly Workspace[] {
  if (!isRecord(value)) return [];
  return asArray(field(value, "workspaces"))
    .map(decodeWorkspace)
    .filter((w): w is Workspace => w !== null);
}

/**
 * One configurable option of the running harness, in the shape the gateway
 * forwards from ACP: an id, and the values it accepts.
 *
 * This is intermediate decoding state, deliberately not part of `types.ts`:
 * the screens want the two named lists below, not ACP's generic catalog, so
 * the mapping lives here rather than in every view.
 */
interface ConfigOption {
  readonly id: string;
  readonly values: readonly ConfigOptionValue[];
}

interface ConfigOptionValue {
  /** Opaque, and the exact string a request must send back to select it. */
  readonly id: string;
  /** The harness's presentation string: "Group · Name" when it groups. */
  readonly label: string;
  readonly group: string;
}

function decodeConfigOption(value: unknown): ConfigOption | null {
  if (!isRecord(value)) return null;
  const id = asStringOrNull(field(value, "id"));
  if (id === null) return null;
  const values = asArray(field(value, "options"))
    .map((raw): ConfigOptionValue | null => {
      if (!isRecord(raw)) return null;
      const valueID = asStringOrNull(field(raw, "id"));
      if (valueID === null) return null;
      // `label` is what the harness wants shown, and it already falls back to
      // the id; using it means the picker reads the same as DSH's own desktop
      // rather than inventing a second presentation.
      return {
        id: valueID,
        label: asString(field(raw, "label"), asString(field(raw, "name"), valueID)),
        group: asString(field(raw, "group"), ""),
      };
    })
    .filter((v): v is ConfigOptionValue => v !== null);
  return { id, values };
}

/**
 * Decodes `GET /models`.
 *
 * The payload is ACP's list of configuration options — not the two named lists
 * this used to assume, which is why the model and reasoning-effort pickers sat
 * empty for so long: the keys never matched, so a server that advertised five
 * models still rendered one "Gateway default" row and nothing complained.
 *
 * Only the two ids the gateway acts on are mapped. An option the harness adds
 * later is ignored rather than guessed into one of these lists.
 *
 * `defaults` is the gateway's own answer to "what happens if the caller omits
 * these", and it is deliberately independent of the catalog: a configured id can
 * name a model the harness no longer offers, which is a warning to act on rather
 * than a reason to hide it. An absent or empty value means the harness decides.
 */
export function decodeModels(value: unknown): ModelsResponse {
  const source = isRecord(value) ? value : {};
  const options = asArray(field(source, "config"))
    .map(decodeConfigOption)
    .filter((option): option is ConfigOption => option !== null);
  const valuesOf = (id: string): readonly ConfigOptionValue[] =>
    options.find((option) => option.id === id)?.values ?? [];

  const models: readonly ModelOption[] = valuesOf("model").map((v) => ({
    id: v.id,
    name: v.label,
    provider: v.group,
  }));
  const reasoningEfforts: readonly ReasoningEffortOption[] = valuesOf("reasoning_effort").map((v) => ({
    id: v.id,
    name: v.label,
  }));

  const defaults = isRecord(field(source, "defaults")) ? (field(source, "defaults") as Record<string, unknown>) : {};
  const defaultID = (key: string): string | null => {
    const id = asString(field(defaults, key), "");
    return id === "" ? null : id;
  };

  return {
    models,
    reasoningEfforts,
    defaults: { model: defaultID("model"), reasoningEffort: defaultID("reasoningEffort") },
  };
}

export function decodeSession(value: unknown): Session | null {
  if (!isRecord(value)) return null;
  const id = asStringOrNull(field(value, "id"));
  if (id === null) return null;
  return {
    id,
    title: asString(field(value, "title"), ""),
    workspace: asString(field(value, "workspace"), ""),
    createdAt: asString(field(value, "createdAt"), ""),
    updatedAt: asString(field(value, "updatedAt"), ""),
    leased: asBoolean(field(value, "leased"), false),
    busy: asBoolean(field(value, "busy"), false),
    model: asStringOrNull(field(value, "model")),
    reasoningEffort: asStringOrNull(field(value, "reasoningEffort")),
    preview: asStringOrNull(field(value, "preview")),
    messageCount: asNumber(field(value, "messageCount"), 0),
    archived: asBoolean(field(value, "archived"), false),
    archivedOnDesk: asBoolean(field(value, "archivedOnDesk"), false),
    pinned: asBoolean(field(value, "pinned"), false),
  };
}

/**
 * Decodes a session receipt.
 *
 * Every field has a default: a receipt is a summary, and a gateway that predates
 * one of these numbers should make the panel show less rather than throw.
 */
export function decodeReceipt(value: unknown): SessionReceipt {
  const source = isRecord(value) ? value : {};
  const cost = field(source, "cost");
  const tools = asArray(field(source, "tools"))
    .map((entry): ReceiptTool | null => {
      if (!isRecord(entry)) return null;
      const name = asStringOrNull(field(entry, "name"));
      if (name === null) return null;
      return {
        name,
        calls: asNumber(field(entry, "calls"), 0),
        failed: asNumber(field(entry, "failed"), 0),
      };
    })
    .filter((entry): entry is ReceiptTool => entry !== null);

  const receipt: SessionReceipt = {
    id: asString(field(source, "id"), ""),
    title: asString(field(source, "title"), ""),
    workspace: asString(field(source, "workspace"), ""),
    model: asString(field(source, "model"), ""),
    reasoningEffort: asString(field(source, "reasoningEffort"), ""),
    messages: asNumber(field(source, "messages"), 0),
    turns: asNumber(field(source, "turns"), 0),
    tools,
    files: asArray(field(source, "files")).filter((entry): entry is string => typeof entry === "string"),
    inputTokens: asNumber(field(source, "inputTokens"), 0),
    outputTokens: asNumber(field(source, "outputTokens"), 0),
    cacheReadTokens: asNumber(field(source, "cacheReadTokens"), 0),
    contextTokens: asNumber(field(source, "contextTokens"), 0),
    startedAt: asString(field(source, "startedAt"), ""),
    endedAt: asString(field(source, "endedAt"), ""),
    spanSeconds: asNumber(field(source, "spanSeconds"), 0),
    activeSeconds: asNumber(field(source, "activeSeconds"), 0),
    ...(isRecord(cost)
      ? {
          cost: {
            currency: asString(field(cost, "currency"), ""),
            total: asNumber(field(cost, "total"), 0),
            input: asNumber(field(cost, "input"), 0),
            output: asNumber(field(cost, "output"), 0),
            cacheRead: asNumber(field(cost, "cacheRead"), 0),
          },
        }
      : {}),
  };
  return receipt;
}

/** Decodes a transcript search. */
export function decodeTranscriptSearch(value: unknown): TranscriptSearch {
  const source = isRecord(value) ? value : {};
  const matches = asArray(field(source, "matches"))
    .map((entry): TranscriptMatch | null => {
      if (!isRecord(entry)) return null;
      const sessionId = asStringOrNull(field(entry, "sessionId"));
      if (sessionId === null) return null;
      return {
        sessionId,
        title: asString(field(entry, "title"), ""),
        workspace: asString(field(entry, "workspace"), ""),
        seq: asNumber(field(entry, "seq"), 0),
        role: asString(field(entry, "role"), ""),
        tool: asString(field(entry, "tool"), ""),
        snippet: asString(field(entry, "snippet"), ""),
        time: asString(field(entry, "time"), ""),
        archived: field(entry, "archived") === true,
      };
    })
    .filter((entry): entry is TranscriptMatch => entry !== null);
  return {
    query: asString(field(source, "query"), ""),
    scanned: asNumber(field(source, "scanned"), 0),
    matches,
    truncated: field(source, "truncated") === true,
  };
}

/** Decodes a trash listing. */
export function decodeTrashList(value: unknown): readonly TrashEntry[] {
  const source = isRecord(value) ? value : {};
  return asArray(field(source, "trash"))
    .map((entry): TrashEntry | null => {
      if (!isRecord(entry)) return null;
      const id = asStringOrNull(field(entry, "id"));
      if (id === null) return null;
      return {
        id,
        title: asString(field(entry, "title"), ""),
        preview: asString(field(entry, "preview"), ""),
        deletedAt: asString(field(entry, "deletedAt"), ""),
      };
    })
    .filter((entry): entry is TrashEntry => entry !== null);
}

const TRIAGE_VERDICTS: readonly TriageVerdict[] = ["test", "temp", "draft"];

function decodeTriageCandidate(value: unknown): TriageCandidate | null {
  if (!isRecord(value)) return null;
  const id = asStringOrNull(field(value, "id"));
  if (id === null) return null;
  return {
    id,
    title: asString(field(value, "title"), ""),
    preview: asString(field(value, "preview"), ""),
    workspace: asString(field(value, "workspace"), ""),
    messages: asNumber(field(value, "messages"), 0),
    verdict: asEnum(field(value, "verdict"), TRIAGE_VERDICTS, "test"),
    reason: asString(field(value, "reason"), ""),
  };
}

/** Decodes `GET /sessions/triage`. */
export function decodeTriage(value: unknown): TriageReport {
  const source = isRecord(value) ? value : {};
  const summary = isRecord(field(source, "summary")) ? (field(source, "summary") as Record<string, unknown>) : {};
  return {
    summary: {
      scanned: asNumber(field(summary, "scanned"), 0),
      archived: asNumber(field(summary, "archived"), 0),
      candidates: asNumber(field(summary, "candidates"), 0),
      test: asNumber(field(summary, "test"), 0),
      temp: asNumber(field(summary, "temp"), 0),
      draft: asNumber(field(summary, "draft"), 0),
    },
    candidates: asArray(field(source, "candidates"))
      .map(decodeTriageCandidate)
      .filter((candidate): candidate is TriageCandidate => candidate !== null),
  };
}

export function decodeSessionList(value: unknown): SessionListResponse {
  const source = isRecord(value) ? value : {};
  return {
    sessions: asArray(field(source, "sessions"))
      .map(decodeSession)
      .filter((s): s is Session => s !== null),
    nextCursor: asStringOrNull(field(source, "nextCursor")),
  };
}

function decodeUsage(value: unknown): TokenUsage | null {
  if (!isRecord(value)) return null;
  const inputTokens = asNumberOrNull(field(value, "inputTokens"));
  const outputTokens = asNumberOrNull(field(value, "outputTokens"));
  if (inputTokens === null && outputTokens === null) return null;
  return {
    inputTokens: inputTokens ?? 0,
    outputTokens: outputTokens ?? 0,
    totalTokens: asNumberOrNull(field(value, "totalTokens")),
    contextWindow: asNumberOrNull(field(value, "contextWindow")),
  };
}

const TRANSCRIPT_ROLES: readonly TranscriptRole[] = ["user", "assistant", "tool", "notice"];

function decodeTranscriptItem(value: unknown): TranscriptItem | null {
  if (!isRecord(value)) return null;
  const role = asEnum(field(value, "role"), TRANSCRIPT_ROLES, "notice");
  // A missing id would break the append-only list's keying; synthesise one from
  // the sequence number, which the contract guarantees is present.
  const seq = asNumber(field(value, "seq"), 0);
  const id = asStringOrNull(field(value, "id")) ?? `seq-${seq}-${role}`;
  return {
    id,
    seq,
    time: asString(field(value, "time"), ""),
    role,
    text: asStringOrNull(field(value, "text")),
    thinking: asStringOrNull(field(value, "thinking")),
    model: asStringOrNull(field(value, "model")),
    usage: decodeUsage(field(value, "usage")),
    tool: asStringOrNull(field(value, "tool")),
    toolInput: asStringOrNull(field(value, "input")),
    toolOutput: asStringOrNull(field(value, "output")),
    isError: asBoolean(field(value, "isError"), false),
  };
}

export function decodeTranscript(value: unknown): TranscriptResponse {
  const source = isRecord(value) ? value : {};
  return {
    items: asArray(field(source, "items"))
      .map(decodeTranscriptItem)
      .filter((i): i is TranscriptItem => i !== null),
    nextBefore: asNumberOrNull(field(source, "nextBefore")),
    truncated: asBoolean(field(source, "truncated"), false),
    unsupported: asBoolean(field(source, "unsupported"), false),
  };
}

function decodeApprovalOption(value: unknown): ApprovalOption | null {
  if (!isRecord(value)) return null;
  const id = asStringOrNull(field(value, "id"));
  if (id === null) return null;
  return { id, name: asString(field(value, "name"), id) };
}

function decodeApproval(value: unknown): Approval | null {
  if (!isRecord(value)) return null;
  const id = asStringOrNull(field(value, "id"));
  const sessionId = asStringOrNull(field(value, "sessionId"));
  if (id === null || sessionId === null) return null;
  return {
    id,
    sessionId,
    toolCallId: asString(field(value, "toolCallId"), ""),
    tool: asString(field(value, "tool"), "unknown tool"),
    input: asString(field(value, "input"), ""),
    requestedAt: asString(field(value, "requestedAt"), ""),
    expiresAt: asString(field(value, "expiresAt"), ""),
    options: asArray(field(value, "options"))
      .map(decodeApprovalOption)
      .filter((o): o is ApprovalOption => o !== null),
  };
}

export function decodeApprovalList(value: unknown): readonly Approval[] {
  if (!isRecord(value)) return [];
  return asArray(field(value, "approvals"))
    .map(decodeApproval)
    .filter((a): a is Approval => a !== null);
}

/* --------------------------------------------------------------- events */

function decodeHello(value: unknown): HelloData {
  const source = isRecord(value) ? value : {};
  return {
    deviceId: asString(field(source, "deviceId"), ""),
    replayFrom: asNumberOrNull(field(source, "replayFrom")),
  };
}

function decodeSessionPatch(value: unknown): SessionPatch {
  const source = isRecord(value) ? value : {};
  return {
    title: asStringOrNull(field(source, "title")),
    leased: typeof field(source, "leased") === "boolean" ? asBoolean(field(source, "leased"), false) : null,
    busy: typeof field(source, "busy") === "boolean" ? asBoolean(field(source, "busy"), false) : null,
    model: asStringOrNull(field(source, "model")),
    reasoningEffort: asStringOrNull(field(source, "reasoningEffort")),
    updatedAt: asStringOrNull(field(source, "updatedAt")),
  };
}

const MESSAGE_ROLES: readonly ("user" | "assistant")[] = ["user", "assistant"];

function decodeMessage(value: unknown): MessageData {
  const source = isRecord(value) ? value : {};
  return {
    id: asStringOrNull(field(source, "id")),
    // The gateway has always sent a role; a client that ignored it could only
    // ever render half of what a followed session produces.
    role: asEnum(field(source, "role"), MESSAGE_ROLES, "assistant"),
    text: asString(field(source, "text"), ""),
    thinking: asStringOrNull(field(source, "thinking")),
    model: asStringOrNull(field(source, "model")),
    usage: decodeUsage(field(source, "usage")),
  };
}

const TOOL_PHASES: readonly ToolPhase[] = ["start", "end"];

function decodeTool(value: unknown): ToolData {
  const source = isRecord(value) ? value : {};
  return {
    phase: asEnum(field(source, "phase"), TOOL_PHASES, "end"),
    tool: asString(field(source, "tool"), "unknown tool"),
    callId: asString(field(source, "callId"), ""),
    input: asStringOrNull(field(source, "input")),
    output: asStringOrNull(field(source, "output")),
    isError: asBoolean(field(source, "isError"), false),
  };
}

const TURN_STATUSES: readonly TurnStatus[] = ["running", "completed", "cancelled", "failed"];

function decodeTurnState(value: unknown): TurnStateData {
  const source = isRecord(value) ? value : {};
  return {
    turnId: asString(field(source, "turnId"), ""),
    state: asEnum(field(source, "state"), TURN_STATUSES, "failed"),
    stopReason: asStringOrNull(field(source, "stopReason")),
  };
}

const HARNESS_STATUSES: readonly HarnessStatus[] = ["starting", "ready", "restarting", "failed"];

function decodeHarnessState(value: unknown): HarnessStateData {
  const source = isRecord(value) ? value : {};
  return {
    state: asEnum(field(source, "state"), HARNESS_STATUSES, "failed"),
    detail: asStringOrNull(field(source, "detail")),
  };
}

/**
 * Decodes one server frame. Returns `null` for anything unrecognised so the
 * caller can ignore a frame type it does not know about yet — a newer server
 * must never crash an older client.
 */
export function decodeServerEvent(value: unknown): ServerEvent | null {
  if (!isRecord(value)) return null;
  const type = asStringOrNull(field(value, "type"));
  if (type === null) return null;

  // `hello` is the one control frame, and it carries no sequence number: it
  // reports where the server resumed from, which is a statement about the
  // connection rather than a position in the stream. It is therefore decoded
  // before the guard below — that guard is what made this frame invisible to
  // every client, which is how a phone that reconnected without a resume point
  // kept showing a conversation the server had already moved past.
  if (type === "hello") {
    return {
      seq: -1,
      time: asString(field(value, "time"), ""),
      sessionId: null,
      type,
      data: decodeHello(field(value, "data")),
    };
  }

  const seq = asNumber(field(value, "seq"), -1);
  // `seq` drives reconnect replay. A frame without one cannot be replayed
  // correctly, so treating it as authoritative would risk skipping events.
  if (seq < 0) return null;

  const envelope = {
    seq,
    time: asString(field(value, "time"), ""),
    sessionId: asStringOrNull(field(value, "sessionId")),
  };
  const data = field(value, "data");

  switch (type) {
    case "hello":
      return { ...envelope, type, data: decodeHello(data) };
    case "session.state":
      return { ...envelope, type, data: decodeSessionPatch(data) };
    case "session.message":
      return { ...envelope, type, data: decodeMessage(data) };
    case "session.tool":
      return { ...envelope, type, data: decodeTool(data) };
    case "usage.update": {
      const usage = decodeUsage(data);
      return usage === null ? null : { ...envelope, type, data: usage };
    }
    case "approval.requested": {
      const approval = decodeApproval(data);
      return approval === null ? null : { ...envelope, type, data: approval };
    }
    case "approval.resolved": {
      const source = isRecord(data) ? data : {};
      const id = asStringOrNull(field(source, "id"));
      if (id === null) return null;
      const resolved: ApprovalResolved = {
        id,
        optionId: asString(field(source, "optionId"), ""),
        decidedBy: asStringOrNull(field(source, "decidedBy")),
      };
      return { ...envelope, type, data: resolved };
    }
    case "turn.state":
      return { ...envelope, type, data: decodeTurnState(data) };
    case "harness.state":
      return { ...envelope, type, data: decodeHarnessState(data) };
    case "resync":
      return { ...envelope, type, data: null };
    default:
      return null;
  }
}
