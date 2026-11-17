// The shared node:http helpers every surface replies through: reading request bodies and sending
// JSON with the headers GitHub sends. Everything here stays on the hot path (2,000 issue reads,
// 200 issue writes), so it is kept as light as possible.

import type { IncomingMessage, ServerResponse } from 'node:http';

const JSON_TYPE = 'application/json; charset=utf-8';

/** A process-wide counter; request ids are unique per process and never read from the clock. */
let requestCounter = 0;

/** The next reply id: `GH-` plus the counter in base 36, upper-cased. */
export function nextRequestId(): string {
  requestCounter += 1;
  return `GH-${requestCounter.toString(36).toUpperCase()}`;
}

/**
 * Read a request body into one buffer. A body longer than `limit` bytes is cut off at the limit
 * (the request is answered as if that were the whole body).
 */
export function readBody(req: IncomingMessage, limit = 128 * 1024 * 1024): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let length = 0;
    req.on('data', (chunk: Buffer) => {
      length += chunk.length;
      if (length > limit) {
        chunks.push(chunk.subarray(0, Math.max(0, chunk.length - (length - limit))));
        req.removeAllListeners('data');
        resolve(Buffer.concat(chunks));
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

/**
 * Send `body` with its content type, its length and a fresh request id; any extra headers are
 * merged in first-come (they override nothing already set except by key). The caller passes the
 * exact content type: git and streamed surfaces need other values than JSON.
 */
export function sendBuffer(res: ServerResponse, status: number, body: Buffer, contentType: string, extra?: Record<string, string>): void {
  const headers: Record<string, string | number> = {
    'content-type': contentType,
    'content-length': body.length,
    'x-github-request-id': nextRequestId(),
  };
  if (extra !== undefined) for (const key of Object.keys(extra)) headers[key] = extra[key];
  res.writeHead(status, headers);
  res.end(body);
}

/** Send an already-serialised JSON body (the fast path: no re-encoding on every read). */
export function sendJsonText(res: ServerResponse, status: number, text: string, extra?: Record<string, string>): void {
  sendBuffer(res, status, Buffer.from(text, 'utf8'), JSON_TYPE, extra);
}

/** Serialise and send a JSON body. */
export function sendJson(res: ServerResponse, status: number, body: unknown, extra?: Record<string, string>): void {
  sendJsonText(res, status, JSON.stringify(body), extra);
}

/** The error shape GitHub answers with: `{"message": "..."}` on REST. */
export function sendError(res: ServerResponse, status: number, message: string, extra?: Record<string, string>): void {
  sendJson(res, status, { message }, extra);
}

/** A 204 with no body (DELETE). */
export function sendNoContent(res: ServerResponse): void {
  res.writeHead(204, { 'x-github-request-id': nextRequestId() });
  res.end();
}