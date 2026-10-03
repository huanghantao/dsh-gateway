/**
 * The controller layer: everything that turns user intent into API calls and
 * store commits.
 *
 * Views are deliberately dumb — they render state and call these methods — so
 * that all the cross-cutting rules live in one readable file:
 *
 * - **401 means re-pair.** Any request can lose its cookie (the device was
 *   revoked, the profile expired). Rather than teaching every view about it,
 *   `#fail` funnels every `ApiError` through one check.
 * - **The event stream is the source of truth for live state.** Actions issue
 *   the request and let frames update the store, except where the contract
 *   gives no echo (there is no `session.message` for a user turn, so the local
 *   bubble is appended only after the server accepts the prompt).
 * - **`resync` refetches, `snapshot` repairs.** A `resync` says the client's
 *   view is no longer trustworthy; the `snapshot` that follows says what the
 *   state actually is. The first recovers history the client never received,
 *   the second recovers the present it can no longer infer.
 */

import { recordActivity, saveActivities, saveReadAt, unreadCount, type Activity, type Outcome } from "./activity.js";
import { ApiError, ERROR_CODES, api, isAbortError } from "./api.js";
import { EventClient } from "./events.js";
import {
  appendMessage,
  appendNotice,
  appendUserMessage,
  applyToolEnd,
  applyToolStart,
  feedFromTranscript,
  latestUsage,
  mergeSessionPatch,
} from "./feed.js";
import { normalizePairingCode } from "./format.js";
import { parseSettlement, type Settlement } from "./settlement.js";
import type { ActiveSession, AppState, AppStore, Route } from "./store.js";
import { NO_FEATURES, NO_LIMITS, localStore, withSession } from "./store.js";
import type {
  Approval,
  ApprovalGranted,
  ApprovalResolved,
  CurationDecision,
  ModelsResponse,
  PromptBlock,
  RevertReport,
  ServerEvent,
  Session,
  SessionChanges,
  SessionTurnSnapshot,
  SessionReceipt,
  TokenUsage,
  TranscriptItem,
  TranscriptSearch,
  TrashEntry,
  TriageReport,
  Turn,
  TurnStateData,
  Workspace,
} from "./types.js";

const NOTICE_MS = 4000;
const SESSION_PAGE_SIZE = 30;

/** One line about a prompt waiting behind the turn in flight. */
function queuedNotice(turn: Turn): string {
  const place = turn.position > 0 ? ` (${turn.position} in line)` : "";
  return `Queued${place}. It runs when the turn in flight finishes.`;
}

/** One line about a turn that has settled, in the operator's terms. */
function settleNotice(turn: TurnStateData): string {
  switch (turn.state) {
    case "failed":
      return turn.detail ?? turn.stopReason ?? "The turn failed.";
    case "cancelled":
      return "The turn was stopped.";
    default:
      return "The turn finished.";
  }
}

/** One line about an approval nobody answered. */
function refusedNotice(decision: ApprovalResolved): string {
  const tool = decision.tool === "" ? "The tool" : decision.tool;
  return `${tool} was refused: nobody answered the approval in time.`;
}

/** One line about a tool a standing grant authorised. */
function grantedNotice(granted: ApprovalGranted): string {
  const tool = granted.tool === "" ? "A tool" : granted.tool;
  const scope = granted.grant.scope === "exact" ? "this exact call" : `${tool} in this session`;
  return `Auto-approved by your standing grant: ${scope}.`;
}

/**
 * Applies a snapshot's turn state to the open conversation.
 *
 * A session absent from the snapshot has no turn, and that absence is a claim
 * the snapshot can make and the event stream cannot — which is the whole point
 * of it. It is therefore applied as a replacement rather than as a merge: a
 * client that had a stale `running` turn for a session the server says is idle
 * would otherwise keep showing Stop for work that ended while it was away.
 */
function applySnapshot(
  active: ActiveSession | null,
  turns: readonly SessionTurnSnapshot[],
  openSessionId: string | null,
): ActiveSession | null {
  if (active === null || openSessionId === null) return active;
  const entry = turns.find((item) => item.sessionId === openSessionId);
  return {
    ...active,
    turn: entry?.running ?? null,
    queue: entry?.queued ?? [],
  };
}

/** Inserts or replaces a queued turn, keeping the queue in position order. */
function upsertQueued(queue: readonly Turn[], turn: TurnStateData): readonly Turn[] {
  const next = queue.filter((item) => item.turnId !== turn.turnId);
  next.push(turn);
  return next.slice().sort((a, b) => a.position - b.position);
}

/**
 * One image on its way out.
 *
 * `data` is base64 with no `data:` prefix, which is what the contract carries.
 * The app downscales and re-encodes before it gets here: a phone photograph is
 * several megabytes, and a prompt that the gateway refuses for size is a worse
 * outcome than one that arrives slightly softer.
 */
export interface OutgoingImage {
  readonly mimeType: string;
  readonly data: string;
}

/**
 * Renders a composer's contents as prompt blocks.
 *
 * Text first, then images: a model reading a screenshot with a caption does
 * better with the question before the picture, and it matches how the operator
 * wrote it.
 */
export function promptBlocks(text: string, images: readonly OutgoingImage[]): readonly PromptBlock[] {
  const blocks: PromptBlock[] = [];
  const trimmed = text.trim();
  if (trimmed !== "" || images.length === 0) blocks.push({ type: "text", text });
  for (const image of images) blocks.push({ type: "image", mimeType: image.mimeType, data: image.data });
  return blocks;
}

function emptyActive(sessionId: string): ActiveSession {
  return {
    sessionId,
    session: null,
    feed: [],
    historyLoading: true,
    historyError: null,
    unsupported: false,
    nextBefore: null,
    loadingOlder: false,
    usage: null,
    turn: null,
    queue: [],
    promptError: null,
    releaseError: null,
    releasing: false,
  };
}

export interface Ctx {
  readonly store: AppStore;
  readonly events: EventClient;

  navigate(route: Route): void;

  bootstrap(): Promise<void>;
  loadSessions(): Promise<void>;
  loadMoreSessions(): Promise<void>;
  loadWorkspaces(): Promise<void>;
  loadModels(): Promise<void>;
  loadDevices(): Promise<void>;
  loadApprovals(): Promise<void>;
  dropQueued(turnId: string): Promise<void>;
  changes(sessionId: string): Promise<SessionChanges>;
  revert(sessionId: string, paths: readonly string[]): Promise<RevertReport>;
  loadGrants(): Promise<void>;
  revokeGrant(id: string): Promise<void>;
  refresh(): Promise<void>;

  searchSessions(query: string): Promise<void>;
  showArchived(show: boolean): Promise<void>;
  curate(ids: readonly string[], decision: CurationDecision): Promise<void>;
  triage(): Promise<TriageReport>;
  searchTranscripts(query: string): Promise<TranscriptSearch>;
  receipt(id: string): Promise<SessionReceipt>;
  deleteSession(id: string): Promise<void>;
  restoreSession(id: string): Promise<void>;
  trash(): Promise<readonly TrashEntry[]>;

  openSession(id: string): Promise<void>;
  closeSession(): void;
  loadOlder(): Promise<void>;
  /**
   * Sends a prompt, optionally with images.
   *
   * A prompt that arrives while a turn is running is queued rather than
   * refused; the ticket is what says which happened.
   */
  sendPrompt(text: string, images?: readonly OutgoingImage[]): Promise<void>;
  cancelTurn(): Promise<void>;
  releaseSession(): Promise<void>;

  decideApproval(approval: Approval, optionId: string): Promise<void>;

  createSession(workspace: string, model: string | null, reasoningEffort: string | null): Promise<void>;
  updateSessionModel(id: string, model: string | null, reasoningEffort: string | null): Promise<void>;

  revokeDevice(id: string): Promise<void>;
  signOut(): Promise<void>;

  reconnect(): void;
  setNotice(text: string | null): void;
  /** Marks every recorded activity as read. */
  markActivitiesRead(): void;
  handleEvent(event: ServerEvent): void;
  /** The stream may have missed frames; refetch what the app is showing. */
  handleStale(): void;
}

/** One delegation the gateway reported, remembered so its settlement can be named. */
interface DelegateRecord {
  readonly callId: string;
  readonly task: string;
  /** A settlement has already claimed this name; a second must not reuse it. */
  used: boolean;
}

/** The tools that run a child agent. Mirrors the gateway's own list. */
const DELEGATION_TOOLS: readonly string[] = ["subagent", "subagent_fork", "workflow"];
/** The tools whose arguments record a file mutation. Mirrors the gateway's list. */
const FILE_TOOLS: readonly string[] = ["edit", "write"];

/**
 * The bare name of a tool, with a producer's decoration stripped.
 *
 * The ACP bridge sends what the harness called the call, which may be namespaced
 * (`mcp__server__subagent`); a stray prefix must not hide a delegation.
 */
function bareToolName(tool: string): string {
  const lowered = tool.trim().toLowerCase();
  const afterNamespace = lowered.includes("__") ? lowered.slice(lowered.lastIndexOf("__") + 2) : lowered;
  const afterColon = afterNamespace.includes(":") ? afterNamespace.slice(afterNamespace.lastIndexOf(":") + 1).trim() : afterNamespace;
  return afterColon;
}

/** The task a delegation was given, read from its arguments. */
function delegateTask(input: string | null): string {
  if (input === null || input.trim() === "") return "";
  try {
    const parsed: unknown = JSON.parse(input);
    if (typeof parsed !== "object" || parsed === null) return "";
    const args = parsed as Record<string, unknown>;
    const description = typeof args["description"] === "string" ? args["description"].trim() : "";
    if (description !== "") return description;
    const prompt = typeof args["prompt"] === "string" ? args["prompt"] : "";
    const first = prompt.split(/\r?\n/)[0]?.trim() ?? "";
    return first;
  } catch {
    return "";
  }
}

/** One countable fact about a settled call, as the gateway counts them. */
interface CallFact {
  readonly tool: string;
  readonly failed: boolean;
}

/**
 * What a turn amounted to, in the same words the gateway's notifications use.
 *
 * The count is kept client-side rather than sent on the frame so that the row a
 * reader sees in the app and the notification that reached their lock screen say
 * the same thing, from the same definitions.
 */
function digestSummary(calls: readonly CallFact[]): string {
  if (calls.length === 0) return "";
  const edits = calls.filter((call) => FILE_TOOLS.includes(bareToolName(call.tool))).length;
  const delegations = calls.filter((call) => DELEGATION_TOOLS.includes(bareToolName(call.tool))).length;
  const failed = calls.filter((call) => call.failed).length;
  const parts = [`${calls.length} tool call${calls.length === 1 ? "" : "s"}`];
  if (edits > 0) parts.push(`${edits} file${edits === 1 ? "" : "s"} changed`);
  if (delegations > 0) parts.push(`${delegations} delegation${delegations === 1 ? "" : "s"}`);
  if (failed > 0) parts.push(`${failed} failure${failed === 1 ? "" : "s"}`);
  return parts.join(" · ");
}

/** The outcome word for a settled call. */
function callFailed(isError: boolean, exitCode: number | null): boolean {
  return isError || (exitCode !== null && exitCode !== 0);
}

export function createContext(store: AppStore, events: EventClient): Ctx {
  /**
   * What the gateway has told us about delegated tasks, most recent last.
   *
   * A settlement message names its child by id and never by task — see
   * settlement.ts — so the only place a task's name exists is the delegation
   * tool call that started it. This is where the two are joined.
   */
  const delegates = new Map<string, DelegateRecord[]>();
  /** Tool calls that settled during the current turn, per session. */
  const turnDigests = new Map<string, CallFact[]>();

  /**
   * Commits one activity and writes it to this device.
   *
   * The store updater stays pure — the side effect happens once, before it — so
   * a re-run of the updater cannot double a browser write.
   */
  const record = (activity: Activity): void => {
    const next = recordActivity(store.state.activities, activity);
    if (next === store.state.activities) return;
    saveActivities(localStore(), next);
    store.set((state) => ({ ...state, activities: next, activityUnread: unreadCount(next, state.activityReadAt) }));
  };

  /**
   * Names a settlement's child with the task from the delegation that started it.
   *
   * The settlement itself never says — it names the child by session id — so the
   * name is claimed from the matching delegation, most recent first. A name is
   * claimed once: two children settling in a row must not both be reported as
   * whichever task happened to be last.
   */
  const claimDelegateTask = (sessionId: string): string => {
    const known = delegates.get(sessionId);
    if (known === undefined) return "";
    for (let index = known.length - 1; index >= 0; index -= 1) {
      const record = known[index];
      if (record !== undefined && !record.used && record.task !== "") {
        record.used = true;
        return record.task;
      }
    }
    return "";
  };

  const turnActivity = (sessionId: string, turn: TurnStateData, time: string): Activity => {
    const outcome: Outcome = turn.state === "failed" ? "failed" : turn.state === "cancelled" ? "cancelled" : "completed";
    const calls = turnDigests.get(sessionId) ?? [];
    // The turn is over; the next one starts from an empty ledger.
    turnDigests.delete(sessionId);
    return {
      id: `${sessionId}:turn:${turn.turnId === "" ? time : turn.turnId}`,
      kind: "turn",
      actor: { kind: "main", name: "" },
      outcome,
      sessionId,
      time,
      summary: digestSummary(calls),
      detail: outcome === "cancelled" ? "" : (turn.detail ?? turn.stopReason ?? ""),
    };
  };

  const subagentActivity = (
    sessionId: string,
    settlement: Settlement,
    time: string,
    seq: number,
  ): Activity => ({
    id: `${sessionId}:settle:${seq}`,
    kind: "task",
    actor: { kind: "subagent", name: claimDelegateTask(sessionId) },
    outcome: settlement.outcome,
    sessionId,
    time,
    summary: settlement.subject,
    // Bounded: a child's closing message is arbitrary model output and can run
    // to tens of kilobytes, and this list lives in the device's storage.
    detail: settlement.report.slice(0, 2000),
  });

  const approvalActivity = (sessionId: string, tool: string, outcome: Outcome, time: string, id: string): Activity => ({
    id: `approval:${id}`,
    kind: "approval",
    actor: { kind: "system", name: "" },
    outcome,
    sessionId,
    time,
    summary: tool,
    detail: outcome === "waiting" ? "Waiting for your decision." : "Nobody answered, so the tool was refused.",
  });

  let noticeTimer = 0;

  const setNotice = (text: string | null): void => {
    if (noticeTimer !== 0) {
      window.clearTimeout(noticeTimer);
      noticeTimer = 0;
    }
    store.patch({ notice: text });
    if (text !== null) {
      noticeTimer = window.setTimeout(() => {
        noticeTimer = 0;
        store.patch({ notice: null });
      }, NOTICE_MS);
    }
  };

  /**
   * Marks everything recorded so far as read.
   *
   * The marker is the newest activity's own timestamp rather than "now": a
   * device whose clock runs behind the gateway's would otherwise leave new rows
   * permanently unread, and one running ahead would swallow the next arrival.
   */
  const markActivitiesRead = (): void => {
    const activities = store.state.activities;
    const newest = activities[activities.length - 1];
    if (newest === undefined || store.state.activityUnread === 0) return;
    saveReadAt(localStore(), newest.time);
    store.set((state) => ({ ...state, activityReadAt: newest.time, activityUnread: 0 }));
  };

  /** The one place a 401 is interpreted. Returns a message for other failures. */
  const fail = (error: unknown, fallback: string): string => {    if (isAbortError(error)) return "";
    if (error instanceof ApiError && error.isUnauthenticated) {
      requirePairing();
      return "This device is no longer paired.";
    }
    return error instanceof ApiError ? error.message : fallback;
  };

  const requirePairing = (): void => {
    // `reset` rather than a full teardown: the same page keeps running, and a
    // successful re-pair has to be able to reconnect the stream.
    events.reset("Not paired.");
    store.set((state) => {
      // A code that arrived in the URL survives the trip to the pair screen.
      // Bootstrap fails with a 401 on a phone that has never paired, and that
      // failure must not discard the code the user just scanned.
      const code = state.route.kind === "pair" ? state.route.code : undefined;
      return {
        ...state,
        boot: "unauthenticated",
        bootError: null,
        route: code === undefined ? { kind: "pair" } : { kind: "pair", code },
        principal: null,
        features: NO_FEATURES,
        limits: NO_LIMITS,
        sessions: [],
        sessionsCursor: null,
        devices: [],
        approvals: [],
        grants: [],
        grantsError: null,
        active: null,
        connection: "closed",
        connectionDetail: null,
      };
    });
  };

  const currentActive = (): ActiveSession | null => store.state.active;

  const navigate = (route: Route): void => {
    const next = hashFor(route);
    if (window.location.hash === next) {
      // Re-navigating to the same hash fires no `hashchange`, so route directly;
      // otherwise "new session" from the list would not land in the
      // conversation when the hash already matches.
      store.patch({ route });
      return;
    }
    window.location.hash = next;
  };

  /* ------------------------------------------------------------- loading */

  /** The list request the current view asks for: filter and search included. */
  const sessionsQuery = (state: AppState, cursor?: string) => ({
    limit: SESSION_PAGE_SIZE,
    state: (state.showArchived ? "archived" : "active") as "archived" | "active",
    ...(state.sessionsQuery === "" ? {} : { q: state.sessionsQuery }),
    ...(cursor === undefined ? {} : { cursor }),
  });

  const loadSessions = async (): Promise<void> => {
    store.patch({ sessionsLoading: true, sessionsError: null });
    try {
      const page = await api.sessions(sessionsQuery(store.state));
      store.set((state) => ({
        ...state,
        sessions: page.sessions,
        sessionsCursor: page.nextCursor,
        sessionsLoading: false,
      }));
    } catch (error) {
      const message = fail(error, "Could not load sessions.");
      if (message !== "") store.patch({ sessionsLoading: false, sessionsError: message });
    }
  };

  const loadMoreSessions = async (): Promise<void> => {
    const state = store.state;
    if (state.sessionsLoading || state.sessionsCursor === null) return;
    const cursor = state.sessionsCursor;
    store.patch({ sessionsLoading: true, sessionsError: null });
    try {
      const page = await api.sessions(sessionsQuery(state, cursor));
      store.set((current) => {
        // The cursor pages backwards through a newest-first list, so a session
        // touched since the previous page can repeat; skip anything we hold.
        const existing = new Set(current.sessions.map((session) => session.id));
        return {
          ...current,
          sessions: [...current.sessions, ...page.sessions.filter((session) => !existing.has(session.id))],
          sessionsCursor: page.nextCursor,
          sessionsLoading: false,
        };
      });
    } catch (error) {
      const message = fail(error, "Could not load more sessions.");
      if (message !== "") store.patch({ sessionsLoading: false, sessionsError: message });
    }
  };

  const loadWorkspaces = async (): Promise<void> => {
    try {
      const workspaces: readonly Workspace[] = await api.workspaces();
      store.set((state) => ({ ...state, workspaces, workspacesError: null }));
    } catch (error) {
      const message = fail(error, "Could not load workspaces.");
      if (message !== "") store.patch({ workspacesError: message });
    }
  };

  const loadModels = async (): Promise<void> => {
    try {
      const models: ModelsResponse = await api.models();
      store.set((state) => ({ ...state, models, modelsError: null }));
    } catch (error) {
      const message = fail(error, "Could not load models.");
      if (message !== "") store.patch({ modelsError: message });
    }
  };

  const loadDevices = async (): Promise<void> => {
    try {
      const devices = await api.devices();
      store.set((state) => ({ ...state, devices, devicesError: null }));
    } catch (error) {
      const message = fail(error, "Could not load devices.");
      if (message !== "") store.patch({ devicesError: message });
    }
  };

  const loadApprovals = async (): Promise<void> => {
    try {
      const approvals = await api.approvals();
      // Replace wholesale: this is the recovery path for a missed
      // `approval.requested` while the socket was down, so anything pending on
      // the server must survive, and anything resolved must disappear.
      store.set((state) => ({ ...state, approvals }));
    } catch (error) {
      const message = fail(error, "Could not load pending approvals.");
      if (message !== "") store.patch({ approvalError: message });
    }
  };

  /**
   * Loads the standing authorisations currently in force.
   *
   * Fetched on demand rather than held in sync from events: a grant list is
   * something an operator opens deliberately, and a view that has to stay
   * current with `approval.granted` frames would be more machinery than the
   * answer is worth.
   */
  /**
   * Drops one prompt waiting behind the turn in flight.
   *
   * Local first, then confirmed by the server: the queue strip is a live view of
   * a list the gateway owns, and the `turn.state` frame it republishes is what
   * actually settles the row.
   */
  const dropQueued = async (turnId: string): Promise<void> => {
    const active = store.state.active;
    if (active === null) return;
    try {
      await api.dropQueued(active.sessionId, turnId);
      store.set((state) => {
        const current = state.active;
        if (current === null) return state;
        return { ...state, active: { ...current, queue: current.queue.filter((item) => item.turnId !== turnId) } };
      });
    } catch (error) {
      const message = fail(error, "That waiting prompt could not be dropped.");
      if (message !== "") store.patch({ notice: message });
    }
  };

  /**
   * What a session changed.
   *
   * Unlike the other loaders this one does not write to the store: the answer
   * belongs to a sheet that is open for a moment, and putting a page of file
   * hunks in global state would keep it alive long after the reader closed it.
   * It throws rather than reporting through `state`, so the caller that owns the
   * screen owns the message — which is what the sheet's own error path expects.
   */
  const changes = async (sessionId: string): Promise<SessionChanges> => api.changes(sessionId);

  /**
   * Undoes recorded changes, and answers with a per-file report.
   *
   * Deliberately not a silent success: the caller renders which files came back
   * and which did not, because there is no transaction and a half-undone tree
   * needs to be looked at rather than assumed.
   */
  const revert = async (sessionId: string, paths: readonly string[]): Promise<RevertReport> => api.revert(sessionId, paths);

  const loadGrants = async (): Promise<void> => {
    try {
      const grants = await api.grants();
      store.set((state) => ({ ...state, grants }));
    } catch (error) {
      const message = fail(error, "Could not load standing approvals.");
      if (message !== "") store.patch({ grantsError: message });
    }
  };

  const revokeGrant = async (id: string): Promise<void> => {
    try {
      await api.revokeGrant(id);
      store.set((state) => ({ ...state, grants: state.grants.filter((grant) => grant.id !== id) }));
    } catch (error) {
      const message = fail(error, "That authorisation could not be withdrawn.");
      if (message !== "") store.patch({ grantsError: message });
    }
  };

  /* ------------------------------------------------------------ sessions */

  const loadTranscript = async (sessionId: string): Promise<void> => {
    try {
      const transcript = await api.transcript(sessionId);
      store.set((state) => {
        const active = state.active;
        if (active === null || active.sessionId !== sessionId) return state;
        return {
          ...state,
          active: {
            ...active,
            feed: feedFromTranscript(transcript.items),
            historyLoading: false,
            historyError: null,
            unsupported: transcript.unsupported,
            nextBefore: transcript.nextBefore,
          },
        };
      });
    } catch (error) {
      const message = fail(error, "Could not load the transcript.");
      store.set((state) => {
        const active = state.active;
        if (active === null || active.sessionId !== sessionId) return state;
        return { ...state, active: { ...active, historyLoading: false, historyError: message } };
      });
    }
  };

  const loadSessionMeta = async (sessionId: string): Promise<void> => {
    try {
      const session = await api.session(sessionId);
      store.set((state) => withSession(state, session));
    } catch (error) {
      const message = fail(error, "Could not load the session.");
      if (message !== "") setNotice(message);
    }
  };

  /**
   * Sets the search text and reloads. The list is filtered by the gateway, so a
   * search reaches sessions that are not in the page the phone already holds.
   */
  const searchSessions = async (query: string): Promise<void> => {
    store.patch({ sessionsQuery: query, sessions: [], sessionsCursor: null });
    await loadSessions();
  };

  const showArchived = async (show: boolean): Promise<void> => {
    store.patch({ showArchived: show, sessions: [], sessionsCursor: null });
    await loadSessions();
  };

  /**
   * Archives, unarchives, pins or unpins a set of sessions, and reflects the
   * answer in the list without a refetch: the operator is looking at the rows
   * they just changed, and a round trip would show them a spinner instead.
   */
  const curate = async (ids: readonly string[], decision: CurationDecision): Promise<void> => {
    await api.curate(ids, decision);
    const archived = decision.archived;
    store.set((state) => {
      const changed = new Set(ids);
      const showingArchived = state.showArchived;
      const kept: Session[] = [];
      for (const session of state.sessions) {
        if (!changed.has(session.id)) {
          kept.push(session);
          continue;
        }
        const next: Session = {
          ...session,
          archived: archived ?? session.archived,
          archivedOnDesk: archived === false ? false : session.archivedOnDesk,
          pinned: decision.pinned ?? session.pinned,
        };
        // A row leaves the list exactly when the view it is in stops matching
        // it: archiving while looking at the archive keeps the row, and
        // restoring while looking at the active list keeps it too.
        if (next.archived === showingArchived) kept.push(next);
      }
      return { ...state, sessions: kept };
    });
  };

  /**
   * Moves a session to the trash, and takes it out of the list.
   *
   * A delete that leaves the row behind until a refresh is a delete nobody
   * trusts, so the row goes immediately — and comes back the same way if the
   * operator presses Undo on the toast.
   */
  const deleteSession = async (id: string): Promise<void> => {
    await api.deleteSession(id);
    store.set((state) => ({ ...state, sessions: state.sessions.filter((session) => session.id !== id) }));
  };

  const restoreSession = async (id: string): Promise<void> => {
    await api.restoreFromTrash(id);
    await loadSessions();
  };

  /** Looks inside session transcripts, on demand: it reads logs, so it is not free. */
  const searchTranscripts = async (query: string): Promise<TranscriptSearch> => api.searchTranscripts(query);

  /** A session's receipt, fetched when the panel is opened. */
  const receipt = async (id: string): Promise<SessionReceipt> => api.receipt(id);

  /** What is in the trash, fetched when the screen is opened. */
  const trash = async (): Promise<readonly TrashEntry[]> => api.trash();

  /** The gateway's suggestion about what can be put away, fetched on demand. */
  const triage = async (): Promise<TriageReport> => api.triage();

  const openSession = async (sessionId: string): Promise<void> => {
    store.set((state) => ({ ...state, active: emptyActive(sessionId) }));
    // Subscribe before fetching so no frame from the opening turn is missed;
    // the transcript fetch then establishes the baseline the frames extend.
    events.subscribe(sessionId);
    await Promise.all([loadSessionMeta(sessionId), loadTranscript(sessionId), loadApprovals()]);
  };

  const closeSession = (): void => {
    const active = currentActive();
    if (active !== null) events.unsubscribe(active.sessionId);
    store.set((state) => ({ ...state, active: null }));
  };

  const loadOlder = async (): Promise<void> => {
    const active = currentActive();
    if (active === null || active.nextBefore === null || active.loadingOlder) return;
    const before = active.nextBefore;
    store.set((state) => {
      const current = state.active;
      return current === null ? state : { ...state, active: { ...current, loadingOlder: true } };
    });
    try {
      const page = await api.transcript(active.sessionId, before);
      const older: readonly TranscriptItem[] = page.items;
      store.set((state) => {
        const current = state.active;
        if (current === null || current.sessionId !== active.sessionId) return state;
        return {
          ...state,
          active: {
            ...current,
            feed: [...feedFromTranscript(older), ...current.feed],
            nextBefore: page.nextBefore,
            loadingOlder: false,
          },
        };
      });
    } catch (error) {
      const message = fail(error, "Could not load older messages.");
      if (message !== "") {
        store.set((state) => {
          const current = state.active;
          return current === null ? state : { ...state, active: { ...current, loadingOlder: false, historyError: message } };
        });
      }
    }
  };

  /**
   * Sends a prompt.
   *
   * `images` travel as blocks beside the text. The gateway queues the prompt if
   * a turn is already running, and the ticket it answers with says which
   * happened — so the row appears immediately with its real state rather than an
   * optimistic "running" the server may not have agreed to.
   */
  const sendPrompt = async (text: string, images: readonly OutgoingImage[] = []): Promise<void> => {
    const active = currentActive();
    if (active === null) return;
    const sessionId = active.sessionId;
    store.set((state) => {
      const current = state.active;
      return current === null ? state : { ...state, active: { ...current, promptError: null } };
    });
    try {
      const ticket = await api.prompt(sessionId, promptBlocks(text, images));
      // The contract only echoes assistant messages, so the user's own turn is
      // appended here — after the server accepted it, never optimistically.
      store.set((state) => {
        const current = state.active;
        if (current === null || current.sessionId !== sessionId) return state;
        const at = new Date().toISOString();
        let feed = appendUserMessage(current.feed, text, at, images.length);
        let turn = current.turn;
        let queue = current.queue;
        if (ticket.state === "queued") {
          // Said in the transcript, not only in the header: the row the operator
          // just added is where they will look to find out what happened to it.
          feed = appendNotice(feed, queuedNotice(ticket), at, 0);
          queue = upsertQueued(queue, { ...ticket, queueDepth: null });
        } else {
          turn = ticket;
        }
        return { ...state, active: { ...current, feed, turn, queue } };
      });
    } catch (error) {
      const message =
        error instanceof ApiError && error.is(ERROR_CODES.promptInFlight)
          ? "This session is already running a turn. Stop it first, or wait for it to finish."
          : error instanceof ApiError && error.is(ERROR_CODES.queueFull)
            ? "This session already has as many prompts waiting as it allows. Wait for one to run, or stop the turn."
            : fail(error, "The prompt could not be sent.");
      if (message !== "") {
        store.set((state) => {
          const current = state.active;
          return current === null ? state : { ...state, active: { ...current, promptError: message } };
        });
      }
    }
  };

  const cancelTurn = async (): Promise<void> => {
    const active = currentActive();
    if (active === null) return;
    try {
      await api.cancel(active.sessionId);
    } catch (error) {
      const message = fail(error, "The turn could not be cancelled.");
      if (message !== "") setNotice(message);
    }
  };

  const releaseSession = async (): Promise<void> => {
    const active = currentActive();
    if (active === null) return;
    const sessionId = active.sessionId;
    store.set((state) => {
      const current = state.active;
      return current === null ? state : { ...state, active: { ...current, releasing: true, releaseError: null } };
    });
    try {
      await api.release(sessionId);
      store.set((state) => {
        const current = state.active;
        const cleared = current === null ? state : { ...state, active: { ...current, releasing: false } };
        const session = current?.session;
        if (session === null || session === undefined) return cleared;
        return withSession(cleared, { ...session, leased: false });
      });
      setNotice("Session released. The desktop can open it again.");
    } catch (error) {
      const message = fail(error, "The session could not be released.");
      if (message !== "") {
        store.set((state) => {
          const current = state.active;
          return current === null ? state : { ...state, active: { ...current, releasing: false, releaseError: message } };
        });
      }
    }
  };

  const decideApproval = async (approval: Approval, optionId: string): Promise<void> => {
    store.patch({ approvalBusyId: approval.id, approvalError: null });
    try {
      await api.decide(approval.id, optionId);
      store.set((state) => ({ ...state, approvals: state.approvals.filter((item) => item.id !== approval.id), approvalBusyId: null }));
    } catch (error) {
      if (error instanceof ApiError && error.is(ERROR_CODES.approvalClosed)) {
        // Unknown, expired or already decided — all three mean this card is
        // stale, so it is dismissed rather than left blocking the UI.
        store.set((state) => ({
          ...state,
          approvals: state.approvals.filter((item) => item.id !== approval.id),
          approvalBusyId: null,
        }));
        setNotice("That approval had already expired or been decided.");
        return;
      }
      const message = fail(error, "The decision could not be sent.");
      if (message !== "") store.patch({ approvalBusyId: null, approvalError: message });
    }
  };

  const createSession = async (workspace: string, model: string | null, reasoningEffort: string | null): Promise<void> => {
    const session = await api.createSession({ workspace, model, reasoningEffort });
    store.set((state) => withSession(state, session));
    // `POST /sessions` returns the session already leased, so the caller lands
    // straight in the conversation.
    navigate({ kind: "conversation", sessionId: session.id });
  };

  const updateSessionModel = async (id: string, model: string | null, reasoningEffort: string | null): Promise<void> => {
    const session = await api.updateSession(id, { model, reasoningEffort });
    store.set((state) => withSession(state, session));
    setNotice("Session updated.");
  };

  /* ----------------------------------------------------- devices / auth */

  const revokeDevice = async (id: string): Promise<void> => {
    await api.revokeDevice(id);
    if (store.state.principal?.id === id) {
      // The contract revokes the calling device immediately, so this is how a
      // browser signs out: there is no separate sign-out endpoint.
      requirePairing();
      setNotice("This device was revoked.");
      return;
    }
    await loadDevices();
  };

  const signOut = async (): Promise<void> => {
    const principal = store.state.principal;
    if (principal !== null) {
      try {
        await api.revokeDevice(principal.id);
      } catch (error) {
        // A failed revoke must not trap the user in the app; the local session
        // ends either way and the device can be revoked from another one.
        if (error instanceof ApiError && !error.isUnauthenticated) {
          setNotice(`Signed out locally, but the server refused the revoke: ${error.message}`);
        }
      }
    }
    requirePairing();
  };

  /* ------------------------------------------------------------- events */

  /**
   * Re-reads the conversation on screen, because the stream can no longer be
   * trusted to have carried everything: either the server dropped frames for a
   * client that fell behind, or the connection came back with no resume point.
   *
   * The live turn frame goes with it. It is the one piece of state the refetch
   * cannot restore — the transcript carries no turn — and a frame that may have
   * missed the `completed` that ended it would leave the composer offering Stop
   * for a turn that has already settled. `loadSessionMeta` asks the server
   * instead, and `runningHere` reads the answer off the session's `busy` flag.
   */
  const refetchConversation = (): void => {
    const active = currentActive();
    if (active === null) return;
    store.set((state) => {
      const current = state.active;
      return current === null
        ? state
        : { ...state, active: { ...current, historyLoading: true, turn: null } };
    });
    void loadTranscript(active.sessionId);
    void loadSessionMeta(active.sessionId);
  };

  const handleStale = (): void => {
    void loadApprovals();
    void loadSessions();
    refetchConversation();
  };

  const handleEvent = (event: ServerEvent): void => {
    switch (event.type) {
      case "hello":
        // A connection is open. Refresh what is cheap and shared; the open
        // conversation is handled by `handleStale`, which the event client raises
        // only when it cannot prove the reconnect covered the gap. Refetching a
        // conversation the reader has paged back through would be neither free
        // nor harmless, so it is not done on every connect.
        void loadApprovals();
        void loadSessions();
        return;

      case "resync":
        // The server could not honour this client's cursor — a redeploy, a gap
        // it fell behind, or a bookmark from a process that is gone. Everything
        // it shows is suspect, including the conversation.
        //
        // The refetch is issued here rather than deferred to the `snapshot` that
        // follows because the two answer different questions: the snapshot says
        // what is happening *now* (a running turn, a queue, a decision waiting),
        // and this says what has *happened* (rows the client never received).
        // Only the transcript server can answer the second.
        void loadApprovals();
        void loadSessions();
        refetchConversation();
        return;

      case "snapshot": {
        // The present state, as the server sees it. Applied in place so that a
        // client which cannot prove its stream was continuous shows the truth
        // immediately rather than after its refetches land.
        const active = store.state.active;
        const turns = event.data.turns;
        store.set((state) => ({
          ...state,
          harness: event.data.harness,
          approvals: event.data.approvals,
          active: applySnapshot(state.active, turns, active?.sessionId ?? null),
        }));
        return;
      }

      case "gateway.draining":
        // The gateway is being replaced, not failing: a turn that is running
        // keeps running, and this page will reconnect to its successor. It is
        // surfaced as a notice because a silent freeze during a deploy is
        // indistinguishable from a hang.
        store.patch({ notice: "The gateway is restarting; a running turn continues." });
        return;

      case "session.state": {
        const sessionId = event.sessionId;
        if (sessionId === null) return;
        const existing = store.state.sessions.find((session) => session.id === sessionId);
        if (existing === undefined) {
          // A session we have never listed — created on the desktop, or older
          // than the first page. Pull it once so the header and list can show
          // it. The fetch happens outside the updater so the commit stays pure.
          void loadSessionMeta(sessionId);
          return;
        }
        // `withSession` already rewrites the active header when the ids match.
        store.set((state) => withSession(state, mergeSessionPatch(existing, event.data)));
        return;
      }

      case "session.message": {
        const sessionId = event.sessionId;
        if (sessionId === null) return;
        // A settlement is the only place a child agent's finish is reported, and
        // it is reported as a user-role message with no envelope. It is recorded
        // here as its own activity — named with the task from the delegation
        // that started it — before the feed decides how to draw the row.
        const settlement = event.data.role === "user" ? parseSettlement(event.data.text) : null;
        if (settlement !== null) {
          record(subagentActivity(sessionId, settlement, event.time, event.seq));
        }
        store.set((state) => {
          const active = state.active;
          if (active === null || active.sessionId !== sessionId) return state;
          return { ...state, active: { ...active, feed: appendMessage(active.feed, event.data, event.time, event.seq) } };
        });
        return;
      }

      case "session.tool": {
        const sessionId = event.sessionId;
        if (sessionId === null) return;
        const data = event.data;
        // The ledger the turn's activity summary is built from. It is fed for
        // every session, not only the open one, because the reader who needs the
        // summary is the one who was not looking.
        if (data.phase === "end") {
          const calls = turnDigests.get(sessionId) ?? [];
          calls.push({ tool: data.tool, failed: callFailed(data.isError, data.exitCode) });
          turnDigests.set(sessionId, calls);
        }
        if (data.phase === "start" && DELEGATION_TOOLS.includes(bareToolName(data.tool))) {
          const known = delegates.get(sessionId) ?? [];
          // Bounded: a session that delegated a hundred times must not grow this
          // list forever, and an old name is only ever used as a fallback.
          known.push({ callId: data.callId, task: delegateTask(data.input), used: false });
          delegates.set(sessionId, known.slice(-20));
        }
        store.set((state) => {
          const active = state.active;
          if (active === null || active.sessionId !== sessionId) return state;
          const feed =
            data.phase === "start"
              ? applyToolStart(active.feed, data, event.time, event.seq)
              : applyToolEnd(active.feed, data, event.time, event.seq);
          return { ...state, active: { ...active, feed } };
        });
        return;
      }

      case "usage.update": {
        const sessionId = event.sessionId;
        const usage: TokenUsage = event.data;
        store.set((state) => {
          const active = state.active;
          if (active === null) return state;
          if (sessionId !== null && active.sessionId !== sessionId) return state;
          return { ...state, active: { ...active, usage: latestUsage(active.feed, usage) } };
        });
        return;
      }

      case "approval.requested": {
        const approval = event.data;
        if (approval.sessionId !== "") {
          record(approvalActivity(approval.sessionId, approval.tool, "waiting", event.time, approval.id));
        }
        store.set((state) => {
          const known = state.approvals.some((item) => item.id === approval.id);
          return { ...state, approvals: known ? state.approvals : [...state.approvals, approval], approvalError: null };
        });
        return;
      }

      case "approval.resolved": {
        const decision = event.data;
        // A decision nobody made is the case the product used to lose: the
        // operator who missed the notification came back to a session that had
        // carried on without the tool and no record of why.
        const refused = decision.decidedBy === "timeout" || decision.decidedBy === "shutdown";
        if (refused && decision.sessionId !== "") {
          record(approvalActivity(decision.sessionId, decision.tool, "expired", event.time, decision.id));
        }
        store.set((state) => {
          const approvals = state.approvals.filter((item) => item.id !== decision.id);
          const active = state.active;
          if (!refused || active === null || (decision.sessionId !== "" && active.sessionId !== decision.sessionId)) {
            return { ...state, approvals };
          }
          return { ...state, approvals, active: { ...active, feed: appendNotice(active.feed, refusedNotice(decision), event.time, event.seq) } };
        });
        return;
      }

      case "approval.granted": {
        // A tool ran without anyone answering *this* prompt, because a decision
        // they made earlier applied. It belongs in the transcript: otherwise the
        // only evidence is a tool card with no approval beside it.
        const granted = event.data;
        store.set((state) => {
          const active = state.active;
          if (active === null || (event.sessionId !== null && active.sessionId !== event.sessionId)) return state;
          const feed = appendNotice(active.feed, grantedNotice(granted), event.time, event.seq);
          return { ...state, active: { ...active, feed } };
        });
        return;
      }

      case "turn.state": {
        // Three cases, not one. A queued prompt is not the running one; a
        // running one may be a queued prompt being promoted; and a settled one
        // has to leave the header *and* the queue, because the server will send
        // a separate `running` frame for whatever it promotes next.
        const sessionId = event.sessionId;
        const turn = event.data;
        // A settled turn is recorded whether or not its session is the one on
        // screen: the reader who needs the record is the one who was elsewhere.
        if (sessionId !== null && turn.state !== "queued" && turn.state !== "running") {
          record(turnActivity(sessionId, turn, event.time));
        }
        store.set((state) => {
          const active = state.active;
          if (active === null || (sessionId !== null && active.sessionId !== sessionId)) return state;
          const without = active.queue.filter((item) => item.turnId !== turn.turnId);

          switch (turn.state) {
            case "queued":
              return { ...state, active: { ...active, queue: upsertQueued(active.queue, turn), promptError: null } };

            case "running":
              return {
                ...state,
                active: { ...active, turn, queue: without, promptError: null },
              };

            default: {
              const feed = appendNotice(active.feed, settleNotice(turn), event.time, event.seq);
              return {
                ...state,
                active: {
                  ...active,
                  // Only the turn that is actually in flight is cleared: a stale
                  // `completed` for an older turn must not blank a newer one.
                  turn: active.turn?.turnId === turn.turnId ? null : active.turn,
                  queue: without,
                  promptError: null,
                  feed,
                },
              };
            }
          }
        });
        return;
      }

      case "harness.state": {
        // A stopped agent is worth a row of its own: nothing else reports it, and
        // it is invisible until someone opens the app and finds it dead.
        if (event.data.state === "failed") {
          record({
            id: `harness:${event.time}`,
            kind: "harness",
            actor: { kind: "system", name: "" },
            outcome: "failed",
            sessionId: "",
            time: event.time,
            summary: "",
            detail: event.data.detail ?? "The agent is not running and could not be restarted.",
          });
        }
        store.set((state) => ({ ...state, harness: event.data }));
        return;
      }
    }
  };

  const bootstrap = async (): Promise<void> => {
    store.patch({ boot: "starting", bootError: null });
    try {
      const principal = await api.me();
      store.set((state) => ({
        ...state,
        boot: "ready",
        bootError: null,
        principal: principal.device,
        features: principal.features,
        limits: principal.limits,
      }));

      // wake(), not connect().
      //
      // A phone that has never paired fails this call with a 401, and the 401
      // handler calls events.reset() — which *pauses* the stream, because the
      // old sequence number and subscriptions described a different principal.
      // connect() begins with `if (this.#paused) return`, so calling it here
      // left a freshly paired device with a working session list and a
      // permanently dead event stream: the badge read "Reconnecting" until the
      // app was backgrounded and foregrounded, which is what finally called
      // wake(). Only a real browser on a genuinely unpaired device reproduces
      // it, because the mock never answered the first request with a 401.
      events.wake();
      // Seed the harness banner immediately; `harness.state` frames keep it
      // current from here on.
      void api.ready().then((readiness) => {
        store.set((state) => {
          if (readiness === "ready") return { ...state, harness: { state: "ready", detail: null } };
          if (readiness === "starting") return { ...state, harness: { state: "starting", detail: null } };
          return { ...state, harness: { state: "failed", detail: "The gateway is not ready." } };
        });
      });
      await Promise.all([loadSessions(), loadWorkspaces(), loadModels(), loadApprovals()]);
    } catch (error) {
      if (error instanceof ApiError && error.isUnauthenticated) {
        requirePairing();
        return;
      }
      const message = fail(error, "Could not reach the gateway.");
      store.patch({ boot: "failed", bootError: message });
    }
  };

  const refresh = async (): Promise<void> => {
    await Promise.all([loadSessions(), loadApprovals()]);
  };

  return {
    store,
    events,
    navigate,
    bootstrap,
    loadSessions,
    loadMoreSessions,
    searchSessions,
    showArchived,
    curate,
    triage,
    searchTranscripts,
    receipt,
    deleteSession,
    restoreSession,
    trash,
    loadWorkspaces,
    loadModels,
    loadDevices,
    loadApprovals,
    dropQueued,
    changes,
    revert,
    loadGrants,
    revokeGrant,
    refresh,
    openSession,
    closeSession,
    loadOlder,
    sendPrompt,
    cancelTurn,
    releaseSession,
    decideApproval,
    createSession,
    updateSessionModel,
    revokeDevice,
    signOut,
    reconnect: () => {
      events.wake();
    },
    setNotice,
    markActivitiesRead,
    handleEvent,
    handleStale,
  };
}

/** The router's URL grammar: `#/sessions/<id>`, `#/settings`, `#/pair`. */
export function hashFor(route: Route): string {
  switch (route.kind) {
    case "pair":
      return "#/pair";
    case "sessions":
      return "#/sessions";
    case "settings":
      return "#/settings";
    case "activity":
      return "#/activity";
    case "conversation":
      return `#/sessions/${encodeURIComponent(route.sessionId)}`;
    case "boot":
      return "#/";
  }
}

export function routeFromHash(hash: string): Route {
  const raw = hash.startsWith("#") ? hash.slice(1) : hash;
  // The router's grammar owns the path; everything after the first `?` is a
  // parameter bag. A pairing QR encodes its one-time code there, so dropping
  // it would leave the user retyping eight characters they just scanned.
  const queryAt = raw.indexOf("?");
  const path = queryAt === -1 ? raw : raw.slice(0, queryAt);
  const query = queryAt === -1 ? "" : raw.slice(queryAt + 1);

  const parts = path.split("/").filter((part) => part !== "");
  const [head, second] = parts;
  if (head === "pair") {
    // Normalised through the same alphabet as typed input, so a malformed or
    // truncated code prefills as far as it is usable instead of being sent.
    const code = normalizePairingCode(new URLSearchParams(query).get("code") ?? "");
    return code === "" ? { kind: "pair" } : { kind: "pair", code };
  }
  if (head === "settings") return { kind: "settings" };
  if (head === "activity") return { kind: "activity" };
  if (head === "sessions") {
    if (second === undefined || second === "") return { kind: "sessions" };
    let sessionId = second;
    try {
      sessionId = decodeURIComponent(second);
    } catch {
      sessionId = second;
    }
    return { kind: "conversation", sessionId };
  }
  return { kind: "boot" };
}
