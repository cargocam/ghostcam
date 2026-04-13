import { test, expect } from '@playwright/test';
import { ADMIN_EMAIL, ADMIN_PASSWORD, waitForServerReady } from './fixtures/stack';
import { createHash, sign, generateKeyPairSync } from 'node:crypto';

const API = 'http://localhost:3000';

// Generate an ed25519 keypair and derive the device ID the same way the
// camera does: SHA-256(public_key)[:16] as hex.
function generateCameraIdentity() {
  const { publicKey, privateKey } = generateKeyPairSync('ed25519');
  const pubRaw = publicKey.export({ type: 'spki', format: 'der' }).subarray(-32);
  const privRaw = privateKey.export({ type: 'pkcs8', format: 'der' }).subarray(-32);
  const hash = createHash('sha256').update(pubRaw).digest();
  const deviceId = hash.subarray(0, 16).toString('hex');
  return {
    publicKeyHex: pubRaw.toString('hex'),
    privateKey,
    privRaw,
    deviceId,
  };
}

// Sign a request the same way the camera does:
// METHOD\nPATH\nTIMESTAMP\nDEVICE_ID
function signRequest(
  method: string,
  path: string,
  deviceId: string,
  privateKey: ReturnType<typeof generateKeyPairSync>['privateKey'],
) {
  const ts = Math.floor(Date.now() / 1000).toString();
  const message = `${method}\n${path}\n${ts}\n${deviceId}`;
  const sig = sign(null, Buffer.from(message), privateKey);
  const sigB64 = sig
    .toString('base64url')
    .replace(/=+$/, ''); // raw base64url (no padding)
  return `Signature device_id=${deviceId},ts=${ts},sig=${sigB64}`;
}

// Helper: login and get a session cookie.
async function loginCookie(): Promise<string> {
  const res = await fetch(`${API}/api/v1/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email: ADMIN_EMAIL, password: ADMIN_PASSWORD }),
  });
  expect(res.ok).toBe(true);
  const cookie = res.headers.get('set-cookie');
  expect(cookie).toBeTruthy();
  return cookie!.split(';')[0];
}

test.describe('ed25519 camera provisioning and auth', () => {
  test.beforeAll(async () => {
    await waitForServerReady();
  });

  test('provision → signed telemetry round trip', async () => {
    const cookie = await loginCookie();
    const identity = generateCameraIdentity();

    // 1. Enroll: generate a provision token.
    const enrollRes = await fetch(`${API}/api/v1/cameras`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Cookie: cookie },
      body: JSON.stringify({ display_name: 'e2e-test-camera' }),
    });
    expect(enrollRes.ok).toBe(true);
    const { token } = (await enrollRes.json()) as { token: string };
    expect(token).toBeTruthy();

    // 2. Provision: register the camera's public key.
    const provRes = await fetch(`${API}/api/v1/cameras/provision`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        token,
        device_serial: `e2e-serial-${Date.now()}`,
        public_key: identity.publicKeyHex,
        fw_version: 'e2e-test',
      }),
    });
    expect(provRes.ok).toBe(true);
    const provBody = (await provRes.json()) as { device_id: string; status: string };
    expect(provBody.device_id).toBe(identity.deviceId);
    expect(provBody.status).toBe('registered');

    // 3. Authenticated request: POST telemetry with signature.
    const telePath = `/api/v1/cameras/${identity.deviceId}/telemetry`;
    const authHeader = signRequest('POST', telePath, identity.deviceId, identity.privateKey);
    const teleRes = await fetch(`${API}${telePath}`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: authHeader,
      },
      body: JSON.stringify({
        telemetry: { cpu: 25, mem: 40, temp: 45, uptime: 100 },
      }),
    });
    expect(teleRes.ok).toBe(true);
  });

  test('reject request with bad signature', async () => {
    const cookie = await loginCookie();
    const identity = generateCameraIdentity();

    // Enroll + provision.
    const enrollRes = await fetch(`${API}/api/v1/cameras`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Cookie: cookie },
      body: JSON.stringify({ display_name: 'e2e-bad-sig' }),
    });
    const { token } = (await enrollRes.json()) as { token: string };
    await fetch(`${API}/api/v1/cameras/provision`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        token,
        device_serial: `e2e-badsig-${Date.now()}`,
        public_key: identity.publicKeyHex,
      }),
    });

    // Use a DIFFERENT key to sign → should be rejected.
    const wrongIdentity = generateCameraIdentity();
    const telePath = `/api/v1/cameras/${identity.deviceId}/telemetry`;
    const badAuth = signRequest('POST', telePath, identity.deviceId, wrongIdentity.privateKey);
    const res = await fetch(`${API}${telePath}`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: badAuth,
      },
      body: JSON.stringify({ telemetry: { cpu: 0 } }),
    });
    expect(res.status).toBe(401);
  });

  test('reject request with expired timestamp', async () => {
    const cookie = await loginCookie();
    const identity = generateCameraIdentity();

    // Enroll + provision.
    const enrollRes = await fetch(`${API}/api/v1/cameras`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Cookie: cookie },
      body: JSON.stringify({ display_name: 'e2e-expired-ts' }),
    });
    const { token } = (await enrollRes.json()) as { token: string };
    await fetch(`${API}/api/v1/cameras/provision`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        token,
        device_serial: `e2e-expired-${Date.now()}`,
        public_key: identity.publicKeyHex,
      }),
    });

    // Sign with a timestamp 10 minutes in the past.
    const telePath = `/api/v1/cameras/${identity.deviceId}/telemetry`;
    const staleTs = (Math.floor(Date.now() / 1000) - 600).toString();
    const message = `POST\n${telePath}\n${staleTs}\n${identity.deviceId}`;
    const sig = sign(null, Buffer.from(message), identity.privateKey);
    const sigB64 = sig.toString('base64url').replace(/=+$/, '');
    const authHeader = `Signature device_id=${identity.deviceId},ts=${staleTs},sig=${sigB64}`;

    const res = await fetch(`${API}${telePath}`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: authHeader,
      },
      body: JSON.stringify({ telemetry: { cpu: 0 } }),
    });
    expect(res.status).toBe(401);
  });
});
