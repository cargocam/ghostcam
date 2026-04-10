// Stack readiness helpers. These only check health — they do NOT bring
// the stack up or down. That responsibility lives in the CI workflow
// and in the developer's local shell.

const STACK_READY_TIMEOUT_MS = 60_000;
const POLL_INTERVAL_MS = 1_000;

/**
 * Poll /readyz until the Go server reports healthy, or throw after
 * STACK_READY_TIMEOUT_MS. Use this inside a `beforeAll` so the first
 * spec doesn't race the compose-up.
 */
export async function waitForServerReady(
  baseURL = 'http://localhost:3000',
): Promise<void> {
  const deadline = Date.now() + STACK_READY_TIMEOUT_MS;
  let lastError: unknown = null;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`${baseURL}/readyz`);
      if (res.ok) return;
      lastError = new Error(`readyz returned ${res.status}`);
    } catch (err) {
      lastError = err;
    }
    await new Promise((r) => setTimeout(r, POLL_INTERVAL_MS));
  }
  throw new Error(
    `server did not become ready within ${STACK_READY_TIMEOUT_MS}ms: ${String(lastError)}`,
  );
}

/**
 * Poll the Vite dev server until it responds. The ui container takes a
 * few seconds to start Vite even after the server is up.
 */
export async function waitForUiReady(
  baseURL = 'http://localhost:5173',
): Promise<void> {
  const deadline = Date.now() + STACK_READY_TIMEOUT_MS;
  let lastError: unknown = null;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(baseURL);
      if (res.ok || res.status === 304) return;
      lastError = new Error(`${baseURL} returned ${res.status}`);
    } catch (err) {
      lastError = err;
    }
    await new Promise((r) => setTimeout(r, POLL_INTERVAL_MS));
  }
  throw new Error(
    `ui did not become ready within ${STACK_READY_TIMEOUT_MS}ms: ${String(lastError)}`,
  );
}

export const ADMIN_EMAIL = 'admin@ghostcam.dev';
export const ADMIN_PASSWORD = 'dev-password';
