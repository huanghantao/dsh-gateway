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
 * - **`resync` refetches.** It is the one frame that says the client's view is
 *   no longer trustworthy.
 */

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
import type { ActiveSession, AppState, AppStore, Route } from "./store.js";
import { withSession } from "./store.js";
import type {
  Approval,
  CurationDecision,
  ModelsResponse,
  ServerEvent,
  Session,
  TokenUsage,
  TranscriptItem,
  SessionReceipt,
  TranscriptSearch,
  TrashEntry,
  TriageReport,
  Workspace,
} from "./types.js";

const NOTICE_MS = 4000;
const SESSION_PAGE_SIZE = 30;

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
  sendPrompt(text: string): Promise<void>;
  cancelTurn(): Promise<void>;
  releaseSession(): Promise<void>;

  decideApproval(approval: Approval, optionId: string): Promise<void>;

  createSession(workspace: string, model: string | null, reasoningEffort: string | null): Promise<void>;
  updateSessionModel(id: string, model: string | null, reasoningEffort: string | null): Promise<void>;

  revokeDevice(id: string): Promise<void>;
  signOut(): Promise<void>;

  reconnect(): void;
  setNotice(text: string | null): void;
  handleEvent(event: ServerEvent): void;
  /** The stream may have missed frames; refetch what the app is showing. */
  handleStale(): void;
}

export function createContext(store: AppStore, events: EventClient): Ctx {
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

  /** The one place a 401 is interpreted. Returns a message for other failures. */
  const fail = (error: unknown, fallback: string): string => {
    if (isAbortError(error)) return "";
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
        sessions: [],
        sessionsCursor: null,
        devices: [],
        approvals: [],
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

  const sendPrompt = async (text: string): Promise<void> => {
    const active = currentActive();
    if (active === null) return;
    const sessionId = active.sessionId;
    store.set((state) => {
      const current = state.active;
      return current === null ? state : { ...state, active: { ...current, promptError: null } };
    });
    try {
      await api.prompt(sessionId, text);
      // The contract only echoes assistant messages, so the user's own turn is
      // appended here — after the server accepted it, never optimistically.
      store.set((state) => {
        const current = state.active;
        if (current === null || current.sessionId !== sessionId) return state;
        return {
          ...state,
          active: {
            ...current,
            feed: appendUserMessage(current.feed, text, new Date().toISOString()),
            turn: current.turn ?? { turnId: "", state: "running", stopReason: null },
          },
        };
      });
    } catch (error) {
      const message =
        error instanceof ApiError && error.is(ERROR_CODES.promptInFlight)
          ? "This session is already running a turn. Stop it first, or wait for it to finish."
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
        // The server dropped frames for this client. Everything it shows is
        // suspect, including the conversation.
        void loadApprovals();
        void loadSessions();
        refetchConversation();
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
        store.set((state) => {
          const active = state.active;
          if (active === null || active.sessionId !== sessionId) return state;
          const feed =
            event.data.phase === "start"
              ? applyToolStart(active.feed, event.data, event.time, event.seq)
              : applyToolEnd(active.feed, event.data, event.time, event.seq);
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
        store.set((state) => {
          const known = state.approvals.some((item) => item.id === approval.id);
          return { ...state, approvals: known ? state.approvals : [...state.approvals, approval], approvalError: null };
        });
        return;
      }

      case "approval.resolved": {
        const resolvedId = event.data.id;
        store.set((state) => ({ ...state, approvals: state.approvals.filter((item) => item.id !== resolvedId) }));
        return;
      }

      case "turn.state": {
        const sessionId = event.sessionId;
        store.set((state) => {
          const active = state.active;
          if (active === null || (sessionId !== null && active.sessionId !== sessionId)) return state;
          const feed =
            event.data.state === "failed"
              ? appendNotice(active.feed, event.data.stopReason ?? "The turn failed.", event.time, event.seq)
              : active.feed;
          return { ...state, active: { ...active, turn: event.data, promptError: null, feed } };
        });
        return;
      }

      case "harness.state":
        store.set((state) => ({ ...state, harness: event.data }));
        return;
    }
  };

  const bootstrap = async (): Promise<void> => {
    store.patch({ boot: "starting", bootError: null });
    try {
      const principal = await api.me();
      store.set((state) => ({ ...state, boot: "ready", bootError: null, principal }));

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
