"""Lock the public API surface.

Anything in `ghostcam.__all__` is part of the SemVer contract. Adding
to this list is fine; removing or renaming requires a major-version
bump. This test makes the public-surface decision visible in PRs:
adding a re-export means updating EXPECTED_API; removing or renaming
fails the test loudly.

If you're refactoring an internal module (capture, watcher, upload,
live_ws, telemetry_poll, commands, provisioning, firmware, config,
credentials, platform), this test should not need to change. If it
breaks, you've leaked an internal symbol into the public surface or
the other way around.
"""

from __future__ import annotations

import importlib

import ghostcam

# The contract. Sorted to make `git diff` reviewable.
EXPECTED_API = {
    "BatteryRule",
    "Client",
    "DEFAULT_RING_SIZE",
    "EffectiveMode",
    "Identity",
    "LiveFrame",
    "LiveRelay",
    "LiveWriter",
    "MotionDetector",
    "NullLiveRelay",
    "OggError",
    "PowerModeState",
    "S3UploadError",
    "ScheduleWindow",
    "SegmentIndex",
    "SegmentRow",
    "__version__",
    "build_signature_header",
    "load_identity_if_exists",
    "load_or_create_identity",
    "provision",
    "read_ogg_opus_packets",
    "wire",
}


def test_all_matches_expected_api() -> None:
    assert set(ghostcam.__all__) == EXPECTED_API, (
        "Public API drift detected. Compare ghostcam.__all__ to "
        "EXPECTED_API in this test. If the change is intentional, "
        "update EXPECTED_API and bump the SemVer accordingly."
    )


def test_every_exported_name_is_actually_importable() -> None:
    """Catches typos in __all__ that would let names go missing."""
    for name in ghostcam.__all__:
        assert hasattr(ghostcam, name), f"ghostcam.__all__ lists {name!r} but it doesn't exist on the module"


def test_wire_subpackage_is_curated() -> None:
    """The wire submodule is part of the public surface; its own
    __all__ should expose every codegen'd type."""
    expected_wire = {
        "CameraCommand",
        "PresignedUrl",
        "PresignRequest",
        "PresignResponse",
        "ProvisionRequest",
        "ProvisionResponse",
        "QRPayload",
        "TelemetryDatagram",
        "TelemetryPollRequest",
        "TelemetryPollResponse",
        "UploadedSegment",
    }
    assert set(ghostcam.wire.__all__) == expected_wire


def test_internal_modules_are_importable_but_not_re_exported() -> None:
    """Smoke-check: internal modules still work for advanced users
    who reach past __all__, but they aren't part of the contract."""
    for internal in [
        "ghostcam.capture",
        "ghostcam.watcher",
        "ghostcam.upload",
        "ghostcam.live_ws",
        "ghostcam.telemetry_poll",
        "ghostcam.commands",
        "ghostcam.provisioning",
        "ghostcam.firmware",
        "ghostcam.main",
        "ghostcam.config",
        "ghostcam.credentials",
        "ghostcam.platform",
    ]:
        importlib.import_module(internal)
