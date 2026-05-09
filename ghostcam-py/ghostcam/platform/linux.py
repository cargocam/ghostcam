"""Real-Pi platform: /proc + /sys sensors, gpsd, nmcli, rpicam-still QR.

Mirrors camera/sensors_linux.go, camera/gpsd.go, camera/network_linux.go,
and camera/qr_linux.go. The Go camera uses build tags to swap these out
for stubs on non-Linux; here we select at import time inside
ghostcam.platform.__init__.

QR scanning needs the libzbar0 system library (pyzbar) and rpicam-still
on PATH. If either is missing, scan_qr() reports the absence and returns
None — same as the Go camera's no-op stub.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import shutil
import struct
from pathlib import Path

from ghostcam.wire import QRPayload, TelemetryDatagram

logger = logging.getLogger(__name__)


def read_device_serial(data_dir: Path) -> str:
    """Read the Pi serial from /proc/cpuinfo. Caller writes it to disk."""
    try:
        for line in Path("/proc/cpuinfo").read_text().splitlines():
            if line.startswith("Serial"):
                _, _, val = line.partition(":")
                serial = val.strip()
                if serial:
                    return serial
    except OSError:
        pass
    return ""


def read_telemetry() -> TelemetryDatagram:
    return TelemetryDatagram(
        ts=int(__import__("time").time() * 1000),
        cpu=_read_cpu(),
        mem=_read_memory(),
        temp=_read_temperature(),
        uptime=_read_uptime(),
        sig=_read_wifi_signal(),
        **_read_gps_fields(),
    )


# --- /proc + /sys readers ---


def _read_cpu() -> int | None:
    try:
        line = Path("/proc/stat").read_text().split("\n", 1)[0]
    except OSError:
        return None
    fields = line.split()
    if len(fields) < 5:  # "cpu" + at least 4 columns
        return None
    nums: list[int] = []
    for f in fields[1:]:
        try:
            nums.append(int(f))
        except ValueError:
            continue
    if not nums:
        return None
    total = sum(nums)
    idle = nums[3] if len(nums) >= 4 else 0
    if total == 0:
        return 0
    return (total - idle) * 100 // total


def _read_memory() -> int | None:
    try:
        text = Path("/proc/meminfo").read_text()
    except OSError:
        return None
    total = available = 0
    for line in text.splitlines():
        if line.startswith("MemTotal:"):
            total = _parse_kb(line)
        elif line.startswith("MemAvailable:"):
            available = _parse_kb(line)
    if total == 0:
        return None
    return (total - available) // 1024  # kB → MB


def _parse_kb(line: str) -> int:
    parts = line.split()
    if len(parts) < 2:
        return 0
    try:
        return int(parts[1])
    except ValueError:
        return 0


def _read_temperature() -> int | None:
    try:
        millideg = int(Path("/sys/class/thermal/thermal_zone0/temp").read_text().strip())
    except (OSError, ValueError):
        return None
    return millideg // 1000


def _read_uptime() -> int | None:
    try:
        first = Path("/proc/uptime").read_text().split()[0]
    except (OSError, IndexError):
        return None
    try:
        return int(float(first))
    except ValueError:
        return None


def _read_wifi_signal() -> int | None:
    try:
        lines = Path("/proc/net/wireless").read_text().splitlines()
    except OSError:
        return None
    if len(lines) < 3:
        return None
    fields = lines[2].split()
    if len(fields) < 4:
        return None
    try:
        return int(float(fields[3].rstrip(".")))
    except ValueError:
        return None


# --- gpsd ---


def _read_gps_fields() -> dict[str, float | int | None]:
    lat, lon, alt, fix = _gpsd_query()
    return {"lat": lat, "lon": lon, "alt": alt, "gps_fix": fix}


def _gpsd_query(
    *, host: str = "127.0.0.1", port: int = 2947, timeout: float = 5.0
) -> tuple[float | None, float | None, float | None, int | None]:
    """Synchronous gpsd query (sensor reader runs in the asyncio thread but
    is fast enough — total round-trip is <100ms for a fix)."""
    import socket

    try:
        sock = socket.create_connection((host, port), timeout=timeout)
    except OSError:
        return None, None, None, None

    try:
        sock.settimeout(timeout)
        sock.recv(4096)  # gpsd version banner
        sock.sendall(b'?WATCH={"enable":true,"json":true}\n')
        deadline = asyncio.get_event_loop().time() + timeout if False else None  # noqa: F841
        # Read up to 16 KB total or until we see a TPV with mode>=2.
        buf = b""
        while len(buf) < 16384:
            chunk = sock.recv(4096)
            if not chunk:
                break
            buf += chunk
            for line in buf.split(b"\n"):
                if not line.strip():
                    continue
                try:
                    obj = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if obj.get("class") != "TPV":
                    continue
                mode = int(obj.get("mode", 0))
                if mode < 2:
                    continue
                lat = float(obj["lat"])
                lon = float(obj["lon"])
                alt = float(obj.get("altHAE") or obj.get("alt") or 0.0)
                return lat, lon, alt, mode
        return None, None, None, None
    finally:
        sock.close()


# --- network ---


_DEFAULT_ROUTE_PATH = Path("/proc/net/route")


def _has_default_route() -> bool:
    try:
        text = _DEFAULT_ROUTE_PATH.read_text()
    except OSError:
        return False
    for i, line in enumerate(text.splitlines()):
        if i == 0:
            continue
        fields = line.split("\t")
        if len(fields) >= 2 and fields[1] == "00000000":
            return True
    return False


async def wait_for_route(timeout: float | None = None) -> bool:
    """Poll /proc/net/route every 500ms until a default route appears or
    `timeout` elapses. None timeout = block forever.
    """
    if _has_default_route():
        return True
    logger.info("no default route, waiting for network...")
    elapsed = 0.0
    while True:
        await asyncio.sleep(0.5)
        elapsed += 0.5
        if _has_default_route():
            logger.info("default route appeared after %.1fs", elapsed)
            return True
        if timeout is not None and elapsed >= timeout:
            return False


async def ensure_wifi(ssid: str, psk: str | None) -> None:
    if shutil.which("nmcli") is None:
        logger.warning("nmcli not on PATH, can't configure WiFi")
        return

    try:
        proc = await asyncio.create_subprocess_exec(
            "nmcli", "connection", "show", "--active",
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL,
        )
        out, _ = await proc.communicate()
        if ssid.encode() in out:
            logger.debug("already connected to WiFi: %s", ssid)
            return
    except OSError:
        return

    args = ["device", "wifi", "connect", ssid]
    if psk:
        args.extend(["password", psk])
    logger.info("connecting to WiFi network: %s", ssid)
    proc = await asyncio.create_subprocess_exec(
        "nmcli", *args,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.STDOUT,
    )
    out, _ = await proc.communicate()
    if proc.returncode != 0:
        logger.warning("WiFi connection failed: %s", out.decode(errors="replace").strip())
        return
    logger.info("WiFi connected: %s", ssid)


# --- QR scan ---


_QR_WIDTH = 640
_QR_HEIGHT = 480
_QR_Y_SIZE = _QR_WIDTH * _QR_HEIGHT
_QR_YUV_SIZE = _QR_Y_SIZE * 3 // 2


async def scan_qr(timeout: float = 300.0) -> QRPayload | None:
    """Capture YUV420 frames from rpicam-still and decode QR via pyzbar.

    Pyzbar (libzbar0 binding) replaces gozxing. Slower than the Go path
    on Pi Zero 2W but QR is a one-time provisioning event.
    """
    if shutil.which("rpicam-still") is None:
        logger.debug("rpicam-still not on PATH, skipping QR scan")
        return None
    try:
        from pyzbar.pyzbar import decode  # noqa: F401
        from PIL import Image  # noqa: F401
    except ImportError:
        logger.warning("pyzbar/Pillow not installed; install ghostcam[real] for QR support")
        return None

    logger.info("scanning for provisioning QR code (timeout=%.0fs)", timeout)
    proc = await asyncio.create_subprocess_exec(
        "rpicam-still",
        "--width", str(_QR_WIDTH),
        "--height", str(_QR_HEIGHT),
        "-n",
        "-t", "0",
        "--timelapse", "500",
        "--encoding", "yuv420",
        "-o", "-",
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.DEVNULL,
        preexec_fn=os.setsid,
    )

    try:
        return await asyncio.wait_for(_qr_loop(proc), timeout)
    except asyncio.TimeoutError:
        logger.info("QR scan timed out")
        return None
    finally:
        if proc.returncode is None:
            try:
                os.killpg(os.getpgid(proc.pid), 15)
            except (ProcessLookupError, OSError):
                pass
            try:
                await asyncio.wait_for(proc.wait(), 5.0)
            except asyncio.TimeoutError:
                proc.kill()


async def _qr_loop(proc: asyncio.subprocess.Process) -> QRPayload | None:
    from pyzbar.pyzbar import decode
    from PIL import Image

    assert proc.stdout is not None
    while True:
        try:
            yuv = await proc.stdout.readexactly(_QR_YUV_SIZE)
        except asyncio.IncompleteReadError:
            return None
        # Y plane only — pyzbar reads grayscale.
        gray = Image.frombytes("L", (_QR_WIDTH, _QR_HEIGHT), bytes(yuv[:_QR_Y_SIZE]))
        results = decode(gray)
        for r in results:
            try:
                obj = json.loads(r.data)
            except (json.JSONDecodeError, UnicodeDecodeError):
                continue
            try:
                payload = QRPayload.model_validate(obj)
            except Exception:  # pydantic.ValidationError
                continue
            if not payload.s or not payload.t:
                continue
            logger.info("QR code decoded: server=%s", payload.s)
            return payload


# Silence unused-import warning under static analysis.
_ = struct
