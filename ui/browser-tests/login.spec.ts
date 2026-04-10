import { test, expect } from '@playwright/test';
import { mockApiRoutes, MOCK_CAMERAS } from './helpers.js';

test.describe('Login flow', () => {
  test('shows the login form on first load', async ({ page }) => {
    await mockApiRoutes(page, { authenticated: false });
    await page.goto('/');

    await expect(page.getByRole('heading', { name: 'Ghostcam' })).toBeVisible();
    await expect(page.getByPlaceholder('Email')).toBeVisible();
    await expect(page.getByPlaceholder('Password')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Sign in' })).toBeVisible();
  });

  test('sign-in button is disabled when email or password is empty', async ({ page }) => {
    await mockApiRoutes(page, { authenticated: false });
    await page.goto('/');

    await expect(page.getByRole('button', { name: 'Sign in' })).toBeDisabled();

    await page.getByPlaceholder('Email').fill('test@example.com');
    await expect(page.getByRole('button', { name: 'Sign in' })).toBeDisabled();
  });

  test('shows error on wrong password', async ({ page }) => {
    await mockApiRoutes(page, { authenticated: false });
    await page.goto('/');

    await page.getByPlaceholder('Email').fill('test@example.com');
    await page.getByPlaceholder('Password').fill('wrong-password');
    await page.getByRole('button', { name: 'Sign in' }).click();

    await expect(page.getByText('Invalid email or password')).toBeVisible();
  });

  test('successful login shows the main view', async ({ page }) => {
    await mockApiRoutes(page, { authenticated: false });
    await page.goto('/');

    await page.getByPlaceholder('Email').fill('test@example.com');
    await page.getByPlaceholder('Password').fill('correct-password');

    // After login succeeds, the app calls checkSession (GET /cameras) and listCameras.
    // Switch the cameras route to return 200 after login.
    await page.route('**/api/v1/cameras', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify(MOCK_CAMERAS),
      });
    });

    await page.getByRole('button', { name: 'Sign in' }).click();

    // After login, the login form should disappear
    await expect(page.getByPlaceholder('Password')).toBeHidden({ timeout: 5000 });

    // Camera names from mock data should appear (use first() since name shows in sidebar + grid)
    await expect(page.getByText('Front Door').first()).toBeVisible({ timeout: 5000 });
  });

  test('shows connection error when login request fails', async ({ page }) => {
    // Override login to return a network error
    await page.route('**/api/v1/auth/login', async (route) => {
      await route.abort('connectionrefused');
    });
    await page.route('**/api/v1/cameras', async (route) => {
      await route.fulfill({ status: 401, body: 'Unauthorized' });
    });
    await page.route('**/events', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: ':ok\n\n',
      });
    });
    await page.route('**/hls/**', async (route) => {
      await route.fulfill({ status: 404, body: '' });
    });

    await page.goto('/');
    await page.getByPlaceholder('Email').fill('test@example.com');
    await page.getByPlaceholder('Password').fill('any-password');
    await page.getByRole('button', { name: 'Sign in' }).click();

    await expect(page.getByText('Connection failed')).toBeVisible();
  });
});
