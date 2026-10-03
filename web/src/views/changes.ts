/**
 * What changed, and how to put it back.
 *
 * The receipt lists file *names*; this answers the two questions that come
 * before anything else when an operator picks the phone back up: what did the
 * agent actually change, and how do I undo it. Both answers come from the
 * session's own log.
 *
 * It is a projection of what the file tools recorded, not a workspace diff, and
 * the screen says so before it shows a line of it. That distinction is not
 * pedantry: an empty or short list must not be read as "nothing else happened",
 * because a `bash` command running `sed -i` leaves no trace here at all.
 *
 * Undo is the only control in the product that writes to an operator's files.
 * It is offered per file and for the session as a whole, confirmed by name and
 * count, and reported per file afterwards — "9 files came back and 2 did not" is
 * the answer someone needs, not "done".
 */

import type { Ctx } from "../actions.js";
import { el } from "../dom.js";
import type {
  ChangedFile,
  ChangeHunk,
  ChangesSummary,
  RevertOutcome,
  RevertReport,
  RevertResult,
  Session,
  SessionChanges,
} from "../types.js";
import { openSheet, spinner } from "./ui.js";

/**
 * How many file blocks open with their changes showing.
 *
 * A session that touched two hundred files still has to open on a phone, and a
 * folded file keeps its head row — the path, the counts, the markers — so the
 * list stays a summary rather than losing information to the fold.
 */
const EXPANDED_FILES = 12;

/**
 * How many diff lines of one file open showing.
 *
 * The server caps a hunk at 400 lines but not a file: forty of those is 16,000
 * lines of DOM, which is more than a phone should build while someone waits to
 * see what happened.
 */
const EXPANDED_LINES = 400;

/**
 * The one sentence that keeps this screen from lying by omission.
 *
 * An incomplete list and a complete one look identical, so this is stated up
 * front rather than left to a footnote. No tool name is emphasised because the
 * sentence is about the shape of the projection, not about a feature.
 */
const PROJECTION_NOTE =
  "Built from the arguments the file tools recorded — the edit and write calls in the session log — not from a comparison of the workspace. A change made any other way, such as a bash command running sed -i, is not recorded here and will not appear.";

/** Everything a renderer needs that is not in the payload. */
interface Screen {
  readonly ctx: Ctx;
  readonly session: Session;
  /** Draws an undo's report where the projection was. */
  report(result: RevertReport, request: UndoRequest): void;
  /** Reads the projection again, after an undo has moved the files under it. */
  reload(): void;
}

/**
 * One undo, as the confirmation needs to describe it.
 *
 * `paths` empty is not "no files" — it is what the gateway reads as every file
 * it can reverse, and it makes the gateway, rather than this screen, the author
 * of the list of what gets touched.
 */
interface UndoRequest {
  readonly label: string;
  readonly paths: readonly string[];
  /** What will happen, in the operator's terms. */
  readonly what: string;
}

export function openChangesSheet(ctx: Ctx, session: Session): void {
  const body = el("div", { class: "stack" }, spinner("Reading the session log…"));
  openSheet({ title: "What changed", body });

  function load(): void {
    body.replaceChildren(spinner("Reading the session log…"));
    void ctx
      .changes(session.id)
      .then((changes) => {
        body.replaceChildren(...renderChanges(screen, changes));
      })
      .catch((cause: unknown) => {
        body.replaceChildren(
          el("p", {
            class: "alert alert-error",
            attrs: { role: "alert" },
            text: cause instanceof Error ? cause.message : "The session's changes could not be read.",
          }),
        );
      });
  }

  const screen: Screen = {
    ctx,
    session,
    report: (result) => {
      body.replaceChildren(...renderReport(result, load));
    },
    reload: load,
  };

  load();
}

function renderChanges(screen: Screen, changes: SessionChanges): readonly Node[] {
  if (changes.unsupported) {
    // The same degradation the transcript shows: the session is real and only
    // this view is unavailable, so it is named rather than reported as a
    // failure — and the desktop is where the answer lives.
    return [
      el("p", {
        class: "muted",
        text: `What changed cannot be read here: ${
          changes.detail === ""
            ? "this session's log was written by a newer version of DSH than the gateway understands"
            : changes.detail
        }. Open the desktop app to review the changes.`,
      }),
    ];
  }

  const nodes: Node[] = [];
  const summary = changes.summary;

  // An empty projection is not evidence that nothing changed, so the note
  // about what this screen can see is never conditional on there being
  // something to see.
  if (changes.files.length > 0) {
    nodes.push(el("p", { class: "changes-summary", text: summaryLine(summary) }));
    if (summary.truncated) nodes.push(el("p", { class: "muted changes-note", text: truncatedNote(summary) }));
  } else {
    nodes.push(el("p", { class: "muted", text: "This session's log records no changes to any file." }));
  }

  nodes.push(el("p", { class: "muted changes-note", text: PROJECTION_NOTE }));
  // Two facts, both required before undo is offered. `revertible` is the
  // projection's verdict on a file — the log records enough to reverse it — and
  // `revertEnabled` is whether this deployment writes to a workspace at all.
  // They are separate fields so that a read-only gateway does not report every
  // file as unknowably irreversible; the AND belongs here, at the point of
  // offering.
  const canUndo = changes.revertEnabled;
  if (!canUndo) {
    nodes.push(
      el("p", {
        class: "muted changes-note",
        text: "Undo is switched off on this gateway, so nothing here can be put back from the phone.",
      }),
    );
  }

  for (const [index, file] of changes.files.entries()) {
    nodes.push(fileBlock(screen, file, index < EXPANDED_FILES, canUndo));
  }

  const revertible = canUndo ? changes.files.filter((file) => file.revertible) : [];
  if (revertible.length > 0) {
    const request = undoAllRequest(revertible.length, summary);
    nodes.push(
      el(
        "div",
        { class: "changes-actions" },
        el("button", {
          class: "btn btn-danger btn-block",
          attrs: { type: "button" },
          text: request.label,
          on: {
            click: () => {
              confirmUndo(screen, request, (report) => {
                screen.report(report, request);
              });
            },
          },
        }),
      ),
    );
  }

  return nodes;
}

function summaryLine(summary: ChangesSummary): string {
  const files = `${summary.files} file${summary.files === 1 ? "" : "s"}`;
  // A slice that does not announce itself reads as the whole: someone who
  // cannot tell they are looking at 200 of 231 will assume they saw it all.
  const shown = summary.total > summary.files ? ` · showing ${summary.files} of ${summary.total}` : "";
  return `${files} · +${summary.added} −${summary.deleted}${shown}`;
}

function truncatedNote(summary: ChangesSummary): string {
  const omitted = summary.total - summary.files;
  if (omitted > 0) {
    return `The list stops at ${summary.files} files: ${omitted} more changed. Their lines are counted above, but the files themselves are not listed.`;
  }
  // The file list is complete, so the cut was inside a file: more changes to
  // it than the projection carries.
  return "Some files changed more times than are listed here; the counts above cover every one of them.";
}

function fileBlock(screen: Screen, file: ChangedFile, open: boolean, canUndo: boolean): HTMLElement {
  const article = el(
    "article",
    { class: "changes-file" },
    el(
      "div",
      { class: "changes-file-head" },
      el("code", { class: "changes-path", text: file.display }),
      el("span", { class: "changes-stat", text: `+${file.added} −${file.deleted}` }),
      ...markers(file),
    ),
  );

  const body: HTMLElement[] = [];

  // Most files land here rather than in the revertible case, so the reason is
  // stated plainly instead of being left as the absence of a button.
  if (!file.revertible && file.reason !== "") {
    body.push(el("p", { class: "muted changes-reason", text: file.reason }));
  }

  // Hunks are the unit of folding because the server caps one at 400 lines: the
  // first always fits inside the budget, so a change is never cut in half for
  // display, and what is folded away is always whole.
  let used = 0;
  const folded: HTMLElement[] = [];
  let hiddenLines = 0;
  for (const hunk of file.hunks) {
    const node = hunkBlock(hunk);
    if (used >= EXPANDED_LINES) {
      node.hidden = true;
      folded.push(node);
      hiddenLines += hunk.lines.length;
    } else {
      used += hunk.lines.length;
    }
    body.push(node);
  }
  if (folded.length > 0) body.push(moreLines(folded, hiddenLines, file.display));

  if (canUndo && file.revertible) {
    const undo = el("button", {
      class: "btn btn-ghost",
      attrs: { type: "button", "aria-label": `Undo changes to ${file.display}` },
      text: "Undo",
      on: {
        click: () => {
          const request = singleUndoRequest(file);
          confirmUndo(screen, request, (report) => {
            screen.report(report, request);
          });
        },
      },
    });
    body.push(el("div", { class: "changes-actions" }, undo));
  }

  if (open) {
    article.append(...body);
    return article;
  }

  for (const node of body) node.hidden = true;
  const where = `the changes to ${file.display}`;
  let expanded = false;
  const toggle = el("button", {
    class: "btn btn-ghost",
    attrs: { type: "button", "aria-expanded": "false", "aria-label": `Show ${where}` },
    text: "Show changes",
    on: {
      click: () => {
        expanded = !expanded;
        for (const node of body) node.hidden = !expanded;
        toggle.textContent = expanded ? "Hide changes" : "Show changes";
        // The name follows the text, so the two never disagree about what a
        // press will do — and neither is the bare "Show changes" that two
        // hundred files would all share.
        toggle.setAttribute("aria-expanded", expanded ? "true" : "false");
        toggle.setAttribute("aria-label", `${expanded ? "Hide" : "Show"} ${where}`);
      },
    },
  });
  article.append(toggle, ...body);
  return article;
}

function markers(file: ChangedFile): readonly HTMLElement[] {
  const badges: HTMLElement[] = [];
  if (file.binary) badges.push(el("span", { class: "badge", text: "binary" }));
  if (file.writes > 0) {
    badges.push(el("span", { class: "badge", text: `${file.writes} write${file.writes === 1 ? "" : "s"}` }));
  }
  if (file.truncated) badges.push(el("span", { class: "badge", text: "truncated" }));
  return badges;
}

function hunkBlock(hunk: ChangeHunk): HTMLElement {
  const nodes: Node[] = [];
  if (hunk.wholeFile) {
    // Without this, a whole-file write reads as a file that was always this
    // way: the log genuinely does not hold what was there before.
    nodes.push(
      el("p", {
        class: "diff-note",
        text: "A whole file was written here. The log does not record what it held before, so every line reads as added.",
      }),
    );
  }

  if (hunk.lines.length > 0) {
    const diff = el("div", { class: "diff" });
    for (const line of hunk.lines) diff.appendChild(diffLine(line));
    nodes.push(diff);
  }

  if (hunk.truncated) {
    nodes.push(
      el("p", {
        class: "diff-note",
        text: "This change is longer than one hunk carries. The rest is missing from this view, not from the file.",
      }),
    );
  }

  return el("div", { class: "changes-hunk" }, ...nodes);
}

/**
 * One line of a hunk.
 *
 * The leading `+`, `-` or space is markup rather than the line's text: the class
 * carries it for the stylesheet and an aria-hidden span carries it for the eye,
 * so what is read aloud does not begin with a sign on every line. It stays
 * *visible* because colour alone must not be the only thing distinguishing an
 * addition from a deletion — and it stays in the flow so copied text still reads
 * like a diff. This is the same rendering the approval sheet uses, deliberately:
 * the two screens show the same hunks, and a reader who learned to scan one
 * should not have to learn the other.
 */
function diffLine(raw: string): HTMLElement {
  const marker = raw.slice(0, 1);
  // A context line leads with a space; an unprefixed line is shown exactly as it
  // arrived rather than with its first character eaten.
  const prefixed = marker === "+" || marker === "-" || marker === " ";
  const kind = marker === "+" ? "diff-add" : marker === "-" ? "diff-del" : "diff-ctx";
  return el(
    "div",
    { class: `diff-line ${kind}` },
    el("span", { attrs: { "aria-hidden": "true" }, text: prefixed ? marker : " " }),
    prefixed ? raw.slice(1) : raw,
  );
}

function moreLines(folded: readonly HTMLElement[], count: number, where: string): HTMLElement {
  const more = `Show ${count} more line${count === 1 ? "" : "s"} of ${where}`;
  let expanded = false;
  const button = el("button", {
    class: "btn btn-ghost",
    attrs: { type: "button", "aria-expanded": "false", "aria-label": more },
    text: `Show ${count} more line${count === 1 ? "" : "s"}`,
    on: {
      click: () => {
        expanded = !expanded;
        for (const node of folded) node.hidden = !expanded;
        button.textContent = expanded ? "Hide" : `Show ${count} more line${count === 1 ? "" : "s"}`;
        button.setAttribute("aria-expanded", expanded ? "true" : "false");
        button.setAttribute("aria-label", expanded ? `Hide the rest of ${where}` : more);
      },
    },
  });
  return el("div", { class: "changes-more" }, button);
}

function undoAllRequest(count: number, summary: ChangesSummary): UndoRequest {
  const head =
    count === 1
      ? "One file in this session can be put back. It is restored to what it held before the session touched it."
      : `${count} files in this session can be put back, each restored to what it held before the session touched it.`;
  // The gateway walks every file its plan holds, so an undo with no paths
  // reaches further than the list does when the list was cut short. Saying so
  // here is the difference between a confirmation and a surprise.
  const beyond =
    summary.total > summary.files
      ? ` The gateway also looks at the ${summary.total - summary.files} changed files this list is too short to show.`
      : "";
  return {
    label: `Undo ${count} file${count === 1 ? "" : "s"}`,
    paths: [],
    what: `${head}${beyond} Files the log cannot reverse are left alone and reported as refused. The restore is exact — a file that has moved on since is refused rather than guessed at — and it cannot be undone.`,
  };
}

function singleUndoRequest(file: ChangedFile): UndoRequest {
  return {
    label: "Undo this file",
    paths: [file.path],
    what: `“${file.display}” is restored to what it held before this session changed it. The restore is exact: if the file no longer holds what the log recorded, it is refused rather than guessed at. This cannot be undone.`,
  };
}

/**
 * Asks before writing to an operator's files.
 *
 * The pattern is the revoke confirmation's, because the risk is the same shape:
 * one irreversible act with a blast radius worth reading before it happens.
 */
function confirmUndo(screen: Screen, request: UndoRequest, done: (report: RevertReport) => void): void {
  const error = el("p", { class: "alert alert-error", attrs: { role: "alert", hidden: "" } });
  const status = el("p", { class: "muted", attrs: { role: "status" }, text: "" });
  const confirm = el("button", {
    class: "btn btn-danger btn-block",
    attrs: { type: "button" },
    text: request.label,
    on: {
      click: () => {
        // Both controls go down together: a second press while the first is
        // still in flight would ask the gateway to undo what it is already
        // undoing.
        confirm.disabled = true;
        cancel.disabled = true;
        status.textContent = "Putting the files back…";
        void screen.ctx
          .revert(screen.session.id, request.paths)
          .then((report) => {
            sheet.close();
            done(report);
          })
          .catch((cause: unknown) => {
            error.textContent = cause instanceof Error ? cause.message : "The undo could not be attempted.";
            error.removeAttribute("hidden");
            confirm.disabled = false;
            cancel.disabled = false;
            // A failure can mean "never started" or "stopped part-way", and the
            // error alone does not say which. Reading the list again does.
            status.textContent = "The undo did not complete. Re-read the list before trying again.";
          });
      },
    },
  });
  const cancel = el("button", {
    class: "btn btn-ghost btn-block",
    attrs: { type: "button" },
    text: "Cancel",
    on: {
      click: () => {
        sheet.close();
      },
    },
  });
  const body = el("div", { class: "stack" }, el("p", { text: request.what }), error, confirm, cancel, status);
  const sheet = openSheet({ title: request.label, body });
}

function renderReport(report: RevertReport, reload: () => void): readonly Node[] {
  // The report is already about the request and nothing else: a path-scoped undo
  // comes back with a row per requested path, not one per file in the session's
  // plan, so there is nothing here to filter.
  const results = report.files;

  const nodes: Node[] = [el("p", { class: "changes-summary", text: outcomeLine(results) })];

  if (results.length > 0) {
    const list = el("div", { class: "changes-report" });
    for (const result of results) {
      list.appendChild(
        el(
          "div",
          { class: `changes-result is-${result.status}` },
          el("code", { class: "changes-path", text: result.display }),
          el("span", { class: outcomeBadge(result.status), text: result.status }),
        ),
      );
      // The row is [path | outcome] and the stylesheet lays it out as two
      // columns, so a refusal's reason is a line of its own underneath rather
      // than a third column that would break the alignment.
      if (result.reason !== "") list.appendChild(el("p", { class: "muted changes-reason", text: result.reason }));
    }
    nodes.push(list);
  }

  if (results.some((result) => result.status !== "reverted")) {
    nodes.push(el("p", { class: "muted changes-note", text: "Everything not marked reverted was left exactly as it was." }));
  }

  // The projection is stale the moment an undo lands, so the report is a dead
  // end by design and the way out is one press away.
  nodes.push(
    el("button", {
      class: "btn btn-primary btn-block",
      attrs: { type: "button" },
      text: "Re-read",
      on: {
        click: () => {
          reload();
        },
      },
    }),
  );
  return nodes;
}

function outcomeLine(results: readonly RevertResult[]): string {
  const count = (status: RevertOutcome): number => results.filter((result) => result.status === status).length;
  const parts: string[] = [];
  for (const status of ["reverted", "refused", "skipped"] as const) {
    const files = count(status);
    if (files > 0) parts.push(`${files} file${files === 1 ? "" : "s"} ${status}`);
  }
  return parts.length === 0 ? "Nothing was undone." : parts.join(" · ");
}

/**
 * The badge for one outcome.
 *
 * "Which of these came back" is the question the report exists to answer, so
 * the two outcomes that differ in consequence differ in colour; a skipped file
 * was not touched at all and stays neutral.
 */
function outcomeBadge(status: RevertOutcome): string {
  if (status === "reverted") return "badge badge-ok";
  if (status === "refused") return "badge badge-error";
  return "badge";
}
