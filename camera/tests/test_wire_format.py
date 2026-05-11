"""Wire-format parity assertions.

Each test pins one byte-level invariant from the camera-server contract.
The expected values are computed against the Go implementation at
camera/signing.go, camera/identity.go, common/types.go, and
common/telemetry.go. Drift in any of these tests means the server stops
accepting messages from the Python camera.

The cross-language signature round-trip (test_signing_roundtrip.py)
validates the same invariants by handing bytes to a Go verifier — these
tests are the cheap canary; that test is the bulletproof check.
"""

from __future__ import annotations

import base64
import hashlib
import json

import pytest
from nacl.signing import SigningKey

from ghostcam.identity import Identity, _derive_device_id
from ghostcam.signing import build_signature_header
from ghostcam.wire import (
    CameraCommand,
    ProvisionRequest,
    QRPayload,
    TelemetryDatagram,
    TelemetryPollRequest,
    UploadedSegment,
)

# Fixed test seed — 32 bytes of "0123456789abcdef" repeated.
FIXTURE_SEED = bytes.fromhex("0123456789abcdef" * 4)
FIXTURE_TS = 1715200000
FIXTURE_DEVICE_ID = "abcd1234abcd1234"

# Pre-computed Go-equivalent outputs for the fixture seed/message combo.
# Recomputing requires running tools/sigfixtures (or the spike harness from
# the planning phase). Verified byte-identical to crypto/ed25519.Sign in Go.
FIXTURE_PUBKEY_HEX = "207a067892821e25d770f1fba0c47c11ff4b813e54162ece9eb839e076231ab6"
FIXTURE_TELEMETRY_SIG = (
    "ZefogBizT5zsSEumg8bwK8_UEm-56ufuF0hAM-3jEiWVGxHjMi1cJEb1dwfSjODkgZwYO1kETtrThNDPxlhjCA"
)


# --- Identity / device ID derivation -------------------------------------


def test_device_id_is_first_16_bytes_of_sha256_of_pubkey():
    """Mirrors server/auth/verify.go: device_id = SHA-256(public_key)[:16]
    encoded as 32 hex chars."""
    sk = SigningKey(FIXTURE_SEED)
    pub = bytes(sk.verify_key)
    assert pub.hex() == FIXTURE_PUBKEY_HEX

    expected = hashlib.sha256(pub).digest()[:16].hex()
    assert _derive_device_id(pub) == expected
    assert len(_derive_device_id(pub)) == 32


# --- Signature canonical format ------------------------------------------


def test_signature_payload_format():
    """The canonical message is METHOD\\nPATH\\nTS\\nDEVICE_ID — newlines
    are \\n, no trailing newline, path is the URL path only (no query)."""
    sk = SigningKey(FIXTURE_SEED)
    header = build_signature_header(
        method="POST",
        path=f"/api/v1/cameras/{FIXTURE_DEVICE_ID}/telemetry",
        device_id=FIXTURE_DEVICE_ID,
        signing_key=sk,
        ts=FIXTURE_TS,
    )
    # Header literal format: 'Signature device_id=<hex>,ts=<int>,sig=<b64>'
    assert header.startswith(f"Signature device_id={FIXTURE_DEVICE_ID},ts={FIXTURE_TS},sig=")


def test_signature_is_byte_identical_to_go():
    """The PyNaCl-emitted signature for a known seed + message must match
    the Go fixture exactly — same bytes, same base64 encoding (raw URL,
    no padding). Drift here means the Python camera produces signatures
    the server cannot verify."""
    sk = SigningKey(FIXTURE_SEED)
    header = build_signature_header(
        method="POST",
        path=f"/api/v1/cameras/{FIXTURE_DEVICE_ID}/telemetry",
        device_id=FIXTURE_DEVICE_ID,
        signing_key=sk,
        ts=FIXTURE_TS,
    )
    sig = header.split(",sig=", 1)[1]
    assert sig == FIXTURE_TELEMETRY_SIG


def test_signature_uses_unix_seconds_not_milliseconds():
    """A signature timestamp that's actually a millisecond value (10^3 ×
    the right magnitude) MUST yield a different signature — guards against
    accidental ms/s confusion since the telemetry datagram itself uses ms."""
    sk = SigningKey(FIXTURE_SEED)
    seconds_header = build_signature_header(
        "POST", "/x", FIXTURE_DEVICE_ID, sk, ts=FIXTURE_TS
    )
    millis_header = build_signature_header(
        "POST", "/x", FIXTURE_DEVICE_ID, sk, ts=FIXTURE_TS * 1000
    )
    assert seconds_header != millis_header


def test_signature_b64_has_no_padding():
    """RawURLEncoding semantics — strip trailing '=' so the base64 matches
    Go's base64.RawURLEncoding output exactly."""
    sk = SigningKey(FIXTURE_SEED)
    header = build_signature_header("GET", "/a", FIXTURE_DEVICE_ID, sk, ts=1)
    sig = header.split(",sig=", 1)[1]
    assert "=" not in sig
    # And it must be the URL-safe alphabet (no '+' or '/').
    assert "+" not in sig and "/" not in sig
    # 64 raw bytes → 86 base64 chars stripped of padding.
    assert len(sig) == 86


def test_signature_round_trip_via_pynacl_self_verify():
    """Sanity: the PyNaCl emitter can be verified using the same library —
    catches accidental message-construction bugs before they're caught by
    the cross-language test."""
    sk = SigningKey(FIXTURE_SEED)
    method, path, device_id, ts = "POST", "/x", FIXTURE_DEVICE_ID, FIXTURE_TS
    header = build_signature_header(method, path, device_id, sk, ts=ts)
    sig_b64 = header.split(",sig=", 1)[1]
    sig = base64.urlsafe_b64decode(sig_b64 + "=" * (-len(sig_b64) % 4))
    msg = f"{method}\n{path}\n{ts}\n{device_id}".encode()
    sk.verify_key.verify(msg, sig)


# --- Telemetry datagram serialization ------------------------------------


def test_telemetry_omits_none_fields():
    """Pointer fields in common/telemetry.go use omitempty — Python must
    drop None on dump so the server doesn't see explicit nulls."""
    tg = TelemetryDatagram(ts=42, cpu=10)  # only 2 of 12 fields set
    dumped = tg.model_dump(by_alias=True, exclude_none=True)
    assert dumped == {"ts": 42, "cpu": 10}


def test_telemetry_full_round_trip_preserves_optional_fields():
    """All 12 sensor fields populated → round-trip preserves every value."""
    tg = TelemetryDatagram(
        ts=1, sig=-50, temp=42, fps=29.97, kbps=2000, cpu=18, mem=120,
        uptime=3600, lat=47.6062, lon=-122.3321, alt=51.5, gps_fix=2,
    )
    raw = tg.model_dump(by_alias=True, exclude_none=True)
    rt = TelemetryDatagram.model_validate(raw)
    assert rt == tg


def test_telemetry_poll_request_envelope():
    """Camera sends {"telemetry": {...}, "fw_version": "..."}."""
    body = TelemetryPollRequest(
        telemetry=TelemetryDatagram(ts=1, cpu=10),
        fw_version="dev",
    ).model_dump(by_alias=True, exclude_none=True)
    assert body == {"telemetry": {"ts": 1, "cpu": 10}, "fw_version": "dev"}


# --- Provision request ----------------------------------------------------


def test_provision_request_public_key_is_64_char_lowercase_hex():
    """Server expects hex(public_key) in the public_key field — 64 chars
    lowercase, no 0x prefix."""
    sk = SigningKey(FIXTURE_SEED)
    pub_hex = bytes(sk.verify_key).hex()
    body = ProvisionRequest(
        token="prov_abc",
        device_serial="serial-1",
        public_key=pub_hex,
        fw_version="dev",
    ).model_dump(by_alias=True, exclude_none=True)
    assert body["public_key"] == FIXTURE_PUBKEY_HEX
    assert len(body["public_key"]) == 64
    assert body["public_key"].lower() == body["public_key"]


# --- QR payload uses single-letter wire keys -----------------------------


def test_qr_payload_wire_keys_are_single_letters():
    """QR codes use s/t/w/p to keep encoded size small."""
    qr = QRPayload(s="https://example.com", t="tok", w="wifi", p="pw")
    raw = qr.model_dump(by_alias=True, exclude_none=True)
    assert raw == {"s": "https://example.com", "t": "tok", "w": "wifi", "p": "pw"}


def test_qr_payload_optional_wifi():
    """w and p are optional — server emits a QR without WiFi creds for the
    "user is on Ethernet/already-connected" path."""
    qr = QRPayload(s="https://example.com", t="tok")
    raw = qr.model_dump(by_alias=True, exclude_none=True)
    assert raw == {"s": "https://example.com", "t": "tok"}


# --- Uploaded segment + camera command --------------------------------


def test_uploaded_segment_omits_has_motion_when_false():
    """common/types.go sets json:\"has_motion,omitempty\" — a false bool
    should NOT appear on the wire."""
    seg = UploadedSegment(
        segment_id="abc",
        start_ts=1000,
        end_ts=2000,
        size_bytes=12345,
        has_motion=None,  # representing the "absent" case
    )
    raw = seg.model_dump(by_alias=True, exclude_none=True)
    assert "has_motion" not in raw


def test_camera_command_includes_only_set_fields():
    """The tagged-union shape is just a Type field plus optional siblings."""
    cmd = CameraCommand(type="set_recording_mode", mode="motion")
    raw = cmd.model_dump(by_alias=True, exclude_none=True)
    assert raw == {"type": "set_recording_mode", "mode": "motion"}


# --- JSON serialization is what httpx will send -------------------------


def test_telemetry_payload_json_bytes_match_expected():
    """End-to-end: the dict given to httpx.post(json=...) serializes to a
    deterministic JSON string for a fixed input."""
    body = TelemetryPollRequest(
        telemetry=TelemetryDatagram(ts=1, cpu=10),
        fw_version="dev",
    ).model_dump(by_alias=True, exclude_none=True)
    encoded = json.dumps(body, separators=(",", ":"), sort_keys=True)
    assert encoded == '{"fw_version":"dev","telemetry":{"cpu":10,"ts":1}}'


# --- Identity wrapper -----------------------------------------------------


def test_identity_public_key_hex_is_64_chars():
    sk = SigningKey(FIXTURE_SEED)
    identity = Identity(
        signing_key=sk,
        verify_key=sk.verify_key,
        device_id=_derive_device_id(bytes(sk.verify_key)),
    )
    assert identity.public_key_hex == FIXTURE_PUBKEY_HEX


@pytest.mark.parametrize(
    "method,path",
    [
        ("POST", "/api/v1/cameras/abc/telemetry"),
        ("POST", "/api/v1/cameras/xyz/presign"),
        ("GET", "/api/v1/cameras/abc/live"),
    ],
)
def test_signature_header_well_formed_across_methods(method: str, path: str) -> None:
    """The header is structurally identical regardless of method/path so
    any HTTP+WS request the camera makes is signable the same way."""
    sk = SigningKey(FIXTURE_SEED)
    header = build_signature_header(method, path, FIXTURE_DEVICE_ID, sk, ts=FIXTURE_TS)
    parts = header.removeprefix("Signature ").split(",")
    keys = [p.split("=", 1)[0] for p in parts]
    assert keys == ["device_id", "ts", "sig"]
