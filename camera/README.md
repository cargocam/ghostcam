# Ghostcam Camera

The Python daemon that turns a Raspberry Pi (or any Linux box with a
camera and ffmpeg) into a Ghostcam node. Captures H.264 + Opus, uploads
MPEG-TS segments to S3 via presigned URLs, posts telemetry, and relays
live frames to the server over a WebSocket.

This package replaces the Go implementation under `legacy_camera/`. The
Go one is kept buildable until the cutover commit deletes it.

## What it talks to

```
rpicam-vid (or ffmpeg testsrc2)
   │ raw H.264 Annex B
   ▼
ffmpeg ─────────► MPEG-TS .ts files in segment_dir   ──► S3 (presigned PUT)
   │ OGG / Opus on a side-channel pipe
   ▼
LiveRelay (NAL parser) ──► asyncio.Queue ──► WebSocket ──► server
                                                 ▲
                              POST /telemetry every 10 s
```

## Quick start

```bash
# Install from source
cd camera
pip install -e ".[dev]"

# Run against a synthetic source (no rpicam-vid needed)
GHOSTCAM_SYNTHETIC=1 \
GHOSTCAM_SERVER_URL=http://localhost:3000 \
GHOSTCAM_PROVISION_TOKEN=your-token \
ghostcam-camera --test-source --data-dir /tmp/cam --segment-dir /tmp/cam/seg
```

Get a provision token from the server's UI (or `POST /api/v1/cameras`
as an admin). On a fresh data dir the camera generates an ed25519
keypair, uses the token to register its public key, and you'll never
need the token again — the keypair is permanent.

## Configuration

Resolution order (last wins): defaults → TOML file → env vars → CLI flags.

The TOML file is searched in order:
1. `--config <path>`
2. `$GHOSTCAM_CONFIG_FILE`
3. `$GHOSTCAM_DATA_DIR/camera.toml`
4. `/boot/ghostcam.conf`

Most-used env vars:

| Variable | Default | What it does |
|---|---|---|
| `GHOSTCAM_SERVER_URL` | _(required)_ | Server URL — `https://...` for prod, `http://localhost:3000` for dev. |
| `GHOSTCAM_PROVISION_TOKEN` | _(none)_ | One-time token from the server. Only needed before first enrollment. |
| `GHOSTCAM_DATA_DIR` | `/var/ghostcam` | Where the keypair, credentials, and pending-confirm queue live. |
| `GHOSTCAM_SEGMENT_DIR` | `$GHOSTCAM_DATA_DIR/segments` | Where ffmpeg writes `.ts` segments. |
| `GHOSTCAM_RECORDING_MODE` | `never` | `constant`, `motion`, or `never`. The server can override this at runtime via a `set_recording_mode` command; the override is persisted to `$GHOSTCAM_DATA_DIR/recording_mode` and survives restarts. |
| `GHOSTCAM_VIDEO_PROFILE` | _(none)_ | Preset: `zero2w`/`480p`, `pi4`/`720p`, `pi5`/`1080p`. |
| `GHOSTCAM_LOCAL_STORAGE_CAP_MB` | `4096` | Local segment ring cap. Oldest evicted on overflow. |
| `GHOSTCAM_SYNTHETIC` | _(unset)_ | Set to `1` for fake sensors (Docker, CI, dev machines). |
| `GHOSTCAM_LOG_LEVEL` | `INFO` | Standard Python log levels. |

Run `ghostcam-camera --help` for the full CLI surface.

## Development

```bash
pip install -e ".[dev]"

pytest -q                 # unit + parity tests (no Docker)
ruff check .              # lint (CI gate)
mypy ghostcam             # type-check (CI gate)
python -m build --wheel   # produce a distributable .whl

# Integration tests against the real Go server (needs Docker daemon)
pip install testcontainers
pytest tests/integration/ --integration
```

The unit suite covers wire-format parity and the camera's core logic
without touching the network. The integration suite spins up Postgres
+ Redis + MinIO via testcontainers, builds the Go server, and runs
the Python camera against it through real sockets.

### Wire-format invariants (must not drift)

Every item in this list has a fixture in
`tests/test_wire_format.py`. Failing one of these is the canary for "the
server won't accept this." See the file for full coverage; the
load-bearing items:

  * Device ID = first 16 bytes of `SHA-256(public_key)` as hex (32 chars).
  * Signature payload: `f"{method}\n{path}\n{ts}\n{device_id}"` — Unix
    **seconds** in `ts` (NOT milliseconds; telemetry timestamps are ms,
    signatures are s — easy to confuse).
  * Signature encoding: `base64.urlsafe_b64encode(sig).rstrip(b"=")` —
    Go `RawURLEncoding`, no padding.
  * Auth header: `Authorization: Signature device_id=<hex>,ts=<int>,sig=<b64>`
    — exactly that, no spaces inside the comma-separated list.
  * WebSocket binary frame: `[ts:4 BE][flags:1][payload]`. One NAL unit
    per binary message. Flags: `0x01` = keyframe, `0x02` = audio.
  * Init segment S3 key: `{deviceID}/init.mp4`, presigned via
    `init_url`, only on first call.

A cross-language harness in `tools/sigverify` signs in Python and
verifies in Go (and vice versa) on every CI run, so signing parity
isn't on the honor system.

### Architecture

Five subsystems, each an `asyncio` task under one `TaskGroup` in
`main.py`:

  * **`live_ws`** — persistent WebSocket; sends frames when the server
    has flipped streaming on (`start_stream` / `stop_stream` JSON
    control messages).
  * **`capture`** — orchestrates `rpicam-vid` + `ffmpeg`. The Opus
    audio side-channel uses ffmpeg's `pipe:{wfd}` URL with `pass_fds`
    instead of fixing on fd 3 — Python's child interpreter recycles
    low fds during startup so the Go-style fd-3 layout silently
    fails. See the comment block at the top of `capture.py`.
  * **`watcher`** — polls `segment_dir` every 2 s, validates the
    MPEG-TS sync byte, runs motion detection, pushes onto the upload
    queue.
  * **`upload`** — drains the segment queue, requests presigned URLs,
    PUTs to S3, persists confirms atomically to
    `pending_confirms.json`. Resumes on restart.
  * **`telemetry_poll`** — POSTs sensor readings every 10 s, dispatches
    server-issued commands.

Provisioning runs once before the TaskGroup. Graceful shutdown:
SIGTERM/SIGINT cancels the group with a 15 s drain budget.

`docs/architecture.md` has the file-by-file breakdown.

### Performance and the Rust path

The H.264 NAL Annex B scanner in `live_relay.py::_find_start_code` is
the only byte-level hot path. A planning spike measured ~883 MB/s in
pure-Python `bytes.find` on a modern x86 host; conservatively
extrapolated to a Pi Zero 2W's single core that's ~221 MB/s. The 2
Mbps Pi production rate is 0.25 MB/s — leaves an ~883x margin, so
pure Python is overwhelmingly fine.

Long-term plan is to move that scan into a Rust crate behind a pyo3
binding. The hot path is isolated in one function so the swap is a
two-line change in `_native.py`. Don't rewrite anything else for
performance reasons without profiling on a Zero 2W first.

### Platform abstraction

Replaces the Go camera's build tags. Selected at import time:

  * `GHOSTCAM_SYNTHETIC=1` → `platform/synthetic.py` (fake sensors,
    deterministic GPS orbit). Used by the docker compose test fleet
    and CI integration tests.
  * Linux + `/proc` available → `platform/linux.py` (`/proc/stat`,
    `/proc/meminfo`, `/sys/class/thermal`, `/proc/net/wireless`,
    gpsd, nmcli, rpicam-still + pyzbar).
  * Otherwise → synthetic fallback (macOS/Windows dev).

Real-Pi sensors require `python3-cryptography` system bindings only if
you opt into the `[real]` extra (which pulls `pyzbar`). The default
install runs synthetic.

## Deployables

Three distinct artifacts, all wrapping the same wheel:

  * **`pip install ghostcam`** — once published to PyPI. Today: install
    from a release wheel or directly from this directory.
  * **`.deb`** — produced by `.github/workflows/release.yml`. Installs
    the wheel into `/opt/ghostcam` (a venv) with a `/usr/local/bin/ghostcam-camera`
    symlink, and ships a systemd unit. (NOTE: today the `.deb` still
    wraps the Go binary; the cutover commit replaces it.)
  * **Pi `.img.xz`** — produced by `.github/workflows/pi-images.yml`
    via `rpi-image-gen`, consumes the `.deb`. Flashable directly from
    the Get Started UI in the viewer.

The on-Pi development loop is `./scripts/pi.sh deploy` — that builds
the wheel locally, scp's it, and `pip install --upgrade`s into
`/opt/ghostcam`. No `.deb` round-trip required for iteration.

## Contributing

Drift between the Go wire contract (`common/types.go`,
`common/telemetry.go`) and these Python types is a CI failure, not a
runtime mystery. Type changes start in Go:

```bash
# 1. Edit the Go struct in common/ or server/apitypes/.
# 2. Regenerate both consumers:
go generate ./...
# 3. Commit the Go change AND the regenerated trees:
#    - ui/src/lib/api-types/  (TypeScript via tygo)
#    - camera/ghostcam/wire/  (pydantic via tools/pydanticgen)
```

CI's drift check (`go generate ./... && git diff --exit-code`) blocks
PRs that forget step 3.

For wire-format-level changes (signature scheme, WebSocket frame
layout, S3 key shape), update the corresponding fixtures in
`tests/test_wire_format.py` AND the cross-language harness in
`tools/sigverify`. Both languages must agree on every byte; the harness
asserts that round-tripping in either direction is byte-identical.
