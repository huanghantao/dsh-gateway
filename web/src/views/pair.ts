/**
 * Screen 1 — Pair.
 *
 * The code is the only secret in the product, so the input does the work: it
 * accepts the contract's alphabet, uppercases, strips the characters a human
 * misreads (I/L/O/0/1 are not in the alphabet) and caps the length, so a typo
 * is corrected rather than round-tripped to a `401`.
 *
 * A code can also arrive already filled in, from the QR link the desktop prints
 * (`#/pair?code=…`). That is a prefill, never a submit: the code is one-time and
 * the pairing endpoint is rate limited, so a page that merely gets previewed,
 * restored from a tab or opened by a link scanner must not spend either. The
 * scan saves typing and nothing else.
 *
 * The two documented failures get distinct, actionable copy: a bad code means
 * "make a new one", a lockout means "wait", and conflating them would send the
 * user round a loop that cannot succeed.
 */

import { ApiError, ERROR_CODES, api, isAbortError } from "../api.js";
import type { Ctx } from "../actions.js";
import { el, on } from "../dom.js";
import { PAIRING_CODE_LENGTH, isPairingCodeComplete, normalizePairingCode } from "../format.js";

/**
 * A reasonable starting value: the server shows this name in the device list.
 *
 * Platform *and* browser, because the name's only job is telling two of your own
 * devices apart in that list, and "Android phone" does not do that for anyone
 * with more than one Android phone — or for the same phone in two browsers. The
 * user agent is the only signal available before the device has a name, and the
 * field is editable, so the cost of guessing wrong is one tap.
 */
function defaultDeviceName(): string {
  const ua = navigator.userAgent;

  const platform = /iPhone/.test(ua)
    ? "iPhone"
    : /iPad/.test(ua)
      ? "iPad"
      : /Android/.test(ua)
        ? "Android"
        : "Browser";

  // Order matters: Edge and Chrome both claim Safari, and Chrome on iOS claims
  // CriOS while Firefox on iOS claims FxiOS.
  const browser = /Edg\//.test(ua)
    ? "Edge"
    : /Firefox\/|FxiOS/.test(ua)
      ? "Firefox"
      : /CriOS|Chrome\//.test(ua)
        ? "Chrome"
        : /Safari\//.test(ua)
          ? "Safari"
          : "";

  return browser === "" ? platform : `${platform} · ${browser}`;
}

function pairingErrorCopy(error: unknown): string {
  if (error instanceof ApiError) {
    switch (error.code) {
      case ERROR_CODES.invalidPairingCode:
        return "That code is not valid, or it has already been used. Generate a fresh one on the desktop.";
      case ERROR_CODES.pairingLocked:
        return "Too many wrong codes, so pairing is locked. Wait a few minutes and try again with a new code.";
      case ERROR_CODES.network:
        return "The gateway could not be reached. Check that this device is on the same network.";
      default:
        return error.message;
    }
  }
  return "Pairing failed.";
}

export function mountPair(root: HTMLElement, ctx: Ctx): () => void {
  const code = el("input", {
    class: "input input-code",
    attrs: {
      id: "pair-code",
      type: "text",
      name: "code",
      inputmode: "text",
      // `characters` tells mobile keyboards to start in caps; `one-time-code`
      // lets iOS offer the code from a nearby Messages/notification.
      autocapitalize: "characters",
      autocorrect: "off",
      autocomplete: "one-time-code",
      spellcheck: "false",
      maxlength: PAIRING_CODE_LENGTH,
      placeholder: "ABCD2345",
      "aria-describedby": "pair-code-hint",
      required: "",
    },
  });

  const name = el("input", {
    class: "input",
    attrs: {
      id: "pair-name",
      type: "text",
      name: "deviceName",
      autocomplete: "off",
      maxlength: 64,
      placeholder: "My phone",
      required: "",
    },
  });
  name.value = defaultDeviceName();

  const errorBox = el("p", {
    class: "alert alert-error",
    attrs: { id: "pair-error", role: "alert", hidden: "" },
  });

  // Announced politely rather than assertively: the user has not done anything
  // wrong, they just need to know the scan landed.
  const scanNotice = el("p", {
    class: "alert alert-info",
    attrs: { role: "status", hidden: "" },
    text: "Code scanned from the QR link. Check the device name, then pair.",
  });

  const submit = el("button", {
    class: "btn btn-primary btn-block",
    attrs: { type: "submit" },
    text: "Pair this device",
  });

  const form = el(
    "form",
    { class: "card pair-card", attrs: { novalidate: "" } },
    el("h1", { class: "pair-title", text: "Pair this device" }),
    el("p", {
      class: "muted",
      text: "On the desktop running dsh-gateway, run the pairing command and type the code it prints.",
    }),
    el(
      "label",
      { class: "field" },
      el("span", { class: "field-label", text: "Pairing code" }),
      code,
      el("span", {
        class: "field-hint",
        attrs: { id: "pair-code-hint" },
        text: `${PAIRING_CODE_LENGTH} characters. Letters and digits only.`,
      }),
    ),
    el(
      "label",
      { class: "field" },
      el("span", { class: "field-label", text: "Device name" }),
      name,
      el("span", { class: "field-hint", text: "How this phone appears in the device list." }),
    ),
    scanNotice,
    errorBox,
    submit,
  );

  const showError = (message: string | null): void => {
    if (message === null) {
      errorBox.setAttribute("hidden", "");
      errorBox.textContent = "";
      return;
    }
    errorBox.textContent = message;
    errorBox.removeAttribute("hidden");
  };

  let pending: AbortController | null = null;

  const syncSubmit = (): void => {
    submit.disabled = pending !== null || !isPairingCodeComplete(code.value) || name.value.trim() === "";
  };

  const offInput = on(code, "input", () => {
    const normalized = normalizePairingCode(code.value);
    if (normalized !== code.value) code.value = normalized;
    showError(null);
    syncSubmit();
  });
  const offName = on(name, "input", () => {
    showError(null);
    syncSubmit();
  });

  const offSubmit = on(form, "submit", (event) => {
    event.preventDefault();
    if (pending !== null) return;

    const pairingCode = normalizePairingCode(code.value);
    const deviceName = name.value.trim();
    if (!isPairingCodeComplete(pairingCode)) {
      showError(`The code is ${PAIRING_CODE_LENGTH} characters long.`);
      code.focus();
      return;
    }
    if (deviceName === "") {
      showError("Give this device a name so you can recognise it later.");
      name.focus();
      return;
    }

    pending = new AbortController();
    const signal = pending.signal;
    submit.textContent = "Pairing…";
    showError(null);
    syncSubmit();

    void api
      .pair({ code: pairingCode, deviceName }, signal)
      .then(async () => {
        submit.textContent = "Paired";
        // `bootstrap` re-reads `GET /me` and every catalogue; the response also
        // set the cookie, so the whole app can come up from here.
        await ctx.bootstrap();
      })
      .catch((error: unknown) => {
        if (isAbortError(error)) return;
        showError(pairingErrorCopy(error));
        code.select();
      })
      .finally(() => {
        pending = null;
        submit.textContent = "Pair this device";
        syncSubmit();
      });
  });

  root.appendChild(el("section", { class: "view view-pair" }, form));

  // A code from the QR link is placed in the field but never submitted: the
  // user still has to confirm the device name, and an automatic POST would burn
  // a rate-limit slot and a one-time code just for opening the page.
  const route = ctx.store.state.route;
  const scanned = route.kind === "pair" ? (route.code ?? null) : null;
  if (scanned !== null) {
    code.value = scanned;
    // The code is already correct, so the next useful action is naming this
    // device — put the caret there instead of back on what was just scanned.
    name.focus({ preventScroll: true });
    name.select();
    scanNotice.removeAttribute("hidden");
  } else {
    // Autofocus is hostile on a phone before the user has seen the screen, but
    // focusing the *field* (not raising the keyboard) keeps desktop parity.
    code.focus({ preventScroll: true });
  }
  syncSubmit();

  return () => {
    pending?.abort();
    offInput();
    offName();
    offSubmit();
  };
}
