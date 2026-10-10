/**
 * dsh-gateway question answerer — a DeepSeek Harness (cordis) plugin.
 *
 * WHY THIS FILE EXISTS
 *
 * DeepSeek Harness can stop mid-turn and ask the operator a question
 * (`ask_user_question`), but the ACP surface the gateway drives projects only
 * approvals: the ACP bridge composes no answerer, and the tool itself is not
 * mounted in the `acp` profile, so on that profile the model can neither ask nor
 * be answered. This plugin is the missing end of the seam. It registers an
 * answerer on the harness's `user-questions/request` waterfall, forwards each
 * request over loopback HTTP to the gateway, and hands the human's answer back to
 * the tool call that is waiting for it.
 *
 * It is installed by the gateway — copied into the gateway's own state directory
 * and mounted with a generated `--patch` overlay — so nothing here has to be
 * placed by hand and the running copy always matches the binary that wrote it.
 *
 * TWO RULES, BOTH LEARNED THE HARD WAY
 *
 * 1. Never serialise `request.agent`. It is a live Cordis context, and touching
 *    it as data (JSON.stringify, structuredClone, a spread into a log line)
 *    throws "cannot get property ... without inject" from the context proxy and
 *    fails the tool call. Only `request.agent.id` is read here, which is a
 *    documented property and happens to be the session id.
 * 2. Never throw while this module is loading or while `apply` runs. A plugin
 *    that fails to mount fails the whole profile boot, which would take the
 *    operator's agent down with it. Everything that can fail — reading the
 *    endpoint file, the HTTP round trip — happens inside the listener, where the
 *    worst outcome is that one question goes unanswered.
 *
 * THE CONTRACT WITH THE GATEWAY
 *
 * The gateway writes a JSON endpoint description (URL, per-process bearer token,
 * question timeout) to a path named by the environment variable below. It is
 * read on every ask rather than cached, because the token changes when the
 * gateway restarts and the child outlives that restart by design.
 *
 *   POST <url>            {"id","sessionId","questions":[{id,header,question,
 *                          detail,options:[{label,description,recommended}],
 *                          multiSelect}]}
 *   ->  200 {"outcome":"answered","answers":[{id,selected:[],custom?}]}
 *   ->  200 {"outcome":"unanswered","reason":"timeout"|"cancelled"|...}
 *
 * The request id is minted here, once, and reused across every retry, which is
 * what makes a gateway redeploy mid-question recoverable: the new process parks
 * the same id again instead of showing the operator a second card.
 */

import { randomUUID } from "node:crypto";
import { readFile } from "node:fs/promises";

/** Cordis plugin name, used in loader diagnostics. */
export const name = "dsh-gateway-questions";

/**
 * The plugin contributes a listener, not a service, so it injects nothing.
 *
 * Declaring `userQuestions` here would be wrong: the service is consumed by the
 * tool that asks, and injecting it would make this plugin inactive on any
 * composition that mounts the answerer without the tool. An answerer that is
 * simply never called costs nothing.
 */
export const inject = [];

/** Names the file the gateway writes with the URL, token and timeout. */
const ENDPOINT_ENV = "DSH_GATEWAY_QUESTIONS_ENDPOINT_FILE";

/** How much longer than the gateway's own window this side waits, in ms. */
const GRACE_MS = 30_000;

/** Retry delays for an unreachable gateway, in ms; the last value repeats. */
const RETRY_MS = [1_000, 2_000, 5_000];

/** The harness appends this to the label of the option it recommends. */
const RECOMMENDED = /\s*\(recommended\)\s*$/i;

/**
 * Ask the gateway, and answer the tool call.
 *
 * Declining — `return next()` — is reserved for "this answerer is not
 * configured", the one case where another answerer, or the harness's own
 * NO_PROVIDER error, is the more accurate story. A gateway that exists but
 * cannot be reached is reported as itself, because that is a different repair.
 */
export function apply(ctx) {
  ctx.on("user-questions/request", async (request, next) => {
    const endpoint = await readEndpoint().catch(() => null);
    if (endpoint === null) return next();

    const ask = {
      id: randomUUID(),
      sessionId: sessionIDOf(request),
      questions: questionsOf(request),
    };

    // The gateway expires the question on its own clock; this deadline is
    // deliberately later, so the card is never still on screen while the model
    // has already been told nobody answered.
    const deadline = Date.now() + endpoint.timeoutMs + GRACE_MS;

    for (let attempt = 0; ; attempt += 1) {
      if (isAborted(request)) throw aborted();

      let response;
      try {
        response = await fetch(endpoint.url, {
          method: "POST",
          headers: {
            "content-type": "application/json",
            authorization: `Bearer ${endpoint.token}`,
          },
          body: JSON.stringify(ask),
          signal: request.signal,
        });
      } catch (error) {
        if (isAborted(request)) throw aborted();
        if (Date.now() >= deadline) throw unreachable(error);
        await sleep(RETRY_MS[Math.min(attempt, RETRY_MS.length - 1)]);
        continue;
      }

      // 401 means the token is from a previous gateway process, and 404 means
      // this gateway has no question bridge at all. Both are recovered by
      // re-reading the endpoint file: the first because the file has been
      // rewritten, the second because it never will be and the retry loop will
      // run out its deadline.
      if (response.status === 401 || response.status === 404) {
        const reread = await readEndpoint().catch(() => null);
        if (reread !== null) {
          endpoint.url = reread.url;
          endpoint.token = reread.token;
        }
        if (Date.now() >= deadline) return next();
        await sleep(RETRY_MS[Math.min(attempt, RETRY_MS.length - 1)]);
        continue;
      }

      if (response.status === 400) {
        // The gateway rejected the request shape, which is this plugin's bug and
        // not something a retry can fix. Declining keeps the model's own error
        // path intact instead of inventing one.
        warn(ctx, `the gateway rejected a question request (${await bodyText(response)})`);
        return next();
      }

      if (!response.ok) {
        if (Date.now() >= deadline) return next();
        await sleep(RETRY_MS[Math.min(attempt, RETRY_MS.length - 1)]);
        continue;
      }

      const result = await response.json().catch(() => null);
      if (result === null || typeof result !== "object") return next();

      if (result.outcome === "answered") {
        return { answers: answersOf(result.answers) };
      }

      // An unanswered response is not always the same thing, and the turn's own
      // signal is what tells the two apart.
      //
      // A turn that was stopped has already aborted this call, and the harness's
      // own ASK_ABORTED is the accurate report — declining hands it that. A turn
      // that is still running means the *gateway* let the question go: it is
      // being redeployed, or its process is going away, and the human has still
      // never seen the question. Asking again under the same id is what turns
      // that into a pause instead of a lost decision, because the successor
      // parks the same card.
      //
      // Only "timeout" is final: that one means the human's window closed while
      // the gateway was up and answering, so there is nothing left to re-ask.
      if (isAborted(request)) throw aborted();
      if (result.reason !== "timeout" && Date.now() < deadline) {
        await sleep(RETRY_MS[Math.min(attempt, RETRY_MS.length - 1)]);
        continue;
      }

      throw new Error(unansweredMessage(result.reason));
    }
  });
}

/** Read and validate the endpoint description the gateway writes. */
async function readEndpoint() {
  const path = process.env[ENDPOINT_ENV];
  if (typeof path !== "string" || path === "") return null;

  const parsed = JSON.parse(await readFile(path, "utf8"));
  if (parsed === null || typeof parsed !== "object") return null;
  if (typeof parsed.url !== "string" || typeof parsed.token !== "string") return null;

  const timeoutMs = Number.isFinite(parsed.timeoutMs) && parsed.timeoutMs > 0
    ? parsed.timeoutMs
    : 600_000;
  return { url: parsed.url, token: parsed.token, timeoutMs };
}

/** The session that asked, or "" when the harness did not say. */
function sessionIDOf(request) {
  const agent = request === null || typeof request !== "object" ? undefined : request.agent;
  // Only `.id` is read: see rule 1 at the top of this file.
  return agent !== undefined && agent !== null && typeof agent.id === "string" ? agent.id : "";
}

/** Translate the harness's question shape into the gateway's. */
function questionsOf(request) {
  const items = Array.isArray(request.questions) ? request.questions : [];
  return items.map((item) => ({
    id: String(item.id ?? ""),
    ...(typeof item.header === "string" ? { header: item.header } : {}),
    question: String(item.question ?? ""),
    ...(typeof item.detail === "string" ? { detail: item.detail } : {}),
    ...(Array.isArray(item.options)
      ? {
          options: item.options.map((option) => ({
            label: String(option.label ?? ""),
            ...(typeof option.description === "string" ? { description: option.description } : {}),
            // Derived, never applied to the label: the model matches the label it
            // wrote, so the suffix has to survive into the answer verbatim. The
            // flag only lets a client draw a badge instead of printing it.
            ...(RECOMMENDED.test(String(option.label ?? "")) ? { recommended: true } : {}),
          })),
        }
      : {}),
    // Both spellings are accepted, and that is not defensiveness for its own
    // sake: the model writes `multi_select`, and the model-facing tool renames it
    // to `multiSelect` before dispatching this waterfall, so reading only the
    // model's spelling silently turned every multi-select question into a
    // single-choice one. Found by asking a live harness for one and watching the
    // gateway reject the second label.
    ...(item.multiSelect === true || item.multi_select === true ? { multiSelect: true } : {}),
  }));
}

/** Translate the gateway's answer shape back into the harness's. */
function answersOf(answers) {
  const list = Array.isArray(answers) ? answers : [];
  return list.map((answer) => ({
    id: String(answer.id ?? ""),
    selected: Array.isArray(answer.selected) ? answer.selected.map(String) : [],
    ...(typeof answer.custom === "string" && answer.custom !== ""
      ? { custom: answer.custom }
      : {}),
  }));
}

/** What the model is told when nobody answered. */
function unansweredMessage(reason) {
  switch (reason) {
    case "timeout":
      return "the user did not answer in time; ask again only if the answer is required, " +
        "or proceed with the most reasonable assumption and say which one you made";
    case "cancelled":
      return "the question was withdrawn because the turn was stopped";
    case "shutdown":
      return "the gateway restarted before the user answered; the question is no longer pending";
    default:
      return `the user did not answer (${String(reason) || "unknown reason"})`;
  }
}

/**
 * The harness's own wording for a call that was cancelled before it was answered.
 *
 * Thrown rather than declined, and the difference is not cosmetic: a listener
 * that declines is indistinguishable from one that does not exist, so the model
 * would be told "no user-questions answerer accepted the request" — which sends
 * it looking for a missing plugin when what actually happened is that the
 * operator pressed stop. The shape below is the one the harness restores into
 * its own `UserQuestionError` with code `ASK_ABORTED`. Verified against a live
 * turn cancellation, which is how the wrong message was found.
 */
function aborted() {
  const error = new Error("ask_user_question was aborted before the user answered");
  error.name = "UserQuestionError";
  error.code = "ASK_ABORTED";
  return error;
}

/** An error for a gateway that could not be reached at all. */
function unreachable(cause) {
  const detail = cause instanceof Error ? cause.message : String(cause);
  return new Error(
    "the dsh-gateway question bridge could not be reached, so the question was not shown " +
      `to anyone (${detail})`,
  );
}

/** Whether the harness has cancelled the call this listener is serving. */
function isAborted(request) {
  return request !== null && typeof request === "object" && request.signal?.aborted === true;
}

/** Read a bounded amount of an error response for a log line. */
async function bodyText(response) {
  try {
    return (await response.text()).slice(0, 200);
  } catch {
    return "";
  }
}

/** Log a warning without ever letting logging fail the tool call. */
function warn(ctx, message) {
  try {
    ctx.logger?.warn?.(`dsh-gateway-questions: ${message}`);
  } catch {
    // A logger that is absent or unhappy is not a reason to fail a question.
  }
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
