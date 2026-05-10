"""Ghostcam camera daemon — Python implementation.

The package both ships a turn-key camera daemon (`ghostcam-camera`
console script) AND exposes a small library API for tools that want to
talk to a Ghostcam server, parse H.264 streams, or build alternative
front-ends.

Public API (stable across minor versions):

  * Wire contract — see `ghostcam.wire`. Codegen'd from the Go
    server's common/ package; drift is a CI failure, not a runtime
    surprise.

  * Server client — `Client`, `provision`, `S3UploadError`. ed25519-
    signed HTTP wrapper covering presign/upload/telemetry/provision.

  * Identity — `Identity`, `load_or_create_identity`,
    `load_identity_if_exists`. Permanent ed25519 keypair management;
    persistent across reboots and server switches.

  * Request signing — `build_signature_header`. The exact wire format
    expected by `server/auth/verify.go`.

  * Live H.264 parsing — `LiveRelay`, `LiveFrame`, `NullLiveRelay`,
    `LiveWriter`, `DEFAULT_RING_SIZE`. Annex B start-code splitter +
    bounded ring buffer with drop-oldest semantics.

  * Motion detection — `MotionDetector`. ffprobe P-frame size analysis
    with file-size fallback.

  * OGG/Opus parsing — `read_ogg_opus_packets`, `OggError`. Async page
    reader for ffmpeg's `-f ogg -c:a libopus` output.

Anything not re-exported here is daemon assembly (`capture`, `watcher`,
`upload`, `live_ws`, `telemetry_poll`, `commands`, `provisioning`,
`firmware`, `main`, `config`, `credentials`, `platform/`) — importable
for advanced users but not part of the API contract. Refactoring those
modules is not a SemVer break; refactoring the symbols below is.
"""

from __future__ import annotations

from ghostcam import wire
from ghostcam.client import Client, S3UploadError, provision
from ghostcam.identity import (
    Identity,
    load_identity_if_exists,
    load_or_create_identity,
)
from ghostcam.live_relay import (
    DEFAULT_RING_SIZE,
    LiveFrame,
    LiveRelay,
    LiveWriter,
    NullLiveRelay,
)
from ghostcam.motion import MotionDetector
from ghostcam.ogg_reader import OggError, read_ogg_opus_packets
from ghostcam.signing import build_signature_header

__version__ = "0.1.0"

__all__ = [
    "Client",
    "DEFAULT_RING_SIZE",
    "Identity",
    "LiveFrame",
    "LiveRelay",
    "LiveWriter",
    "MotionDetector",
    "NullLiveRelay",
    "OggError",
    "S3UploadError",
    "__version__",
    "build_signature_header",
    "load_identity_if_exists",
    "load_or_create_identity",
    "provision",
    "read_ogg_opus_packets",
    "wire",
]
