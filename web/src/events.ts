/**
 * The single multiplexed WebSocket to `GET /api/v1/events`.
 *
 * Everything here exists to survive a phone: the screen locks, the tab is
 * backgrounded, the radio moves between Wi-Fi and cellular, and every one of
 * those kills the socket without an error frame. So the client owns four
 * behaviours:
 *
 * - **Resume by sequence.** `since=<last seq seen>` lets the server replay the
 *   ring buffer, so a reconnect is not a lost turn. The counter only ever moves
 *   forward, across connections, which is what the contract asks for.
 * - **Equal-jitter backoff.** Half the delay is fixed, half is random, capped
 *   at 10s. Pure doubling would resynchronise every phone that dropped together
 *   after a gateway restart.
 * - **Offline pause.** Reconnecting while the radio is off just burns battery;
 *   `online` resumes and resets the backoff, because a network change is new
 *   information rather than another failure.
 * - **Application heartbeat.** A `ping` every 20s keeps NAT mappings and carrier
 *   middleboxes from silently reaping an idle connection; a heartbeat that finds
 *   the socket no longer OPEN is itself the liveness check.
 *
 * Socket event handlers are bound to a *generation* number. A socket we have
 * deliberately dropped still fires `close` afterwards, and without this guard
 * that stale event would schedule a retry for a connection we no longer want.
 */

import { decodeServerEvent } from "./decode.js";
import type { ServerEvent } from "./types.js";
import { API_BASE } from "./api.js";
import type { ConnectionStatus } from "./store.js";

const BACKOFF_BASE_MS = 500;
const BACKOFF_CAP_MS = 10_000;
const HEARTBEAT_MS = 20_000;

/** Client frames from the contract. */
type ClientFrame =
  | { readonly type: "subscribe"; readonly sessionId: string }
  | { readonly type: "unsubscribe"; readonly sessionId: string }
  | { readonly type: "ping" };

export interface EventClientHandlers {
  readonly onEvent: (event: ServerEvent) => void;
  readonly onStatus: (status: ConnectionStatus, detail: string | null) => void;
  /**
   * The stream came back with nothing to resume from, so whatever happened while
   * it was down is unreachable: the server starts such a connection "from now".
   * The app must refetch the state it is showing rather than trust the stream.
   */
  readonly onStale?: (reason: string) => void;
}

export class EventClient {
  readonly #handlers: EventClientHandlers;
  /** Session ids the app wants frames for; re-sent after every reconnect. */
  readonly #subscriptions = new Set<string>();

  #socket: WebSocket | null = null;
  #lastSeq = -1;
  #attempt = 0;
  #retryTimer = 0;
  #heartbeatTimer = 0;
  #generation = 0;
  #paused = false;
  /** Whether a connection has succeeded before; the first one has no gap. */
  #connected = false;

  constructor(handlers: EventClientHandlers) {
    this.#handlers = handlers;
  }

  /** The last `seq` observed, or -1 before the first frame. */
  get lastSeq(): number {
    return this.#lastSeq;
  }

  // There is deliberately no connect() alongside wake().
  //
  // There used to be, and it began with `if (this.#paused) return`. Every other
  // caller used wake(); the one place that reached for connect() was bootstrap,
  // which runs again after pairing — and pairing happens on a device whose
  // earlier 401 had just called reset(), which pauses. So the freshly paired
  // phone got a working session list and a stream that never opened, and the
  // badge sat on "Reconnecting" until the app was backgrounded and
  // foregrounded. Two resume methods with subtly different semantics is the bug;
  // one is the fix.

  /** Stops without forgetting the sequence, so `wake()` resumes where it left. */
  pause(detail: string | null): void {
    this.#paused = true;
    this.#drop();
    this.#handlers.onStatus("offline", detail);
  }

  /**
   * Forgets everything and stops. Used when the device identity changes
   * (re-pairing, or a 401), where the old sequence number and session
   * subscriptions describe a different principal.
   */
  reset(detail: string | null): void {
    this.#subscriptions.clear();
    this.#lastSeq = -1;
    this.#attempt = 0;
    // The next connection is a first connection for a new principal: the app
    // bootstraps after pairing, so it must not also be told its view is stale.
    this.#connected = false;
    this.pause(detail);
  }

  /** Resumes after `pause()`, or after the page becomes visible again. */
  wake(): void {
    this.#paused = false;
    if (!navigator.onLine) return;
    if (this.#socket !== null && this.#socket.readyState === WebSocket.OPEN) return;
    // A frozen socket looks open to us and dead to the network, so drop it and
    // let `since` replay whatever was missed.
    this.#attempt = 0;
    this.#drop();
    this.#open();
  }

  subscribe(sessionId: string): void {
    this.#subscriptions.add(sessionId);
    this.#send({ type: "subscribe", sessionId });
  }

  unsubscribe(sessionId: string): void {
    this.#subscriptions.delete(sessionId);
    this.#send({ type: "unsubscribe", sessionId });
  }

  #url(): string {
    const scheme = window.location.protocol === "https:" ? "wss:" : "ws:";
    const url = new URL(`${API_BASE}/events`, `${scheme}//${window.location.host}`);
    if (this.#lastSeq >= 0) url.searchParams.set("since", String(this.#lastSeq));
    return url.toString();
  }

  #open(): void {
    this.#generation += 1;
    const generation = this.#generation;
    this.#handlers.onStatus("connecting", null);

    let socket: WebSocket;
    try {
      socket = new WebSocket(this.#url());
    } catch (cause) {
      this.#scheduleRetry(cause instanceof Error ? cause.message : "could not open the event stream");
      return;
    }
    this.#socket = socket;

    const stale = (): boolean => generation !== this.#generation;

    socket.addEventListener("open", () => {
      if (stale()) return;
      this.#attempt = 0;
      this.#handlers.onStatus("open", null);
      for (const sessionId of this.#subscriptions) this.#send({ type: "subscribe", sessionId });
      // A first connection owes nothing: bootstrap fetches the state it needs
      // after this point, so the stream only has to carry what happens next. A
      // *re*connection with no bookmark is different — the server resumed "from
      // now", so the frames published in between are unreachable, and the only
      // honest thing to do is say so and let the app refetch. This is the phone's
      // ordinary case: the screen locked, and a turn ran while nobody watched.
      if (this.#connected && this.#lastSeq < 0) {
        this.#handlers.onStale?.("reconnected without a resume point");
      }
      this.#connected = true;
      this.#heartbeatTimer = window.setInterval(() => this.#beat(generation), HEARTBEAT_MS);
    });

    socket.addEventListener("message", (event: MessageEvent<unknown>) => {
      if (stale() || typeof event.data !== "string") return;
      let parsed: unknown;
      try {
        parsed = JSON.parse(event.data) as unknown;
      } catch {
        return;
      }
      const decoded = decodeServerEvent(parsed);
      // Unknown frame types are ignored on purpose: a newer server must be able
      // to add one without breaking an already-installed PWA.
      if (decoded === null) return;
      if (decoded.seq > this.#lastSeq) this.#lastSeq = decoded.seq;
      this.#handlers.onEvent(decoded);
    });

    socket.addEventListener("close", (event: CloseEvent) => {
      if (stale()) return;
      this.#drop();
      if (this.#paused) {
        this.#handlers.onStatus("offline", "This device is offline.");
        return;
      }
      if (!navigator.onLine) {
        this.#handlers.onStatus("offline", "This device is offline.");
        return;
      }
      // 1000 is a deliberate server-side close, e.g. a revoked device; the
      // message tells the user something actionable either way.
      this.#scheduleRetry(event.code === 1000 ? "The gateway closed the stream." : `Stream closed (${event.code}).`);
    });

    // `error` is intentionally not handled: it is always followed by `close`,
    // and recovering in one place keeps the state machine readable.
  }

  #beat(generation: number): void {
    if (generation !== this.#generation) return;
    const socket = this.#socket;
    if (socket === null) return;
    if (socket.readyState !== WebSocket.OPEN) {
      this.#drop();
      this.#scheduleRetry("The event stream stopped responding.");
      return;
    }
    this.#send({ type: "ping" });
  }

  #send(frame: ClientFrame): void {
    const socket = this.#socket;
    if (socket === null || socket.readyState !== WebSocket.OPEN) return;
    socket.send(JSON.stringify(frame));
  }

  #scheduleRetry(detail: string): void {
    if (this.#paused || this.#retryTimer !== 0) return;
    const ceiling = Math.min(BACKOFF_CAP_MS, BACKOFF_BASE_MS * 2 ** this.#attempt);
    this.#attempt += 1;
    // Equal jitter: a floor on the delay, minus the correlation between clients
    // that all dropped at the same instant.
    const delay = ceiling / 2 + Math.random() * (ceiling / 2);
    this.#handlers.onStatus("closed", detail);
    this.#retryTimer = window.setTimeout(() => {
      this.#retryTimer = 0;
      this.#open();
    }, delay);
  }

  /** Invalidates the current socket and releases its timers. */
  #drop(): void {
    this.#generation += 1;
    if (this.#heartbeatTimer !== 0) {
      window.clearInterval(this.#heartbeatTimer);
      this.#heartbeatTimer = 0;
    }
    if (this.#retryTimer !== 0) {
      window.clearTimeout(this.#retryTimer);
      this.#retryTimer = 0;
    }
    const socket = this.#socket;
    this.#socket = null;
    socket?.close();
  }
}

/** Wires the browser's own connectivity signals to the client. */
export function bindConnectivity(client: EventClient): () => void {
  const onOnline = (): void => {
    client.wake();
  };
  const onOffline = (): void => {
    client.pause("This device is offline.");
  };
  const onVisible = (): void => {
    if (!document.hidden) client.wake();
  };
  window.addEventListener("online", onOnline);
  window.addEventListener("offline", onOffline);
  document.addEventListener("visibilitychange", onVisible);
  return () => {
    window.removeEventListener("online", onOnline);
    window.removeEventListener("offline", onOffline);
    document.removeEventListener("visibilitychange", onVisible);
  };
}
