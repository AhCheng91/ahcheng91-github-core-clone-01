import type { IncomingMessage, ServerResponse } from 'node:http';
import { sendError } from './http.ts';
import type { Store } from './store.ts';

export async function handleMcp(req: IncomingMessage, res: ServerResponse, store: Store, body: Buffer): Promise<void> {
  sendError(res, 404, 'Not Found');
}
