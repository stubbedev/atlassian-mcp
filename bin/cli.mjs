#!/usr/bin/env node
// npm/npx launcher. Ensures the prebuilt Go binary for this platform is present
// (npx @latest guarantees the newest version), then hands stdio to it directly.
//
// It also injects the bundled ffmpeg-static / ffprobe-static binaries via env so
// video and animated-image attachments work with zero extra setup on the npm
// install path. Users who install the Go binary directly (go install / Nix)
// supply ffmpeg/ffprobe on PATH, or set ATLASSIAN_MCP_FFMPEG_PATH / _FFPROBE_PATH.
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

// Resolve bundled ffmpeg/ffprobe and expose them to the Go binary unless the
// user already pinned a path. Best-effort: missing modules are non-fatal.
const env = { ...process.env };
async function resolveBundled(envKey, importer) {
  if (env[envKey]) return;
  try {
    const p = await importer();
    if (p) env[envKey] = p;
  } catch { /* dependency absent — fall back to PATH lookup in the binary */ }
}
await resolveBundled('ATLASSIAN_MCP_FFMPEG_PATH', async () => {
  const m = await import('ffmpeg-static');
  return m.default ?? m;
});
await resolveBundled('ATLASSIAN_MCP_FFPROBE_PATH', async () => {
  const m = await import('ffprobe-static');
  return (m.default ?? m)?.path;
});

const child = spawn(bin, process.argv.slice(2), { stdio: 'inherit', env });

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
