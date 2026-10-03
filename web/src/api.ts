/**
 * The typed HTTP client. Every `fetch` in the application lives in this file —
 * views never touch the network directly, so the contract has exactly one
 * implementation and one place to change.
 *
 * Two contract details are handled here rather than at call sites:
 *
 * - **Auth** rides on an `HttpOnly`, `Secure`, `SameSite=Strict` cookie, so the
 *   only thing the client does is `credentials: "same-origin"`. The bearer
 *   token from `POST /pair` is deliberately never persisted: the contract says
 *   it is returned once, and a browser has a better store than `localStorage`.
 * - **CSRF** is enforced by the server comparing `Origin` with `Host`. The
 *   browser sets that header itself on every non-GET request; the client's job
 *   is simply to never proxy a request through anything that would forge it.
 */

import { decodeApprovalList, decodeChanges, decodeDevice, decodeDeviceList, decodeGrantList, decodeModels, decodePrincipal, decodeProblem, decodeReceipt, decodeRevertReport, decodeSession, decodeSessionList, decodeTranscript, decodeTranscriptSearch, decodeTrashList, decodeTriage, decodeTurn, decodeWorkspaceList, isRecord } from "./decode.js";
import type {
  Approval,
  ApprovalGrant,
  CreateSessionRequest,
  CurationDecision,
  Device,
  ModelsResponse,
  PairRequest,
  PairResponse,
  Principal,
  Problem,
  PromptBlock,
  PromptResponse,
  PushKey,
  PushSubscriptionRequest,
  Readiness,
  RevertReport,
  Session,
  SessionChanges,
  SessionListResponse,
  TranscriptResponse,
  SessionReceipt,
  TranscriptSearch,
  TrashEntry,
  TriageReport,
  UpdateSessionRequest,
  Workspace,
} from "./types.js";

export const API_BASE = "/api/v1";

/**
 * Stable problem codes from the contract. Branching on these instead of on
 * `detail` is the difference between working after a server reword and not.
 */
export const ERROR_CODES = {
  invalidPairingCode: "invalid_pairing_code",
  pairingLocked: "pairing_locked",
  promptInFlight: "prompt_in_flight",
  queueFull: "queue_full",
  tooManyTurns: "too_many_turns",
  revertDisabled: "revert_disabled",
  imageTooLarge: "image_too_large",
  imagesUnsupported: "images_unsupported",
  unsupportedImageType: "unsupported_image_type",
  approvalClosed: "approval_closed",
  unauthenticated: "unauthenticated",
  forbidden: "forbidden",
  notFound: "not_found",
  /** Synthesised client-side; the server never sends this. */
  network: "network_error",
  /** Synthesised client-side for a 2xx body that did not decode. */
  malformed: "malformed_response",
} as const;

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly retryable: boolean;
  readonly problem: Problem | null;

  constructor(problem: Problem | null, status: number, fallbackCode: string, fallbackMessage: string) {
    super(problem?.detail ?? problem?.title ?? fallbackMessage);
    this.name = "ApiError";
    this.status = problem?.status !== undefined && problem.status > 0 ? problem.status : status;
    this.code = problem?.code ?? fallbackCode;
    this.retryable = problem?.retryable ?? false;
    this.problem = problem;
  }

  is(code: string): boolean {
    return this.code === code;
  }

  /** 401 anywhere means the cookie is gone and the app must re-pair. */
  get isUnauthenticated(): boolean {
    return this.status === 401 || this.code === ERROR_CODES.unauthenticated;
  }
}

export function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === "AbortError";
}

type QueryValue = string | number | boolean | null | undefined;

function withQuery(path: string, query: Readonly<Record<string, QueryValue>> | undefined): string {
  if (query === undefined) return path;
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value === null || value === undefined || value === "") continue;
    params.set(key, String(value));
  }
  const encoded = params.toString();
  return encoded === "" ? path : `${path}?${encoded}`;
}

interface SendOptions {
  readonly method: string;
  readonly body?: unknown;
  readonly query?: Readonly<Record<string, QueryValue>>;
  readonly signal?: AbortSignal;
}

/**
 * How long a read may keep being retried while the gateway is being replaced.
 *
 * A redeploy is a *fast* failure for anything talking to it: a connection in
 * flight gets a FIN or a reset immediately, a connection still in the accept
 * queue is reset, and a new one is refused — none of them waits for a timeout.
 * A phone that happens to issue a GET in that window would otherwise show an
 * error for a deployment that is over before the user finishes reading it.
 *
 * Only reads are retried. A POST may have been received and acted on before the
 * connection died, and the honest answer to "did my prompt reach the agent" is
 * the server's, not a guess: exactly-once for a mutation needs an idempotency
 * key, which this contract does not have. A retried GET is free; a retried
 * prompt is a duplicate turn.
 */
const RETRY_BUDGET_MS = 1_500;
const RETRY_BASE_MS = 150;

/** Whether a method may be retried after a failure that reached no handler. */
function retryableMethod(method: string): boolean {
  return method === "GET" || method === "HEAD";
}

/** Whether a status is a gateway that is restarting rather than refusing. */
function transientStatus(status: number): boolean {
  return status === 502 || status === 503 || status === 504;
}

/** Sleeps, unless the caller aborts first. */
function delay(ms: number, signal: AbortSignal | undefined): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = window.setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = (): void => {
      window.clearTimeout(timer);
      reject(new DOMException("aborted", "AbortError"));
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

// There is no rate-limit retry here, deliberately: a 429 carries a Retry-After
// that is measured in seconds, and a client that silently waits that long looks
// broken while it does it. The caller surfaces the problem instead.

/** Performs one request and returns the parsed body, or throws `ApiError`. */
async function send(path: string, options: SendOptions): Promise<unknown> {
  const headers = new Headers({ Accept: "application/json, application/problem+json" });
  let payload: string | undefined;
  if (options.body !== undefined) {
    headers.set("Content-Type", "application/json");
    payload = JSON.stringify(options.body);
  }

  const deadline = Date.now() + RETRY_BUDGET_MS;
  let attempt = 0;

  for (;;) {
    let response: Response;
    try {
      response = await fetch(withQuery(`${API_BASE}${path}`, options.query), {
        method: options.method,
        headers,
        credentials: "same-origin",
        ...(payload === undefined ? {} : { body: payload }),
        ...(options.signal === undefined ? {} : { signal: options.signal }),
      });
    } catch (cause) {
      if (isAbortError(cause)) throw cause;
      // A transport failure is not a problem document, but callers should not
      // have to care which layer failed.
      if (retryNow(attempt, deadline, options)) {
        attempt += 1;
        await delay(backoff(attempt), options.signal);
        continue;
      }
      throw new ApiError(null, 0, ERROR_CODES.network, "network request failed");
    }

    const text = await response.text();
    let raw: unknown = null;
    if (text !== "") {
      try {
        raw = JSON.parse(text) as unknown;
      } catch {
        raw = null;
      }
    }

    if (!response.ok) {
      if (transientStatus(response.status) && retryNow(attempt, deadline, options)) {
        attempt += 1;
        await delay(backoff(attempt), options.signal);
        continue;
      }
      throw new ApiError(
        decodeProblem(raw),
        response.status,
        response.status === 401 ? ERROR_CODES.unauthenticated : `http_${response.status}`,
        `request failed with ${response.status}`,
      );
    }
    return raw;
  }
}

/** Whether another attempt is both allowed and still inside the budget. */
function retryNow(attempt: number, deadline: number, options: SendOptions): boolean {
  if (!retryableMethod(options.method)) return false;
  if (options.signal?.aborted === true) return false;
  if (Date.now() >= deadline) return false;
  return attempt < 4;
}

/** Equal-jitter backoff, so a hundred phones do not retry in lockstep. */
function backoff(attempt: number): number {
  const ceiling = RETRY_BASE_MS * 2 ** (attempt - 1);
  return ceiling / 2 + Math.random() * (ceiling / 2);
}

/** Same as `send`, but rejects a 2xx body that does not decode as expected. */
async function sendDecoded<T>(path: string, options: SendOptions, decode: (raw: unknown) => T | null, what: string): Promise<T> {
  const raw = await send(path, options);
  const value = decode(raw);
  if (value === null) {
    throw new ApiError(null, 200, ERROR_CODES.malformed, `the server returned an unexpected ${what}`);
  }
  return value;
}

function decodePair(raw: unknown): PairResponse | null {
  if (!isRecord(raw)) return null;
  const device = decodeDevice(raw["device"]);
  if (device === null) return null;
  return { device, token: typeof raw["token"] === "string" ? raw["token"] : "" };
}

function decodePrompt(raw: unknown): PromptResponse | null {
  // The answer is the whole ticket, not just an id: a client has to be able to
  // tell "running" from "queued, second in line" without a second request.
  return decodeTurn(raw);
}

function decodeSessionEnvelope(raw: unknown): Session | null {
  // Some handlers wrap the object; accept either shape rather than guessing.
  return decodeSession(isRecord(raw) && "session" in raw ? raw["session"] : raw);
}

/** `true`, `false`, or `null` when the probe could not be answered. */
async function probe(path: string): Promise<boolean | null> {
  try {
    const response = await fetch(path, { method: "GET", credentials: "same-origin", headers: { Accept: "application/json" } });
    return response.ok;
  } catch {
    return null;
  }
}

export interface SessionQuery {
  readonly workspace?: string;
  readonly cursor?: string;
  readonly limit?: number;
  /** active (default), archived, or all. */
  readonly state?: "active" | "archived" | "all";
  /** Free text over title, preview, workspace and id. */
  readonly q?: string;
}

export const api = {
  /* -------------------------------------------------------------- pairing */

  async pair(request: PairRequest, signal?: AbortSignal): Promise<PairResponse> {
    return sendDecoded(
      "/pair",
      { method: "POST", body: request, ...(signal === undefined ? {} : { signal }) },
      decodePair,
      "pairing response",
    );
  },

  /* -------------------------------------------------------------- devices */

  /**
   * Who this device is, and what the deployment offers.
   *
   * The feature flags are why this is not just a device: a client that offered
   * an attach button on a harness that cannot take images, or an undo on a
   * read-only gateway, would be offering an action that fails.
   */
  async me(signal?: AbortSignal): Promise<Principal> {
    return sendDecoded("/me", { method: "GET", ...(signal === undefined ? {} : { signal }) }, decodePrincipal, "principal");
  },

  async devices(signal?: AbortSignal): Promise<readonly Device[]> {
    const raw = await send("/devices", { method: "GET", ...(signal === undefined ? {} : { signal }) });
    return decodeDeviceList(raw);
  },

  /** Idempotent. Revoking the calling device is how a browser signs out. */
  async revokeDevice(id: string, signal?: AbortSignal): Promise<void> {
    await send(`/devices/${encodeURIComponent(id)}`, { method: "DELETE", ...(signal === undefined ? {} : { signal }) });
  },

  /* ----------------------------------------------------- workspaces/models */

  async workspaces(signal?: AbortSignal): Promise<readonly Workspace[]> {
    const raw = await send("/workspaces", { method: "GET", ...(signal === undefined ? {} : { signal }) });
    return decodeWorkspaceList(raw);
  },

  async models(signal?: AbortSignal): Promise<ModelsResponse> {
    const raw = await send("/models", { method: "GET", ...(signal === undefined ? {} : { signal }) });
    return decodeModels(raw);
  },

  /* ------------------------------------------------------------- sessions */

  async sessions(query: SessionQuery = {}, signal?: AbortSignal): Promise<SessionListResponse> {
    const raw = await send("/sessions", {
      method: "GET",
      query: {
        workspace: query.workspace ?? null,
        cursor: query.cursor ?? null,
        limit: query.limit ?? null,
        state: query.state ?? null,
        q: query.q ?? null,
      },
      ...(signal === undefined ? {} : { signal }),
    });
    return decodeSessionList(raw);
  },

  /**
   * Looks for text inside session transcripts.
   *
   * Bounded on the gateway side and honest about it: `scanned` and `truncated`
   * say how much of the history was read, because "nothing found" means
   * different things depending on the answer.
   */
  async searchTranscripts(query: string, signal?: AbortSignal): Promise<TranscriptSearch> {
    const raw = await send(`/sessions/search?q=${encodeURIComponent(query)}`, {
      method: "GET",
      ...(signal === undefined ? {} : { signal }),
    });
    return decodeTranscriptSearch(raw);
  },

  /** What a session actually did: tokens, tools, files, durations, cost. */
  async receipt(id: string, signal?: AbortSignal): Promise<SessionReceipt> {
    const raw = await send(`/sessions/${encodeURIComponent(id)}/receipt`, { method: "GET", ...(signal === undefined ? {} : { signal }) });
    return decodeReceipt(raw);
  },

  /**
   * Moves a session to the gateway's trash.
   *
   * Named for what it does rather than for the HTTP verb it uses: the session is
   * not destroyed, and a client that believed otherwise would show the wrong
   * confirmation.
   */
  async deleteSession(id: string, signal?: AbortSignal): Promise<void> {
    await send(`/sessions/${encodeURIComponent(id)}`, { method: "DELETE", ...(signal === undefined ? {} : { signal }) });
  },

  async trash(signal?: AbortSignal): Promise<readonly TrashEntry[]> {
    const raw = await send("/trash", { method: "GET", ...(signal === undefined ? {} : { signal }) });
    return decodeTrashList(raw);
  },

  async restoreFromTrash(id: string, signal?: AbortSignal): Promise<void> {
    await send("/trash/restore", { method: "POST", body: { id }, ...(signal === undefined ? {} : { signal }) });
  },

  /** The VAPID key a browser needs in order to subscribe to notifications. */
  async pushKey(signal?: AbortSignal): Promise<PushKey> {
    const raw = await send("/push/key", { method: "GET", ...(signal === undefined ? {} : { signal }) });
    return isRecord(raw)
      ? {
          enabled: raw["enabled"] === true,
          publicKey: typeof raw["publicKey"] === "string" ? raw["publicKey"] : "",
          turnThresholdSeconds: typeof raw["turnThresholdSeconds"] === "number" ? raw["turnThresholdSeconds"] : 0,
          appURL: typeof raw["appURL"] === "string" ? raw["appURL"] : "",
          caURL: typeof raw["caURL"] === "string" ? raw["caURL"] : "",
          channels: Array.isArray(raw["channels"])
            ? (raw["channels"] as unknown[]).filter((name): name is string => typeof name === "string")
            : [],
        }
      : { enabled: false, publicKey: "", turnThresholdSeconds: 0, appURL: "", caURL: "", channels: [] };
  },

  async pushSubscribe(subscription: PushSubscriptionRequest, signal?: AbortSignal): Promise<void> {
    await send("/push/subscribe", { method: "POST", body: subscription, ...(signal === undefined ? {} : { signal }) });
  },

  async pushUnsubscribe(endpoint: string, signal?: AbortSignal): Promise<void> {
    await send("/push/unsubscribe", { method: "POST", body: { endpoint }, ...(signal === undefined ? {} : { signal }) });
  },

  async pushTest(signal?: AbortSignal): Promise<void> {
    await send("/push/test", { method: "POST", body: {}, ...(signal === undefined ? {} : { signal }) });
  },

  /**
   * The gateway's own suggestion about what can be put away.
   *
   * The rules live on the server: they need every session's metadata, and a
   * heuristic that decides what leaves someone's list should be one
   * implementation that tests hold still, not a copy in each client.
   */
  async triage(signal?: AbortSignal): Promise<TriageReport> {
    const raw = await send("/sessions/triage", { method: "GET", ...(signal === undefined ? {} : { signal }) });
    return decodeTriage(raw);
  },

  /** Archives, unarchives, pins or unpins a set of sessions in one request. */
  async curate(ids: readonly string[], decision: CurationDecision, signal?: AbortSignal): Promise<void> {
    await send("/sessions/curate", {
      method: "POST",
      body: decision.archived === undefined ? { ids, pinned: decision.pinned } : { ids, archived: decision.archived },
      ...(signal === undefined ? {} : { signal }),
    });
  },

  async createSession(request: CreateSessionRequest, signal?: AbortSignal): Promise<Session> {
    const body: Record<string, string> = { workspace: request.workspace };
    if (request.model !== null) body["model"] = request.model;
    if (request.reasoningEffort !== null) body["reasoningEffort"] = request.reasoningEffort;
    return sendDecoded("/sessions", { method: "POST", body, ...(signal === undefined ? {} : { signal }) }, decodeSessionEnvelope, "session");
  },

  async session(id: string, signal?: AbortSignal): Promise<Session> {
    return sendDecoded(
      `/sessions/${encodeURIComponent(id)}`,
      { method: "GET", ...(signal === undefined ? {} : { signal }) },
      decodeSessionEnvelope,
      "session",
    );
  },

  /** Idempotent: attaching an already-leased session is not an error. */
  async lease(id: string, signal?: AbortSignal): Promise<void> {
    await send(`/sessions/${encodeURIComponent(id)}/lease`, { method: "POST", body: {}, ...(signal === undefined ? {} : { signal }) });
  },

  /** Releases the DSH write lock so the desktop GUI can open the session. */
  async release(id: string, signal?: AbortSignal): Promise<void> {
    await send(`/sessions/${encodeURIComponent(id)}/lease`, { method: "DELETE", ...(signal === undefined ? {} : { signal }) });
  },

  async transcript(id: string, before?: number, limit?: number, signal?: AbortSignal): Promise<TranscriptResponse> {
    const raw = await send(`/sessions/${encodeURIComponent(id)}/transcript`, {
      method: "GET",
      query: { before: before ?? null, limit: limit ?? null },
      ...(signal === undefined ? {} : { signal }),
    });
    return decodeTranscript(raw);
  },

  /**
   * Admits a prompt.
   *
   * A prompt that arrives while a turn is running is queued rather than
   * refused, and the answer says which happened — unless the deployment set
   * `promptQueueDepth` to zero, in which case it is a `409 prompt_in_flight`.
   */
  async prompt(id: string, blocks: readonly PromptBlock[], signal?: AbortSignal): Promise<PromptResponse> {
    return sendDecoded(
      `/sessions/${encodeURIComponent(id)}/prompt`,
      { method: "POST", body: { blocks }, ...(signal === undefined ? {} : { signal }) },
      decodePrompt,
      "prompt acknowledgement",
    );
  },

  /** Stops the running turn *and* discards anything queued behind it. */
  async cancel(id: string, signal?: AbortSignal): Promise<void> {
    await send(`/sessions/${encodeURIComponent(id)}/cancel`, { method: "POST", body: {}, ...(signal === undefined ? {} : { signal }) });
  },

  /** Drops one queued prompt without stopping the turn in progress. */
  async dropQueued(id: string, turnId: string, signal?: AbortSignal): Promise<void> {
    await send(`/sessions/${encodeURIComponent(id)}/queue/${encodeURIComponent(turnId)}`, {
      method: "DELETE",
      ...(signal === undefined ? {} : { signal }),
    });
  },

  /** What this session changed, projected from its own log. */
  async changes(id: string, signal?: AbortSignal): Promise<SessionChanges> {
    const raw = await send(`/sessions/${encodeURIComponent(id)}/changes`, {
      method: "GET",
      ...(signal === undefined ? {} : { signal }),
    });
    return decodeChanges(raw);
  },

  /**
   * Undoes recorded changes.
   *
   * `403 revert_disabled` when the deployment has not opted in — the app hides
   * the control in that case, so reaching this is a bug rather than a case.
   */
  async revert(id: string, paths: readonly string[], signal?: AbortSignal): Promise<RevertReport> {
    const raw = await send(`/sessions/${encodeURIComponent(id)}/revert`, {
      method: "POST",
      body: paths.length === 0 ? {} : { paths },
      ...(signal === undefined ? {} : { signal }),
    });
    return decodeRevertReport(raw);
  },

  /** Only the fields that are non-null are sent. */
  async updateSession(id: string, patch: UpdateSessionRequest, signal?: AbortSignal): Promise<Session> {
    const body: Record<string, string | boolean> = {};
    if (patch.model !== null) body["model"] = patch.model;
    if (patch.reasoningEffort !== null) body["reasoningEffort"] = patch.reasoningEffort;
    if (patch.archived !== null && patch.archived !== undefined) body["archived"] = patch.archived;
    if (patch.pinned !== null && patch.pinned !== undefined) body["pinned"] = patch.pinned;
    return sendDecoded(
      `/sessions/${encodeURIComponent(id)}`,
      { method: "PATCH", body, ...(signal === undefined ? {} : { signal }) },
      decodeSessionEnvelope,
      "session",
    );
  },

  /* ------------------------------------------------------------ approvals */

  async approvals(signal?: AbortSignal): Promise<readonly Approval[]> {
    const raw = await send("/approvals", { method: "GET", ...(signal === undefined ? {} : { signal }) });
    return decodeApprovalList(raw);
  },

  /** `409 approval_closed` covers unknown, expired and already-decided ids. */
  async decide(approvalId: string, optionId: string, signal?: AbortSignal): Promise<void> {
    await send(`/approvals/${encodeURIComponent(approvalId)}`, {
      method: "POST",
      body: { optionId },
      ...(signal === undefined ? {} : { signal }),
    });
  },

  /**
   * The standing authorisations currently answering approvals without asking.
   *
   * They are a resource rather than a detail of the sheet, because the only way
   * to know one is in force is to be able to look.
   */
  async grants(signal?: AbortSignal): Promise<readonly ApprovalGrant[]> {
    const raw = await send("/approvals/grants", { method: "GET", ...(signal === undefined ? {} : { signal }) });
    return decodeGrantList(raw);
  },

  /** Withdrawing one takes effect on the next request that would have matched. */
  async revokeGrant(id: string, signal?: AbortSignal): Promise<void> {
    await send(`/approvals/grants/${encodeURIComponent(id)}`, { method: "DELETE", ...(signal === undefined ? {} : { signal }) });
  },

  /* --------------------------------------------------------------- health */

  /**
   * Liveness. The contract table lists this as `/healthz` while the document's
   * base path is `/api/v1`, and both mountings are plausible, so the first
   * answer is memoised and reused. See README ("Judgement calls").
   */
  async health(): Promise<boolean | null> {
    const atRoot = await probe("/healthz");
    if (atRoot !== null) return atRoot;
    return probe(`${API_BASE}/healthz`);
  },

  /** Readiness: the DSH child process behind the gateway. */
  async ready(): Promise<Readiness> {
    try {
      const response = await fetch(`${API_BASE}/readyz`, { method: "GET", credentials: "same-origin" });
      if (response.ok) return "ready";
      if (response.status === 503) return "starting";
      return "down";
    } catch {
      return "down";
    }
  },
};
