import { test, expect } from '@playwright/test';
import { mockAuthenticatedSession, MOCK_CAMERAS } from './helpers.js';

test.describe('Camera grid', () => {
  test.beforeEach(async ({ page }) => {
    await mockAuthenticatedSession(page);
    await page.goto('/');
    // Wait for camera grid to populate — use first() since name appears in sidebar + grid
    await expect(page.getByText('Front Door').first()).toBeVisible({ timeout: 5000 });
  });

  test('displays camera cards with names', async ({ page }) => {
    // Camera card area (main content, not sidebar) shows camera names
    // The camera cards are in the <main> element. Check both names exist.
    await expect(page.getByText('Front Door').first()).toBeVisible();
    await expect(page.getByText('Backyard').first()).toBeVisible();
  });

  test('shows online/offline status indicators', async ({ page }) => {
    // Camera cards show LIVE / OFF badges in uppercase
    // The sidebar shows "Live" / "Off" (capitalized).
    // Camera cards show "LIVE" / "OFF" (all caps).
    await expect(page.getByText('LIVE', { exact: true }).first()).toBeVisible();
    await expect(page.getByText('OFF', { exact: true }).first()).toBeVisible();
  });

  test('clicking a camera card selects it (ring highlight)', async ({ page }) => {
    // Camera cards are <button> elements with aspect-video class.
    // Find the card in the main grid area (not sidebar).
    const cameraCards = page.locator('main button.aspect-video');

    // Click the first camera card (Front Door, which is online and sorted first)
    const firstCard = cameraCards.first();
    await firstCard.click();

    // Selected card should have ring-primary (the selection ring)
    await expect(firstCard).toHaveClass(/ring-primary/);
  });

  test('clicking selected card again deselects it', async ({ page }) => {
    const cameraCards = page.locator('main button.aspect-video');
    const firstCard = cameraCards.first();

    // Select
    await firstCard.click();
    await expect(firstCard).toHaveClass(/ring-primary/);

    // Deselect
    await firstCard.click();
    await expect(firstCard).not.toHaveClass(/ring-primary/);
  });

  test('double-clicking a camera card opens camera view', async ({ page }) => {
    const cameraCards = page.locator('main button.aspect-video');
    await cameraCards.first().dblclick();

    // Camera view renders a full-screen container with bg-black
    await expect(page.locator('.h-dvh.bg-black')).toBeVisible({ timeout: 3000 });
  });

  test('shows empty state when no cameras are connected', async ({ page }) => {
    // Override cameras route to return empty list
    await page.route('**/api/v1/cameras', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify([]),
      });
    });

    await page.goto('/');
    // The empty state text appears in both sidebar CameraList and LiveView
    await expect(page.getByText('No cameras connected').first()).toBeVisible({ timeout: 5000 });
  });
});
