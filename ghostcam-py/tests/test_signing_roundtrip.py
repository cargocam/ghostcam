"""Cross-language signature parity.

This is the bulletproof check: Python signs, the Go harness verifies; the
Go harness signs, Python verifies. If both directions pass, the Python
camera and the Go server are on the same wire bit-for-bit.

The harness lives at tools/sigverify/ and is `go run` invoked at test
time so we don't need a build step in CI — the same `go test` runner that
exercises the rest of the suite has the toolchain.
"""

from __future__ import annotations

import base64
import json
import shutil
import subprocess
from pathlib import Path

import pytest
from nacl.signing import SigningKey

from ghostcam.signing import build_signature_header

REPO_ROOT = Path(__file__).resolve().parents[2]
SIGVERIFY_PKG = "./tools/sigverify"


def _go_available() -> bool:
    return shutil.which("go") is not None


pytestmark = pytest.mark.skipif(not _go_available(), reason="go toolchain not on PATH")


def _run_sigverify(mode: str, payload: dict) -> str:
    """Pipe payload as JSON into `go run ./tools/sigverify <mode>`. Returns
    the stdout-stripped output."""
    proc = subprocess.run(
        ["go", "run", SIGVERIFY_PKG, mode],
        cwd=REPO_ROOT,
        input=json.dumps(payload),
        capture_output=True,
        text=True,
        timeout=60,
        check=True,
    )
    return proc.stdout.strip()


@pytest.fixture(scope="module")
def fixture_seed_hex() -> str:
    return "0123456789abcdef" * 4


@pytest.mark.parametrize(
    "method,path,ts,device_id",
    [
        ("POST", "/api/v1/cameras/abcd1234abcd1234/telemetry", 1715200000, "abcd1234abcd1234"),
        ("POST", "/api/v1/cameras/zzzz/presign", 0, "zzzz"),
        ("GET", "/api/v1/cameras/aa11/live", 9999999999, "aa11"),
    ],
)
def test_python_signature_byte_identical_to_go(
    fixture_seed_hex: str, method: str, path: str, ts: int, device_id: str
) -> None:
    """Sign the same message in Python and Go; the base64 outputs must
    match byte-for-byte."""
    seed = bytes.fromhex(fixture_seed_hex)
    sk = SigningKey(seed)
    py_header = build_signature_header(method, path, device_id, sk, ts=ts)
    py_sig = py_header.split(",sig=", 1)[1]

    go_sig = _run_sigverify(
        "sign",
        {"seed": fixture_seed_hex, "method": method, "path": path, "ts": ts, "device_id": device_id},
    )
    assert py_sig == go_sig


def test_go_verifies_python_signature(fixture_seed_hex: str) -> None:
    """Go's crypto/ed25519.Verify accepts a signature produced by PyNaCl —
    the path the production server takes."""
    seed = bytes.fromhex(fixture_seed_hex)
    sk = SigningKey(seed)
    method, path, ts, device_id = "POST", "/x", 1, "dev"
    header = build_signature_header(method, path, device_id, sk, ts=ts)
    py_sig = header.split(",sig=", 1)[1]

    out = _run_sigverify(
        "verify",
        {
            "seed": fixture_seed_hex,
            "method": method,
            "path": path,
            "ts": ts,
            "device_id": device_id,
            "signature": py_sig,
        },
    )
    assert out == "ok"


def test_python_verifies_go_signature(fixture_seed_hex: str) -> None:
    """Reverse direction: the Go-emitted signature is accepted by PyNaCl's
    verifier."""
    seed = bytes.fromhex(fixture_seed_hex)
    sk = SigningKey(seed)
    method, path, ts, device_id = "POST", "/x", 1, "dev"
    go_sig = _run_sigverify(
        "sign",
        {"seed": fixture_seed_hex, "method": method, "path": path, "ts": ts, "device_id": device_id},
    )
    sig = base64.urlsafe_b64decode(go_sig + "=" * (-len(go_sig) % 4))
    msg = f"{method}\n{path}\n{ts}\n{device_id}".encode()
    sk.verify_key.verify(msg, sig)


def test_tampered_signature_fails_go_verifier(fixture_seed_hex: str) -> None:
    """Sanity: flip a byte in the signature and the Go verifier rejects."""
    seed = bytes.fromhex(fixture_seed_hex)
    sk = SigningKey(seed)
    method, path, ts, device_id = "POST", "/x", 1, "dev"
    header = build_signature_header(method, path, device_id, sk, ts=ts)
    py_sig = header.split(",sig=", 1)[1]
    # Flip a base64 char.
    bad = py_sig[:5] + ("A" if py_sig[5] != "A" else "B") + py_sig[6:]

    out = _run_sigverify(
        "verify",
        {
            "seed": fixture_seed_hex,
            "method": method,
            "path": path,
            "ts": ts,
            "device_id": device_id,
            "signature": bad,
        },
    )
    assert out.startswith("fail")
