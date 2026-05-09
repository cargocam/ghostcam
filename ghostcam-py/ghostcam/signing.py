"""Ed25519-signed Authorization header.

Mirrors camera/signing.go exactly. The server (server/auth/verify.go)
reconstructs the same canonical message and rejects timestamps more than
5 minutes stale.

Wire-format invariants (changing any of these breaks production):
  - Timestamp is Unix **seconds** (not milliseconds — telemetry payload
    timestamps are ms but signature timestamps are s).
  - Canonical message: f"{method}\\n{path}\\n{ts}\\n{device_id}".
  - Path is the URL path only — no query string.
  - Signature is base64.urlsafe_b64encode(sig).rstrip(b"=") — RawURLEncoding
    semantics matching Go's base64.RawURLEncoding (URL-safe alphabet,
    no padding).
  - Header literal: 'Authorization: Signature device_id=<hex>,ts=<int>,sig=<b64>'
    No spaces inside the comma-separated list.
"""

from __future__ import annotations

import base64
import time

from nacl.signing import SigningKey


def build_signature_header(
    method: str,
    path: str,
    device_id: str,
    signing_key: SigningKey,
    *,
    ts: int | None = None,
) -> str:
    """Produce the value for the Authorization header.

    The optional ts argument exists for tests that need deterministic
    output. Production callers pass nothing and get the current Unix
    second.
    """
    if ts is None:
        ts = int(time.time())
    message = f"{method}\n{path}\n{ts}\n{device_id}".encode()
    sig = signing_key.sign(message).signature
    sig_b64 = base64.urlsafe_b64encode(sig).rstrip(b"=").decode()
    return f"Signature device_id={device_id},ts={ts},sig={sig_b64}"
