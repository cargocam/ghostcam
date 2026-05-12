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
import contextlib
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
    """Snapshot all platform sensors. Synchronous; the asyncio caller in
    telemetry_poll wraps the whole thing in `asyncio.to_thread` so a
    slow gpsd or nmcli can't block the event loop."""
    lat, lon, alt, fix = _gpsd_query()
    return TelemetryDatagram(
        ts=int(__import__("time").time() * 1000),
        cpu=_read_cpu(),
        mem=_read_memory(),
        temp=_read_temperature(),
        uptime=_read_uptime(),
        sig=_read_wifi_signal(),
        lat=lat,
        lon=lon,
        alt=alt,
        gps_fix=fix,
        gpsd_query_ms=last_gpsd_query_ms or None,
        disk_used_pct=_read_disk_used_pct(),
        modem_rat=_read_modem_rat(),
    )


def _read_disk_used_pct() -> int | None:
    """Percent disk used at the segment dir's filesystem. Falls back to
    the daemon's CWD if segment_dir isn't readable from here (we don't
    take it as a parameter to keep the platform sensor interface
    stable)."""
    import shutil

    for candidate in ("/var/ghostcam/segments", "/var/ghostcam"):
        try:
            usage = shutil.disk_usage(candidate)
        except OSError:
            continue
        return int(100 * (usage.total - usage.free) // usage.total)
    return None


def _read_modem_rat() -> str | None:
    """Radio Access Technology in use. Reads `nmcli -t -f
    GENERAL.STATE,GENERAL.CONNECTION,...` for the cellular device — but
    nmcli's exit code and field set vary by version, so we treat any
    failure as "no modem, no signal" and return None.

    Output is one of: LTE, 5G_NSA, 5G_SA, WCDMA, GSM, CDMA, EVDO. We
    extract from ModemManager's RM (registration) text rather than
    NetworkManager, because the latter only reports "gsm" generically.
    """
    import subprocess

    try:
        out = subprocess.run(
            ["mmcli", "-m", "any", "--output-keyvalue"],
            capture_output=True,
            text=True,
            timeout=2.0,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if out.returncode != 0:
        return None
    for line in out.stdout.splitlines():
        # `modem.generic.access-technologies.value[1]: lte` etc.
        if "access-technologies.value[1]" in line:
            _, _, val = line.partition(":")
            val = val.strip().upper().replace("-", "_")
            return val or None
    return None


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


# Last gpsd query duration in milliseconds, set by `_gpsd_query`. Read
# by the telemetry poll and surfaced as TelemetryDatagram.gpsd_query_ms.
last_gpsd_query_ms: int = 0


def _gpsd_query(
    *, host: str = "127.0.0.1", port: int = 2947, timeout: float = 5.0
) -> tuple[float | None, float | None, float | None, int | None]:
    """One-shot gpsd query via ?POLL.

    The original implementation opened a socket, sent ?WATCH (the
    *streaming* subscription), then sat in a recv loop parsing every
    newline-delimited JSON message until a TPV arrived — pure waste for
    a single-fix poll. py-spy traces during the 2026-05-12 perf run
    showed 100 % of non-idle Python CPU here. `?POLL` is gpsd's reply-
    with-one-cached-snapshot command (per gpsd_json(5)) and returns
    immediately with the latest fix, so we skip both the recv-loop and
    most of the JSON-parsing cost.

    Still synchronous on a fresh socket: the caller (telemetry_poll)
    wraps it in `asyncio.to_thread` so a slow gpsd doesn't stall the
    asyncio event loop.
    """
    global last_gpsd_query_ms
    import socket
    import time as _time

    started = _time.monotonic()
    try:
        sock = socket.create_connection((host, port), timeout=timeout)
    except OSError:
        last_gpsd_query_ms = int((_time.monotonic() - started) * 1000)
        return None, None, None, None

    try:
        sock.settimeout(timeout)
        # Drain the version banner gpsd sends on connect. One recv is
        # enough — the banner is well under 4 KB.
        sock.recv(4096)
        sock.sendall(b"?POLL;\n")
        # The POLL response is a single JSON object containing `tpv` and
        # `sky` arrays. Read until we see a newline (gpsd terminates
        # responses with \r\n).
        buf = b""
        while b"\n" not in buf and len(buf) < 16384:
            chunk = sock.recv(4096)
            if not chunk:
                break
            buf += chunk
        line = buf.split(b"\n", 1)[0]
        try:
            obj = json.loads(line)
        except json.JSONDecodeError:
            return None, None, None, None
        if obj.get("class") != "POLL":
            return None, None, None, None
        tpvs = obj.get("tpv") or []
        if not tpvs:
            return None, None, None, None
        # gpsd's tpv array can have multiple entries (one per receiver);
        # take the best-quality fix.
        best = max(tpvs, key=lambda t: int(t.get("mode", 0)))
        mode = int(best.get("mode", 0))
        if mode < 2 or "lat" not in best or "lon" not in best:
            return None, None, None, None
        lat = float(best["lat"])
        lon = float(best["lon"])
        alt = float(best.get("altHAE") or best.get("alt") or 0.0)
        return lat, lon, alt, mode
    finally:
        sock.close()
        last_gpsd_query_ms = int((_time.monotonic() - started) * 1000)


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


async def recover_network() -> bool:
    """Attempt to bring the uplink back when the daemon has been silent
    for long enough that we suspect a black-hole. See GH #82.

    Escalation order (each step waits and verifies before falling through):
      1. `nmcli connection down + up` on every active connection
         (cheapest — re-runs DHCP, re-associates with the AP).
      2. `nmcli radio wifi off + on` (full radio cycle — heavier-weight,
         clears state in the kernel/wpa_supplicant).
      3. `mmcli -m any --reset` for the cellular modem if one is
         present. Empty on WiFi-only Pis.

    Returns True if any step succeeded — i.e. we returned to having a
    default route within a short timeout. Returns False if every step
    failed; the caller should re-attempt after another grace window
    rather than spam recoveries (telemetry_poll's job).
    """
    logger.warning("recover_network: attempting to restore uplink")

    if await _try_cycle_nmcli_connection():
        return True
    if await _try_cycle_wifi_radio():
        return True
    if await _try_reset_modem():
        return True

    logger.error("recover_network: all recovery steps failed")
    return False


async def _try_cycle_nmcli_connection() -> bool:
    """Down + up every active nmcli connection. Cheapest recovery."""
    if shutil.which("nmcli") is None:
        return False
    try:
        proc = await asyncio.create_subprocess_exec(
            "nmcli", "-t", "-f", "NAME", "connection", "show", "--active",
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL,
        )
        out, _ = await proc.communicate()
    except OSError:
        return False
    names = [n.strip() for n in out.decode("utf-8", "replace").splitlines() if n.strip()]
    if not names:
        return False
    logger.info("recover_network: cycling nmcli connections: %s", names)
    for name in names:
        await _run("nmcli", "connection", "down", name)
        await _run("nmcli", "connection", "up", name)
    # Verify with a short wait.
    return await wait_for_route(timeout=15.0)


async def _try_cycle_wifi_radio() -> bool:
    """`nmcli radio wifi off + on` — heavier than cycling a connection."""
    if shutil.which("nmcli") is None:
        return False
    logger.info("recover_network: cycling wifi radio")
    rc1 = await _run("nmcli", "radio", "wifi", "off")
    await asyncio.sleep(2.0)
    rc2 = await _run("nmcli", "radio", "wifi", "on")
    if rc1 != 0 or rc2 != 0:
        return False
    return await wait_for_route(timeout=20.0)


async def _try_reset_modem() -> bool:
    """Reset the cellular modem via ModemManager. Returns False on
    machines without a modem (mmcli reports no modems)."""
    if shutil.which("mmcli") is None:
        return False
    try:
        proc = await asyncio.create_subprocess_exec(
            "mmcli", "-L",
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL,
        )
        out, _ = await proc.communicate()
        if b"/Modem/" not in out:
            # No modem present — not a failure, just no-op.
            return False
    except OSError:
        return False
    logger.info("recover_network: resetting cellular modem")
    if await _run("mmcli", "-m", "any", "--reset") != 0:
        return False
    return await wait_for_route(timeout=30.0)


async def _run(*args: str) -> int:
    """Subprocess helper: run, return rc, swallow OSError."""
    try:
        proc = await asyncio.create_subprocess_exec(
            *args,
            stdout=asyncio.subprocess.DEVNULL,
            stderr=asyncio.subprocess.DEVNULL,
        )
        return await proc.wait()
    except OSError:
        return -1


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
        from PIL import Image  # noqa: F401
        from pyzbar.pyzbar import decode  # noqa: F401
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
    except TimeoutError:
        logger.info("QR scan timed out")
        return None
    finally:
        if proc.returncode is None:
            with contextlib.suppress(ProcessLookupError, OSError):
                os.killpg(os.getpgid(proc.pid), 15)
            try:
                await asyncio.wait_for(proc.wait(), 5.0)
            except TimeoutError:
                proc.kill()


async def _qr_loop(proc: asyncio.subprocess.Process) -> QRPayload | None:
    from PIL import Image
    from pyzbar.pyzbar import decode

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
