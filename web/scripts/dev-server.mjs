/**
 * A development static server for `web/dist`.
 *
 * This is not part of the product — the Go gateway serves and embeds `dist/` —
 * but without it the frontend can only be exercised by building the whole Go
 * binary. It exists mainly to make the nonce contract executable: it substitutes
 * `{{NONCE}}` in `index.html` and sends the same Content-Security-Policy the Go
 * middleware does, so a style attribute or a `style.setProperty` added by
 * mistake fails here rather than in production.
 *
 * Usage: `npm run serve -- --port 8099`
 */

import { createHash, randomBytes } from "node:crypto";
import { readFile, stat } from "node:fs/promises";
import { createServer } from "node:http";
import { dirname, extname, join, normalize } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const dist = join(here, "..", "dist");

const portArg = process.argv.indexOf("--port");
const port = portArg === -1 ? 8099 : Number(process.argv[portArg + 1]);

const TYPES = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".json": "application/json; charset=utf-8",
  ".webmanifest": "application/manifest+json; charset=utf-8",
  ".svg": "image/svg+xml",
  ".map": "application/json; charset=utf-8",
};

function policy(nonce) {
  return [
    "default-src 'self'",
    `script-src 'self' 'nonce-${nonce}'`,
    `style-src 'self' 'nonce-${nonce}'`,
    "img-src 'self' data: blob:",
    "font-src 'self'",
    "connect-src 'self'",
    "frame-ancestors 'none'",
    "base-uri 'none'",
    "object-src 'none'",
  ].join("; ");
}

const server = createServer((request, response) => {
  void (async () => {
    const url = new URL(request.url ?? "/", `http://${request.headers.host ?? "localhost"}`);
    const nonce = randomBytes(16).toString("base64");
    const headers = {
      "Content-Security-Policy": policy(nonce),
      "X-Content-Type-Options": "nosniff",
      "Referrer-Policy": "no-referrer",
    };

    if (url.pathname.startsWith("/api/")) {
      response.writeHead(501, { ...headers, "Content-Type": "application/problem+json" });
      response.end(
        JSON.stringify({
          type: "urn:dsh-gateway:problem:no_backend",
          title: "Not Implemented",
          status: 501,
          code: "no_backend",
          retryable: false,
          detail: "The dev server serves the shell only; run the Go gateway for the API.",
        }),
      );
      return;
    }

    const relative = url.pathname === "/" ? "index.html" : normalize(url.pathname).replace(/^\/+/, "");
    const file = join(dist, relative === "" ? "index.html" : relative);
    if (!file.startsWith(dist)) {
      response.writeHead(403, headers);
      response.end();
      return;
    }

    try {
      const info = await stat(file);
      if (!info.isFile()) throw new Error("not a file");
      const body = await readFile(file);
      const type = TYPES[extname(file)] ?? "application/octet-stream";
      if (type.startsWith("text/html")) {
        response.writeHead(200, { ...headers, "Content-Type": type, "Cache-Control": "no-store" });
        response.end(body.toString("utf8").replaceAll("{{NONCE}}", nonce));
        return;
      }
      const etag = `"${createHash("sha1").update(body).digest("hex")}"`;
      response.writeHead(200, { ...headers, "Content-Type": type, ETag: etag, "Cache-Control": "no-cache" });
      response.end(body);
    } catch {
      response.writeHead(404, { ...headers, "Content-Type": "text/plain; charset=utf-8" });
      response.end("not found");
    }
  })();
});

server.listen(port, "127.0.0.1", () => {
  process.stdout.write(`web: shell on http://127.0.0.1:${port}/ (API calls will 501)\n`);
});
