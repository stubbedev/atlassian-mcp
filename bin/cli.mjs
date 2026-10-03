#!/usr/bin/env node
// npm/npx launcher. Ensures the prebuilt Go binary for this platform is present
// (npx @latest guarantees the newest version), then hands stdio to it directly.
//
// Note on overhead: with stdio 'inherit' the Go binary reads/writes the real
// stdin/stdout, so this launcher adds ZERO per-message latency — it only relays
// signals and the exit code.
import { spawn } from 'node:child_process';
import { ensureBinary } from '../scripts/download.mjs';

let bin;
try {
  bin = await ensureBinary();
} catch (err) {
  console.error(`[atlassian-mcp] ${err.message}`);
  process.exit(1);
}

const child = spawn(bin, process.argv.slice(2), { stdio: 'inherit' });

// Forward termination signals so the Go binary shuts down cleanly with its host.
for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP', 'SIGQUIT']) {
  process.on(sig, () => {
    if (!child.killed) child.kill(sig);
  });
}

child.on('exit', (code, signal) => {
  if (signal) process.kill(process.pid, signal);
  else process.exit(code ?? 0);
});
child.on('error', (err) => {
  console.error(`[atlassian-mcp] failed to start binary: ${err.message}`);
  process.exit(1);
});
