import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  // Browser-based frontend smoke tests. These run in a real Chromium but
  // intercept every /api/v1/**, /hls/**, and /events request via page.route()
  // and return hand-written fixtures — they do NOT exercise the Go server,
  // the database, Redis, or S3. Treat them as UI integration tests, not
  // end-to-end tests. See browser-tests/helpers.ts for the mock surface.
  testDir: './browser-tests',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: process.env.CI ? 'github' : 'list',
  timeout: 30_000,
  expect: { timeout: 5_000 },

  use: {
    baseURL: 'http://localhost:5173',
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
  },

  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],

  webServer: {
    command: 'bun run dev',
    url: 'http://localhost:5173',
    reuseExistingServer: !process.env.CI,
    timeout: 15_000,
  },
});
