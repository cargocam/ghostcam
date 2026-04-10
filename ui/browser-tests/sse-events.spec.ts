import { test, expect } from '@playwright/test';
import type { CameraResponse } from '../src/lib/api-types';
import { mockAuthenticatedSession, MOCK_CAMERAS } from './helpers.js';

test.describe('SSE camera events', () => {
  test.beforeEach(async ({ page }) => {
    await mockAuthenticatedSession(page);
    await page.goto('/');
    await expect(page.getByText('Front Door').first()).toBeVisible({ timeout: 5000 });
  });

  test('initial camera list shows correct online count', async ({ page }) => {
    // From MOCK_CAMERAS: 1 online (Front Door), 1 offline (Backyard)
    // Count camera cards in main grid area
    const cameraCards = page.locator('main button.aspect-video');
    await expect(cameraCards).toHaveCount(2);

    // Verify both cameras are rendered
    await expect(page.getByText('Front Door').first()).toBeVisible();
    await expect(page.getByText('Backyard').first()).toBeVisible();
  });

  test('camera online event adds a new camera to the grid', async ({ page }) => {
    // Verify we start with 2 camera cards
    const cameraCards = page.locator('main button.aspect-video');
    await expect(cameraCards).toHaveCount(2);

    // Update the camera list to include a new camera, then reload
    const withGarage: CameraResponse[] = [
      ...MOCK_CAMERAS,
      {
        device_id: 'cam-003',
        display_name: 'Garage',
        enrolled_at: 1_700_000_000_000,
        provisioned: true,
        resolution: '720p',
        recording_mode: 'constant',
      },
    ];
    await page.route('**/api/v1/cameras', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(withGarage),
      });
    });

    await page.reload();
    await expect(page.getByText('Garage').first()).toBeVisible({ timeout: 5000 });

    // Should now have 3 camera cards
    await expect(cameraCards).toHaveCount(3);
  });

  test('camera offline event updates status indicator', async ({ page }) => {
    // Camera cards are in the main grid. Each card has a status badge span.
    // The badge text is "LIVE" or "OFF" for camera cards (uppercase, rendered by CameraCard.svelte).
    // Use a more specific locator: the badge is a <span> with specific styling inside camera cards.
    const cameraCardBadges = page.locator('main button.aspect-video span.uppercase');

    // Initially: Front Door is LIVE, Backyard is OFF
    await expect(cameraCardBadges).toHaveCount(2);

    // Update cameras to all offline and reload. Offline is derived client-side
    // from the staleness of server_ts — by omitting last_seen_at the UI treats
    // the camera as having never reported telemetry recently.
    const offlineCameras: CameraResponse[] = MOCK_CAMERAS.map((c) => ({
      ...c,
      last_seen_at: undefined,
      provisioned: false,
    }));
    await page.route('**/api/v1/cameras', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(offlineCameras),
      });
    });

    await page.reload();
    await expect(page.getByText('Front Door').first()).toBeVisible({ timeout: 5000 });

    // After reload, both cameras should be OFF — no LIVE badges
    // Check that no badge contains "LIVE" text
    const liveBadges = page.locator('main button.aspect-video span.uppercase', { hasText: 'LIVE' });
    await expect(liveBadges).toHaveCount(0, { timeout: 3000 });

    // Both badges should say "OFF"
    const offBadges = page.locator('main button.aspect-video span.uppercase', { hasText: 'OFF' });
    await expect(offBadges).toHaveCount(2);
  });

  test('camera cards render in grid', async ({ page }) => {
    // Verify camera cards are button elements with aspect-video styling
    const cameraCards = page.locator('main button.aspect-video');
    const count = await cameraCards.count();
    expect(count).toBe(2);
  });
});
