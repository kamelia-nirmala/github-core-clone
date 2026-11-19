// Entry point for the Go forge: the lifecycle between this process and the
// trial environment. Same contract as the Node main.ts — (1) start the forge
// on a kernel-chosen port, (2) tell the env which port that is, (3) stay
// alive until the env reports the trial is finished, then shut down.
//
// The forge itself is a vendored static Go binary (./forge-go). The binary
// prints "PORT=<n>" on stdout once listening. Because a file pushed through
// the GitHub Contents API lands with mode 100644 (not executable), the
// launcher tries a direct spawn first and, on EACCES, copies the binary to
// the temp dir, chmods it 0755, and spawns the copy. /bot is read-only in
// the grading pod, so the copy goes to os.tmpdir().
//
// Only Node builtins are used here: no other package, no model call.

import { spawn, type ChildProcess } from 'node:child_process';
import { copyFile, chmod, mkdtemp } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ENV_ORIGIN = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_ORIGIN === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const CALL_TIMEOUT_MS = 30_000;
const FIRST_BACKOFF_MS = 250;
const MAX_BACKOFF_MS = 2_000;

const pause = (ms: number): Promise<void> => new Promise((done) => setTimeout(done, ms));

function backoffFor(n: number): number {
  const exponent = n < 8 ? n : 8;
  const grown = FIRST_BACKOFF_MS * 2 ** exponent;
  return grown < MAX_BACKOFF_MS ? grown : MAX_BACKOFF_MS;
}

async function register(port: number): Promise<void> {
  let attempt = 0;
  for (;;) {
    try {
      const res = await fetch(`${ENV_ORIGIN}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(CALL_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) {
        throw new Error(`register rejected with HTTP ${res.status}: ${await res.text()}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register rejected with HTTP ')) throw err;
    }
    await pause(backoffFor(attempt));
    attempt += 1;
  }
}

async function waitUntilDone(): Promise<void> {
  let attempt = 0;
  for (;;) {
    try {
      const res = await fetch(`${ENV_ORIGIN}/done`, { signal: AbortSignal.timeout(CALL_TIMEOUT_MS) });
      if (res.ok) {
        const payload = (await res.json()) as { done?: unknown };
        if (payload.done === true) return;
      }
    } catch {
      // No answer yet, or the poll timed out: the trial is not over, poll again.
    }
    await pause(backoffFor(attempt));
    attempt += 1;
  }
}

// Spawn the binary and wait for its "PORT=<n>" line. Resolves with the
// child process and the port. Rejects if the binary exits before printing.
function spawnForge(binaryPath: string): Promise<{ child: ChildProcess; port: number }> {
  return new Promise((resolve, reject) => {
    const child = spawn(binaryPath, [], {
      stdio: ['ignore', 'pipe', 'inherit'],
      // GOMAXPROCS=1: measured (PERF_LOG, ronde margin M1) — 1 thread is as
      // fast as 2/4 in the hard harness and has the smallest jitter; the
      // forge serializes requests on one mutex anyway.
      env: { ...process.env, GOMAXPROCS: '1' },
    });
    let buffer = '';
    let settled = false;
    child.stdout?.on('data', (chunk: Buffer) => {
      buffer += chunk.toString('utf8');
      const match = /PORT=(\d+)/.exec(buffer);
      if (match && !settled) {
        settled = true;
        resolve({ child, port: Number(match[1]) });
      }
    });
    child.on('error', (err) => {
      if (!settled) {
        settled = true;
        reject(err);
      }
    });
    child.on('exit', (code) => {
      if (!settled) {
        settled = true;
        reject(new Error(`forge binary exited early with code ${code}`));
      }
    });
  });
}

async function run(): Promise<void> {
  const here = dirname(fileURLToPath(import.meta.url));
  const binaryPath = join(here, 'forge-go');
  let child: ChildProcess;
  let port: number;
  try {
    ({ child, port } = await spawnForge(binaryPath));
  } catch (err) {
    // Direct spawn failed (most likely EACCES: the binary lost its exec bit
    // on the way through the GitHub Contents API). Copy it to the temp dir,
    // make it executable, and spawn the copy.
    const isAccess =
      err instanceof Error && ((err as NodeJS.ErrnoException).code === 'EACCES' || err.message.includes('EACCES'));
    if (!isAccess) throw err;
    process.stderr.write('[entry] direct spawn failed (EACCES); copying binary to tmpdir\n');
    const dir = await mkdtemp(join(tmpdir(), 'forge-'));
    const copyPath = join(dir, 'forge-go');
    await copyFile(binaryPath, copyPath);
    await chmod(copyPath, 0o755);
    ({ child, port } = await spawnForge(copyPath));
  }
  process.stdout.write(`[entry] listening on port ${port}\n`);

  await register(port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await waitUntilDone();
  child.kill('SIGTERM');
  process.stdout.write('[entry] done\n');
}

try {
  await run();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
