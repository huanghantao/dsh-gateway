/**
 * Test driver for the question answerer plugin.
 *
 * The plugin is JavaScript that runs inside DeepSeek Harness, so the only way to
 * test what it actually sends is to run it. This driver loads the real module,
 * hands it a stubbed Cordis context, a stubbed `fetch`, and a request shaped the
 * way the harness's `user-questions` seam shapes one, then prints what the plugin
 * posted and what it returned.
 *
 * It is deliberately dumb: no assertions live here, because a failure should read
 * as a Go test failure with the JSON in hand rather than as a stack trace from a
 * script.
 *
 * Usage: node answerer_probe.mjs <plugin.mjs> <endpoint.json> <request.json>
 */

import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

const [pluginPath, endpointPath, requestPath] = process.argv.slice(2);

// The plugin finds the gateway through this variable; the file is the one the
// gateway writes for real, just pointed at a stub.
process.env.DSH_GATEWAY_QUESTIONS_ENDPOINT_FILE = endpointPath;

/** Everything the plugin posted to the "gateway". */
const posted = [];

globalThis.fetch = async (url, init) => {
  posted.push({ url, body: JSON.parse(init.body), headers: init.headers });
  const endpoint = JSON.parse(readFileSync(endpointPath, "utf8"));
  // `responses` scripts a sequence — the last entry repeats — so a test can make
  // the first attempt look like a gateway that withdrew the question and the
  // second like its successor answering it.
  const scripted = Array.isArray(endpoint.responses) ? endpoint.responses : null;
  const reply =
    scripted === null
      ? { outcome: "answered", answers: endpoint.answers }
      : scripted[Math.min(posted.length - 1, scripted.length - 1)];
  return {
    ok: true,
    status: 200,
    json: async () => reply,
  };
};

const module = await import(pathToFileURL(pluginPath).href);

let listener = null;
const warnings = [];
module.apply({
  logger: { warn: (message) => warnings.push(String(message)) },
  on(name, callback) {
    if (name === "user-questions/request") listener = callback;
  },
});

if (listener === null) {
  console.log(JSON.stringify({ error: "the plugin registered no answerer" }));
  process.exit(0);
}

const request = JSON.parse(readFileSync(requestPath, "utf8"));

// The calling agent is a live Cordis context in the harness: reading `.id` is
// documented, and every other property throws. Modelling that here is the point
// — an implementation that merely logs the request would fail loudly instead of
// silently working in a test and breaking in production.
const agent = new Proxy(
  { id: request.agentId },
  {
    get(target, property) {
      if (property === "id") return target.id;
      if (typeof property === "symbol") return undefined;
      throw new Error(`cannot get property "${String(property)}" without inject`);
    },
    ownKeys() {
      throw new Error("cannot enumerate a live agent context");
    },
    getOwnPropertyDescriptor() {
      throw new Error("cannot describe a live agent context");
    },
  },
);

let declined = false;
const next = async () => {
  declined = true;
  return { declined: true };
};

// `aborted` models the turn signal the harness hands the tool call: it is how a
// stopped turn reaches the answerer.
const signal = request.aborted === true ? { aborted: true } : undefined;

let result = null;
let thrown = null;
let thrownName = "";
let thrownCode = "";
try {
  result = await listener({ ...request, agent, signal }, next);
  declined = declined || result === undefined;
} catch (error) {
  thrown = String(error && error.message ? error.message : error);
  thrownName = error && error.name ? String(error.name) : "";
  thrownCode = error && error.code ? String(error.code) : "";
}

console.log(
  JSON.stringify({
    posted,
    warnings,
    declined,
    result: declined ? null : result,
    thrown,
    thrownName,
    thrownCode,
  }),
);
