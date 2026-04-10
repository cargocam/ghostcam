import { test, expect } from '@playwright/test';
import { mockAuthenticatedSession } from './helpers.js';

test.describe('Settings', () => {
  test.beforeEach(async ({ page }) => {
    await mockAuthenticatedSession(page);
    await page.goto('/');
    // Wait for main view to load — use first() since name appears in sidebar + grid
    await expect(page.getByText('Front Door').first()).toBeVisible({ timeout: 5000 });
  });

  test('theme persists in localStorage', async ({ page }) => {
    // Set dark theme via localStorage and verify it sticks after reload
    await page.evaluate(() => {
      localStorage.setItem('ghostcam-theme', 'dark');
    });
    await page.reload();
    await expect(page.getByText('Front Door').first()).toBeVisible({ timeout: 5000 });

    const theme = await page.evaluate(() => localStorage.getItem('ghostcam-theme'));
    expect(theme).toBe('dark');

    // The html element should have the 'dark' class
    await expect(page.locator('html')).toHaveClass(/dark/);
  });

  test('light theme removes dark class', async ({ page }) => {
    await page.evaluate(() => {
      localStorage.setItem('ghostcam-theme', 'light');
    });
    await page.reload();
    await expect(page.getByText('Front Door').first()).toBeVisible({ timeout: 5000 });

    await expect(page.locator('html')).not.toHaveClass(/dark/);
  });

  test('grid layout persists in localStorage', async ({ page }) => {
    await page.evaluate(() => {
      localStorage.setItem('ghostcam-grid', '1+5');
    });
    await page.reload();
    await expect(page.getByText('Front Door').first()).toBeVisible({ timeout: 5000 });

    const layout = await page.evaluate(() => localStorage.getItem('ghostcam-grid'));
    expect(layout).toBe('1+5');
  });

  test('mute state persists in localStorage', async ({ page }) => {
    // Default is muted (globalMuted=true). Set to unmuted.
    await page.evaluate(() => {
      localStorage.setItem('ghostcam-muted', 'false');
    });
    await page.reload();
    await expect(page.getByText('Front Door').first()).toBeVisible({ timeout: 5000 });

    const muted = await page.evaluate(() => localStorage.getItem('ghostcam-muted'));
    expect(muted).toBe('false');
  });
});
