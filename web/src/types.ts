/**
 * Hand-written mirror of `docs/api.md` (Gateway API v1).
 *
 * The contract is frozen, so these types are the single place where its shape
 * is written down. Two deliberate conventions:
 *
 * 1. Anything the server may omit is `T | null` rather than `T | undefined`.
 *    With `exactOptionalPropertyTypes` an optional field cannot be assigned an
 *    explicit `undefined`, and decoding untrusted JSON into optionals is a
 *    constant source of friction. `null` means "the server did not say".
 * 2. Wire frames are *not* trusted. `decode.ts` turns `unknown` into these
 *    types; nothing else in the app is allowed to assume a shape.
 */

/* ------------------------------------------------------------------ errors */

/** RFC 9457 problem document. `code` is the stable handle for client logic. */
export interface Problem {
  readonly type: string;
  readonly title: string;
  readonly status: number;
  readonly detail: string | null;
  readonly instance: string | null;
  readonly code: string;
  readonly retryable: boolean;
}

/* ------------------------------------------------------------------ pairing */

export interface Device {
  readonly id: string;
  readonly name: string;
  readonly createdAt: string;
  readonly expiresAt: string;
}

export interface PairRequest {
  readonly code: string;
  readonly deviceName: string;
}

export interface PairResponse {
  readonly device: Device;
  /** Returned exactly once. A browser relies on the cookie instead. */
  readonly token: string;
}

/* ---------------------------------------------------- workspaces and models */

export interface Workspace {
  readonly path: string;
  readonly name: string;
  readonly exists: boolean;
}

export interface ModelOption {
  readonly id: string;
  /** The harness's presentation string, already "Group · Name" when grouped. */
  readonly name: string;
  readonly provider: string;
}

export interface ReasoningEffortOption {
  readonly id: string;
  readonly name: string;
}

/**
 * What the gateway applies to a new session when the caller does not choose.
 *
 * These are value ids from the catalog above (`null` when the operator has not
 * configured one), and a picker preselects them so the sheet shows the session
 * that is actually about to be created rather than an implied default.
 */
export interface ModelsDefaults {
  readonly model: string | null;
  readonly reasoningEffort: string | null;
}

/** The two lists the pickers want, picked out of the harness's option catalog. */
export interface ModelsResponse {
  readonly models: readonly ModelOption[];
  readonly reasoningEfforts: readonly ReasoningEffortOption[];
  readonly defaults: ModelsDefaults;
}

/* ----------------------------------------------------------------- sessions */

export interface Session {
  readonly id: string;
  readonly title: string;
  readonly workspace: string;
  readonly createdAt: string;
  readonly updatedAt: string;
  /** Attached to the gateway's DSH process; the desktop cannot open it. */
  readonly leased: boolean;
  readonly busy: boolean;
  readonly model: string | null;
  readonly reasoningEffort: string | null;
  /**
   * The first thing the operator typed. Most sessions never get a title — DSH
   * names one from the first turn — so this is what a row shows instead of
   * "Untitled session".
   */
  readonly preview: string | null;
  /** Conversation rows the session holds, for "is there anything in here". */
  readonly messageCount: number;
  /** Put away: out of the default list, still searchable, restorable. */
  readonly archived: boolean;
  /** The desktop is the reason it is archived, so the phone cannot undo it alone. */
  readonly archivedOnDesk: boolean;
  readonly pinned: boolean;
}

/* ---------------------------------------------------------- notifications */

/** What a browser needs in order to subscribe, and what it is subscribing to. */
export interface PushKey {
  readonly enabled: boolean;
  readonly publicKey: string;
  /** How long a turn must run before its end is worth a notification. */
  readonly turnThresholdSeconds: number;
  /** The address this gateway should be reached at, for a client on the wrong one. */
  readonly appURL: string;
  /** Where to download the CA root, when this deployment runs its own. */
  readonly caURL: string;
  /**
   * Chat channels that receive the same notifications, named by service.
   *
   * They matter most exactly when Web Push cannot work — Android without Google
   * Play services, or a network that cannot reach Google's push service — which
   * is why the settings screen shows them even when the browser refuses.
   */
  readonly channels: readonly string[];
}

/** One browser's push endpoint and keys, as the Push API hands them out. */
export interface PushSubscriptionRequest {
  readonly endpoint: string;
  readonly p256dh: string;
  readonly auth: string;
}

/** One tool's tally inside a receipt. */
export interface ReceiptTool {
  readonly name: string;
  readonly calls: number;
  readonly failed: number;
}

/** What a session cost, when unit prices are configured. */
export interface ReceiptCost {
  readonly currency: string;
  readonly total: number;
  readonly input: number;
  readonly output: number;
  readonly cacheRead: number;
}

/**
 * What a session actually did, derived from its own log.
 *
 * `activeSeconds` is time the agent was demonstrably working; `spanSeconds` is
 * the session's whole life including idle stretches. Both are here because they
 * answer different questions, and showing only one would mislead.
 */
export interface SessionReceipt {
  readonly id: string;
  readonly title: string;
  readonly workspace: string;
  readonly model: string;
  readonly reasoningEffort: string;
  readonly messages: number;
  readonly turns: number;
  readonly tools: readonly ReceiptTool[];
  readonly files: readonly string[];
  readonly inputTokens: number;
  readonly outputTokens: number;
  readonly cacheReadTokens: number;
  readonly contextTokens: number;
  readonly startedAt: string;
  readonly endedAt: string;
  readonly spanSeconds: number;
  readonly activeSeconds: number;
  readonly cost?: ReceiptCost;
}

/** One place a transcript search found the query. */
export interface TranscriptMatch {
  readonly sessionId: string;
  readonly title: string;
  readonly workspace: string;
  readonly seq: number;
  readonly role: string;
  readonly tool: string;
  readonly snippet: string;
  readonly time: string;
  readonly archived: boolean;
}

/** What a transcript search read, and what it found. */
export interface TranscriptSearch {
  readonly query: string;
  readonly scanned: number;
  readonly matches: readonly TranscriptMatch[];
  /** The scan hit its bound, so there may be more matches than these. */
  readonly truncated: boolean;
}

/** One deleted session, as the trash lists it. */
export interface TrashEntry {
  readonly id: string;
  readonly title: string;
  readonly preview: string;
  readonly deletedAt: string;
}

/* --------------------------------------------------------------- curation */

export type TriageVerdict = "test" | "temp" | "draft";

/** One session the gateway suggests putting away, and why. */
export interface TriageCandidate {
  readonly id: string;
  readonly title: string;
  readonly preview: string;
  readonly workspace: string;
  readonly messages: number;
  readonly verdict: TriageVerdict;
  readonly reason: string;
}

/**
 * What a tidy-up would do, before it does it.
 *
 * `scanned` and `archived` are there so the sheet can be honest about what it
 * did not look at: a summary that says "131 of 251" is a different claim from
 * one that says "131".
 */
export interface TriageSummary {
  readonly scanned: number;
  readonly archived: number;
  readonly candidates: number;
  readonly test: number;
  readonly temp: number;
  readonly draft: number;
}

export interface TriageReport {
  readonly summary: TriageSummary;
  readonly candidates: readonly TriageCandidate[];
}

export interface SessionListResponse {
  readonly sessions: readonly Session[];
  readonly nextCursor: string | null;
}

export interface CreateSessionRequest {
  readonly workspace: string;
  readonly model: string | null;
  readonly reasoningEffort: string | null;
}

/** `PATCH /sessions/{id}` — send only what changes. */
/** Exactly one of archived or pinned is set: a request makes one decision. */
export interface CurationDecision {
  readonly archived?: boolean;
  readonly pinned?: boolean;
}

export interface UpdateSessionRequest {
  /** Curation travels the same request as configuration: one row, one PATCH. */
  readonly archived?: boolean | null;
  readonly pinned?: boolean | null;
  readonly model: string | null;
  readonly reasoningEffort: string | null;
}

export interface TokenUsage {
  readonly inputTokens: number;
  readonly outputTokens: number;
  readonly totalTokens: number | null;
  readonly contextWindow: number | null;
}

export interface PromptBlock {
  readonly type: "text";
  readonly text: string;
}

export interface PromptRequest {
  readonly blocks: readonly PromptBlock[];
}

export interface PromptResponse {
  readonly turnId: string;
}

/* --------------------------------------------------------------- transcript */

export type TranscriptRole = "user" | "assistant" | "tool" | "notice";

export interface TranscriptItem {
  readonly id: string;
  readonly seq: number;
  readonly time: string;
  readonly role: TranscriptRole;
  readonly text: string | null;
  readonly thinking: string | null;
  readonly model: string | null;
  readonly usage: TokenUsage | null;
  /** role === "tool" only. */
  readonly tool: string | null;
  /** role === "tool" only. A JSON-encoded string, per the contract. */
  readonly toolInput: string | null;
  readonly toolOutput: string | null;
  readonly isError: boolean;
}

export interface TranscriptResponse {
  readonly items: readonly TranscriptItem[];
  /** Cursor for the next page backwards; `null` when history is exhausted. */
  readonly nextBefore: number | null;
  readonly truncated: boolean;
  /**
   * The on-disk session-log format is newer than the server understands. The
   * contract makes this a `200` with no items, not an error.
   */
  readonly unsupported: boolean;
}

/* --------------------------------------------------------------- approvals */

export interface ApprovalOption {
  readonly id: string;
  readonly name: string;
}

export interface Approval {
  readonly id: string;
  readonly sessionId: string;
  readonly toolCallId: string;
  readonly tool: string;
  /** Raw JSON-encoded tool input, shown verbatim: the human judges this. */
  readonly input: string;
  readonly requestedAt: string;
  readonly expiresAt: string;
  readonly options: readonly ApprovalOption[];
}

export interface ApprovalDecision {
  readonly optionId: string;
}

export interface ApprovalResolved {
  readonly id: string;
  readonly optionId: string;
  readonly decidedBy: string | null;
}

/* ------------------------------------------------------------------- events */

export interface HelloData {
  readonly deviceId: string;
  readonly replayFrom: number | null;
}

/**
 * `session.state` carries only what changed, so absent fields mean "unchanged"
 * and are represented as `null` rather than a guessed default.
 */
export interface SessionPatch {
  readonly title: string | null;
  readonly leased: boolean | null;
  readonly busy: boolean | null;
  readonly model: string | null;
  readonly reasoningEffort: string | null;
  readonly updatedAt: string | null;
}

export interface MessageData {
  readonly id: string | null;
  /**
   * Who wrote it. Assistant messages come from the agent; a user message arrives
   * when the prompt was typed in another client — the desktop, or a headless
   * run the phone is watching — and the phone still has to show it.
   */
  readonly role: "user" | "assistant";
  readonly text: string;
  readonly thinking: string | null;
  readonly model: string | null;
  readonly usage: TokenUsage | null;
}

export type ToolPhase = "start" | "end";

export interface ToolData {
  readonly phase: ToolPhase;
  readonly tool: string;
  readonly callId: string;
  readonly input: string | null;
  readonly output: string | null;
  readonly isError: boolean;
}

export type TurnStatus = "running" | "completed" | "cancelled" | "failed";

export interface TurnStateData {
  readonly turnId: string;
  readonly state: TurnStatus;
  readonly stopReason: string | null;
}

export type HarnessStatus = "starting" | "ready" | "restarting" | "failed";

export interface HarnessStateData {
  readonly state: HarnessStatus;
  readonly detail: string | null;
}

interface EventEnvelope {
  readonly seq: number;
  readonly time: string;
  readonly sessionId: string | null;
}

export type ServerEvent =
  | (EventEnvelope & { readonly type: "hello"; readonly data: HelloData })
  | (EventEnvelope & { readonly type: "session.state"; readonly data: SessionPatch })
  | (EventEnvelope & { readonly type: "session.message"; readonly data: MessageData })
  | (EventEnvelope & { readonly type: "session.tool"; readonly data: ToolData })
  | (EventEnvelope & { readonly type: "usage.update"; readonly data: TokenUsage })
  | (EventEnvelope & { readonly type: "approval.requested"; readonly data: Approval })
  | (EventEnvelope & { readonly type: "approval.resolved"; readonly data: ApprovalResolved })
  | (EventEnvelope & { readonly type: "turn.state"; readonly data: TurnStateData })
  | (EventEnvelope & { readonly type: "harness.state"; readonly data: HarnessStateData })
  | (EventEnvelope & { readonly type: "resync"; readonly data: null });

export type ServerEventType = ServerEvent["type"];

/* ------------------------------------------------------------------- health */

export type Liveness = "ok" | "down";
export type Readiness = "ready" | "starting" | "down";

/* -------------------------------------------------------- conversation feed */

/**
 * One rendered row of the conversation. History and live events are folded into
 * the same list so a view never has to know which source a row came from.
 */
export type FeedItem =
  | {
      readonly key: string;
      readonly kind: "message";
      readonly role: "user" | "assistant";
      readonly time: string | null;
      readonly text: string;
      readonly thinking: string | null;
      readonly model: string | null;
      readonly usage: TokenUsage | null;
    }
  | {
      readonly key: string;
      readonly kind: "tool";
      readonly time: string | null;
      readonly tool: string;
      readonly callId: string;
      readonly input: string | null;
      readonly output: string | null;
      readonly open: boolean;
      readonly isError: boolean;
    }
  | {
      readonly key: string;
      readonly kind: "notice";
      readonly time: string | null;
      readonly text: string;
    };
