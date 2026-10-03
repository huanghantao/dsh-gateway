/**
 * What an agent finished, in one record the reader can come back to.
 *
 * A lock-screen notification is read once and gone: the operating system keeps
 * it for an hour, and the app keeps nothing at all. So a reader who was driving,
 * asleep, or simply looking at another screen learns that *something* happened
 * and never learns what — which is the complaint this module exists to answer.
 * Every event worth interrupting someone about is folded into an Activity here,
 * kept on the device, and rendered as a row that says who, what and how it went.
 *
 * Three properties are deliberate:
 *
 *   - **It is local.** No server state, no sync, no account. The gateway is a
 *     process on a desktop with no database, and a notification centre that
 *     needed one would be a feature it could not ship.
 *   - **It is bounded.** A phone that has been paired for a year must not carry
 *     a year of rows: the list is capped and old entries expire.
 *   - **It records what the event said, not what a view wants.** The name of a
 *     session is resolved when the row is drawn, so a session renamed tomorrow
 *     reads correctly in yesterday's activity.
 */

/** Who an activity is about. Mirrors the gateway's own vocabulary. */
export type ActorKind = "main" | "subagent" | "system";

export interface Actor {
  readonly kind: ActorKind;
  /** The task a delegation was given; empty for the main agent. */
  readonly name: string;
}

/** How a piece of work ended. */
export type Outcome = "completed" | "failed" | "cancelled" | "expired" | "waiting";

/** What kind of thing happened, which is what a row's icon and verb come from. */
export type ActivityKind = "turn" | "task" | "approval" | "harness";

export interface Activity {
  /** Stable, so a re-delivered frame cannot double a row. */
  readonly id: string;
  readonly kind: ActivityKind;
  readonly actor: Actor;
  readonly outcome: Outcome;
  /** The session it belongs to, and empty for a gateway-wide event. */
  readonly sessionId: string;
  /** When it happened, as the event said. */
  readonly time: string;
  /** What the work amounted to: "12 tool calls · 3 files changed". */
  readonly summary: string;
  /** The one line a reader needs if they read nothing else. */
  readonly detail: string;
}

/** A row as the activity screen draws it: the activity, plus its session's name. */
export interface ActivityRow {
  readonly activity: Activity;
  /**
   * The session's name *now*.
   *
   * Resolved at draw time rather than stored, so a session renamed this morning
   * reads correctly in yesterday's rows.
   */
  readonly sessionName: string;
}

/** How an outcome prints: one word, capitalised, for a badge. */
export function outcomeLabel(outcome: Outcome | "completed"): string {
  switch (outcome) {
    case "failed":
      return "Failed";
    case "cancelled":
      return "Stopped";
    case "expired":
      return "Expired";
    case "waiting":
      return "Waiting";
    default:
      return "Completed";
  }
}

/** How an actor prints, in the same words the gateway's notifications use. */
export function actorLabel(actor: Actor): string {
  switch (actor.kind) {
    case "subagent":
      return actor.name === "" ? "Subagent" : `Subagent · ${actor.name}`;
    case "system":
      return "Gateway";
    default:
      return "Main agent";
  }
}

/** How many rows are kept. Older ones fall off the end. */
const MAX_ACTIVITIES = 200;

/** How long a row is worth keeping. */
const MAX_AGE_MS = 30 * 24 * 60 * 60 * 1000;

const STORAGE_KEY = "dsh.activity.v1";
const READ_KEY = "dsh.activity.read.v1";

/**
 * Loads what this device has recorded.
 *
 * A corrupt or unreadable store is treated as empty rather than fatal: the
 * activity list is a convenience, and a phone whose storage was cleared should
 * come up with an empty list rather than a broken app.
 */
export function loadActivities(storage: Pick<Storage, "getItem"> | null): readonly Activity[] {
  if (storage === null) return [];
  let raw: string | null = null;
  try {
    raw = storage.getItem(STORAGE_KEY);
  } catch {
    return [];
  }
  if (raw === null || raw === "") return [];
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    const now = Date.now();
    const kept = parsed.filter((item): item is Activity => isActivity(item) && !expired(item, now));
    return kept.slice(-MAX_ACTIVITIES);
  } catch {
    return [];
  }
}

/** Saves the list, dropping what is too old to matter. */
export function saveActivities(storage: Pick<Storage, "setItem"> | null, activities: readonly Activity[]): void {
  if (storage === null) return;
  const now = Date.now();
  const kept = activities.filter((item) => !expired(item, now)).slice(-MAX_ACTIVITIES);
  try {
    storage.setItem(STORAGE_KEY, JSON.stringify(kept));
  } catch {
    // A full or disabled store costs the history and nothing else.
  }
}

/** When the reader last acknowledged what had arrived. */
export function loadReadAt(storage: Pick<Storage, "getItem"> | null): string | null {
  if (storage === null) return null;
  try {
    const value = storage.getItem(READ_KEY);
    return value === null || value === "" ? null : value;
  } catch {
    return null;
  }
}

/** Records that the reader has seen everything up to `time`. */
export function saveReadAt(storage: Pick<Storage, "setItem"> | null, time: string): void {
  if (storage === null) return;
  try {
    storage.setItem(READ_KEY, time);
  } catch {
    // As above: the marker is a convenience, and losing it costs one badge.
  }
}

/** Adds one activity to the list, newest last.
 *
 * A re-delivered frame is dropped rather than appended: the gateway replays
 * events from a cursor after a reconnect, and the same completion arriving twice
 * must not read as two pieces of work.
 */
export function recordActivity(activities: readonly Activity[], activity: Activity): readonly Activity[] {
  if (activities.some((item) => item.id === activity.id)) return activities;
  const now = Date.now();
  return [...activities.filter((item) => !expired(item, now)), activity].slice(-MAX_ACTIVITIES);
}

/**
 * How many activities are newer than the reader's marker.
 *
 * A timestamp marker rather than a set of read ids: the list is append-only and
 * ordered, so "everything up to this moment" is smaller to store and cannot get
 * the count wrong as old rows fall off the end.
 */
export function unreadCount(activities: readonly Activity[], readAt: string | null): number {
  if (readAt === null) return activities.length;
  const seen = Date.parse(readAt);
  if (Number.isNaN(seen)) return activities.length;
  let count = 0;
  for (const activity of activities) {
    const at = Date.parse(activity.time);
    if (!Number.isNaN(at) && at > seen) count += 1;
  }
  return count;
}

/** Whether a row is older than the list is willing to carry. */
function expired(activity: Activity, now: number): boolean {
  const at = Date.parse(activity.time);
  if (Number.isNaN(at)) return false;
  return now - at > MAX_AGE_MS;
}

/** Narrows one parsed row. Only the fields a row cannot be drawn without. */
function isActivity(value: unknown): value is Activity {
  if (typeof value !== "object" || value === null) return false;
  const row = value as Record<string, unknown>;
  return (
    typeof row["id"] === "string" &&
    typeof row["kind"] === "string" &&
    typeof row["outcome"] === "string" &&
    typeof row["sessionId"] === "string" &&
    typeof row["time"] === "string" &&
    typeof row["summary"] === "string" &&
    typeof row["detail"] === "string" &&
    typeof row["actor"] === "object" &&
    row["actor"] !== null
  );
}
