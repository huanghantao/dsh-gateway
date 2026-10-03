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

/**
 * One admitted prompt.
 *
 * The same shape describes a turn on the session resource and a turn in a
 * `turn.state` event. That is deliberate: they are the same thing seen at
 * different moments, and two types would mean two decoders and a conversion
 * somewhere in the middle that could disagree with the server.
 *
 * `state` says whether it is running or waiting, and `position` is its 1-based
 * place in the queue while it waits. The gateway republishes a turn whenever the
 * queue moves, so a follow-up creeping to the front needs no polling.
 *
 * `stopReason` and `detail` are only ever set on the settled event; the session
 * resource carries `null`, because a running turn has not stopped yet.
 */
export interface Turn {
  readonly turnId: string;
  readonly state: TurnStatus;
  readonly position: number;
  readonly queuedAt: string;
  readonly startedAt: string | null;
  readonly stopReason: string | null;
  readonly detail: string | null;
}

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
  /**
   * The turn running in this session right now, when this gateway is the one
   * running it. It carries `startedAt`, which is what a client that reconnects
   * mid-turn ticks an elapsed timer from — the event that announced the turn may
   * be long past the replay window.
   */
  readonly turn: Turn | null;
  /** Prompts waiting behind `turn`, oldest first. */
  readonly queue: readonly Turn[];
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
  /**
   * Which part of the row matched: "text", "tool", "input" or "output".
   *
   * It matters because a hit in tool output answers "which session printed
   * this", and a result list that did not distinguish it from something the
   * operator typed would send them looking in the wrong place.
   */
  readonly field: string;
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

/**
 * One element of a prompt.
 *
 * `data` is base64 because that is what a JSON body carries. The app downscales
 * and re-encodes before sending: a phone photograph is several megabytes, and
 * the gateway's limit is a per-image policy rather than a target.
 */
export type PromptBlock =
  | { readonly type: "text"; readonly text: string }
  | { readonly type: "image"; readonly mimeType: string; readonly data: string };

export interface PromptRequest {
  readonly blocks: readonly PromptBlock[];
}

/** The ticket the gateway answers a prompt with. */
export type PromptResponse = Turn;

/* --------------------------------------------------------------- transcript */

export type TranscriptRole = "user" | "assistant" | "tool" | "notice";

/**
 * What a recorded result says about how a call ended.
 *
 * These are the harness's own facts, carried verbatim from the session log or
 * the ACP stream, and they exist because `isError` is not the question a reader
 * is asking: DSH reports a command's non-zero exit rather than erroring, so a
 * command that failed is recorded as a success. See `toolresult` on the server.
 */
export interface ToolResultFacts {
  /** Absent when the result did not say; zero is a real exit status. */
  readonly exitCode: number | null;
  /** DSH's structured error, from the session log only. */
  readonly errorName: string | null;
  readonly errorCode: string | null;
  /** The harness's own stop markers, verbatim: a timeout, a signal, a denial. */
  readonly notices: readonly string[];
  /** The harness cut the output itself, and where it put the rest. */
  readonly harnessTruncated: boolean;
  readonly spillPath: string | null;
}

/** Which of a call's payloads the deployment bounded on the way to this phone. */
export interface ToolTrim {
  readonly inputTruncated: boolean;
  readonly outputTruncated: boolean;
}

export interface TranscriptItem extends ToolResultFacts, ToolTrim {
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
  /**
   * role === "tool" only, and true while the log holds a call and no result: a
   * phone opening a session mid-turn sees the call that is running. Without it
   * such a row would render as a finished card with no output.
   */
  readonly pending: boolean;
  /** role === "tool" only: when the result was recorded, so a duration exists. */
  readonly endedAt: string | null;
  /**
   * How many non-text blocks the message carried. The image itself is not in
   * the projection — see the contract — so a row shows that something was
   * attached rather than pretending the words were the whole message.
   */
  readonly attachments: number;
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
  /**
   * True for a scoped choice this gateway synthesised — "allow this tool in
   * this session", "allow this exact call" — as opposed to the harness's own
   * allow-once and reject-once.
   */
  readonly grant: boolean;
}

/**
 * A standing authorisation: a scope a person agreed to once, and when it stops
 * applying.
 *
 * It is a resource rather than a detail of the sheet, because an authorisation
 * that is in force has to be visible somewhere — otherwise the only way to know
 * one exists is to remember giving it.
 */
export interface ApprovalGrant {
  readonly id: string;
  readonly sessionId: string;
  readonly tool: string;
  /** "tool" for every invocation, "exact" for one identical call. */
  readonly scope: string;
  /** A reminder of what was agreed to, for an exact grant. */
  readonly summary: string;
  readonly createdAt: string;
  readonly expiresAt: string;
  /** How many requests this grant has answered. */
  readonly uses: number;
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
  /**
   * Who answered. "operator" means a person did; "timeout" and "shutdown" mean
   * the tool was refused because nobody did, which a reader must be able to tell
   * apart.
   */
  readonly decidedBy: string | null;
  readonly tool: string;
  readonly sessionId: string;
  /** Set when a standing grant, rather than a person, answered. */
  readonly grantId: string | null;
}

/** A request answered from a standing grant instead of by a prompt. */
export interface ApprovalGranted {
  readonly grant: ApprovalGrant;
  readonly tool: string;
  readonly input: string;
}

/* ------------------------------------------------------------------- events */

export interface HelloData {
  readonly deviceId: string;
  /**
   * Names this run's sequence space. A `seq` is only a position *within* a
   * generation: a redeploy numbers its events from one again, so a bookmark kept
   * across one is not behind the new stream, it is meaningless to it. The client
   * sends it back on the next reconnect and treats a change as "my view is from
   * a process that is gone".
   */
  readonly generation: string;
  /**
   * The position the server actually resumed from, which is not necessarily the
   * one that was asked for: a resumable cursor is honoured, and anything else
   * resolves to a resync plus a snapshot.
   */
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
  /**
   * How many non-text blocks the message carried. See TranscriptItem.attachments.
   *
   * A prompt this gateway echoed back carries the count too: it is the only
   * thing that makes an image-only prompt visible before the log commits it.
   */
  readonly attachments: number;
}

/** The `session.thought` payload: one committed reasoning block. */
export interface ThoughtData {
  readonly id: string | null;
  readonly text: string;
}

export type ToolPhase = "start" | "end";

/** The `session.tool` payload: one lifecycle frame for one call. */
export interface ToolData extends ToolResultFacts, ToolTrim {
  readonly phase: ToolPhase;
  readonly tool: string;
  readonly callId: string;
  readonly input: string | null;
  readonly output: string | null;
  readonly isError: boolean;
}

export type TurnStatus = "queued" | "running" | "completed" | "cancelled" | "failed";

/**
 * The `turn.state` payload.
 *
 * `queueDepth` is the one field the session resource does not carry — it is a
 * fact about the moment, and a client that fetched the session would get the
 * live count from `queue` anyway.
 */
export interface TurnStateData extends Turn {
  readonly queueDepth: number | null;
}

export type HarnessStatus = "starting" | "ready" | "restarting" | "failed";

export interface HarnessStateData {
  readonly state: HarnessStatus;
  readonly detail: string | null;
}

/**
 * The `snapshot` payload: what is true right now, sent after a `resync` and when
 * a session is first subscribed to.
 *
 * It exists so that a client which cannot prove its stream was continuous —
 * after a redeploy, after falling behind, on opening an old conversation that
 * turns out to be mid-turn — renders the truth immediately instead of showing a
 * plausible-looking stale screen. `seq` is the position the snapshot is current
 * as of: frames at or below it are already reflected here.
 */
export interface SnapshotData {
  readonly generation: string;
  readonly seq: number;
  readonly time: string;
  readonly harness: HarnessStateData;
  /** One entry per session with a turn, running or waiting. */
  readonly turns: readonly SessionTurnSnapshot[];
  /** Decisions waiting on a person, as `GET /approvals` would return them. */
  readonly approvals: readonly Approval[];
}

export interface SessionTurnSnapshot {
  readonly sessionId: string;
  readonly running: TurnStateData | null;
  readonly queued: readonly TurnStateData[];
}

/**
 * The gateway is going away on purpose — a redeploy, not a fault. It is its own
 * frame type rather than a `harness.state` so that the UI can say "restarting
 * for an update" instead of "the agent crashed".
 */
export interface DrainingData {
  readonly reason: string;
}

/**
 * Why a `resync` arrived. The client does not branch on the text — every resync
 * means the same thing, "refetch what you are showing" — but it is surfaced in
 * the connection detail, because "the gateway restarted" and "you fell behind"
 * are different stories to tell an operator reading a bug report.
 */
export interface ResyncData {
  readonly reason: string;
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
  | (EventEnvelope & { readonly type: "session.thought"; readonly data: ThoughtData })
  | (EventEnvelope & { readonly type: "usage.update"; readonly data: TokenUsage })
  | (EventEnvelope & { readonly type: "approval.requested"; readonly data: Approval })
  | (EventEnvelope & { readonly type: "approval.resolved"; readonly data: ApprovalResolved })
  | (EventEnvelope & { readonly type: "approval.granted"; readonly data: ApprovalGranted })
  | (EventEnvelope & { readonly type: "turn.state"; readonly data: TurnStateData })
  | (EventEnvelope & { readonly type: "harness.state"; readonly data: HarnessStateData })
  | (EventEnvelope & { readonly type: "snapshot"; readonly data: SnapshotData })
  | (EventEnvelope & { readonly type: "gateway.draining"; readonly data: DrainingData })
  | (EventEnvelope & { readonly type: "resync"; readonly data: ResyncData });

export type ServerEventType = ServerEvent["type"];

/* ------------------------------------------------------------------ changes */

/**
 * One hunk of a recorded change, as unified-diff lines.
 *
 * There are no line numbers: the session log records what an edit replaced, not
 * where in the file it landed. `lines` are prefixed with "+", "-" or a space.
 */
export interface ChangeHunk {
  readonly seq: number;
  readonly callId: string;
  readonly tool: string;
  readonly lines: readonly string[];
  readonly added: number;
  readonly deleted: number;
  /** A write whose previous contents the log does not record. All additions. */
  readonly wholeFile: boolean;
  readonly truncated: boolean;
}

/** One file the session changed. */
export interface ChangedFile {
  readonly path: string;
  /** The path as a reader thinks of it: relative to the session's workspace. */
  readonly display: string;
  readonly added: number;
  readonly deleted: number;
  readonly edits: number;
  readonly writes: number;
  readonly binary: boolean;
  readonly hunks: readonly ChangeHunk[];
  readonly truncated: boolean;
  /**
   * Whether an undo is offered *and* possible. False when the deployment has
   * undo switched off, or when the log does not record enough to reverse this
   * file — `reason` says which.
   */
  readonly revertible: boolean;
  readonly reason: string;
}

export interface ChangesSummary {
  readonly files: number;
  readonly total: number;
  readonly added: number;
  readonly deleted: number;
  readonly edits: number;
  /** Where the projection came from. "tool-calls" today. */
  readonly source: string;
  readonly truncated: boolean;
}

export interface SessionChanges {
  readonly files: readonly ChangedFile[];
  readonly summary: ChangesSummary;
  readonly revertEnabled: boolean;
  /** True when the log is newer than the gateway understands. */
  readonly unsupported: boolean;
  readonly detail: string;
}

export type RevertOutcome = "reverted" | "skipped" | "refused";

export interface RevertResult {
  readonly path: string;
  readonly display: string;
  readonly status: RevertOutcome;
  readonly replacements: number;
  readonly reason: string;
}

export interface RevertReport {
  readonly files: readonly RevertResult[];
  readonly reverted: number;
  readonly refused: number;
  readonly skipped: number;
}

/* -------------------------------------------------------------- deployment */

/**
 * What this gateway can do, so a client does not have to probe and fail.
 *
 * Every optional surface is named here: a composer that offers an attach button
 * on a harness that cannot take images is a button that produces an error, and
 * a revert control on a read-only deployment offers an action that cannot
 * happen.
 */
export interface Features {
  readonly transcript: boolean;
  readonly desktopUI: boolean;
  readonly imagePrompts: boolean;
  readonly approvalTimeoutSecs: number;
  readonly sessionIdleTimeoutSec: number;
  /** Zero means scoped approvals are off and must not be offered. */
  readonly approvalGrantTTLSecs: number;
  /** Zero means a mid-turn prompt is refused rather than queued. */
  readonly promptQueueDepth: number;
  readonly revertEnabled: boolean;
}

export interface DeploymentLimits {
  readonly maxPromptBytes: number;
  readonly maxBodyBytes: number;
  readonly maxImageBytes: number;
  readonly transcriptPage: number;
}

/** `GET /me`: who this device is, and what the deployment offers. */
export interface Principal {
  readonly device: Device;
  readonly features: Features;
  readonly limits: DeploymentLimits;
}

/* ------------------------------------------------------------------- health */

export type Liveness = "ok" | "down";
export type Readiness = "ready" | "starting" | "down";

/* -------------------------------------------------------- conversation feed */

/**
 * One rendered row of the conversation. History and live events are folded into
 * the same list so a view never has to know which source a row came from.
 *
 * A tool row is the only one that changes after it is rendered — it is published
 * when the call starts and completed when the result lands — so it is also the
 * only one whose fields are replaced rather than added to.
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
      /** Non-text blocks the message carried. See TranscriptItem.attachments. */
      readonly attachments: number;
    }
  | {
      readonly key: string;
      readonly kind: "tool";
      /** When the call started, from whichever source first reported it. */
      readonly time: string | null;
      /** When the result landed. Null while the call is still running. */
      readonly endedAt: string | null;
      readonly tool: string;
      readonly callId: string;
      readonly input: string | null;
      readonly output: string | null;
      /** The call has been reported and not yet settled. */
      readonly open: boolean;
      readonly isError: boolean;
      readonly exitCode: number | null;
      readonly errorName: string | null;
      readonly errorCode: string | null;
      readonly notices: readonly string[];
      readonly inputTruncated: boolean;
      readonly outputTruncated: boolean;
      readonly harnessTruncated: boolean;
      readonly spillPath: string | null;
    }
  | {
      readonly key: string;
      readonly kind: "notice";
      readonly time: string | null;
      readonly text: string;
    };

export type FeedMessageItem = Extract<FeedItem, { kind: "message" }>;
export type FeedToolItem = Extract<FeedItem, { kind: "tool" }>;
export type FeedNoticeItem = Extract<FeedItem, { kind: "notice" }>;
