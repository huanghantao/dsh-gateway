/**
 * Screen 5 — Settings.
 *
 * Three unrelated concerns share one screen because on a phone each is a rare,
 * deliberate visit: the state of the connection, the devices that can reach the
 * gateway, and which model the next turn will use.
 *
 * Two contract quirks are made visible here rather than papered over:
 *
 * - There is no sign-out endpoint. A browser session ends by revoking its own
 *   device, which the contract defines as immediate, so the button says exactly
 *   that.
 * - `PATCH /sessions/{id}` needs a session, and settings has no implicit one, so
 *   the model picker is bound to an explicit session chooser rather than
 *   silently doing nothing when no conversation is open.
 */

import { ApiError, api } from "../api.js";
import type { Ctx } from "../actions.js";
import { el, on } from "../dom.js";
import { catalogModelValue, effortLabel, formatDateTime, modelLabel, offeredValue, relativeTime } from "../format.js";
import type { Readiness } from "../types.js";
import { describePush, disablePush, enablePush, sendTestNotification, type PushState } from "../notifications.js";
import { badge, openSheet, selectField } from "./ui.js";

export function mountSettings(root: HTMLElement, ctx: Ctx): () => void {
  const scroll = el("div", { class: "scroll" });
  const view = el("section", { class: "view view-settings" }, scroll);
  root.appendChild(view);

  /** Probed on demand: `GET /healthz` is cheap but not worth polling. */
  let health: boolean | null = null;
  let healthChecked = false;
  let readiness: Readiness = "starting";
  let pickerSession: string | null = null;
  /** Which session the picker values were seeded from. */
  let pickerValuesFor: string | null = null;
  /** The operator has changed a picker since it was last seeded. */
  let pickerTouched = false;
  let modelValue = "";
  let effortValue = "";

  const checkHealth = async (): Promise<void> => {
    healthChecked = false;
    health = null;
    render();
    const [alive, ready] = await Promise.all([api.health(), api.ready()]);
    health = alive;
    readiness = ready;
    healthChecked = true;
    render();
  };

  /* ------------------------------------------------------------ sections */

  function connectionSection(): HTMLElement {
    const state = ctx.store.state;
    const rows = el("div", { class: "rows rows-plain" });

    const add = (label: string, value: string, variant: "ok" | "error" | "muted"): void => {
      rows.appendChild(
        el(
          "div",
          { class: "setting-row" },
          el("span", { class: "setting-label", text: label }),
          el("span", { class: "setting-value" }, badge(value, variant)),
        ),
      );
    };

    add("Network", state.offline ? "offline" : "online", state.offline ? "error" : "ok");
    add(
      "Event stream",
      state.connectionDetail === null ? state.connection : `${state.connection} — ${state.connectionDetail}`,
      state.connection === "open" ? "ok" : state.connection === "connecting" ? "muted" : "error",
    );
    add(
      "Gateway",
      !healthChecked ? "checking…" : health === true ? "reachable" : health === false ? "refused" : "unreachable",
      !healthChecked ? "muted" : health === true ? "ok" : "error",
    );
    add(
      "Agent",
      state.harness.state === "ready" ? "ready" : `${state.harness.state}${state.harness.detail === null ? "" : ` — ${state.harness.detail}`}`,
      state.harness.state === "ready" ? "ok" : "error",
    );
    add("Readiness probe", readiness, readiness === "ready" ? "ok" : "muted");

    return el(
      "section",
      { class: "card" },
      el("h2", { class: "card-title", text: "Connection" }),
      rows,
      el(
        "div",
        { class: "card-actions" },
        el("button", {
          class: "btn btn-ghost",
          attrs: { type: "button" },
          text: "Reconnect stream",
          on: {
            click: () => {
              ctx.reconnect();
            },
          },
        }),
        el("button", {
          class: "btn btn-ghost",
          attrs: { type: "button" },
          text: "Re-check",
          on: {
            click: () => {
              void checkHealth();
            },
          },
        }),
      ),
    );
  }

  function devicesSection(): HTMLElement {
    const state = ctx.store.state;
    const list = el("div", { class: "rows rows-plain" });

    if (state.devicesError !== null) {
      list.appendChild(el("p", { class: "alert alert-error", attrs: { role: "alert" }, text: state.devicesError }));
    }
    if (state.devices.length === 0) {
      list.appendChild(el("p", { class: "muted", text: "No devices enrolled." }));
    }

    for (const device of state.devices) {
      const isSelf = state.principal?.id === device.id;
      list.appendChild(
        el(
          "div",
          { class: "setting-row" },
          el(
            "span",
            { class: "setting-value" },
            el("span", { class: "device-name", text: device.name }),
            isSelf ? badge("this device", "ok") : null,
            el("span", {
              class: "device-meta",
              text: `added ${relativeTime(device.createdAt)} · expires ${formatDateTime(device.expiresAt)}`,
            }),
          ),
          el("button", {
            class: "btn btn-danger btn-small",
            attrs: { type: "button" },
            text: isSelf ? "Sign out" : "Revoke",
            on: {
              click: () => {
                confirmRevoke(device.id, device.name, isSelf);
              },
            },
          }),
        ),
      );
    }

    return el(
      "section",
      { class: "card" },
      el("h2", { class: "card-title", text: "Devices" }),
      el("p", { class: "muted", text: "Every device that can reach this gateway. Revoking takes effect immediately." }),
      list,
    );
  }

  function confirmRevoke(id: string, name: string, isSelf: boolean): void {
    const error = el("p", { class: "alert alert-error", attrs: { role: "alert", hidden: "" } });
    const confirm = el("button", {
      class: "btn btn-danger btn-block",
      attrs: { type: "button" },
      text: isSelf ? "Sign out this device" : `Revoke ${name}`,
    });
    const body = el(
      "div",
      { class: "stack" },
      el("p", {
        text: isSelf
          ? "This is the device you are using. Revoking it signs you out immediately and you will need a new pairing code."
          : `“${name}” will lose access immediately. Use this if a device was lost or is no longer trusted.`,
      }),
      error,
      confirm,
    );
    const sheet = openSheet({ title: isSelf ? "Sign out" : "Revoke device", body });

    on(confirm, "click", () => {
      confirm.disabled = true;
      void ctx
        .revokeDevice(id)
        .then(() => {
          sheet.close();
        })
        .catch((cause: unknown) => {
          error.textContent = cause instanceof ApiError ? cause.message : "The device could not be revoked.";
          error.removeAttribute("hidden");
          confirm.disabled = false;
        });
    });
  }

  function modelSection(): HTMLElement {
    const state = ctx.store.state;
    const body = el("div", { class: "stack" });

    if (state.sessionsError !== null) {
      body.appendChild(el("p", { class: "alert alert-error", text: state.sessionsError }));
    }
    if (state.modelsError !== null) {
      body.appendChild(el("p", { class: "alert alert-error", text: state.modelsError }));
    }
    if (state.sessions.length === 0) {
      body.appendChild(el("p", { class: "muted", text: "No sessions yet. Open or create one first." }));
      return el("section", { class: "card" }, el("h2", { class: "card-title", text: "Model and reasoning effort" }), body);
    }

    // Default the picker to the session the user is actually looking at.
    const fallback = state.active?.sessionId ?? state.sessions[0]?.id ?? null;
    if (pickerSession === null || !state.sessions.some((session) => session.id === pickerSession)) {
      pickerSession = fallback;
    }
    const selected = state.sessions.find((session) => session.id === pickerSession) ?? null;
    const names = new Map(state.models.models.map((option) => [option.id, option.name]));
    const effortNames = new Map(state.models.reasoningEfforts.map((option) => [option.id, option.name]));

    // What the pickers start on, in the order of what the operator may assume:
    // the session's own route when the harness still offers it, otherwise the
    // gateway's default for new sessions. Either way it is a catalog id, so the
    // row they read is one they could apply. "Leave unchanged" is left for the
    // case where neither resolves: a catalog that is empty, or a default naming
    // a model the harness no longer offers.
    const sessionModel = catalogModelValue(selected?.model ?? null, state.models.models);
    // A session reports its effort as the value id already, so this is the
    // same check the default gets — unlike the model, which the log spells
    // without its provider.
    const sessionEffort = offeredValue(selected?.reasoningEffort ?? null, state.models.reasoningEfforts);
    const seedModel = sessionModel !== "" ? sessionModel : offeredValue(state.models.defaults.model, state.models.models);
    const seedEffort =
      sessionEffort !== "" ? sessionEffort : offeredValue(state.models.defaults.reasoningEffort, state.models.reasoningEfforts);

    // Seed the pickers from the chosen session, but only when the choice
    // changed, when a catalog that arrived late can now resolve what the last
    // render had to leave blank, or when the session's own route moved under
    // it. Re-seeding on every store commit would silently discard a selection
    // the moment any unrelated event arrived, so a picker the operator has
    // touched is left alone until they apply it or pick another session.
    const resolvedSinceSeed = modelValue !== seedModel || effortValue !== seedEffort;
    if (selected !== null && (pickerValuesFor !== selected.id || (!pickerTouched && resolvedSinceSeed))) {
      pickerValuesFor = selected.id;
      modelValue = seedModel;
      effortValue = seedEffort;
      pickerTouched = false;
    }

    body.appendChild(
      selectField(
        "settings-session",
        "Session",
        state.sessions.map((session) => ({ value: session.id, label: `${session.title} — ${modelLabel(session.model, names)}` })),
        pickerSession ?? "",
        (value) => {
          pickerSession = value;
          render();
        },
      ),
    );
    body.appendChild(
      selectField(
        "settings-model",
        "Model",
        [{ value: "", label: "Leave unchanged" }, ...state.models.models.map((option) => ({ value: option.id, label: option.name }))],
        modelValue,
        (value) => {
          modelValue = value;
          pickerTouched = true;
        },
      ),
    );
    body.appendChild(
      selectField(
        "settings-effort",
        "Reasoning effort",
        [
          { value: "", label: "Leave unchanged" },
          ...state.models.reasoningEfforts.map((option) => ({ value: option.id, label: option.name })),
        ],
        effortValue,
        (value) => {
          effortValue = value;
          pickerTouched = true;
        },
      ),
    );

    if (selected !== null) {
      // "What this session runs on" and "what the picker starts on" are
      // different claims, so a picker that fell back to the gateway default
      // says why instead of passing one off as the other. A route the catalog
      // no longer offers is called out separately from no route at all: the
      // first is a session that will fail the moment it is prompted.
      const notes: string[] = [];
      if ((selected.model ?? "") === "") notes.push("no model of its own recorded");
      else if (sessionModel === "") notes.push("a model the catalog no longer offers");
      if ((selected.reasoningEffort ?? "") === "") notes.push("no reasoning effort of its own recorded");
      else if (sessionEffort === "") notes.push("a reasoning effort the catalog no longer offers");
      const current = `Currently ${modelLabel(selected.model, names)} at ${effortLabel(selected.reasoningEffort, effortNames)}.`;
      const fallback =
        notes.length === 0
          ? ""
          : ` This session has ${notes.join(" and ")}, so the picker starts on the gateway default (${modelLabel(seedModel === "" ? null : seedModel, names)} at ${effortLabel(seedEffort === "" ? null : seedEffort, effortNames)}); applying it pins that choice to this session.`;
      body.appendChild(el("p", { class: "muted", text: `${current}${fallback} The change applies to the next turn.` }));
    }

    const save = el("button", { class: "btn btn-primary btn-block", attrs: { type: "button" }, text: "Apply to session" });
    save.disabled = pickerSession === null || (modelValue === "" && effortValue === "");
    const status = el("p", { class: "muted", attrs: { role: "status" }, text: "" });

    on(save, "click", () => {
      if (pickerSession === null) return;
      save.disabled = true;
      status.textContent = "Saving…";
      void ctx
        .updateSessionModel(pickerSession, modelValue === "" ? null : modelValue, effortValue === "" ? null : effortValue)
        .then(() => {
          status.textContent = "Saved.";
        })
        .catch((cause: unknown) => {
          status.textContent = cause instanceof ApiError ? cause.message : "The session could not be updated.";
        })
        .finally(() => {
          save.disabled = false;
        });
    });

    body.appendChild(save);
    body.appendChild(status);

    return el("section", { class: "card" }, el("h2", { class: "card-title", text: "Model and reasoning effort" }), body);
  }

  /**
   * Notifications: the switch, the state, and a way to prove it works.
   *
   * The state is read from the browser rather than remembered, because the
   * browser is the authority: permission can be withdrawn in its settings, and
   * a screen that says "on" while the browser refuses to deliver is worse than
   * one that says nothing.
   */
  function notificationsSection(): HTMLElement {
    let state: PushState = "off";
    let detail = "";
    let channels: readonly string[] = [];

    const body = el("div", { class: "stack" });
    const status = el("p", { class: "muted", attrs: { role: "status" }, text: "Checking…" });
    const toggle = el("button", { class: "btn btn-block", attrs: { type: "button" }, text: "…" });
    const test = el("button", {
      class: "btn btn-ghost btn-block",
      attrs: { type: "button" },
      text: "Send a test notification",
    });

    const paint = (): void => {
      // A chat channel delivers whatever the browser decides, so a window that
      // cannot receive Web Push is not a window without notifications — and the
      // test button has to stay reachable, because it is the only way to prove
      // the channel works.
      const bridged = channels.length > 0;
      test.hidden = !bridged && state !== "on";
      switch (state) {
        case "insecure":
        case "untrusted":
        case "unsupported":
        case "denied":
          toggle.hidden = true;
          // The diagnosis carries the sentence, because "cannot receive
          // notifications" is the conclusion and not the instruction: an
          // insecure origin, a browser without the APIs, iOS before install and
          // a refused permission each need something different from the reader.
          status.textContent = detail + (bridged ? ` ${channelSentence(channels)}` : "");
          return;
        case "on":
          toggle.hidden = false;
          toggle.textContent = "Turn off notifications";
          status.textContent =
            (detail !== "" ? detail : "On. The gateway will notify this phone about approvals and long turns.") +
            (bridged ? ` ${channelSentence(channels)}` : "");
          return;
        default:
          toggle.hidden = false;
          toggle.textContent = "Turn on notifications";
          status.textContent =
            (detail !== "" ? detail : "Off. Turn them on to be told when the agent needs you.") +
            (bridged ? ` ${channelSentence(channels)}` : "");
      }
    };

    on(toggle, "click", () => {
      toggle.disabled = true;
      status.textContent = state === "on" ? "Turning off…" : "Turning on…";
      const work = state === "on" ? disablePush() : enablePush().then((next) => void (state = next));

      void work
        .then(async () => {
          const diagnosis = await describePush();
          state = diagnosis.state;
          detail = diagnosis.detail;
          channels = diagnosis.channels;
        })
        .catch((cause: unknown) => {
          detail = cause instanceof Error ? cause.message : "That did not work.";
        })
        .finally(() => {
          toggle.disabled = false;
          paint();
        });
    });

    on(test, "click", () => {
      test.disabled = true;
      status.textContent = "Sending…";
      void sendTestNotification()
        .then(() => {
          status.textContent = "Sent. It should appear in a moment.";
        })
        .catch((cause: unknown) => {
          status.textContent = cause instanceof Error ? cause.message : "The notification could not be sent.";
        })
        .finally(() => {
          test.disabled = false;
        });
    });

    void describePush().then((diagnosis) => {
      state = diagnosis.state;
      detail = diagnosis.detail;
      channels = diagnosis.channels;
      paint();
    });

    body.appendChild(toggle);
    body.appendChild(test);
    body.appendChild(status);
    return el("section", { class: "card" }, el("h2", { class: "card-title", text: "Notifications" }), body);
  }

  /** What a chat channel is doing, in one clause. */
  function channelSentence(channels: readonly string[]): string {
    const names = channels.join(", ");
    return `Notifications also go to ${names}, which works on this phone regardless of the browser.`;
  }

  function accountSection(): HTMLElement {
    const state = ctx.store.state;
    return el(
      "section",
      { class: "card" },
      el("h2", { class: "card-title", text: "This device" }),
      el("p", { class: "muted", text: state.principal === null ? "Not paired." : `Paired as “${state.principal.name}”.` }),
      el("button", {
        class: "btn btn-danger btn-block",
        attrs: { type: "button" },
        text: "Sign out and unpair",
        on: {
          click: () => {
            void ctx.signOut();
          },
        },
      }),
    );
  }

  /* -------------------------------------------------------------- render */

  function render(): void {
    const back = el("button", {
      class: "btn btn-ghost btn-icon",
      attrs: { type: "button", "aria-label": "Back" },
      text: "\u2039",
      on: {
        click: () => {
          ctx.navigate({ kind: "sessions" });
        },
      },
    });

    const content = el(
      "div",
      { class: "settings-body" },
      el("header", { class: "view-head" }, back, el("h1", { class: "view-title", text: "Settings" })),
      connectionSection(),
      notificationsSection(),
      modelSection(),
      devicesSection(),
      accountSection(),
      el("p", { class: "muted footer-note", text: "dsh-gateway · mobile console" }),
    );
    scroll.replaceChildren(content);
  }

  const unsubscribe = ctx.store.select(
    (state) => state,
    () => {
      render();
    },
    (a, b) =>
      a.connection === b.connection &&
      a.connectionDetail === b.connectionDetail &&
      a.offline === b.offline &&
      a.harness === b.harness &&
      a.devices === b.devices &&
      a.devicesError === b.devicesError &&
      a.sessions === b.sessions &&
      a.sessionsError === b.sessionsError &&
      a.models === b.models &&
      a.modelsError === b.modelsError &&
      a.principal === b.principal &&
      a.active === b.active,
  );

  void ctx.loadDevices();
  void ctx.loadSessions();
  void ctx.loadModels();
  void checkHealth().catch(() => {
    health = null;
    healthChecked = true;
    render();
  });

  return () => {
    unsubscribe();
    view.remove();
  };
}
