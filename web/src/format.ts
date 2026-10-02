/**
 * Presentation helpers. Pure functions only — no DOM, no store — so they are
 * trivial to reason about and reused across views without coupling.
 */

import type { ModelOption } from "./types.js";

/**
 * The pairing alphabet from the contract: no `I`, `L`, `O`, `0` or `1`, because
 * those are the pairs a human misreads when copying a code off a desktop
 * screen. Keeping the same alphabet client-side means a mistyped character is
 * dropped instead of being sent and rejected.
 */
export const PAIRING_ALPHABET = "ABCDEFGHJKMNPQRSTVWXYZ23456789";
export const PAIRING_CODE_LENGTH = 8;

/** Uppercases and drops every character outside the alphabet, then caps length. */
export function normalizePairingCode(raw: string): string {
  let out = "";
  for (const char of raw.toUpperCase()) {
    if (PAIRING_ALPHABET.includes(char)) out += char;
    if (out.length === PAIRING_CODE_LENGTH) break;
  }
  return out;
}

export function isPairingCodeComplete(code: string): boolean {
  return code.length === PAIRING_CODE_LENGTH;
}

/* ------------------------------------------------------------------ numbers */

export function utf8Length(text: string): number {
  return new TextEncoder().encode(text).byteLength;
}

/** Token counts get long fast; `5195` reads better as `5.2k`. */
export function formatTokens(count: number): string {
  if (count < 1000) return String(count);
  if (count < 1_000_000) return `${(count / 1000).toFixed(count < 10_000 ? 1 : 0)}k`;
  return `${(count / 1_000_000).toFixed(1)}M`;
}

/* -------------------------------------------------------------------- time */

function parse(iso: string): Date | null {
  if (iso === "") return null;
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? null : date;
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/** Compact age for list rows: `now`, `12m`, `3h`, `6d`, then a date. */
/** Renders a duration in seconds as "12m" / "3h 20m" / "2d 4h". */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "0s";
  if (seconds < 60) return `${Math.round(seconds)}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) {
    const rest = minutes % 60;
    return rest === 0 ? `${hours}h` : `${hours}h ${rest}m`;
  }
  const days = Math.floor(hours / 24);
  const rest = hours % 24;
  return rest === 0 ? `${days}d` : `${days}d ${rest}h`;
}

/**
 * Renders money in the currency's own format.
 *
 * `Intl.NumberFormat` rather than a symbol table, because a table only ever
 * covers the currencies its author thought of: an unknown one rendered as
 * "0.42 EUR", which reads as a bug. `Intl` knows every currency, puts the symbol
 * on the correct side for the locale (¥100 in ja-JP, 100 € in de-DE), and
 * degrades to the ISO code on its own for anything it does not recognise.
 *
 * Four decimal places for amounts below a cent, because a cheap session costs
 * less than that and "0.00" would read as "this was free" — so the digits are
 * computed first and handed to `Intl` as an explicit range.
 */
export function formatMoney(amount: number, currency: string): string {
  const magnitude = Math.abs(amount);
  const digits = magnitude > 0 && magnitude < 0.01 ? 4 : 2;
  try {
    return new Intl.NumberFormat(undefined, {
      style: "currency",
      currency,
      minimumFractionDigits: digits,
      maximumFractionDigits: digits,
    }).format(amount);
  } catch {
    // An unknown or malformed currency code throws rather than falling back.
    // Showing the number with its code is better than showing nothing.
    return `${amount.toFixed(digits)} ${currency}`;
  }
}

export function relativeTime(iso: string, now: number = Date.now()): string {
  const date = parse(iso);
  if (date === null) return "";
  const delta = now - date.getTime();
  if (delta < 0) return formatClock(date);
  if (delta < MINUTE) return "now";
  if (delta < HOUR) return `${Math.floor(delta / MINUTE)}m`;
  if (delta < DAY) return `${Math.floor(delta / HOUR)}h`;
  if (delta < 7 * DAY) return `${Math.floor(delta / DAY)}d`;
  return date.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

/** Wall-clock time, used for the `now` case in `relativeTime`. */
function formatClock(date: Date): string {
  return date.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

export function formatDateTime(iso: string): string {
  const date = parse(iso);
  if (date === null) return "unknown";
  return date.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

/** `4:59`, then `45s` under a minute. Returns `0s` once expired. */
export function formatCountdown(remainingMs: number): string {
  const clamped = Math.max(0, remainingMs);
  const totalSeconds = Math.floor(clamped / 1000);
  if (totalSeconds < 60) return `${totalSeconds}s`;
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  return `${minutes}:${String(seconds).padStart(2, "0")}`;
}

/* ------------------------------------------------------------------- paths */

/** Trims a workspace path to its last two segments for a chip-sized label. */
export function shortWorkspace(path: string): string {
  if (path === "") return "(no workspace)";
  const parts = path.split("/").filter((part) => part !== "");
  if (parts.length <= 2) return parts.join("/");
  return `…/${parts.slice(-2).join("/")}`;
}

export function basename(path: string): string {
  if (path === "") return "";
  const parts = path.split("/").filter((part) => part !== "");
  return parts[parts.length - 1] ?? path;
}

/** Human label for a model id, preferring the advertised display name. */
export function modelLabel(id: string | null, names: ReadonlyMap<string, string>): string {
  if (id === null || id === "") return "Default model";
  // A route the catalog has no name for is still readable: the harness spells
  // it as [provider, model], and the model half is the part a person
  // recognises. This is what a session read back out of a log looks like when
  // the picker's catalog has not arrived yet.
  return names.get(id) ?? modelHalf(id) ?? id;
}

export function effortLabel(id: string | null, names: ReadonlyMap<string, string>): string {
  if (id === null || id === "") return "Default effort";
  return names.get(id) ?? id;
}

/* -------------------------------------------------------------- selections */

/**
 * The model half of a compound value id, or null when the id is not one.
 *
 * ACP value ids are opaque, and the only thing this knows about them is the
 * shape the harness happens to use for a model: a JSON array of the route's
 * parts, of which the last is the model. Nothing is composed from it — it is a
 * label of last resort, and a comparison key for a route that reached the app
 * without its provider.
 */
export function modelHalf(id: string): string | null {
  if (!id.startsWith("[")) return null;
  try {
    const parts: unknown = JSON.parse(id);
    if (!Array.isArray(parts) || parts.length === 0) return null;
    const last: unknown = parts[parts.length - 1];
    return typeof last === "string" && last !== "" ? last : null;
  } catch {
    return null;
  }
}

/**
 * The gateway's default for a picker, when the catalog still offers it.
 *
 * A configured default that names a model the harness no longer lists stays as
 * "Gateway default" rather than being forced into the `<select>`: the server
 * would still apply it, but a control showing a value it does not contain is a
 * lie the user cannot see.
 */
export function offeredValue(id: string | null, options: readonly { readonly id: string }[]): string {
  return id !== null && options.some((option) => option.id === id) ? id : "";
}

/**
 * The catalog id behind what a session reports as its model, or "" when the
 * catalog cannot account for it.
 *
 * A session reports the value the gateway read: the route's value id while the
 * gateway holds the session, and the bare model id when it was read back out of
 * a session log (the log records the route's parts, not the id the picker
 * sends). A picker can only offer ids, so the bare id is matched against the
 * model half of each one. "" means nothing on offer matches — an empty catalog,
 * or a route the harness no longer carries — and the caller decides what to
 * fall back to. A display name is never expected here: rendering one is this
 * module's job, not the wire's.
 */
export function catalogModelValue(reported: string | null, models: readonly ModelOption[]): string {
  if (reported === null || reported === "") return "";
  const exact = offeredValue(reported, models);
  if (exact !== "") return exact;
  const half = models.find((model) => modelHalf(model.id) === reported);
  return half?.id ?? "";
}

export function pluralize(count: number, singular: string, plural?: string): string {
  return count === 1 ? singular : (plural ?? `${singular}s`);
}
