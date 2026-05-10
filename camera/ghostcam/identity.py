"""Permanent ed25519 keypair + derived device ID.

Mirrors camera/identity.go and camera/credentials.go. The keypair is
generated on first boot, stored in dataDir, and never regenerated — it
survives server switches the same way ~/.ssh/id_ed25519 does.

Port note: PyNaCl (libsodium) was chosen over `cryptography` because it
produces byte-identical ed25519 signatures vs Go's crypto/ed25519 with a
much smaller dependency surface (no cffi). Verified during the planning
spike.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from pathlib import Path

from nacl.signing import SigningKey, VerifyKey


@dataclass(frozen=True)
class Identity:
    """Permanent camera identity. Key bytes are kept as nacl objects so
    callers don't accidentally serialize the seed."""

    signing_key: SigningKey
    verify_key: VerifyKey
    device_id: str  # SHA-256(public_key)[:16] hex, 32 chars

    @property
    def public_key_hex(self) -> str:
        return bytes(self.verify_key).hex()


def _derive_device_id(public_key: bytes) -> str:
    """First 16 bytes of SHA-256(public_key) as hex (32 chars). Mirrors
    server/auth/verify.go's expectation."""
    return hashlib.sha256(public_key).digest()[:16].hex()


def load_or_create_identity(data_dir: Path) -> Identity:
    """Load the existing keypair from data_dir, or generate one on first
    boot and persist it. Mirrors camera/identity.go::LoadOrCreateIdentity.
    """
    data_dir.mkdir(parents=True, exist_ok=True)
    seed_path = data_dir / "identity_key"
    pub_path = data_dir / "identity_key.pub"

    if seed_path.exists():
        seed_hex = seed_path.read_text().strip()
        try:
            seed = bytes.fromhex(seed_hex)
        except ValueError as e:
            raise ValueError("corrupt identity_key") from e
        if len(seed) != 32:
            raise ValueError("corrupt identity_key")
        signing = SigningKey(seed)
    else:
        signing = SigningKey.generate()
        seed = bytes(signing)
        # 0o600 — same mode as the Go camera writes.
        seed_path.write_text(seed.hex())
        seed_path.chmod(0o600)
        pub_path.write_text(bytes(signing.verify_key).hex())
        pub_path.chmod(0o644)

    verify = signing.verify_key
    return Identity(
        signing_key=signing,
        verify_key=verify,
        device_id=_derive_device_id(bytes(verify)),
    )


def load_identity_if_exists(data_dir: Path) -> Identity | None:
    """Return the keypair if persisted, else None — used by the
    credentials loader to detect "needs provisioning" state."""
    if not (data_dir / "identity_key").exists():
        return None
    return load_or_create_identity(data_dir)
