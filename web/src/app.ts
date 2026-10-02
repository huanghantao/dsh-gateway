/**
 * Router, app shell and bootstrap.
 *
 * The store holds the current route; the URL hash is only an input to it. That
 * inversion matters because several navigations are *decided* rather than
 * requested — landing on `#/sessions` while unpaired must go to `#/pair`, and a
 * deep link to a conversation must wait for bootstrap. Driving the views from
 * `store.route` means there is exactly one place that answers "what is on
 * screen", instead of a `hashchange` handler racing a fetch.
 *
 * The shell is also where the two always-present region live: the approval host
 * (Screen 4 must be answerable from any screen) and the notice host, which is a
 * polite live region so a transient confirmation is announced rather than
 * merely shown.
 */

import { EventClient, bindConnectivity } from "./events.js";
import { createContext, hashFor, routeFromHash, type Ctx } from "./actions.js";
import { bindNotificationClicks, noteServiceWorkerFailure } from "./notifications.js";
import { el, mustFind, on } from "./dom.js";
import type { AppState } from "./store.js";
import { createStore, type ConnectionStatus, type Route } from "./store.js";
import type { ServerEvent } from "./types.js";
import { mountApprovals } from "./views/approval.js";
import { mountConversation } from "./views/conversation.js";
import { mountPair } from "./views/pair.js";
import { mountSessions } from "./views/sessions.js";
import { mountSettings } from "./views/settings.js";

const CONNECTION_LABEL: Record<ConnectionStatus, string> = {
  open: "Live",
  connecting: "Connecting",
  closed: "Reconnecting",
  offline: "Offline",
};

function sameRoute(a: Route, b: Route): boolean {
  if (a.kind !== b.kind) return false;
  if (a.kind === "conversation" && b.kind === "conversation") return a.sessionId === b.sessionId;
  // A second scan while the pair screen is already open must re-mount it with
  // the new code, so the code is part of the route's identity.
  if (a.kind === "pair" && b.kind === "pair") return (a.code ?? "") === (b.code ?? "");
  return true;
}

/**
 * Registers the service worker. Failures are swallowed on purpose: a browser
 * without service workers, or a page served over plain HTTP on a LAN, still
 * gets a fully working app — it just is not installable.
 *
 * Static assets are served stale-while-revalidate, so the launch after a deploy
 * runs the previous bundle and only the one after that runs the new one. That is
 * a poor way to learn a fix has shipped — the reader sees the old screen twice
 * and concludes nothing changed — so when a new worker takes over an existing
 * page, the page reloads itself onto the bundle that is already cached.
 *
 * The `controller` guard is what keeps that from looping: on a first visit there
 * is no controller to replace, and the install itself must not reload the page.
 */
function registerServiceWorker(): void {
  if (!("serviceWorker" in navigator)) return;
  if (window.location.protocol !== "http:" && window.location.protocol !== "https:") return;
  const hadController = navigator.serviceWorker.controller !== null;
  navigator.serviceWorker.addEventListener("controllerchange", () => {
    if (!hadController) return;
    window.location.reload();
  });
  // The failure is kept, not swallowed: notifications cannot work without a
  // worker, and a browser refuses to install one on a page whose certificate it
  // does not trust. Silently ignoring that turns a one-line fix into "the button
  // does nothing".
  void navigator.serviceWorker.register("./sw.js").catch((error: unknown) => {
    noteServiceWorkerFailure(error);
  });
}

export function main(): void {
  const root = mustFind<HTMLElement>(document.body, "#app");
  const noticeHost = mustFind<HTMLElement>(document.body, "#notice-host");

  const store = createStore();

  // `createContext` needs the client and the client needs a handler that routes
  // back into the context; a late-bound dispatcher breaks the cycle without a
  // null check at every frame.
  let dispatch: (event: ServerEvent) => void = () => undefined;
  let dispatchStale: () => void = () => undefined;
  const events = new EventClient({
    onEvent: (event) => {
      dispatch(event);
    },
    onStatus: (status, detail) => {
      store.patch({ connection: status, connectionDetail: detail });
    },
    onStale: () => {
      dispatchStale();
    },
  });
  const ctx: Ctx = createContext(store, events);
  dispatch = (event) => {
    ctx.handleEvent(event);
  };
  dispatchStale = () => {
    ctx.handleStale();
  };

  /* --------------------------------------------------------------- shell */

  const statusPill = el("span", { class: "status-pill" });
  const appBar = el(
    "header",
    { class: "app-bar" },
    el("span", { class: "app-name", text: "dsh-gateway" }),
    el("span", { class: "status", attrs: { role: "status", "aria-live": "polite" } }, statusPill),
  );

  const viewRoot = el("main", { class: "view-root", attrs: { id: "view-root" } });
  const tabBar = el("nav", { class: "tab-bar", attrs: { "aria-label": "Sections" } });
  root.append(appBar, viewRoot, tabBar);

  const tab = (label: string, route: Route): HTMLElement => {
    const node = el("a", {
      class: "tab",
      attrs: { href: hashFor(route) },
      text: label,
      on: {
        click: (event) => {
          // The store owns the route, so the link is intercepted rather than
          // allowed to change the hash behind the router's back.
          event.preventDefault();
          ctx.navigate(route);
        },
      },
    });
    return node;
  };

  const sessionsTab = tab("Sessions", { kind: "sessions" });
  const settingsTab = tab("Settings", { kind: "settings" });
  tabBar.append(sessionsTab, settingsTab);

  const syncTabs = (route: Route, unpaired: boolean): void => {
    const conversation = route.kind === "conversation";
    root.classList.toggle("app-conversation", conversation);
    // Nothing to navigate to while unpaired, so the bar is hidden rather than
    // offering two links that bounce straight back to the pair screen.
    root.classList.toggle("app-unpaired", unpaired);
    const onSessions = !unpaired && (route.kind === "sessions" || conversation);
    const onSettings = !unpaired && route.kind === "settings";
    sessionsTab.classList.toggle("is-current", onSessions);
    settingsTab.classList.toggle("is-current", onSettings);
    if (onSessions) sessionsTab.setAttribute("aria-current", "page");
    else sessionsTab.removeAttribute("aria-current");
    if (onSettings) settingsTab.setAttribute("aria-current", "page");
    else settingsTab.removeAttribute("aria-current");
  };

  const setPill = (label: string, variant: string): void => {
    statusPill.textContent = label;
    statusPill.className = `status-pill status-${variant}`;
  };

  const syncStatus = (status: ConnectionStatus, offline: boolean, boot: AppState["boot"]): void => {
    // Before pairing there is no stream to describe; "Reconnecting" would be a
    // promise the app is not keeping.
    if (boot === "unauthenticated") {
      setPill("Not paired", "unpaired");
      return;
    }
    const effective: ConnectionStatus = offline ? "offline" : status;
    setPill(CONNECTION_LABEL[effective], effective);
  };

  /* --------------------------------------------------------- view mounting */

  let unmount: (() => void) | null = null;

  const show = (mount: (host: HTMLElement, context: Ctx) => () => void): void => {
    unmount?.();
    viewRoot.replaceChildren();
    unmount = mount(viewRoot, ctx);
  };

  const showBoot = (): void => {
    show((host) => {
      const node = el(
        "section",
        { class: "view view-boot" },
        el("p", { class: "loading", attrs: { role: "status" }, text: "Contacting the gateway…" }),
      );
      host.appendChild(node);
      return () => {
        node.remove();
      };
    });
  };

  const showBootFailure = (message: string): void => {
    show((host) => {
      const node = el(
        "section",
        { class: "view view-boot" },
        el(
          "div",
          { class: "card" },
          el("h1", { class: "pair-title", text: "Cannot reach the gateway" }),
          el("p", { class: "alert alert-error", attrs: { role: "alert" }, text: message }),
          el("button", {
            class: "btn btn-primary btn-block",
            attrs: { type: "button" },
            text: "Try again",
            on: {
              click: () => {
                void ctx.bootstrap();
              },
            },
          }),
          el("button", {
            class: "btn btn-ghost btn-block",
            attrs: { type: "button" },
            text: "Pair a device instead",
            on: {
              click: () => {
                ctx.navigate({ kind: "pair" });
              },
            },
          }),
        ),
      );
      host.appendChild(node);
      return () => {
        node.remove();
      };
    });
  };

  const openConversation = (sessionId: string): void => {
    const active = store.state.active;
    if (active !== null && active.sessionId !== sessionId) ctx.closeSession();
    const current = store.state.active;
    if (current === null || current.sessionId !== sessionId) void ctx.openSession(sessionId);
  };

  const applyRoute = (): void => {
    const state = store.state;
    syncTabs(state.route, state.boot === "unauthenticated");

    if (state.boot === "starting") {
      showBoot();
      return;
    }
    if (state.boot === "failed") {
      showBootFailure(state.bootError ?? "The gateway did not answer.");
      return;
    }
    if (state.boot === "unauthenticated") {
      if (state.route.kind !== "pair") {
        ctx.navigate({ kind: "pair" });
        return;
      }
      show(mountPair);
      return;
    }

    // Paired: `#/` and `#/pair` are not destinations any more.
    if (state.route.kind === "boot" || state.route.kind === "pair") {
      ctx.navigate({ kind: "sessions" });
      return;
    }

    switch (state.route.kind) {
      case "sessions":
        if (state.active !== null) ctx.closeSession();
        show(mountSessions);
        return;
      case "settings":
        if (state.active !== null) ctx.closeSession();
        show(mountSettings);
        return;
      case "conversation":
        openConversation(state.route.sessionId);
        show(mountConversation);
        return;
    }
  };

  // Re-entrancy guard: an `applyRoute` may itself navigate (see above), and that
  // navigation commits a route change which would otherwise call straight back
  // into here before the current pass finished.
  let applying = false;
  const routeChanged = (): void => {
    if (applying) return;
    applying = true;
    try {
      applyRoute();
    } finally {
      applying = false;
    }
  };

  /* ---------------------------------------------------------- subscriptions */

  const offRoute = store.select(
    (state) => [state.route, state.boot] as const,
    () => {
      routeChanged();
    },
    (a, b) => sameRoute(a[0], b[0]) && a[1] === b[1],
  );

  const offStatus = store.select(
    (state) => [state.connection, state.offline, state.boot] as const,
    ([connection, offline, boot]) => {
      syncStatus(connection, offline, boot);
    },
    (a, b) => a[0] === b[0] && a[1] === b[1] && a[2] === b[2],
  );

  const offNotice = store.select(
    (state) => state.notice,
    (notice) => {
      // `aria-live` is on the host in index.html; replacing its text is what
      // triggers the announcement.
      noticeHost.textContent = notice ?? "";
      noticeHost.classList.toggle("is-visible", notice !== null);
    },
  );

  const offHash = on(window, "hashchange", () => {
    const next = routeFromHash(window.location.hash);
    if (!sameRoute(next, store.state.route)) store.patch({ route: next });
  });

  const syncOnline = (): void => {
    store.patch({ offline: !navigator.onLine });
  };
  const offOnline = on(window, "online", syncOnline);
  const offOffline = on(window, "offline", syncOnline);
  const unbindConnectivity = bindConnectivity(events);

  // Mounted on `body`, not inside `#app`: the shell is made inert while a decision
  // is pending, and the sheet has to stay interactive through that.
  mountApprovals(document.body, ctx);
  registerServiceWorker();
  bindNotificationClicks();

  // The initial route comes from the URL before anything else runs, so a deep
  // link is honoured rather than being overwritten by the default.
  const initial = routeFromHash(window.location.hash);
  if (!sameRoute(initial, store.state.route)) store.patch({ route: initial });
  routeChanged();

  void ctx.bootstrap().catch(() => {
    store.patch({ boot: "failed", bootError: "Bootstrap failed unexpectedly." });
  });

  window.addEventListener("pagehide", () => {
    offRoute();
    offStatus();
    offNotice();
    offHash();
    offOnline();
    offOffline();
    unbindConnectivity();
    unmount?.();
  });
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", main, { once: true });
} else {
  main();
}
