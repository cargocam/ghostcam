import { test, expect, type APIRequestContext } from '@playwright/test';
import { ADMIN_EMAIL, ADMIN_PASSWORD, waitForServerReady, waitForUiReady } from './fixtures/stack';

// HLS manifest generation against the real stack.
//
// This test bypasses the browser UI and makes direct HTTP calls to
// /api/v1/auth/login → /api/v1/cameras → /hls/{id}/live.m3u8, then
// parses the returned manifest and verifies it has at least one
// segment line. That proves:
//   1. The camera actually uploaded segments to MinIO.
//   2. The presign confirmation path wrote a segment row to postgres.
//   3. GetLiveManifest assembles the sliding-window manifest correctly.
//   4. The wire format matches HLS spec enough for hls.js to parse.
//
// We do the HTTP dance ourselves (not through the UI) so the test is
// fast and focused on the manifest contract, not DOM rendering.

async function loginAndGetJar(request: APIRequestContext): Promise<APIRequestContext> {
  const res = await request.post('/api/v1/auth/login', {
    data: { email: ADMIN_EMAIL, password: ADMIN_PASSWORD },
  });
  expect(res.status(), 'login should succeed against the live server').toBe(200);
  // The cookie jar is retained on the passed-in request context, so
  // subsequent calls on the same context pick up the Set-Cookie header.
  return request;
}

interface CameraRow {
  device_id: string;
  display_name: string;
  provisioned?: boolean;
}

test.describe('HLS manifest against the real stack', () => {
  test.beforeAll(async () => {
    await waitForServerReady();
    await waitForUiReady();
  });

  test('live.m3u8 parses and lists real segment IDs', async ({ playwright }) => {
    const request = await playwright.request.newContext({
      baseURL: 'http://localhost:3000',
    });
    await loginAndGetJar(request);

    // Poll the cameras endpoint until we see at least one row. A fresh
    // compose stack needs a few seconds to provision the first camera.
    let cameras: CameraRow[] = [];
    const deadline = Date.now() + 60_000;
    while (Date.now() < deadline) {
      const res = await request.get('/api/v1/cameras');
      expect(res.status()).toBe(200);
      cameras = (await res.json()) as CameraRow[];
      if (cameras.length > 0) break;
      await new Promise((r) => setTimeout(r, 2_000));
    }
    expect(
      cameras.length,
      'compose --profile test should have provisioned at least one camera',
    ).toBeGreaterThanOrEqual(1);
    const deviceID = cameras[0].device_id;

    // Now poll the live manifest until it's non-empty. Segments appear
    // 6s after camera capture starts, so "first manifest" latency is
    // bounded by: camera startup + one capture cycle + presign confirm.
    // That can take ~15s on a cold stack.
    let manifest = '';
    const manifestDeadline = Date.now() + 60_000;
    while (Date.now() < manifestDeadline) {
      const res = await request.get(`/hls/${deviceID}/live.m3u8`);
      if (res.status() === 200) {
        manifest = await res.text();
        if (manifest.includes('#EXTINF')) break;
      }
      await new Promise((r) => setTimeout(r, 2_000));
    }

    // HLS spec basics: file must start with #EXTM3U and have at least
    // one segment (#EXTINF line followed by a .ts filename).
    expect(manifest.startsWith('#EXTM3U\n')).toBe(true);
    expect(manifest).toContain('#EXT-X-VERSION:7');
    expect(manifest).toContain('#EXT-X-TARGETDURATION:');
    expect(manifest).toContain('#EXTINF:');
    expect(manifest).toMatch(/[a-f0-9-]{36}\.ts/);
    // Live manifests must NOT have EXT-X-ENDLIST (vod.m3u8 does).
    expect(manifest).not.toContain('#EXT-X-ENDLIST');

    await request.dispose();
  });
});
