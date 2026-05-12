# Security policy

Thanks for taking the time to report a security issue.

## Scope

Vulnerability reports are welcome for:

- **Camera ↔ server wire protocol** — bypass of the ed25519
  Authorization signature, replay attacks within or beyond the 5-minute
  window, malformed-input crashes in `server/auth/verify.go` or
  `camera/ghostcam/signing.py`.
- **Provisioning flow** — QR payload handling, one-time token reuse,
  unauthenticated `POST /api/v1/cameras/provision` abuse.
- **Presigned URL handling** — credential leakage through logs, key
  scope mismatches between camera, server, and S3/Tigris.
- **Live WebSocket relay** — auth header bypass on the upgrade
  request, frame injection, denial-of-service via malformed binary
  frames.
- **Identity key storage** — anything that lets a non-root local user
  read `/var/ghostcam/identity_key` (mode 0600 expected).
- **Server HTTP surface** — JWT cookie tampering, CSRF on session
  endpoints, SSRF via segment URLs, SQL injection.
- **Firmware self-update path** — sha256 verification bypass, MITM on
  the staged-update download, rollback attacks against `boot_ok`.

Out of scope:

- Issues requiring already-compromised host (root, kernel exploit).
- Denial-of-service that requires sustained traffic from many sources
  beyond what a normal camera fleet would generate.

## How to report

Email **security@cargocam.dev** with:

- A clear description of the issue.
- A minimal reproduction (commands, payloads, or a test case).
- The commit SHA you reproduced against.

Please do **not** open a public GitHub issue for security
vulnerabilities. We'll acknowledge receipt within **3 business days**
and aim to resolve confirmed issues within **30 days**.

If you'd like attribution in the fix's commit message or release
notes, say so in the report. We'll coordinate disclosure timing with
you before publishing the fix.

## Cryptographic details (for context)

- **Camera identity:** ed25519 keypair via PyNaCl (libsodium); seed
  stored in `/var/ghostcam/identity_key` with mode `0600`. Device ID
  is `SHA-256(public_key)[:16]` hex.
- **Request signing:** `Authorization: Signature device_id=<hex>,ts=<int>,sig=<b64>`
  where the signed payload is `f"{method}\n{path}\n{ts}\n{device_id}"`
  (newlines are `\n`, `ts` is Unix **seconds**) and the signature is
  base64.urlsafe encoded with no padding.
- **Replay window:** the server rejects timestamps more than 300
  seconds in either direction.
- **Server auth:** JWT cookies (HttpOnly + Secure + SameSite=Lax),
  Argon2id password hashing, HMAC token hashing for one-time tokens.

If you're reporting against any of the cryptographic code, the
must-not-drift wire-format items are listed in
`camera/tests/test_wire_format.py` — that file is the authoritative
contract.
