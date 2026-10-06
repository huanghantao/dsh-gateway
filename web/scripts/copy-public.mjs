/**
 * Copies the static half of the app into `dist/`.
 *
 * `tsc` only knows about `.ts`, so this runs as npm's `prebuild` hook: every
 * `npm run build` refreshes the shell before the compiler runs, and `dist/` is
 * never left holding a stylesheet older than the markup that references it.
 *
 * `--clean` empties `dist/` first, and that is what `prebuild` passes. The
 * compiler does not remove output whose source was deleted or renamed, so
 * without it a module that moved keeps its compiled copy in `dist/` forever —
 * and `dist/` is what the Go binary embeds, which makes "the output is the
 * input, file for file" false in a way nothing checks. `npm run assets`
 * deliberately does not pass it: that command exists to refresh CSS and HTML
 * under a running `tsc --watch`, and emptying the directory would take the
 * compiled modules with it.
 *
 * The files are copied *verbatim*, `{{NONCE}}` included. Substitution belongs to
 * the Go server, which is the only thing that knows the per-response nonce; a
 * build-time placeholder would defeat the point.
 */

import { cp, mkdir, readdir, rm, stat } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const webRoot = join(here, "..");
const dist = join(webRoot, "dist");
const clean = process.argv.includes("--clean");

/** `[source, destination]`, relative to `web/`. */
const FILES = [
  ["index.html", "index.html"],
  [join("src", "styles.css"), "styles.css"],
];

async function copyDirectory(from, to) {
  await mkdir(to, { recursive: true });
  for (const entry of await readdir(from)) {
    const source = join(from, entry);
    const target = join(to, entry);
    const info = await stat(source);
    if (info.isDirectory()) await copyDirectory(source, target);
    else await cp(source, target);
  }
}

if (clean) await rm(dist, { recursive: true, force: true });
await mkdir(dist, { recursive: true });

for (const [from, to] of FILES) {
  await cp(join(webRoot, from), join(dist, to));
}

await copyDirectory(join(webRoot, "public"), dist);

process.stdout.write(`web: shell assets copied into ${dist}\n`);
