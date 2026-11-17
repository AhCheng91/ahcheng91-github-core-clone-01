// The thin launcher the contest runs: spawn `./forge` (the Go binary) from this file's directory,
// inherit its stdio so the `[entry]` lines flow to the harness, and exit with its status. The forge
// itself owns the lifecycle: bind an ephemeral port, register it with the env, serve, poll for done.
import { spawn } from 'node:child_process';

let ended = false;

const child = spawn('./forge', [], { cwd: import.meta.dirname, stdio: 'inherit' });

child.on('error', (err) => {
  if (ended) return;
  ended = true;
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
});

child.on('exit', (code) => {
  if (ended) return;
  ended = true;
  process.exit(code ?? 1);
});