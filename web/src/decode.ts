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
  ApprovalGrant,
  ApprovalGranted,
  ChangedFile,
  ChangeHunk,
  ChangesSummary,
  DeploymentLimits,
  Features,
  Principal,
  RevertReport,
  RevertResult,
  SessionChanges,
  Turn,
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
  SessionTurnSnapshot,
  SessionListResponse,
  SessionPatch,
  SnapshotData,
  ThoughtData,
  TokenUsage,
  ToolData,
  ToolPhase,
  ToolResultFacts,
  ToolTrim,
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

const TURN_STATES: readonly TurnStatus[] = ["queued", "running", "completed", "cancelled", "failed"];

/**
 * Decodes one admitted prompt.
 *
 * `position` and `startedAt` are absent on the wire depending on the state, and
 * a client that guessed them would show a queued prompt as running or a running
 * one as first in a queue that does not exist.
 */
export function decodeTurn(value: unknown): Turn | null {
  if (!isRecord(value)) return null;
  const turnId = asStringOrNull(field(value, "turnId"));
  if (turnId === null) return null;
  return {
    turnId,
    state: asEnum(field(value, "state"), TURN_STATES, "failed"),
    position: asNumber(field(value, "position"), 0),
    queuedAt: asString(field(value, "queuedAt"), ""),
    startedAt: asStringOrNull(field(value, "startedAt")),
    // Absent on the session resource, where a turn is running and has not
    // stopped; present on the event that reports it settling.
    stopReason: asStringOrNull(field(value, "stopReason")),
    detail: asStringOrNull(field(value, "detail")),
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
    turn: decodeTurn(field(value, "turn")),
    queue: asArray(field(value, "queue"))
      .map(decodeTurn)
      .filter((t): t is Turn => t !== null),
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
        field: asString(field(entry, "field"), ""),
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

/**
 * Reads the facts a recorded result carries.
 *
 * They are shared verbatim by a transcript item and a `session.tool` frame —
 * `toolresult.Facts` on the server is one definition — so they are read here
 * once rather than twice.
 */
function decodeResultFacts(source: Record<string, unknown>): ToolResultFacts & ToolTrim {
  return {
    // A missing exit code is silence, not zero: the contract omits the field
    // when the result never said, and zero is a real status that means success.
    exitCode: asNumberOrNull(field(source, "exitCode")),
    errorName: asStringOrNull(field(source, "errorName")),
    errorCode: asStringOrNull(field(source, "errorCode")),
    notices: asArray(field(source, "notices")).filter((item): item is string => typeof item === "string"),
    harnessTruncated: asBoolean(field(source, "harnessTruncated"), false),
    spillPath: asStringOrNull(field(source, "spillPath")),
    inputTruncated: asBoolean(field(source, "inputTruncated"), false),
    outputTruncated: asBoolean(field(source, "outputTruncated"), false),
  };
}

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
    // A call the log holds without a result is running right now.
    pending: asBoolean(field(value, "pending"), false),
    endedAt: asStringOrNull(field(value, "endedAt")),
    attachments: asNumber(field(value, "attachments"), 0),
    ...decodeResultFacts(value),
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
  return {
    id,
    name: asString(field(value, "name"), id),
    grant: asBoolean(field(value, "grant"), false),
  };
}

/** Decodes a standing authorisation. */
export function decodeGrant(value: unknown): ApprovalGrant | null {
  if (!isRecord(value)) return null;
  const id = asStringOrNull(field(value, "id"));
  const sessionId = asStringOrNull(field(value, "sessionId"));
  if (id === null || sessionId === null) return null;
  return {
    id,
    sessionId,
    tool: asString(field(value, "tool"), ""),
    scope: asString(field(value, "scope"), "tool"),
    summary: asString(field(value, "summary"), ""),
    createdAt: asString(field(value, "createdAt"), ""),
    expiresAt: asString(field(value, "expiresAt"), ""),
    uses: asNumber(field(value, "uses"), 0),
  };
}

/** Decodes the standing authorisations currently in force. */
export function decodeGrantList(value: unknown): readonly ApprovalGrant[] {
  if (!isRecord(value)) return [];
  return asArray(field(value, "grants"))
    .map(decodeGrant)
    .filter((g): g is ApprovalGrant => g !== null);
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

/* --------------------------------------------------------------- changes */

function decodeHunk(value: unknown): ChangeHunk | null {
  if (!isRecord(value)) return null;
  return {
    seq: asNumber(field(value, "seq"), 0),
    callId: asString(field(value, "callId"), ""),
    tool: asString(field(value, "tool"), ""),
    lines: asArray(field(value, "lines")).filter((line): line is string => typeof line === "string"),
    added: asNumber(field(value, "added"), 0),
    deleted: asNumber(field(value, "deleted"), 0),
    wholeFile: asBoolean(field(value, "wholeFile"), false),
    truncated: asBoolean(field(value, "truncated"), false),
  };
}

function decodeChangedFile(value: unknown): ChangedFile | null {
  if (!isRecord(value)) return null;
  const path = asStringOrNull(field(value, "path"));
  if (path === null) return null;
  return {
    path,
    display: asString(field(value, "display"), path),
    added: asNumber(field(value, "added"), 0),
    deleted: asNumber(field(value, "deleted"), 0),
    edits: asNumber(field(value, "edits"), 0),
    writes: asNumber(field(value, "writes"), 0),
    binary: asBoolean(field(value, "binary"), false),
    hunks: asArray(field(value, "hunks"))
      .map(decodeHunk)
      .filter((h): h is ChangeHunk => h !== null),
    truncated: asBoolean(field(value, "truncated"), false),
    revertible: asBoolean(field(value, "revertible"), false),
    reason: asString(field(value, "reason"), ""),
  };
}

/** Decodes what a session changed. */
export function decodeChanges(value: unknown): SessionChanges {
  const source = isRecord(value) ? value : {};
  const raw = isRecord(field(source, "summary")) ? (field(source, "summary") as Record<string, unknown>) : {};
  const summary: ChangesSummary = {
    files: asNumber(field(raw, "files"), 0),
    total: asNumber(field(raw, "total"), 0),
    added: asNumber(field(raw, "added"), 0),
    deleted: asNumber(field(raw, "deleted"), 0),
    edits: asNumber(field(raw, "edits"), 0),
    source: asString(field(raw, "source"), ""),
    truncated: asBoolean(field(raw, "truncated"), false),
  };
  return {
    files: asArray(field(source, "files"))
      .map(decodeChangedFile)
      .filter((f): f is ChangedFile => f !== null),
    summary,
    revertEnabled: asBoolean(field(source, "revertEnabled"), false),
    unsupported: asBoolean(field(source, "unsupported"), false),
    detail: asString(field(source, "detail"), ""),
  };
}

/** Decodes the per-file report an undo answers with. */
export function decodeRevertReport(value: unknown): RevertReport {
  const source = isRecord(value) ? value : {};
  const files = asArray(field(source, "files"))
    .map((entry): RevertResult | null => {
      if (!isRecord(entry)) return null;
      const path = asStringOrNull(field(entry, "path"));
      if (path === null) return null;
      return {
        path,
        display: asString(field(entry, "display"), path),
        status: asEnum(field(entry, "status"), ["reverted", "skipped", "refused"] as const, "refused"),
        replacements: asNumber(field(entry, "replacements"), 0),
        reason: asString(field(entry, "reason"), ""),
      };
    })
    .filter((f): f is RevertResult => f !== null);
  return {
    files,
    reverted: asNumber(field(source, "reverted"), 0),
    refused: asNumber(field(source, "refused"), 0),
    skipped: asNumber(field(source, "skipped"), 0),
  };
}

/* ------------------------------------------------------------ deployment */

const NOTHING: Principal = {
  device: { id: "", name: "", createdAt: "", expiresAt: "" },
  features: {
    transcript: false,
    desktopUI: false,
    imagePrompts: false,
    approvalTimeoutSecs: 0,
    sessionIdleTimeoutSec: 0,
    approvalGrantTTLSecs: 0,
    promptQueueDepth: 0,
    revertEnabled: false,
  },
  limits: { maxPromptBytes: 0, maxBodyBytes: 0, maxImageBytes: 0, transcriptPage: 0 },
};

/**
 * Decodes `GET /me`.
 *
 * Every optional surface defaults to off. A client that assumed a capability
 * because the field was missing would offer a control that fails, which is
 * exactly the failure this payload exists to prevent.
 */
export function decodePrincipal(value: unknown): Principal {
  if (!isRecord(value)) return NOTHING;
  const device = decodeDevice(value);
  if (device === null) return NOTHING;

  const rawFeatures = isRecord(field(value, "features")) ? (field(value, "features") as Record<string, unknown>) : {};
  const rawLimits = isRecord(field(value, "limits")) ? (field(value, "limits") as Record<string, unknown>) : {};
  const features: Features = {
    transcript: asBoolean(field(rawFeatures, "transcript"), false),
    desktopUI: asBoolean(field(rawFeatures, "desktopUI"), false),
    imagePrompts: asBoolean(field(rawFeatures, "imagePrompts"), false),
    approvalTimeoutSecs: asNumber(field(rawFeatures, "approvalTimeoutSecs"), 0),
    sessionIdleTimeoutSec: asNumber(field(rawFeatures, "sessionIdleTimeoutSec"), 0),
    approvalGrantTTLSecs: asNumber(field(rawFeatures, "approvalGrantTTLSecs"), 0),
    promptQueueDepth: asNumber(field(rawFeatures, "promptQueueDepth"), 0),
    revertEnabled: asBoolean(field(rawFeatures, "revertEnabled"), false),
  };
  const limits: DeploymentLimits = {
    maxPromptBytes: asNumber(field(rawLimits, "maxPromptBytes"), 0),
    maxBodyBytes: asNumber(field(rawLimits, "maxBodyBytes"), 0),
    maxImageBytes: asNumber(field(rawLimits, "maxImageBytes"), 0),
    transcriptPage: asNumber(field(rawLimits, "transcriptPage"), 0),
  };
  return { device, features, limits };
}

/* --------------------------------------------------------------- events */

function decodeHello(value: unknown): HelloData {
  const source = isRecord(value) ? value : {};
  return {
    deviceId: asString(field(source, "deviceId"), ""),
    generation: asString(field(source, "generation"), ""),
    replayFrom: asNumberOrNull(field(source, "replayFrom")),
  };
}

/**
 * Decodes a snapshot. Every part of it is optional in the sense that a missing
 * one degrades to "nothing to say" rather than to a broken frame: an older
 * server that sends no `turns` must not stop a client rendering the harness
 * state it did send.
 */
function decodeSnapshot(value: unknown): SnapshotData {
  const source = isRecord(value) ? value : {};
  const turns: SessionTurnSnapshot[] = [];
  for (const entry of asArray(field(source, "turns"))) {
    if (!isRecord(entry)) continue;
    const sessionId = asStringOrNull(field(entry, "sessionId"));
    if (sessionId === null) continue;
    const running = field(entry, "running");
    turns.push({
      sessionId,
      running: running === undefined || running === null ? null : decodeTurnState(running),
      queued: asArray(field(entry, "queued")).map(decodeTurnState),
    });
  }
  return {
    generation: asString(field(source, "generation"), ""),
    seq: asNumber(field(source, "seq"), 0),
    time: asString(field(source, "time"), ""),
    harness: decodeHarnessState(field(source, "harness")),
    turns,
    approvals: asArray(field(source, "approvals"))
      .map(decodeApproval)
      .filter((a): a is Approval => a !== null),
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
    // A prompt that was only a screenshot has no text at all; the count is what
    // keeps its row from rendering as an empty bubble.
    attachments: asNumber(field(source, "attachments"), 0),
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
    ...decodeResultFacts(source),
  };
}

function decodeTurnState(value: unknown): TurnStateData {
  const turn = decodeTurn(value);
  const source = isRecord(value) ? value : {};
  return {
    turnId: turn?.turnId ?? "",
    state: turn?.state ?? "failed",
    position: turn?.position ?? 0,
    queuedAt: turn?.queuedAt ?? "",
    startedAt: turn?.startedAt ?? null,
    stopReason: turn?.stopReason ?? null,
    detail: turn?.detail ?? null,
    queueDepth: asNumberOrNull(field(source, "queueDepth")),
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
        tool: asString(field(source, "tool"), ""),
        sessionId: asString(field(source, "sessionId"), ""),
        grantId: asStringOrNull(field(source, "grantId")),
      };
      return { ...envelope, type, data: resolved };
    }
    case "approval.granted": {
      const source = isRecord(data) ? data : {};
      const grant = decodeGrant(field(source, "grant"));
      if (grant === null) return null;
      const granted: ApprovalGranted = {
        grant,
        tool: asString(field(source, "tool"), ""),
        input: asString(field(source, "input"), ""),
      };
      return { ...envelope, type, data: granted };
    }
    case "turn.state":
      return { ...envelope, type, data: decodeTurnState(data) };
    case "harness.state":
      return { ...envelope, type, data: decodeHarnessState(data) };
    case "session.thought": {
      const source = isRecord(data) ? data : {};
      const thought: ThoughtData = {
        id: asStringOrNull(field(source, "id")),
        text: asString(field(source, "text"), ""),
      };
      return { ...envelope, type, data: thought };
    }
    case "snapshot":
      return { ...envelope, type, data: decodeSnapshot(data) };
    case "gateway.draining": {
      const source = isRecord(data) ? data : {};
      return { ...envelope, type, data: { reason: asString(field(source, "reason"), "") } };
    }
    case "resync": {
      // The reason is for the operator, not the state machine: every resync
      // means the same thing to the client.
      const source = isRecord(data) ? data : {};
      return { ...envelope, type, data: { reason: asString(field(source, "reason"), "") } };
    }
    default:
      return null;
  }
}
