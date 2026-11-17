// Small HTTP helpers over node:http: bodies, JSON replies with the headers GitHub sends, and the
// request id every reply carries. Kept deliberately thin: it sits on the hot path of the suite's
// 2,000 issue reads and 200 issue writes.

import type { IncomingMessage, ServerResponse } from 'node:http';

const JSON_TYPE = 'application/json; charset=utf-8';

/** A process-wide counter: request ids are unique per process and never read from the clock. */
let requestCounter = 0;

export function nextRequestId(): string {
  requestCounter += 1;
  return `GH-${requestCounter.toString(36).toUpperCase()}`;
}

/** Read a request body into one buffer. A body over `limit` bytes is cut off at the limit. */
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

/** Send a buffer with its content type and the reply headers every REST answer carries. */
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

/** The error shape GitHub answers with: `{"message": "..."}`. */
export function sendError(res: ServerResponse, status: number, message: string, extra?: Record<string, string>): void {
  sendJson(res, status, { message }, extra);
}

/** A 204 with no body (DELETE). */
export function sendNoContent(res: ServerResponse): void {
  res.writeHead(204, { 'x-github-request-id': nextRequestId() });
  res.end();
}
