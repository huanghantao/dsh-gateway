/**
 * Screen 2 — Sessions.
 *
 * The list used to be one flat pile per workspace, newest first, with no way to
 * remove anything. On a real machine that is 250 rows of which 29 are work: test
 * runs from an end-to-end script, scratch directories, sessions created and
 * abandoned — and the work buried under them. A session list is a queue, not a
 * directory, so this screen now leads with what needs the operator (an approval
 * waiting, a turn running), then what they marked as important, then today, and
 * only then the archive of everything else, folded away by workspace and counted
 * without the noise.
 *
 * Three decisions shape the rest:
 *
 * - **Nothing destructive is one tap away.** Archiving is the gesture, deleting
 *   is not offered, and every archive comes back with an undo. A list that can
 *   lose work is a list the operator will not use.
 * - **The row's second line is the useful one.** Workspace, model, how many
 *   messages, how long ago — the things that tell two sessions with the same
 *   opening words apart.
 * - **Search is the gateway's, not the page's.** The phone holds one page of a
 *   list that may be hundreds deep, so filtering locally would lie.
 * - **Search is asked for, not guessed at.** Nothing queries on a keystroke: a
 *   debounce fires in the middle of a word — a Chinese IME reports every
 *   keystroke of a composition long before the character is committed — so the
 *   gateway was asked for "hui" while the operator was still writing 会话, and
 *   every one of those requests blanked the list under their finger.
 */

import { ApiError, ERROR_CODES } from "../api.js";
import type { Ctx } from "../actions.js";
import { copyText } from "../clipboard.js";
import { el, on } from "../dom.js";
import { basename, modelLabel, offeredValue, pluralize, relativeTime, shortWorkspace } from "../format.js";
import type { AppState } from "../store.js";
import type { Session, TranscriptMatch, TranscriptSearch } from "../types.js";
import { openTidySheet } from "./tidy.js";
import { openReceiptSheet } from "./receipt.js";
import { openTrashSheet } from "./sessions_trash.js";
import { badge, emptyState, openSheet, selectField, spinner } from "./ui.js";

/** How far the finger must travel before the release triggers a refresh. */
const PULL_THRESHOLD_PX = 72;

/** How long an archive can be taken back. */
const UNDO_MS = 6000;

/** What a row says when the session has no title of its own. */
function rowTitle(session: Session): string {
  const title = session.title.trim();
  if (title !== "") return title;
  const preview = (session.preview ?? "").trim();
  if (preview !== "") return preview;
  return basename(session.workspace) || "Untitled session";
}

/**
 * A session's second line: where it lives, what it runs, how much is in it.
 *
 * The workspace is shown by name rather than by short path — the row already
 * lives under a workspace heading, and "…/user/code/api" spends the line
 * repeating what the reader knows. The model appears only when the session
 * actually selected one: "Default model" is a label for a picker, not
 * information about a session.
 */
function rowMeta(session: Session, modelNames: ReadonlyMap<string, string>): string {
  const parts: string[] = [];
  const workspace = basename(session.workspace);
  if (workspace !== "") parts.push(workspace);
  if (session.model !== null) {
    const model = modelLabel(session.model, modelNames);
    if (model !== "") parts.push(model);
  }
  if (session.messageCount > 0) parts.push(`${session.messageCount} ${pluralize(session.messageCount, "message")}`);
  parts.push(relativeTime(session.updatedAt));
  return parts.join(" · ");
}

/** Badges for the states a reader scans for. */
function rowBadges(session: Session): HTMLElement | null {
  const badges: HTMLElement[] = [];
  if (session.busy) badges.push(badge("running", "busy"));
  else if (session.leased) badges.push(badge("held", "leased"));
  if (session.archived && session.archivedOnDesk) badges.push(badge("desk", "muted"));
  if (badges.length === 0) return null;
  return el("span", { class: "row-badges" }, ...badges);
}

/* ------------------------------------------------------------------ rows */

interface RowHandlers {
  readonly onOpen: () => void;
  readonly onActions: () => void;
}

function sessionRow(session: Session, modelNames: ReadonlyMap<string, string>, handlers: RowHandlers): HTMLElement {
  const open = el(
    "button",
    { class: "row", attrs: { type: "button" }, on: { click: handlers.onOpen } },
    el(
      "span",
      { class: "row-main" },
      el(
        "span",
        { class: "row-title" },
        session.pinned
          ? el("span", { class: "row-pin", text: "★", attrs: { "aria-label": "Pinned" } })
          : null,
        el("span", { class: "row-title-text", text: rowTitle(session) }),
      ),
      el("span", { class: "row-meta", text: rowMeta(session, modelNames) }),
    ),
    rowBadges(session),
  );

  // The actions live in their own button beside the row rather than inside it:
  // a button inside a button is not a thing a browser will render, and a
  // menu that only exists behind a long-press is a menu half the readers never
  // find. This is also the accessible path for the gesture a future revision
  // may add.
  const actions = el("button", {
    class: "row-actions",
    attrs: { type: "button", "aria-label": `Actions for ${rowTitle(session)}` },
    text: "\u22ef",
  });
  on(actions, "click", (event) => {
    // The row behind it opens the conversation; this button must not.
    event.stopPropagation();
    handlers.onActions();
  });

  // The id travels with the row so a test (and a future swipe handler) can name
  // the session it is acting on without parsing the title back out of the text.
  return el("div", { class: "row-wrap", attrs: { "data-session-id": session.id } }, open, actions);
}

/* -------------------------------------------------------------- sections */

interface Section {
  readonly key: string;
  readonly title: string;
  readonly sessions: readonly Session[];
}

function within(session: Session, hours: number): boolean {
  const at = Date.parse(session.updatedAt);
  if (Number.isNaN(at)) return false;
  return Date.now() - at <= hours * 3600_000;
}

/**
 * Splits what the phone holds into the sections the screen shows.
 *
 * A session appears once, in the first section that claims it: a running pinned
 * session is running, not pinned. The archive keeps its workspace grouping,
 * because that is how someone looks for a session they put away months ago.
 */
function sectionsFor(state: AppState): readonly Section[] {
  if (state.showArchived) {
    const groups = new Map<string, Session[]>();
    for (const session of state.sessions) {
      const key = session.workspace;
      const existing = groups.get(key);
      if (existing === undefined) groups.set(key, [session]);
      else existing.push(session);
    }
    return [...groups.entries()].map(([workspace, sessions]) => ({
      key: `archived:${workspace}`,
      title: basename(workspace) || workspace,
      sessions,
    }));
  }

  const running: Session[] = [];
  const pinned: Session[] = [];
  const today: Session[] = [];
  const earlier = new Map<string, Session[]>();
  for (const session of state.sessions) {
    if (session.busy) running.push(session);
    else if (session.pinned) pinned.push(session);
    else if (within(session, 24)) today.push(session);
    else {
      const key = session.workspace;
      const existing = earlier.get(key);
      if (existing === undefined) earlier.set(key, [session]);
      else existing.push(session);
    }
  }

  const sections: Section[] = [];
  if (running.length > 0) sections.push({ key: "running", title: "Running", sessions: running });
  if (pinned.length > 0) sections.push({ key: "pinned", title: "Pinned", sessions: pinned });
  if (today.length > 0) sections.push({ key: "today", title: "Today", sessions: today });
  for (const [workspace, sessions] of [...earlier.entries()].sort((a, b) => b[1].length - a[1].length)) {
    sections.push({ key: `earlier:${workspace}`, title: "Earlier", sessions });
  }
  return sections;
}

/* ------------------------------------------------------------------ view */

export function mountSessions(root: HTMLElement, ctx: Ctx): () => void {
  const scroll = el("div", { class: "scroll", attrs: { id: "sessions-scroll" } });
  const view = el("section", { class: "view view-sessions" }, scroll);
  root.appendChild(view);

  let searchOpen = false;
  // Transcript results live here rather than in the store: they belong to one
  // visit to this screen, and putting a page of search hits in global state
  // would make every other view render them.
  let transcripts: TranscriptSearch | null = null;
  let transcriptsBusy = false;
  let error: string | null = null;

  /* --------------------------------------------------------- search field */

  /**
   * The search field, built once and re-attached by `render` on every pass.
   *
   * It cannot be rebuilt per render: `render` ends in `replaceChildren`, and
   * moving a focused input into the tree being built — even into the tree it is
   * about to live in — drops the focus and the caret in every browser, taking
   * whatever was typed but not yet submitted with it. Session events arrive
   * while a turn streams, so that was every render, not an edge case.
   */
  let searchInput: HTMLInputElement | null = null;

  /**
   * True while `render` (or the teardown) is moving the field between trees.
   *
   * Reparenting an input fires `blur`, and the node is still connected when it
   * does, so the document cannot tell a rebuild apart from the operator tapping
   * a session row — without this flag every render would submit whatever
   * happened to be in the box.
   */
  let rebuilding = false;

  /**
   * Runs the list search, and only when it was asked for.
   *
   * Enter, the button beside the field and leaving the field are the three
   * gestures that mean "this is the query".
   */
  const submitSearch = (): void => {
    const input = searchInput;
    if (input === null) return;
    const query = input.value.trim();
    if (query === ctx.store.state.sessionsQuery) return;
    void ctx.searchSessions(query);
  };

  const searchField = (): HTMLInputElement => {
    if (searchInput !== null) return searchInput;
    const input = el("input", {
      class: "input search-input",
      attrs: {
        type: "search",
        placeholder: "Search title, workspace, id…",
        "aria-label": "Search sessions",
        value: ctx.store.state.sessionsQuery,
        autocomplete: "off",
        enterkeyhint: "search",
      },
    });
    on(input, "keydown", (event) => {
      if ((event as KeyboardEvent).key !== "Enter") return;
      // Enter commits the filter. "Where did I write that" is a different and
      // heavier question, and it keeps its own button.
      event.preventDefault();
      submitSearch();
    });
    on(input, "blur", () => {
      if (rebuilding) return;
      // After the gesture, not during it. Committing re-renders the list, and a
      // render between mousedown and mouseup replaces the very element the
      // click was aimed at: tapping the search toggle — or a session row —
      // after typing would silently do nothing.
      window.setTimeout(() => {
        // The box was closed, the screen was left, or the operator came back to
        // the field: none of those is a query to run.
        if (rebuilding || !input.isConnected || document.activeElement === input) return;
        submitSearch();
      }, 0);
    });
    searchInput = input;
    return input;
  };

  /* --------------------------------------------------------- new session */

  const openNewSession = (): void => {
    // A harness only reveals its option catalog when a session is created or
    // resumed, so a gateway that has not opened one since it started advertises
    // nothing at all. Ask once more before drawing the pickers in that case
    // rather than showing an empty list for a reason the user cannot see; with
    // a warm catalog the sheet opens immediately, as before.
    const models = ctx.store.state.models;
    if (models.models.length === 0 && models.reasoningEfforts.length === 0) {
      void ctx.loadModels().then(showNewSession);
      return;
    }
    showNewSession();
  };

  const showNewSession = (): void => {
    const state = ctx.store.state;
    let workspace = state.workspaces.find((candidate) => candidate.exists)?.path ?? "";
    // Open on the gateway's own defaults rather than on "Gateway default": the
    // session the server is about to create should be visible in the sheet, not
    // implied by a row the user has to know how to read.
    let model = offeredValue(state.models.defaults.model, state.models.models);
    let effort = offeredValue(state.models.defaults.reasoningEffort, state.models.reasoningEfforts);
    const error = el("p", { class: "alert alert-error", attrs: { role: "alert", hidden: "" } });

    const list = el("div", { class: "choice-list", attrs: { role: "radiogroup", "aria-label": "Workspace" } });
    for (const candidate of state.workspaces) {
      const disabled = !candidate.exists;
      const input = el("input", {
        class: "choice-input",
        attrs: {
          type: "radio",
          name: "workspace",
          value: candidate.path,
          ...(disabled ? { disabled: "" } : {}),
          ...(candidate.path === workspace ? { checked: "" } : {}),
        },
        on: {
          change: () => {
            workspace = candidate.path;
          },
        },
      });
      list.appendChild(
        el(
          "label",
          { class: `choice${disabled ? " is-disabled" : ""}` },
          input,
          el(
            "span",
            { class: "choice-text" },
            el("span", { class: "choice-title", text: candidate.name }),
            el("span", { class: "choice-sub", text: candidate.path }),
          ),
          disabled ? badge("missing", "error") : null,
        ),
      );
    }

    const modelNames = new Map(state.models.models.map((option) => [option.id, option.name]));
    const body = el(
      "div",
      { class: "stack" },
      state.workspacesError !== null ? el("p", { class: "alert alert-error", text: state.workspacesError }) : null,
      el("p", { class: "field-label", text: "Workspace" }),
      state.workspaces.length === 0
        ? emptyState("No workspaces allowlisted", "Add a workspace to the gateway configuration on the desktop, then reopen this sheet.")
        : list,
      selectField(
        "new-model",
        "Model",
        [{ value: "", label: "Gateway default" }, ...[...modelNames].map(([value, label]) => ({ value, label }))],
        model,
        (value) => {
          model = value;
        },
      ),
      selectField(
        "new-effort",
        "Reasoning effort",
        [{ value: "", label: "Gateway default" }, ...state.models.reasoningEfforts.map((o) => ({ value: o.id, label: o.name }))],
        effort,
        (value) => {
          effort = value;
        },
      ),
      error,
    );

    const create = el("button", { class: "btn btn-primary btn-block", attrs: { type: "submit" }, text: "Create session" });
    const form = el("form", { class: "stack", attrs: { novalidate: "" } }, body, create);

    const sheet = openSheet({ title: "New session", body: form });

    on(form, "submit", (event) => {
      event.preventDefault();
      if (workspace === "") {
        error.textContent = "Pick a workspace first.";
        error.removeAttribute("hidden");
        return;
      }
      create.disabled = true;
      create.textContent = "Creating…";
      void ctx
        .createSession(workspace, model === "" ? null : model, effort === "" ? null : effort)
        .then(() => sheet.close())
        .catch((cause: unknown) => {
          error.textContent =
            cause instanceof ApiError && cause.is(ERROR_CODES.network)
              ? "The gateway could not be reached."
              : cause instanceof Error
                ? cause.message
                : "The session could not be created.";
          error.removeAttribute("hidden");
          create.disabled = false;
          create.textContent = "Create session";
        });
    });
  };

  /* ------------------------------------------------------- row actions */

  const showRowActions = (session: Session): void => {
    const body = el("div", { class: "stack" });
    const status = el("p", { class: "muted", attrs: { role: "status" }, text: "" });
    // Assigned once the sheet exists; the buttons below are built first because
    // the sheet needs them as its body.
    let close = (): void => undefined;

    const act = (label: string, run: () => Promise<void>): HTMLElement => {
      const button = el("button", {
        class: "btn btn-block",
        attrs: { type: "button" },
        text: label,
        on: {
          click: () => {
            button.disabled = true;
            status.textContent = "Working…";
            void run()
              .then(() => close())
              .catch((cause: unknown) => {
                status.textContent = cause instanceof Error ? cause.message : "That did not work.";
                button.disabled = false;
              });
          },
        },
      });
      return button;
    };

    body.appendChild(
      act(session.pinned ? "Unpin" : "Pin to the top", async () => {
        await ctx.curate([session.id], { pinned: !session.pinned });
      }),
    );
    if (session.archived) {
      body.appendChild(
        act("Restore to the list", async () => {
          await ctx.curate([session.id], { archived: false });
        }),
      );
      if (session.archivedOnDesk) {
        body.appendChild(
          el("p", {
            class: "muted",
            text: "The desktop archived this one. Restoring here hides it from this phone; the desktop keeps its own list.",
          }),
        );
      }
    } else {
      body.appendChild(
        act("Archive", async () => {
          await ctx.curate([session.id], { archived: true });
        }),
      );
    }

    body.appendChild(
      el("button", {
        class: "btn btn-danger btn-block",
        attrs: { type: "button" },
        text: "Delete…",
        on: {
          click: () => {
            close();
            confirmDelete(session);
          },
        },
      }),
    );

    body.appendChild(
      el("button", {
        class: "btn btn-block",
        attrs: { type: "button" },
        text: "Receipt…",
        on: {
          click: () => {
            close();
            openReceiptSheet(ctx, session);
          },
        },
      }),
    );

    const copy = el("button", { class: "btn btn-ghost btn-block", attrs: { type: "button" }, text: "Copy session id" });
    // A phone that cannot copy must still be able to get the id out. When both
    // paths fail the id lands in this field, already selected, which is one
    // long-press away from the platform's own Copy — and unlike the raw line of
    // text this used to print, it says what it is and can be selected as one run.
    const manual = el("input", {
      class: "input input-id",
      attrs: { type: "text", readonly: "", "aria-label": "Session id", hidden: "" },
    });
    on(copy, "click", () => {
      void copyText(session.id).then((copied) => {
        if (copied) {
          manual.hidden = true;
          status.textContent = "Copied.";
          return;
        }
        manual.value = session.id;
        manual.hidden = false;
        // Read-only fields raise no keyboard on a phone, so this selects without
        // taking the screen over; `select()` alone leaves the run unhighlighted.
        manual.focus({ preventScroll: true });
        manual.select();
        status.textContent = "This browser refused the clipboard. The id is selected below.";
      });
    });
    body.appendChild(copy);
    body.appendChild(manual);
    body.appendChild(status);

    const title = rowTitle(session);
    const sheet = openSheet({ title: title.length > 40 ? `${title.slice(0, 39)}…` : title, body });
    close = () => sheet.close();
  };

  /**
   * Deleting is a move, and the confirmation says so.
   *
   * "Delete" that actually destroys a conversation is not a button a phone
   * should have; what the operator wants is for it to leave the list. Saying
   * where it goes — and that it can come back — is what makes the tap safe to
   * take.
   */
  const confirmDelete = (session: Session): void => {
    const status = el("p", { class: "muted", attrs: { role: "status" }, text: "" });
    const remove = el("button", {
      class: "btn btn-danger btn-block",
      attrs: { type: "button" },
      text: "Move to trash",
    });
    const body = el(
      "div",
      { class: "stack" },
      el("p", { class: "tidy-lead", text: rowTitle(session) }),
      el("p", {
        class: "muted",
        text: "The session and its history move to the gateway's trash. Nothing is destroyed: you can restore it from Trash for 30 days.",
      }),
      remove,
      status,
    );
    const sheet = openSheet({ title: "Delete session?", body });

    on(remove, "click", () => {
      remove.disabled = true;
      status.textContent = "Moving…";
      void ctx
        .deleteSession(session.id)
        .then(() => {
          sheet.close();
          showUndoMessage("Moved to trash", async () => {
            await ctx.restoreSession(session.id);
          });
        })
        .catch((cause: unknown) => {
          status.textContent = cause instanceof Error ? cause.message : "That session could not be moved.";
          remove.disabled = false;
        });
    });
  };

  /**
   * Searches inside the transcripts.
   *
   * Deliberately not automatic: the list search filters what is already on the
   * phone, while this reads session logs on the gateway, so it waits to be asked.
   */
  const runTranscriptSearch = (query: string): void => {
    if (query === "") {
      transcripts = null;
      render();
      return;
    }
    transcriptsBusy = true;
    render();
    void ctx
      .searchTranscripts(query)
      .then((result) => {
        transcripts = result;
      })
      .catch((cause: unknown) => {
        transcripts = { query, scanned: 0, matches: [], truncated: false };
        error = cause instanceof Error ? cause.message : "The search failed.";
      })
      .finally(() => {
        transcriptsBusy = false;
        render();
      });
  };

  /** One transcript result: where it is, and what it says around the match. */
  const transcriptRow = (match: TranscriptMatch): HTMLElement => {
    const open = el(
      "button",
      {
        class: "row row-open",
        attrs: { type: "button" },
        on: {
          click: () => {
            ctx.navigate({ kind: "conversation", sessionId: match.sessionId });
          },
        },
      },
      el(
        "span",
        { class: "row-main" },
        el(
          "span",
          { class: "row-title" },
          el("span", { class: "row-title-text", text: match.title.trim() || match.sessionId }),
          match.archived ? el("span", { class: "row-badges" }, el("span", { class: "badge", text: "archived" })) : null,
        ),
        el("span", { class: "search-snippet", text: match.snippet }),
        el("span", {
          class: "row-meta",
          text: [match.tool !== "" ? match.tool : match.role, relativeTime(match.time)].filter((part) => part !== "").join(" · "),
        }),
      ),
    );
    return el("div", { class: "row-wrap" }, open);
  };

  const transcriptResults = (): readonly Node[] => {
    const nodes: Node[] = [];
    if (transcriptsBusy) {
      nodes.push(el("p", { class: "muted", text: "Reading session logs…" }));
      return nodes;
    }
    if (transcripts === null) return nodes;

    const result = transcripts;
    nodes.push(
      el(
        "div",
        { class: "search-scope" },
        el("span", {
          text: `${result.matches.length} ${pluralize(result.matches.length, "match", "matches")} in the ${result.scanned} most recent sessions`,
        }),
        el("button", {
          class: "btn btn-ghost btn-small",
          attrs: { type: "button" },
          text: "Back to sessions",
          on: {
            click: () => {
              transcripts = null;
              render();
            },
          },
        }),
      ),
    );
    if (result.truncated) {
      nodes.push(el("p", { class: "muted", text: "Older sessions were not read; narrow the query to be sure." }));
    }
    if (result.matches.length === 0) {
      nodes.push(emptyState("Nothing found", "No session in the scanned range contains that text."));
      return nodes;
    }
    for (const match of result.matches) nodes.push(transcriptRow(match));
    return nodes;
  };

  /* --------------------------------------------------------- undo toast */

  const showUndoMessage = (text: string, undo: () => Promise<void>): void => {
    const host = document.getElementById("toast-host");
    if (host === null) return;
    const toast = el(
      "div",
      { class: "toast" },
      el("span", { class: "toast-text", text }),
      el("button", {
        class: "toast-action",
        attrs: { type: "button" },
        text: "Undo",
        on: {
          click: () => {
            toast.remove();
            void undo();
          },
        },
      }),
    );
    host.appendChild(toast);
    window.setTimeout(() => toast.remove(), UNDO_MS);
  };

  const showUndo = (count: number, ids: readonly string[]): void => {
    showUndoMessage(`${count} archived`, async () => {
      await ctx.curate(ids, { archived: false });
    });
  };

  /* -------------------------------------------------------------- render */

  const render = (): void => {
    const state = ctx.store.state;
    // Where the caret is now, before the tree is rebuilt: `replaceChildren` at
    // the end of this function detaches the field, and a detached input is a
    // blurred one.
    const focused = searchInput !== null && document.activeElement === searchInput ? searchInput : null;
    const caret = focused === null ? null : focused.selectionStart;
    rebuilding = true;
    const modelNames = new Map(state.models.models.map((option) => [option.id, option.name]));
    const children: (Node | null)[] = [];

    /* header: search, overflow */

    const searchToggle = el("button", {
      class: `btn btn-ghost btn-icon${searchOpen ? " is-active" : ""}`,
      attrs: { type: "button", "aria-label": "Search sessions", "aria-expanded": String(searchOpen) },
      text: "\u2315",
      on: {
        click: () => {
          searchOpen = !searchOpen;
          // Closing the box drops a half-typed query instead of asking for it:
          // the box was dismissed, and reopening should show the filter the
          // list is actually under.
          if (!searchOpen && searchInput !== null) searchInput.value = ctx.store.state.sessionsQuery;
          render();
          if (searchOpen) {
            const input = view.querySelector<HTMLInputElement>(".search-input");
            input?.focus();
          }
        },
      },
    });

    const overflow = el("button", {
      class: "btn btn-ghost btn-icon",
      attrs: { type: "button", "aria-label": "More" },
      text: "\u22ef",
      on: {
        click: () => {
          let close = (): void => undefined;
          const item = (label: string, run: () => void): HTMLElement =>
            el("button", {
              class: "btn btn-block",
              attrs: { type: "button" },
              text: label,
              on: {
                click: () => {
                  close();
                  run();
                },
              },
            });
          const body = el(
            "div",
            { class: "stack" },
            item("Tidy up…", () => openTidySheet(ctx, { onArchived: showUndo })),
            item("Trash…", () => openTrashSheet(ctx, () => void ctx.loadSessions())),
            item(state.showArchived ? "Show active sessions" : "Show archived", () => {
              void ctx.showArchived(!state.showArchived);
            }),
            item(state.sessionsLoading ? "Refreshing…" : "Refresh", () => {
              void ctx.refresh();
            }),
          );
          const sheet = openSheet({ title: "Sessions", body });
          close = () => sheet.close();
        },
      },
    });

    children.push(
      el(
        "header",
        { class: "view-head" },
        el("h1", { class: "view-title", text: state.showArchived ? "Archived" : "Sessions" }),
        el("div", { class: "view-actions" }, searchToggle, overflow, el("button", {
          class: "btn btn-primary",
          attrs: { type: "button" },
          text: "New",
          on: { click: openNewSession },
        })),
      ),
    );

    if (searchOpen) {
      const input = searchField();
      // A control beside the field, not instead of it: the press is prevented
      // from moving focus, because leaving the field is itself a search and the
      // button is about to run one of its own.
      const beside = (label: string, title: string, run: () => void): HTMLElement => {
        const node = el("button", {
          class: "btn btn-ghost btn-small",
          attrs: { type: "button", title },
          text: label,
          on: { click: run },
        });
        on(node, "mousedown", (event) => event.preventDefault());
        return node;
      };
      children.push(
        el(
          "div",
          { class: "search-bar" },
          input,
          beside("Search", "Search this list", submitSearch),
          beside("Inside", "Search inside sessions", () => {
            const value = input.value.trim();
            if (value === "") return;
            runTranscriptSearch(value);
          }),
        ),
      );
    }

    /* status strip: what is happening, before what exists */

    const running = state.sessions.filter((session) => session.busy).length;
    const waiting = state.approvals.length;
    if (!state.showArchived && (running > 0 || waiting > 0)) {
      children.push(
        el(
          "div",
          { class: "status-strip" },
          waiting > 0
            ? el("span", {
                class: "status-item status-warn",
                text: `⚠ ${waiting} ${pluralize(waiting, "approval")} waiting`,
              })
            : null,
          running > 0
            ? el("span", {
                class: "status-item status-live",
                text: `● ${running} ${pluralize(running, "session")} running`,
              })
            : null,
        ),
      );
    }

    if (state.showArchived) {
      children.push(
        el(
          "p",
          { class: "muted archived-note" },
          "Sessions put away. They stay searchable and can be restored at any time; the desktop keeps its own list.",
        ),
      );
    }

    if (state.sessionsError !== null) {
      children.push(el("p", { class: "alert alert-error", attrs: { role: "alert" }, text: state.sessionsError }));
    }

    /* rows */

    if (error !== null) {
      children.push(el("p", { class: "alert alert-error", attrs: { role: "alert" }, text: error }));
      error = null;
    }

    if (transcripts !== null || transcriptsBusy) {
      children.push(...transcriptResults());
    } else if (state.sessionsLoading && state.sessions.length === 0) {
      children.push(spinner("Loading sessions…"));
    } else if (state.sessions.length === 0) {
      children.push(
        state.sessionsQuery !== ""
          ? emptyState(
              "Nothing matches",
              `No session matches “${state.sessionsQuery}”.`,
              transcriptsBusy
                ? undefined
                : { label: "Search inside sessions", run: () => runTranscriptSearch(state.sessionsQuery) },
            )
          : state.showArchived
            ? emptyState("Nothing archived", "Sessions you archive will be listed here.")
            : emptyState(
                "No sessions yet",
                "Create one with the New button, or start a session on the desktop and it will appear here.",
              ),
      );
    } else {
      for (const section of sectionsFor(state)) {
        const rows = el("div", { class: "rows" });
        for (const session of section.sessions) {
          rows.appendChild(
            sessionRow(session, modelNames, {
              onOpen: () => ctx.navigate({ kind: "conversation", sessionId: session.id }),
              onActions: () => showRowActions(session),
            }),
          );
        }
        // "Earlier" repeats per workspace, so its heading names the workspace
        // rather than saying "Earlier" five times.
        const heading =
          section.key.startsWith("earlier:")
            ? el(
                "h2",
                { class: "group-title" },
                el("span", { class: "group-name", text: section.title }),
                el("span", {
                  class: "group-count",
                  text: `${section.sessions.length} ${pluralize(section.sessions.length, "session")}`,
                }),
                el("span", { class: "group-path", text: shortWorkspace(section.sessions[0]?.workspace ?? "") }),
              )
            : el("h2", { class: "section-title", text: section.key.startsWith("archived:") ? section.title : section.title });
        children.push(el("section", { class: "group" }, heading, rows));
      }

      if (state.sessionsCursor !== null) {
        const more = el("button", {
          class: "btn btn-ghost btn-block",
          attrs: { type: "button" },
          text: state.sessionsLoading ? "Loading…" : "Load more",
          on: {
            click: () => {
              void ctx.loadMoreSessions();
            },
          },
        });
        more.disabled = state.sessionsLoading;
        children.push(more);
      }
    }

    const content = el("div", { class: "sessions-body" }, ...children);
    scroll.replaceChildren(content);
    // The field outlived the render; the caret goes back where it was. Nothing
    // is focused when the box just closed, which is how a dismissal stays one.
    if (focused !== null && focused.isConnected) {
      focused.focus({ preventScroll: true });
      if (caret !== null) focused.setSelectionRange(caret, caret);
    }
    rebuilding = false;
  };

  /* ------------------------------------------------------ pull to refresh */

  let startY = 0;
  let pulling = false;

  const offStart = on(scroll, "touchstart", (event) => {
    const touch = event.touches[0];
    pulling = scroll.scrollTop <= 0 && touch !== undefined;
    startY = touch?.clientY ?? 0;
    scroll.classList.remove("is-pulling", "is-armed");
  });

  const offMove = on(
    scroll,
    "touchmove",
    (event) => {
      if (!pulling) return;
      const touch = event.touches[0];
      if (touch === undefined) return;
      const delta = touch.clientY - startY;
      if (delta <= 0) {
        scroll.classList.remove("is-pulling", "is-armed");
        return;
      }
      scroll.classList.add("is-pulling");
      scroll.classList.toggle("is-armed", delta > PULL_THRESHOLD_PX);
    },
    { passive: true },
  );

  const offEnd = on(scroll, "touchend", () => {
    const armed = scroll.classList.contains("is-armed");
    scroll.classList.remove("is-pulling", "is-armed");
    pulling = false;
    if (armed) void ctx.refresh();
  });

  /* -------------------------------------------------------------- wiring */

  const unsubscribe = ctx.store.select(
    (state) => state,
    () => {
      render();
    },
    // The list re-renders only when something it shows actually changed;
    // without this, every streaming frame in an open session would rebuild it.
    (a, b) =>
      a.sessions === b.sessions &&
      a.sessionsLoading === b.sessionsLoading &&
      a.sessionsError === b.sessionsError &&
      a.sessionsCursor === b.sessionsCursor &&
      a.sessionsQuery === b.sessionsQuery &&
      a.showArchived === b.showArchived &&
      a.approvals === b.approvals &&
      a.workspacesError === b.workspacesError &&
      a.models === b.models &&
      a.workspaces === b.workspaces,
  );

  render();

  // Opening the screen refreshes it. The list used to be whatever the last
  // bootstrap, reconnect or pull happened to leave in the store, so a session
  // created elsewhere — the desktop, another phone — was invisible until the
  // reader thought to pull down. The request costs a couple of milliseconds now
  // that the gateway answers it from a cache, which is what makes this
  // affordable where it would not have been before.
  void ctx.loadSessions();

  return () => {
    offStart();
    offMove();
    offEnd();
    // Leaving the screen is not the operator leaving the field: the blur that
    // the teardown causes must not ask the gateway about an abandoned query.
    rebuilding = true;
    unsubscribe();
    view.remove();
  };
}
