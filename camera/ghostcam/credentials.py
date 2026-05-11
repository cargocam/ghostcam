"""On-disk persistence of camera credentials.

Mirrors camera/credentials.go. The ed25519 keypair is permanent (managed
by identity.py); only `server_url` is written and cleared by this module.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from ghostcam.identity import Identity, load_identity_if_exists


@dataclass(frozen=True)
class Credentials:
    device_id: str
    server_url: str
    identity: Identity


def load_credentials(data_dir: Path) -> Credentials | None:
    """Return persisted credentials, or None if the camera needs
    provisioning. Matches the contract of camera/credentials.go::LoadCredentials.
    """
    identity = load_identity_if_exists(data_dir)
    if identity is None:
        return None
    server_url_path = data_dir / "server_url"
    if not server_url_path.exists():
        return None
    server_url = server_url_path.read_text().strip()
    if not server_url:
        return None
    return Credentials(
        device_id=identity.device_id,
        server_url=server_url,
        identity=identity,
    )


def save_credentials(data_dir: Path, server_url: str) -> None:
    """Persist server_url. Identity files are managed by identity.py and
    are not touched here."""
    data_dir.mkdir(parents=True, exist_ok=True)
    path = data_dir / "server_url"
    path.write_text(server_url)
    path.chmod(0o600)


def clear_credentials(data_dir: Path) -> None:
    """Remove the server binding. The keypair is preserved so the camera
    re-enters provisioning with the same device ID."""
    target = data_dir / "server_url"
    if target.exists():
        target.unlink()
