// Shared setup for the browser-tests/ suite.
//
// IMPORTANT: these tests are NOT end-to-end tests. Every /api/v1/**, /hls/**,
// and /events request is intercepted by page.route() below and answered with
// hand-written fixtures. The Go server, Postgres, Redis, and S3 are never
// touched. What these tests verify is limited to frontend wiring — form
// state, route transitions, DOM rendering against a known response shape —
// and nothing downstream of the HTTP boundary.
//
// EVERY fixture body below is typed against the tygo-generated
// $lib/api-types/ file. TypeScript refuses to compile a fixture that's
// missing a required field, has an extra field, or uses the wrong type.
// When a server struct changes, `make generate-types` regenerates the TS
// types and this file stops compiling until the fixtures are updated.
// That's the point — the type system is the drift detector.

import type { Page } from '@playwright/test';
import type {
  CameraResponse,
  CoverageResponse,
  LoginResponse,
  TelemetryRangeResponse,
} from '../src/lib/api-types';

/** Standard mock camera list returned by /api/v1/cameras */
export const MOCK_CAMERAS: CameraResponse[] = [
  {
    device_id: 'cam-001',
    display_name: 'Front Door',
    enrolled_at: 1_700_000_000_000,
    last_seen_at: 1_775_000_000,
    provisioned: true,
    resolution: '720p',
    recording_mode: 'constant',
  },
  {
    device_id: 'cam-002',
    display_name: 'Backyard',
    enrolled_at: 1_700_000_000_000,
    provisioned: false,
    resolution: '720p',
    // Streaming-only: exercises the new default in the store/settings dialog.
    recording_mode: 'never',
  },
];

/**
 * Set up route intercepts so the app works without a real backend.
 * Call this before navigating to the app.
 */
export async function mockApiRoutes(page: Page, { authenticated = false } = {}) {
  // Login endpoint
  await page.route('**/api/v1/auth/login', async (route) => {
    const body = route.request().postDataJSON();
    if (body?.email === 'test@example.com' && body?.password === 'correct-password') {
      const response: LoginResponse = { user_id: 'mock-user-1' };
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(response),
        headers: { 'Set-Cookie': 'ghostcam-token=mock-session; Path=/' },
      });
    } else {
      await route.fulfill({
        status: 401,
        contentType: 'application/json',
        body: JSON.stringify({ error: 'Invalid password' }),
      });
    }
  });

  // Logout endpoint
  await page.route('**/api/v1/auth/logout', async (route) => {
    await route.fulfill({ status: 200, body: '{}' });
  });

  // Camera list — doubles as session check (checkSession calls GET /api/v1/cameras)
  await page.route('**/api/v1/cameras', async (route) => {
    if (authenticated) {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(MOCK_CAMERAS),
      });
    } else {
      await route.fulfill({ status: 401, body: 'Unauthorized' });
    }
  });

  // HLS playlist/segment requests — return 404 so they don't hit the proxy
  await page.route('**/hls/**', async (route) => {
    const url = route.request().url();
    if (url.includes('/coverage')) {
      const response: CoverageResponse = { segments: [] };
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(response),
      });
    } else {
      await route.fulfill({ status: 404, body: 'Not found' });
    }
  });

  // Telemetry endpoints — return an empty range response.
  await page.route('**/api/v1/telemetry/**', async (route) => {
    const response: TelemetryRangeResponse = { entries: [] };
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(response),
    });
  });

  // SSE events endpoint — return an open connection that sends no events by default.
  await page.route('**/events', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'text/event-stream',
      headers: {
        'Cache-Control': 'no-cache',
        Connection: 'keep-alive',
      },
      body: ':ok\n\n',
    });
  });
}

/**
 * Mock routes for an authenticated session — cameras are returned immediately.
 */
export async function mockAuthenticatedSession(page: Page) {
  await mockApiRoutes(page, { authenticated: true });
}

/**
 * Wait for the main view to be visible after authentication.
 * Uses the sidebar camera list item which has a unique structure.
 */
export async function waitForMainView(page: Page) {
  // The sidebar CameraList renders camera names in a button.
  // Wait for the camera grid area to be populated.
  // Use the LIVE badge from camera cards as the indicator — it's unique to the grid.
  await page.locator('button:has-text("Front Door") >> nth=0').waitFor({ timeout: 5000 });
}

/**
 * Perform login via the UI and set up authenticated mocks after.
 * Navigates to the app, fills the login form, and submits.
 */
export async function loginViaUi(page: Page) {
  // Start unauthenticated
  await mockApiRoutes(page, { authenticated: false });
  await page.goto('/');

  // Fill in and submit
  await page.getByPlaceholder('Email').fill('test@example.com');
  await page.getByPlaceholder('Password').fill('correct-password');

  // After submitting, the app will re-check session and fetch cameras.
  // Switch the cameras route to return 200.
  await page.route('**/api/v1/cameras', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(MOCK_CAMERAS),
    });
  });

  await page.getByRole('button', { name: 'Sign in' }).click();
}
