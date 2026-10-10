/**
 * Screen 6 — Activity.
 *
 * Why a screen exists for this at all: everything else in the app shows the
 * *present*. A conversation shows what is happening in one session; the session
 * list shows which ones are busy. Nothing showed what had *finished* — and the
 * moment work finishes is exactly the moment nobody is looking, because that is
 * what finishing means.
 *
 * So this is the record: one row per thing that settled, newest first, saying
 * which agent it was (the main agent, or a named delegation), how it went, and
 * what it amounted to. Rows are kept on the device, so a phone that was in a
 * pocket all afternoon can still answer "what did my agent do?".
 *
 * Two smaller decisions are visible in the rendering:
 *
 *   - **Unread is relative to a moment, not a set of ids.** Everything newer
 *     than the last acknowledgement is new, which is what a reader means by
 *     "new" and cannot drift as old rows expire.
 *   - **A row taps through to its session.** The activity is the headline; the
 *     conversation is the evidence.
 */

import { actorLabel, outcomeLabel, type Activity, type ActivityRow } from "../activity.js";
import type { Ctx } from "../actions.js";
import { el } from "../dom.js";
import { relativeTime } from "../format.js";

/** One line naming what kind of thing happened, for a row with no summary. */
function kindLabel(kind: Activity["kind"]): string {
  switch (kind) {
    case "task":
      return "Delegated task";
    case "approval":
      return "Approval";
    case "question":
      return "Question";
    case "harness":
      return "Gateway";
    default:
      return "Turn";
  }
}

export function mountActivity(root: HTMLElement, ctx: Ctx): () => void {
  const scroll = el("div", { class: "scroll" });
  const view = el("section", { class: "view view-activity" }, scroll);
  root.appendChild(view);

  /**
   * The marker as it was when this screen opened.
   *
   * Frozen on purpose. Opening the screen acknowledges the rows — that is what
   * clears the badge — but the highlight has to survive long enough to be read:
   * a screen that marked everything read *and* redrew would show the reader a
   * list with nothing marked new on it and no way to tell which rows brought
   * them here.
   */
  let marker = ctx.store.state.activityReadAt;

  /** Whether a row is newer than the marker this visit is highlighting against. */
  const isUnread = (activity: Activity): boolean => {
    if (marker === null) return true;
    const seen = Date.parse(marker);
    const at = Date.parse(activity.time);
    if (Number.isNaN(seen) || Number.isNaN(at)) return false;
    return at > seen;
  };

  /**
   * The session's name as it is known *now*, falling back to its id's tail.
   *
   * Resolved here rather than stored with the row: a session renamed this
   * morning must read correctly in yesterday's activity, and a row for a session
   * that has since been deleted still has to name something.
   */
  const sessionNameOf = (sessionId: string): string => {
    if (sessionId === "") return "Gateway";
    const session = ctx.store.state.sessions.find((item) => item.id === sessionId);
    if (session === undefined) return `Session ${sessionId.slice(0, 8)}`;
    const title = session.title.trim();
    if (title !== "") return title;
    const preview = session.preview?.trim() ?? "";
    if (preview !== "") return preview.split(/\r?\n/)[0] ?? preview;
    return `Session ${sessionId.slice(0, 8)}`;
  };

  /** One row. */
  function rowNode(row: ActivityRow, unread: boolean): HTMLElement {
    const { activity } = row;
    const head = el(
      "header",
      { class: "activity-head" },
      el("span", { class: `activity-actor is-${activity.actor.kind}`, text: actorLabel(activity.actor) }),
      el("span", { class: `activity-outcome is-${activity.outcome}`, text: outcomeLabel(activity.outcome) }),
      el("time", { class: "activity-time", attrs: { datetime: activity.time }, text: relativeTime(activity.time) }),
    );

    // What the work amounted to, in the gateway's own words — or, when there is
    // nothing to count, the sentence the event carried.
    const summary = activity.summary.trim() === "" ? kindLabel(activity.kind) : activity.summary;
    const body: (HTMLElement | null)[] = [el("p", { class: "activity-summary", text: summary })];
    if (activity.detail.trim() !== "") {
      body.push(el("p", { class: "activity-detail", text: activity.detail }));
    }

    const button = el(
      "button",
      {
        class: `activity-row${unread ? " is-unread" : ""}`,
        attrs: { type: "button" },
        on: {
          click: () => {
            if (activity.sessionId !== "") {
              ctx.navigate({ kind: "conversation", sessionId: activity.sessionId });
              return;
            }
            ctx.navigate({ kind: "sessions" });
          },
        },
      },
      el(
        "div",
        { class: "activity-main" },
        head,
        ...body,
        el("span", { class: "activity-session", text: row.sessionName }),
      ),
    );
    return el("article", { class: "activity-item" }, button);
  }

  function render(): void {
    const state = ctx.store.state;
    const activities = state.activities;
    const rows: ActivityRow[] = activities.map((activity) => ({
      activity,
      sessionName: sessionNameOf(activity.sessionId),
    }));
    // Newest first: the reason to open this screen is almost always "what just
    // happened?".
    rows.reverse();

    if (rows.length === 0) {
      scroll.replaceChildren(
        el(
          "div",
          { class: "empty" },
          el("h2", { class: "empty-title", text: "Nothing has finished yet" }),
          el("p", {
            class: "muted",
            text:
              "When a turn or a delegated task settles, it is recorded here — including the ones that settle while this app is closed. " +
              "Notifications are what reach you when you are not looking; this is the record you come back to.",
          }),
        ),
      );
      return;
    }

    const unread = rows.filter((row) => isUnread(row.activity)).length;
    const head = el(
      "div",
      { class: "activity-header" },
      el("h1", { class: "activity-title", text: "Activity" }),
      el("p", {
        class: "muted",
        text:
          unread === 0
            ? "Everything here has been read."
            : `${unread} new since you last looked.`,
      }),
      // Offered only while there is something to clear. Its effect is to drop
      // the highlight on rows the reader has now seen, which is a thing they ask
      // for rather than a thing that happens to them.
      unread === 0
        ? null
        : el("button", {
            class: "btn btn-ghost activity-read",
            attrs: { type: "button" },
            text: "Mark all read",
            on: {
              click: () => {
                ctx.markActivitiesRead();
                marker = ctx.store.state.activities.at(-1)?.time ?? marker;
                render();
              },
            },
          }),
    );

    scroll.replaceChildren(head, ...rows.map((row) => rowNode(row, isUnread(row.activity))));
  }

  const unsubscribe = ctx.store.select(
    (state) => state,
    () => {
      render();
    },
    // Only what this screen draws: a streaming turn in an open conversation must
    // not rebuild a list of settled work.
    (a, b) => a.activities === b.activities && a.activityReadAt === b.activityReadAt && a.sessions === b.sessions,
  );

  render();

  // Opening the screen is the acknowledgement: the reader is looking at the
  // list, so the badge that brought them here has done its job.
  ctx.markActivitiesRead();

  return () => {
    unsubscribe();
    view.remove();
  };
}
