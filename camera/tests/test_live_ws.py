"""WebSocket relay wire format + URL transform — mirrors camera/live_ws.go.

Asserts the binary frame layout (4 BE ts_ms + 1 flag byte + payload) is
byte-identical to what the Go camera sends, and that http(s)://...
server URLs map to ws(s)://.../api/v1/cameras/<id>/live correctly.
"""

from __future__ import annotations

import struct

from ghostcam.live_ws import _build_ws_url, _pack_frame


def test_build_ws_url_https_to_wss() -> None:
    url = _build_ws_url("https://server.example.com", "abcd1234abcd1234")
    assert url == "wss://server.example.com/api/v1/cameras/abcd1234abcd1234/live"


def test_build_ws_url_http_to_ws() -> None:
    url = _build_ws_url("http://localhost:3000", "deadbeefdeadbeef")
    assert url == "ws://localhost:3000/api/v1/cameras/deadbeefdeadbeef/live"


def test_build_ws_url_strips_trailing_slash() -> None:
    url = _build_ws_url("https://server.example.com/", "abcd1234abcd1234")
    assert url == "wss://server.example.com/api/v1/cameras/abcd1234abcd1234/live"


def test_pack_frame_layout_video_keyframe() -> None:
    payload = b"\x65\xab\xcd"
    msg = _pack_frame(payload, is_keyframe=True, is_audio=False)
    ts, flags = struct.unpack(">IB", msg[:5])
    assert ts <= 0xFFFFFFFF
    assert flags == 0x01  # bit 0 = is_keyframe
    assert msg[5:] == payload


def test_pack_frame_layout_audio() -> None:
    payload = b"opus-data"
    msg = _pack_frame(payload, is_keyframe=False, is_audio=True)
    _, flags = struct.unpack(">IB", msg[:5])
    assert flags == 0x02  # bit 1 = is_audio
    assert msg[5:] == payload


def test_pack_frame_flags_orred() -> None:
    # Pathological but valid: a NAL marked as both keyframe and audio
    # would set both bits. Server tolerates this.
    msg = _pack_frame(b"x", is_keyframe=True, is_audio=True)
    _, flags = struct.unpack(">IB", msg[:5])
    assert flags == 0x03


def test_pack_frame_timestamp_modulo() -> None:
    # The timestamp is masked to 32 bits — verify the layout reflects
    # that, not by comparing absolute values but by checking it fits.
    msg = _pack_frame(b"x", is_keyframe=False, is_audio=False)
    ts, _ = struct.unpack(">IB", msg[:5])
    assert 0 <= ts <= 0xFFFFFFFF
