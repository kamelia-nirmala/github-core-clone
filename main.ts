// Entry point: the lifecycle between this process and the trial environment.
//
// The env owns the trial. This process only has to (1) start the forge server on
// a kernel-chosen port, (2) tell the env which port that is, and (3) stay alive
// until the env reports the trial is finished, then shut down cleanly.
//
// The handshake, step by step:
//   - MAKEFUN_ENV_URL points at the env (for example http://store:8080 inside a
//     trial container). With no such variable there is nobody to talk to, so the
//     process stops immediately with exit code 1.
//   - The server listens on port 0, letting the OS pick a free one. Port 8080 is
//     never used directly: that port belongs to the env.
//   - POST {env}/register with {"port": N}. The env remembers the caller's
//     address together with N and sends all later traffic there. Whichever
//     registration lands first wins; a 409 simply means somebody else already
//     won, and this process carries on waiting like any other entry.
//   - GET {env}/done over and over until the answer is {"done": true}. The env
//     replies right away ({"done": false} while the suite is still running), so
//     this is a plain poll. A poll that fails or times out is retried after a
//     short wait that grows exponentially; the process never quits early.
//   - Once done is true, close the server and exit 0. The harness then stops
//     the env, which writes the export the scorer reads.
//
// Only the server factory is imported here: no other package, no Node builtin,
// no model call.

import { createApp } from './app.ts';

// The env's origin, with any trailing slashes trimmed off.
const ENV_ORIGIN = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_ORIGIN === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

// Longest a single register or done call may run before it is given up on.
const CALL_TIMEOUT_MS = 30_000;
// Backoff between retries: start here, double each time, never pass the cap.
const FIRST_BACKOFF_MS = 250;
const MAX_BACKOFF_MS = 2_000;

// Wait `ms` milliseconds.
const pause = (ms: number): Promise<void> => new Promise((done) => setTimeout(done, ms));

// How long to wait after the `n`th failed attempt (0-based): exponential growth,
// capped, with the exponent itself capped so the number stays small.
function backoffFor(n: number): number {
  const exponent = n < 8 ? n : 8;
  const grown = FIRST_BACKOFF_MS * 2 ** exponent;
  return grown < MAX_BACKOFF_MS ? grown : MAX_BACKOFF_MS;
}

// Keep POSTing the bound port until the env takes it. A 200 is the normal
// answer and a 409 means another entry registered first, which is fine: both
// end the loop. A 400 or 405 is a contract error, so it stops the program.
// Anything else (network trouble, an odd status) is retried, because the env
// may still be coming up.
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

// Keep asking GET /done until the env answers {"done": true}. Never come back
// before that, however many polls fail in between.
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

// The whole life of the process: bind, register, wait for done, shut down.
async function run(): Promise<void> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => resolve());
  });
  const bound = server.address();
  if (bound === null || typeof bound === 'string') {
    throw new Error('the server did not bind an ephemeral TCP port');
  }
  process.stdout.write(`[entry] listening on port ${bound.port}\n`);

  await register(bound.port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await waitUntilDone();
  server.close();
  process.stdout.write('[entry] done\n');
}

try {
  await run();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
