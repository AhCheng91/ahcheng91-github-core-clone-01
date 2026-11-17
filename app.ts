// The forge's HTTP surface: one process, one store. The lifecycle lives in main.ts; this file
// only answers requests.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { readBody, sendError, sendJson } from './src/http.ts';
import { handleRest } from './src/rest.ts';
import { handleGraphql } from './src/graphql.ts';
import { handleMcp } from './src/mcp.ts';
import { handleGit } from './src/smart.ts';
import { type Store, VIEWER, createStore } from './src/store.ts';

const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';

/** True when the request carries the trial's bearer token. */
function authorized(req: IncomingMessage): boolean {
  const header = req.headers.authorization;
  if (TOKEN === '') return typeof header === 'string' && header.startsWith('Bearer ');
  return header === `Bearer ${TOKEN}`;
}

/** The path and query of a request URL; never throws. */
function parseUrl(req: IncomingMessage): { path: string; query: URLSearchParams } {
  const raw = req.url ?? '/';
  const index = raw.indexOf('?');
  const path = index < 0 ? raw : raw.slice(0, index);
  const query = new URLSearchParams(index < 0 ? '' : raw.slice(index + 1));
  return { path, query };
}

export function createApp(): Server {
  const store: Store = createStore();
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void handle(req, res, store);
  });
}

async function handle(req: IncomingMessage, res: ServerResponse, store: Store): Promise<void> {
  const method = req.method ?? 'GET';
  const { path, query } = parseUrl(req);
  try {
    // The env's boot probe: two calls, no token.
    if (path === '/healthz') {
      sendJson(res, 200, { ok: true });
      return;
    }

    // Smart HTTP: /{owner}/{repo}.git/...
    const gitIndex = path.indexOf('.git/');
    if (gitIndex > 0 && path.startsWith('/')) {
      if (!authorized(req)) {
        sendError(res, 401, 'Requires authentication');
        return;
      }
      const body = await readBody(req);
      const ownerAndRepo = path.slice(1, gitIndex);
      const slash = ownerAndRepo.indexOf('/');
      if (slash > 0) {
        const owner = decodeURIComponent(ownerAndRepo.slice(0, slash));
        const name = decodeURIComponent(ownerAndRepo.slice(slash + 1));
        const suffix = path.slice(gitIndex + 5);
        await handleGit(req, res, store, owner, name, suffix, query, body);
        return;
      }
    }

    if (path === '/mcp' || path.startsWith('/mcp/')) {
      const body = await readBody(req);
      await handleMcp(req, res, store, body);
      return;
    }

    if (path === '/api/graphql') {
      if (!authorized(req)) {
        sendJson(res, 401, { message: 'Requires authentication' });
        return;
      }
      const body = await readBody(req);
      handleGraphql(res, store, body);
      return;
    }

    if (path === '/api/v3' || path.startsWith('/api/v3/')) {
      if (!authorized(req)) {
        sendError(res, 401, 'Requires authentication');
        return;
      }
      const body = await readBody(req);
      const rest = path === '/api/v3' ? '' : path.slice('/api/v3/'.length);
      const segments = rest === '' ? [] : rest.split('/').map((part) => decodeURIComponent(part));
      const host = req.headers.host ?? '127.0.0.1';
      const handled = handleRest({
        store,
        method,
        segments,
        query,
        base: `http://${host}`,
        body,
        res,
      });
      if (!handled) sendError(res, 404, 'Not Found');
      return;
    }

    sendError(res, 404, 'Not Found');
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    if (!res.headersSent) sendError(res, 500, `Internal error: ${message}`);
    else res.end();
  }
}

export { VIEWER };
