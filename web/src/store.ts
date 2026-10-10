/**
 * The reactive store.
 *
 * Why hand-rolled instead of a framework: the deliverable is a handful of
 * screens shipped as native ES modules with no build step beyond `tsc` and no
 * network budget for a runtime. A framework would cost more bytes than the
 * entire application, and the only two things this app actually needs from one
 * are (a) a single immutable state tree and (b) fine-grained subscriptions so a
 * streaming token does not re-render the session list.
 *
 * Design notes:
 *
 * - State is replaced, never mutated. Views can therefore compare references
 *   (`Object.is`) to decide whether to touch the DOM at all, and a missed
 *   update is always a missing `set`, never a hidden mutation.
 * - Notifications are batched into one microtask. A single server frame often
 *   changes several slices at once (feed + usage + turn state); without
 *   batching each slice would trigger its own layout pass.
 * - `select` is the only subscription API views use. It runs the selector on
 *   every commit but invokes the handler only when the slice's identity
 *   changed, which is what keeps the conversation's per-frame work O(appended).
 */

import { loadActivities, loadReadAt, unreadCount, type Activity } from "./activity.js";
import type { Approval, ApprovalGrant, DeploymentLimits, Device, Features, FeedItem, HarnessStateData, ModelsResponse, Question, Session, TokenUsage, Turn, Workspace } from "./types.js";

/**
 * What a gateway that has not answered `GET /me` yet can do: nothing optional.
 *
 * The defaults live here rather than in each view so that "unknown" and "off"
 * are the same thing everywhere — a control that appears for a moment and then
 * disappears once the truth arrives is worse than one that arrives with it.
 */
export const NO_FEATURES: Features = {
  transcript: false,
  desktopUI: false,
  imagePrompts: false,
  approvalTimeoutSecs: 0,
  sessionIdleTimeoutSec: 0,
  approvalGrantTTLSecs: 0,
  promptQueueDepth: 0,
  revertEnabled: false,
};

export const NO_LIMITS: DeploymentLimits = {
  maxPromptBytes: 256 * 1024,
  maxBodyBytes: 0,
  maxImageBytes: 0,
  transcriptPage: 0,
};
import { decodeModels } from "./decode.js";

export type Listener = () => void;
export type Selector<S, T> = (state: S) => T;
export type Equality<T> = (a: T, b: T) => boolean;

/** Where the hash router currently is. */
export type Route =
  | { readonly kind: "boot" }
  /**
   * `code` is present only when the URL carried one, which is how a scanned
   * pairing QR arrives (`#/pair?code=K7M2QPX4`). It is never set by
   * `hashFor`, so navigating away and back cannot re-inject a spent
   * one-time code.
   */
  | { readonly kind: "pair"; readonly code?: string }
  | { readonly kind: "sessions" }
  | { readonly kind: "conversation"; readonly sessionId: string }
  | { readonly kind: "settings" }
  /** What has finished: the record a notification only points at. */
  | { readonly kind: "activity" };

export type ConnectionStatus = "connecting" | "open" | "offline" | "closed";

export interface ActiveSession {
  readonly sessionId: string;
  /** `null` until the first `GET /sessions/{id}` resolves. */
  readonly session: Session | null;
  readonly feed: readonly FeedItem[];
  readonly historyLoading: boolean;
  readonly historyError: string | null;
  /** Transcript format is newer than the server understands; show a hint. */
  readonly unsupported: boolean;
  /** Cursor for "load older"; `null` means history is exhausted. */
  readonly nextBefore: number | null;
  readonly loadingOlder: boolean;
  readonly usage: TokenUsage | null;
  /**
   * The turn in flight. Typed as the narrow `Turn` rather than the event
   * payload, because it can arrive from either the session resource or an
   * event, and the extra field an event carries is not something a view needs.
   */
  readonly turn: Turn | null;
  /** Prompts waiting behind `turn`, as the server last reported them. */
  readonly queue: readonly Turn[];
  readonly promptError: string | null;
  readonly releaseError: string | null;
  readonly releasing: boolean;
}

export interface AppState {
  readonly boot: "starting" | "ready" | "unauthenticated" | "failed";
  readonly bootError: string | null;
  readonly route: Route;
  readonly connection: ConnectionStatus;
  /** Human sentence about *why* the socket is down, shown in settings. */
  readonly connectionDetail: string | null;
  readonly offline: boolean;
  readonly harness: HarnessStateData;
  readonly principal: Device | null;
  /**
   * What this deployment offers. Held beside the principal rather than inside
   * it: the device is who you are, these are what the gateway can do, and a
   * view that reads one almost never wants the other.
   */
  readonly features: Features;
  readonly limits: DeploymentLimits;

  readonly sessions: readonly Session[];
  /** Opaque cursor from the last `GET /sessions`; `null` means the end. */
  readonly sessionsCursor: string | null;
  readonly sessionsLoading: boolean;
  readonly sessionsError: string | null;
  /**
   * The search box's text, held in state rather than in the view so that a
   * refresh, a reconnect or a store update cannot silently drop the filter and
   * show the operator a different list than the one they asked for.
   */
  readonly sessionsQuery: string;
  /** Whether the archived list is being shown instead of the active one. */
  readonly showArchived: boolean;

  readonly workspaces: readonly Workspace[];
  readonly workspacesError: string | null;

  readonly models: ModelsResponse;
  readonly modelsError: string | null;

  readonly devices: readonly Device[];
  readonly devicesError: string | null;

  readonly approvals: readonly Approval[];
  /**
   * Questions the agent is blocked on, oldest first.
   *
   * Held beside the approvals for the same reason: the sheet that answers them
   * is mounted at the app root, so it has to be renderable from any screen — and
   * a client that only kept them in a view would lose one asked while the reader
   * was elsewhere.
   */
  readonly questions: readonly Question[];
  readonly questionBusyId: string | null;
  readonly questionError: string | null;
  /** Standing authorisations, loaded on demand for the approvals screen. */
  readonly grants: readonly ApprovalGrant[];
  readonly grantsError: string | null;
  readonly approvalBusyId: string | null;
  readonly approvalError: string | null;

  readonly active: ActiveSession | null;
  /** Transient, non-blocking message (e.g. "session released"). */
  readonly notice: string | null;

  /**
   * What this device has recorded, newest last. See activity.ts.
   *
   * It lives in the store rather than inside a view because two things read it:
   * the activity screen, and the app bar's badge, which has to be right on every
   * screen.
   */
  readonly activities: readonly Activity[];
  /**
   * When the reader last acknowledged what had arrived.
   *
   * A marker rather than a set of read ids: the list is append-only and ordered,
   * so "everything up to this moment" is both smaller to store and impossible to
   * get wrong as rows expire off the end.
   */
  readonly activityReadAt: string | null;
  /** How many recorded activities are newer than the marker. */
  readonly activityUnread: number;
}

class Store<S extends object> {
  #state: S;
  readonly #listeners = new Set<Listener>();
  #scheduled = false;

  constructor(initial: S) {
    this.#state = initial;
  }

  get state(): S {
    return this.#state;
  }

  /**
   * Commits a new state. Returning the same reference (or the same object) is a
   * no-op, which lets callers write `set` unconditionally.
   */
  set(updater: (state: S) => S): void {
    const next = updater(this.#state);
    if (Object.is(next, this.#state)) return;
    this.#state = next;
    this.#schedule();
  }

  /** `set` with a shallow merge, for the common "change two fields" case. */
  patch(partial: Partial<S>): void {
    this.set((state) => ({ ...state, ...partial }));
  }

  /** Runs `handler` now, then on every change of the selected slice. */
  select<T>(selector: Selector<S, T>, handler: (value: T, previous: T) => void, equals: Equality<T> = Object.is): () => void {
    let current = selector(this.#state);
    handler(current, current);

    const listener: Listener = () => {
      const next = selector(this.#state);
      if (equals(next, current)) return;
      const previous = current;
      current = next;
      handler(next, previous);
    };
    this.#listeners.add(listener);
    return () => {
      this.#listeners.delete(listener);
    };
  }

  #schedule(): void {
    if (this.#scheduled) return;
    this.#scheduled = true;
    queueMicrotask(() => {
      this.#scheduled = false;
      // Copy first: a listener may unsubscribe (view teardown) while we iterate.
      for (const listener of [...this.#listeners]) listener();
    });
  }
}

export type AppStore = Store<AppState>;

/** Empty model catalogue; used before `GET /models` answers and after failure. */
const NO_MODELS: ModelsResponse = decodeModels(null);

function createAppState(): AppState {
  const storage = localStore();
  const activities = loadActivities(storage);
  const activityReadAt = loadReadAt(storage);
  return {
    boot: "starting",
    bootError: null,
    route: { kind: "boot" },
    connection: "closed",
    connectionDetail: null,
    offline: typeof navigator !== "undefined" && navigator.onLine === false,
    harness: { state: "starting", detail: null },
    principal: null,
    grants: [],
    grantsError: null,
    features: NO_FEATURES,
    limits: NO_LIMITS,
    sessions: [],
    sessionsCursor: null,
    sessionsLoading: false,
    sessionsQuery: "",
    showArchived: false,
    sessionsError: null,
    workspaces: [],
    workspacesError: null,
    models: NO_MODELS,
    modelsError: null,
    devices: [],
    devicesError: null,
    approvals: [],
    approvalBusyId: null,
    approvalError: null,
    questions: [],
    questionBusyId: null,
    questionError: null,
    active: null,
    notice: null,
    activities,
    activityReadAt,
    activityUnread: unreadCount(activities, activityReadAt),
  };
}

/**
 * The browser's storage, or null where touching it throws.
 *
 * Safari in a private window and a page with storage disabled both make
 * `localStorage` itself throw on access, so the activity list treats storage as
 * an optimisation rather than a dependency: without it the screen still works
 * for as long as the tab lives.
 */
export function localStore(): Storage | null {
  try {
    return typeof localStorage === "undefined" ? null : localStorage;
  } catch {
    return null;
  }
}

export function createStore(): AppStore {
  return new Store<AppState>(createAppState());
}

/** Replaces one session everywhere it appears (list, active header). */
export function withSession(state: AppState, updated: Session): AppState {
  let found = false;
  const sessions = state.sessions.map((session) => {
    if (session.id !== updated.id) return session;
    found = true;
    return updated;
  });
  const nextSessions = found ? sessions : [updated, ...state.sessions];
  const active = state.active;
  if (active !== null && active.sessionId === updated.id) {
    return { ...state, sessions: nextSessions, active: { ...active, session: updated } };
  }
  return { ...state, sessions: nextSessions };
}