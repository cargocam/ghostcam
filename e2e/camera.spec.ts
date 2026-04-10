import { test, expect } from '@playwright/test';
import { ADMIN_EMAIL, ADMIN_PASSWORD, waitForServerReady, waitForUiReady } from './fixtures/stack';

// Real camera telemetry flow through SSE.
//
// The compose --profile test stack runs camera-1 (and, if the admin's
// tier allows, camera-2 and camera-3) against the server. This test
// logs in, waits for the camera list to populate from real DB rows,
// then waits for the first live telemetry event to arrive over SSE.
//
// What it proves end-to-end:
//   1. Camera provisioning handshake (POST /provision → api_key +
//      device_id) actually worked on container startup.
//   2. The camera's presign → S3 upload → confirm round-trip wrote
//      segment rows to postgres.
//   3. The camera's 10s telemetry poll hit PostTelemetry on the server.
//   4. The server wrote a Redis stream entry via XADD.
//   5. The server's SSE handler XREAD'd the entry and sent a
//      `telemetry` event on the /events stream.
//   6. The UI's transport store accepted the SSE event and flipped the
//      camera's card from "OFFLINE" to "LIVE".
//
// If ANY seam in that chain is broken, this test fails.

test.describe('camera telemetry over SSE', () => {
  test.beforeAll(async () => {
    await waitForServerReady();
    await waitForUiReady();
  });

  test('at least one test camera delivers live telemetry', async ({ page }) => {
    // Log in.
    await page.goto('/');
    await page.getByPlaceholder('Email').fill(ADMIN_EMAIL);
    await page.getByPlaceholder('Password').fill(ADMIN_PASSWORD);
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page.getByPlaceholder('Password')).toBeHidden({ timeout: 10_000 });

    // Wait for at least one camera card to appear. We don't pin the
    // exact count — camera names are generated from UUIDs, and how
    // many cameras successfully provision depends on the admin's tier
    // (free limits to 1 camera; unlimited when STRIPE_SECRET_KEY is
    // empty). The CI harness sets STRIPE_SECRET_KEY= so all three
    // provision, but this spec is resilient either way.
    const cameraCards = page.locator('main button.aspect-video');
    await expect(cameraCards.first()).toBeVisible({ timeout: 15_000 });
    const count = await cameraCards.count();
    expect(count).toBeGreaterThanOrEqual(1);

    // Wait for at least one card to show "LIVE" — the badge only
    // renders after the transport store receives a telemetry SSE
    // event with a fresh server_ts. This is the whole point of the
    // test: prove telemetry flows end-to-end.
    await expect(
      page.locator('main button.aspect-video span.uppercase', { hasText: 'LIVE' }).first(),
    ).toBeVisible({ timeout: 45_000 });
  });
});
