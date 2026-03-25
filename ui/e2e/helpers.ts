import type { Page } from '@playwright/test';

/** Standard mock camera list returned by /api/v1/cameras */
export const MOCK_CAMERAS = [
  {
    device_id: 'cam-001',
    display_name: 'Front Door',
    group_id: 'default',
    capabilities: ['video', 'audio'],
    online: true,
  },
  {
    device_id: 'cam-002',
    display_name: 'Backyard',
    group_id: 'default',
    capabilities: ['video'],
    online: false,
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
    if (body?.password === 'correct-password') {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ ok: true }),
        headers: { 'Set-Cookie': 'session=mock-session; Path=/' },
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
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ online: true, segments: [] }),
      });
    } else {
      await route.fulfill({ status: 404, body: 'Not found' });
    }
  });

  // Watch endpoint (WebRTC — return a minimal mock)
  await page.route('**/api/v1/watch', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ session_id: 'mock-session-1', sdp_answer: '' }),
    });
  });

  // Session teardown
  await page.route('**/api/v1/session/*', async (route) => {
    await route.fulfill({ status: 200, body: '{}' });
  });

  // Telemetry endpoints
  await page.route('**/api/v1/telemetry/**', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ entries: [] }),
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
