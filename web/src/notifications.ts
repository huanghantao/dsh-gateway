/**
 * Notifications: the one thing that reaches a reader who is not looking.
 *
 * The agent's expensive moments are the ones where it waits for a human — an
 * approval that expires, a long turn that finished while the phone was in a
 * pocket — and nothing else in this app can tell anyone about them. So this is
 * the whole subscription conversation with the gateway:
 *
 *   - ask the browser for permission (only ever from a tap: a permission prompt
 *     nobody asked for is a permission prompt nobody grants);
 *   - subscribe with the gateway's VAPID key, and hand the resulting endpoint
 *     and keys back, bound to this device;
 *   - report the state honestly, including "this browser cannot" and "you said
 *     no", because both look like "no notifications arrived" otherwise.
 */

import { api } from "./api.js";
import type { PushKey } from "./types.js";

export type PushState = "unsupported" | "insecure" | "untrusted" | "denied" | "off" | "on";

/**
 * Why the service worker could not be installed, if it could not.
 *
 * A browser refuses to install one on a page whose certificate it does not
 * trust, and without a worker there is no push at all — but the failure has to
 * be *recorded*, because the alternative is a registration whose promise never
 * settles: `navigator.serviceWorker.ready` waits forever, the button says
 * "Turning on…", and the reader is left with a dead end that names nothing.
 */
let workerFailure: string | null = null;

/** Records why the app's service worker could not be installed. */
export function noteServiceWorkerFailure(error: unknown): void {
  workerFailure = error instanceof Error ? error.message : String(error);
}

/**
 * Why push is unavailable, in a sentence the reader can act on.
 *
 * "This browser cannot receive notifications" is true and useless: the four
 * reasons it can be true — an insecure origin, a browser without the APIs, iOS
 * before the app is installed, a permission that was refused — need four
 * different actions, and a reader who is told only the conclusion has no way to
 * tell which one they are in. Hence a diagnosis rather than a boolean.
 */
export interface PushDiagnosis {
  readonly state: PushState;
  readonly detail: string;
  /** Chat channels that will deliver regardless of what the browser allows. */
  readonly channels: readonly string[];
}

/**
 * Whether a service worker and the Push API exist here.
 *
 * Both are gated on a secure context by every browser, so on `http://` this is
 * false for a reason no amount of browser support can fix — which is the first
 * thing to tell apart.
 */
export function pushSupported(): boolean {
  return (
    typeof navigator !== "undefined" &&
    "serviceWorker" in navigator &&
    "PushManager" in window &&
    "Notification" in window
  );
}

/** The full answer, including what to do about it. */
export async function describePush(): Promise<PushDiagnosis> {
  if (typeof window === "undefined") {
    return { state: "unsupported", detail: "Notifications need a browser window.", channels: [] };
  }
  // Checked first, because it explains the other three checks failing at once.
  const key = await pushKeyQuietly();
  if (!window.isSecureContext) {
    // The correct address comes from the gateway rather than from a guess here:
    // a client on the wrong origin knows the origin it is on, not the one the
    // operator configured.
    let where = "";
    const { appURL } = key;
    if (appURL !== "" && !appURL.startsWith("http://")) where = `${appURL.replace(/\/$/, "")}/m/`;
    return {
      channels: key.channels,
      state: "insecure",
      detail:
        `Notifications need a secure connection, and this page is ${window.location.origin}. ` +
        "Browsers only expose Service Worker and Push over https (or localhost). " +
        (where === "" ? "Open this gateway's https address instead." : `Open ${where} instead.`),
    };
  }
  if (!("serviceWorker" in navigator)) {
    return { channels: key.channels, state: "unsupported", detail: "This browser has no service worker support." };
  }
  if (!("PushManager" in window)) {
    return {
      channels: key.channels,
      state: "unsupported",
      detail: "This browser has no Push API. In-app browsers and WebViews often do not; open the gateway in a full browser — Safari on iOS, Chrome or Firefox elsewhere.",
    };
  }
  if (!("Notification" in window)) {
    return { channels: key.channels, state: "unsupported", detail: "This browser cannot show notifications." };
  }
  if (isIOS() && !isInstalled()) {
    return {
      channels: key.channels,
      state: "unsupported",
      detail: "On iOS, add this app to the Home Screen first: Safari only delivers Web Push to installed apps.",
    };
  }
  if (workerFailure !== null) {
    const { caURL } = key;
    return {
      channels: key.channels,
      state: "untrusted",
      detail:
        "This browser refused to install the service worker notifications need" +
        (caURL === "" ? "." : `: it does not trust this site's certificate. Install the gateway's certificate authority (${caURL}) and reopen the app.`) +
        " On a phone the certificate has to be installed as a CA certificate, not accepted once for a page.",
    };
  }
  if (Notification.permission === "denied") {
    return {
      channels: key.channels,
      state: "denied",
      detail: "Notifications are blocked for this app in the browser's own settings. Allow them there, then reopen this screen.",
    };
  }

  const reg = await registration();
  if (reg === null || (await reg.pushManager.getSubscription()) === null) {
    return { channels: key.channels, state: "off", detail: "" };
  }
  return { channels: key.channels, state: "on", detail: "" };
}

/** The registration, or a failure that explains itself. */
async function serviceWorkerReady(): Promise<ServiceWorkerRegistration> {
  const timeout = new Promise<never>((_, reject) => {
    setTimeout(
      () =>
        reject(
          new Error(
            workerFailure ??
              "The browser did not install the service worker notifications need. If this site uses its own certificate authority, install that certificate on this device and reopen the app.",
          ),
        ),
      8000,
    );
  });
  return Promise.race([navigator.serviceWorker.ready, timeout]);
}

/** The push key, or empty defaults if the gateway cannot be asked right now. */
async function pushKeyQuietly(): Promise<PushKey> {
  try {
    return await api.pushKey();
  } catch {
    return { enabled: false, publicKey: "", turnThresholdSeconds: 0, appURL: "", caURL: "", channels: [] };
  }
}

/** iOS and iPadOS, including the iPad that reports itself as a Mac. */
function isIOS(): boolean {
  const ua = navigator.userAgent;
  if (/iP(hone|ad|od)/.test(ua)) return true;
  return navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1;
}

/** Whether this page is running as the installed app rather than a tab. */
function isInstalled(): boolean {
  const standalone = (navigator as { standalone?: boolean }).standalone === true;
  return standalone || window.matchMedia("(display-mode: standalone)").matches;
}

/** Finds the registration the app already has, if the worker is installed. */
async function registration(): Promise<ServiceWorkerRegistration | null> {
  if (!pushSupported()) return null;
  return (await navigator.serviceWorker.getRegistration()) ?? null;
}

/** What the settings screen shows. */
export async function pushState(): Promise<PushState> {
  return (await describePush()).state;
}

/**
 * Subscribes this browser and tells the gateway.
 *
 * `applicationServerKey` must be an ArrayBuffer of the raw key, and the API
 * hands out base64url — the conversion is the usual place this fails, and it
 * fails silently, with a subscription that no push service will accept.
 */
export async function enablePush(): Promise<PushState> {
  const diagnosis = await describePush();
  if (diagnosis.state !== "off") return diagnosis.state;

  const permission = await Notification.requestPermission();
  if (permission !== "granted") return permission === "denied" ? "denied" : "off";

  const key = await api.pushKey();
  if (!key.enabled || key.publicKey === "") {
    throw new Error("This gateway does not send notifications.");
  }
  // `ready` never settles when no registration exists, so a page that cannot
  // install a worker would hang here instead of saying why.
  const reg = await serviceWorkerReady();
  let subscription: PushSubscription;
  try {
    subscription = await reg.pushManager.subscribe({
      // Every push service requires this: a message must be shown to a person.
      userVisibleOnly: true,
      applicationServerKey: decodeBase64URL(key.publicKey),
    });
  } catch (cause: unknown) {
    // The usual reasons are a device with no push service at all (some phones
    // ship without Google Play services) and a browser that has disabled it.
    // Neither is something the reader can guess from "AbortError".
    const because = cause instanceof Error && cause.message !== "" ? ` (${cause.message})` : "";
    throw new Error(
      `The browser refused to create a push subscription${because}. ` +
        "A device without a push service — a phone without Google Play services, for instance — cannot receive Web Push.",
    );
  }

  const serialized = subscription.toJSON() as {
    endpoint?: string;
    keys?: { p256dh?: string; auth?: string };
  };
  if (!serialized.endpoint || !serialized.keys?.p256dh || !serialized.keys.auth) {
    await subscription.unsubscribe();
    throw new Error("The browser returned an incomplete subscription.");
  }
  await api.pushSubscribe({
    endpoint: serialized.endpoint,
    p256dh: serialized.keys.p256dh,
    auth: serialized.keys.auth,
  });
  return "on";
}

/** Unsubscribes, locally and at the gateway. */
export async function disablePush(): Promise<void> {
  const reg = await registration();
  if (reg === null) return;
  const subscription = await reg.pushManager.getSubscription();
  if (subscription === null) return;
  // Tell the gateway first: if this fails, the browser keeps a subscription the
  // gateway still believes in, which is repairable; the other order leaves the
  // gateway sending into an endpoint that no longer exists.
  await api.pushUnsubscribe(subscription.endpoint);
  await subscription.unsubscribe();
}

/**
 * Routes a tap on a notification to the screen it is about.
 *
 * The worker focuses the existing window and posts the target here rather than
 * navigating it, because this page owns its hash router: a navigation from
 * outside would reload the app and lose whatever the reader was doing.
 */
export function bindNotificationClicks(): () => void {
  if (!pushSupported()) return () => undefined;
  const onMessage = (event: MessageEvent<unknown>): void => {
    const data = event.data;
    if (typeof data !== "object" || data === null) return;
    const { type, url } = data as { type?: unknown; url?: unknown };
    if (type !== "notification-click" || typeof url !== "string") return;
    const hash = url.includes("#") ? url.slice(url.indexOf("#")) : "#/sessions";
    window.location.hash = hash;
  };
  navigator.serviceWorker.addEventListener("message", onMessage);
  return () => navigator.serviceWorker.removeEventListener("message", onMessage);
}

/** Asks the gateway to send one notification to this device now. */
export async function sendTestNotification(): Promise<void> {
  await api.pushTest();
}

/**
 * Decodes base64url into the ArrayBuffer `subscribe` wants.
 *
 * The padding has to go: `atob` accepts it, but the keys the gateway hands out
 * are unpadded by construction and a padded copy would be a different string.
 */
function decodeBase64URL(value: string): ArrayBuffer {
  const padded = value.replace(/-/g, "+").replace(/_/g, "/");
  const binary = atob(padded + "=".repeat((4 - (padded.length % 4)) % 4));
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
  return bytes.buffer;
}
