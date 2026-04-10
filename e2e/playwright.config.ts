import { defineConfig, devices } from '@playwright/test';

// Real end-to-end tests. Unlike ui/browser-tests/ these DO hit the live
// Go server, Postgres, Redis, and MinIO stack brought up by
// `docker compose --profile test up -d`. The suite assumes the stack is
// already running; it does not spin up or tear down compose itself.
// Local run (macOS):
//
//   export GHOSTCAM_PUBLIC_IP=$(ipconfig getifaddr en0)
//   export STRIPE_SECRET_KEY=   # keep empty so the admin is unlimited
//   docker compose --profile test up -d
//   cd e2e && bun install && bunx playwright install --with-deps chromium
//   bun run test
//   docker compose down -v
//
// CI runs the equivalent in .github/workflows/ci.yml (e2e job).

export default defineConfig({
  testDir: '.',
  // Run specs serially. They share a live DB and are small in number;
  // parallelism just introduces flakes from shared state.
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  reporter: process.env.CI ? 'github' : 'list',
  // Generous timeout for real network paths (SSE subscription + first
  // telemetry arrival through Redis streams can take up to ~15s on a
  // cold stack).
  timeout: 60_000,
  expect: { timeout: 15_000 },

  use: {
    baseURL: 'http://localhost:5173',
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },

  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],

  // NOTE: no webServer config. The stack is external — if it isn't up,
  // the tests fail fast at the first navigation.
});
