import type { ServerResponse } from 'node:http';
import { sendJson } from './http.ts';
import type { Store } from './store.ts';

export function handleGraphql(res: ServerResponse, store: Store, body: Buffer): boolean {
  sendJson(res, 200, { data: null, errors: [{ message: 'not implemented yet' }] });
  return true;
}
