# Ghostcam — Camera Firmware

**Status:** Draft

---

## 1. Overview

This document specifies the behaviour of the Ghostcam camera firmware running on a Raspberry Pi Zero 2W. It covers the hardware configuration, capture pipeline, fMP4 recording, QUIC connection lifecycle, telemetry, certificate storage, and build and deployment.

The firmware is written in Rust, cross-compiled for `aarch64-unknown-linux-gnu`, and deployed to the Pi via the project deploy script. It is fully async using Tokio.

A working reference implementation using the same hardware is available at [github.com/andymitch/kodama](https://github.com/andymitch/kodama). The wire protocol spoken by the firmware is specified in `wire-protocol.md`. The recording and playback pipeline is specified in `playback.md`. Certificate lifecycle is specified in `auth.md`.

---

## 2. Hardware

| Component | Hardware | Interface |
|-----------|----------|-----------|
| SoC | Raspberry Pi Zero 2W | — |
| Camera | IMX219 | CSI via `rpicam-vid` |
| Audio | USB or I2S microphone | `cpal` (ALSA backend) |
| GPS | SIM7600G-H (or compatible) | `gpsd` |
| Connectivity | WiFi (primary), cellular (failover) | NetworkManager |

### 2.1 GPS daemon

`gpsd` runs as a system service and exposes GPS readings on a local socket. The firmware reads from `gpsd` using its shared memory interface for low-latency position access. GPS hardware is optional — if the `gpsd` socket is unavailable the firmware skips GPS fields in telemetry datagrams silently without error. System configuration for `gpsd` is in `pi/` in the project repository.

### 2.2 Cellular failover

The Pi uses NetworkManager to manage both WiFi and cellular interfaces. On WiFi loss, NetworkManager automatically routes traffic over the cellular interface. The firmware detects the transition via connection loss and reconnect — no firmware-level interface management is required. Reconnect triggers adaptive bitrate ramp-down (see §4.3).

---

## 3. Process Structure

The firmware runs as a single Tokio async process with the following concurrent tasks:

| Task | Description |
|------|-------------|
| Video capture | `rpicam-vid` subprocess, reads H.264 NAL units via pipe |
| Audio capture | `cpal` stream, reads PCM frames from audio device |
| Opus encoder | Encodes PCM to Opus frames, feeds live audio stream and fMP4 muxer |
| fMP4 muxer | Consumes H.264 and Opus frames, writes segments and manifest to disk |
| QUIC connection | Manages connection to server, owns all stream write loops |
| Telemetry loop | Reads sensors, buffers datagrams, sends live datagrams at configured interval |
| Telemetry buffer | In-memory ring buffer of undelivered telemetry datagrams; drained on reconnect |
| GPS reader | Reads from `gpsd` if available, makes latest fix available to telemetry loop |
| Command handler | Reads Commands stream, dispatches to appropriate task |
| Upload handler | Opens outbound QUIC streams for segment, init, manifest, and telemetry buffer uploads |

Tasks communicate via Tokio channels. The capture tasks are the source; all downstream tasks are consumers.

---

## 4. Capture Pipeline

### 4.1 Video

`rpicam-vid` is launched as a subprocess with H.264 output piped to the firmware process. The firmware reads NAL units from the pipe and fans them to two consumers:

- **Live QUIC stream** — NAL units are written directly to the Video stream as they arrive
- **fMP4 muxer** — NAL units are forwarded to the muxer task via an async channel

`rpicam-vid` is configured to force a keyframe at every segment boundary interval (10 seconds) so that segment files are always GOP-aligned.

### 4.2 Audio

`cpal` opens the audio input device and delivers PCM frames to the Opus encoder task. Encoded Opus frames are fanned to two consumers:

- **Live QUIC stream** — Opus frames are written directly to the Audio stream
- **fMP4 muxer** — Opus frames are forwarded to the muxer task via an async channel

### 4.3 Adaptive bitrate

The firmware adjusts video bitrate dynamically based on network conditions. Four quality tiers are defined:

| Tier | Bitrate |
|------|---------|
| Low | 500 Kbps |
| Medium | 1.5 Mbps |
| High | 2.5 Mbps |
| Max | 4 Mbps |

On connect the firmware starts at the lowest tier and ramps up as the connection stabilises. On reconnect (including cellular failover) the firmware resets to the lowest tier and ramps up again. Bitrate changes are issued to `rpicam-vid` via its control interface without restarting the capture process. Hysteresis is applied to prevent oscillation between tiers.

### 4.4 Live stream gating

The firmware only writes to the Video and Audio QUIC streams when the server has issued `start_video` and `start_audio` respectively. On `stop_video` or `stop_audio`, the firmware stops writing to the corresponding stream. The capture pipeline and fMP4 muxer continue running regardless — recording is independent of live stream demand.

---

## 5. fMP4 Recording

### 5.1 Storage layout

The camera uses a dedicated data partition (`mmcblk0p3`, mounted at `/var/ghostcam/`) separate from the OS partition. All runtime writes — segments, telemetry buffer, certs, WiFi config, enrolled server address — live on this partition. The OS partition is never written to during normal operation.

| Partition | Mount | Contents |
|-----------|-------|----------|
| `mmcblk0p1` | `/boot` | Boot files, `ghostcam.conf` |
| `mmcblk0p2` | `/` | OS, firmware binary (read-only after first boot) |
| `mmcblk0p3` | `/var/ghostcam/` | Segments, telemetry buffer, certs, config |

The data partition is used exclusively by the firmware. No other processes write to it.

### 5.2 Muxer

The fMP4 muxer task receives H.264 and Opus frames from the capture pipeline and writes them to disk as fMP4 files. It runs continuously from capture start regardless of connection state — recording is not interrupted by QUIC disconnects.

On capture start the muxer writes `init.mp4` containing the codec parameters for the current capture session. A new `init.mp4` is written if capture parameters change (e.g. resolution change after reboot).

Every 10 seconds the muxer finalises the current segment, writing it to disk as `{device_id}:{start_ts}.m4s`, and begins the next segment. On finalisation it:

1. Updates `playlist.m3u8` on disk
2. Sends a `recording_segment` alert to the server
3. Opens a manifest push stream to the server and writes the updated manifest

### 5.3 Ring buffer

Segments are stored at `/var/ghostcam/segments/`. The ring buffer fills the data partition — there is no artificial time-based cap. The camera stores as much footage as the partition holds. When a new segment would exceed available space, the oldest segment is evicted.

On eviction the muxer:

1. Deletes the `.m4s` file
2. Updates `playlist.m3u8`
3. Sends a `segment_evicted` alert to the server
4. Opens a manifest push stream and writes the updated manifest

### 5.4 Storage-full handling

If a segment write fails with `ENOSPC` (unexpected full — caused by filesystem overhead, a corrupted file, or OS writes consuming space):

1. Attempt emergency eviction — delete the oldest 5 segments to reclaim space
2. Retry the write
3. If still `ENOSPC`: pause recording and send a `storage_full` alert to the server
4. Live streaming continues unaffected — only recording pauses
5. Poll free space every 60 seconds while paused
6. If free space becomes available (operator removes files): resume recording and send a `storage_resumed` alert

Emergency eviction deletes segments starting from the oldest regardless of the normal eviction policy. Up to 5 segments (~50 seconds of footage) are deleted per attempt.

### 5.5 Startup recovery

On firmware start the muxer scans `/var/ghostcam/segments/` and reconstructs state from files on disk:

- Files that cannot be parsed as valid fMP4 segments are deleted
- Partially written segments (detected by size mismatch against the manifest) are deleted
- The manifest is rebuilt from surviving segments
- Any segments not yet acknowledged by the server are queued for `recording_segment` push on the next QUIC connection

---

## 6. QUIC Connection Lifecycle

### 6.1 Registration mode

On startup, if the camera has no valid user association certificate at `/etc/ghostcam/user.crt`, it enters registration mode. The camera does not attempt a normal QUIC connection.

**QR scanning pipeline:**

1. Activate the IMX219 camera sensor via `rpicam-still` in a continuous capture loop — one frame every 500ms
2. Pass each frame to `rqrr` for QR code detection and decoding
3. Loop runs indefinitely until a valid enrollment QR code is decoded — no timeout
4. On successful decode, parse the JWT payload:
   - Check `exp` — reject if expired (clock skew tolerance: ±5 minutes)
   - Extract `server_addr`, `display_name` (optional), `wifi` credentials (optional)
5. Store any WiFi credentials to `/var/ghostcam/networks.conf` and apply to NetworkManager
6. Store `server_addr` to `/etc/ghostcam/server.addr`
7. Initiate enrollment QUIC connection to the decoded `server_addr`

**Enrollment QUIC connection:**

1. Connect to server presenting device identity certificate only (no user association cert)
2. Server routes to enrollment handler — no `IngestSlot` created
3. Open an Alerts stream
4. Generate a new P-256 key pair locally
5. Send a `csr` alert carrying the PEM-encoded CSR
6. Wait for a `cert_refresh` command carrying the signed certificate and the CA cert
7. Store the user association certificate to `/etc/ghostcam/user.crt`
8. Store the locally generated key to `/etc/ghostcam/user.key`
9. Store the CA certificate to `/etc/ghostcam/ca.crt`
10. On `server-solo`: perform TOFU — store the server's TLS certificate fingerprint to `/etc/ghostcam/server.pin`
11. Send `ack`
12. Exit registration mode, close the enrollment connection, proceed with the normal startup sequence

The private key generated in step 4 never leaves the camera. The server signs only the public key from the CSR.

**Security note:** registration mode is only entered if no valid user association certificate exists. A camera cannot be re-enrolled while enrolled — the only path back to registration mode is a server-initiated `unregister` command from the legitimate owner's authenticated session. See `auth.md` §5.

### 6.2 Normal startup sequence

1. Load device identity cert and user association cert from local storage (see §9)
2. Resolve server address using precedence order: `ghostcam.conf` → `/etc/ghostcam/server.addr` → hardcoded cloud default
3. Establish QUIC/mTLS connection to server, presenting both certs
4. Verify server TLS cert: `server-solo` — check against pinned fingerprint at `/etc/ghostcam/server.pin`; `server-multi` — standard verification against system CA bundle
5. Open three outbound unidirectional streams: **Alerts**, **Video**, **Audio**
6. Wait for server to open inbound **Commands** stream
7. Send `handshake` alert declaring `protocol_version`, `fw_version`, and active streams
8. Send `networks` alert reporting current known WiFi networks
9. Open a manifest push stream and write the current `playlist.m3u8`
10. If the telemetry buffer contains undelivered entries, open a telemetry buffer upload stream and write all buffered entries
11. Begin telemetry datagram loop
12. Begin stream write loops for any active media (gated on `start_video` / `start_audio` commands)
13. Send `recording_segment` alerts for any segments not yet persisted by the server

### 6.3 Keepalives

The firmware sends QUIC keepalives every 15 seconds. If the connection is lost, the firmware attempts reconnection with exponential backoff starting at 1 second, capped at 30 seconds.

### 6.4 Reconnect

On reconnect the full startup sequence is repeated from step 1 of §6.2. All server state is reconstructed from the `handshake` message and subsequent alerts — the firmware makes no assumptions about what state the server retained.

The fMP4 muxer continues running during reconnect. Any segments finalised during the disconnection period are queued and pushed as `recording_segment` alerts after reconnect. Telemetry datagrams generated during disconnection are stored in the telemetry buffer and uploaded on reconnect.

---

## 7. Telemetry

### 7.1 Telemetry tick algorithm

The telemetry task polls sensors every 2 seconds and computes a sparse diff against the previous transmitted payload using per-field thresholds. If any field exceeds its threshold, the new payload is transmitted immediately. Otherwise, a full heartbeat is transmitted every 30 seconds regardless of whether any threshold was crossed.

| Field | Source | Threshold |
|-------|--------|-----------|
| `ts` | System clock (milliseconds) | — |
| `sig` | WiFi signal strength via `iwconfig` or `nl80211` | 5 dBm |
| `temp` | SoC temperature via `/sys/class/thermal/thermal_zone0/temp` | 1°C |
| `fps` | Frame counter maintained by the video capture task | 2 fps |
| `kbps` | Current encoder bitrate from ABR state | 500 Kbps |
| `cpu` | CPU usage via `/proc/stat` | 5% |
| `mem` | Memory usage via `/proc/meminfo` | 5 MB |
| `uptime` | System uptime via `/proc/uptime` | Any change |
| `lat`, `lon` | `gpsd` shared memory interface | 0.0001° (~11 metres) |
| `alt` | `gpsd` shared memory interface | 10 metres |
| `gps_fix` | `gpsd` fix quality | Any change |

GPS hardware is optional. If the `gpsd` socket is not present, the GPS reader task exits silently and GPS fields are omitted from all telemetry datagrams without error. This is not an error condition.

Only changed fields are included in threshold-triggered payloads — the payload is sparse. Full heartbeats include all currently available fields.

### 7.2 Online transmission

When the QUIC connection is available, each telemetry payload is sent directly to the server as a MessagePack-encoded datagram. No local disk write occurs — the server persists the entry to Redis immediately on receipt.

### 7.3 Offline buffer

When the QUIC connection is unavailable, telemetry payloads are written to an on-disk buffer rather than dropped. The buffer is an ordered list of standard telemetry datagrams — no special format or schema is required beyond what is sent live.

To avoid accumulating long runs of identical heartbeat entries, the buffer applies the following deduplication on each write:

```
if new_tick == ticks[-1]:
    if new_tick == ticks[-2]:
        ticks[-1].ts = new_tick.ts  # update timestamp of last entry in place
    else:
        ticks.append(new_tick)
else:
    ticks.append(new_tick)
```

A run of identical heartbeats compresses to exactly two entries — the first and the most recent — preserving the start and end of the stable period. Threshold-breaking ticks are always appended as new entries.

The on-disk buffer is capped at 100,000 entries. When the cap is reached, the oldest entries are evicted to make room for new ones — consistent with the ring buffer and broadcast channel eviction policies. In practice the dedup logic means a run of stable heartbeats compresses to two entries, so the effective coverage before eviction begins is far longer than the raw entry count suggests.

The on-disk buffer is stored in the segment directory alongside fMP4 files. It is append-only during offline periods and cleared on successful upload.

### 7.4 Reconnect upload

On reconnect, after the handshake and manifest push, if the on-disk buffer contains entries the firmware opens a dedicated outbound QUIC unidirectional stream and writes all buffered entries as a MessagePack array. The firmware clears the buffer after the stream closes successfully. The server persists each entry to Redis as a standard telemetry entry — no special handling is required for buffered vs. live entries.

See `wire-protocol.md` §5.2 for the upload stream format.

---

## 8. Command Handling

The firmware reads commands from the Commands QUIC stream and dispatches them to the appropriate task. Commands are processed in order of receipt.

| Command | Handler |
|---------|---------|
| `start_video` | Enables writes to the Video QUIC stream |
| `stop_video` | Disables writes to the Video QUIC stream |
| `start_audio` | Enables writes to the Audio QUIC stream |
| `stop_audio` | Disables writes to the Audio QUIC stream |
| `upload_segment` | Upload handler opens a QUIC stream and writes the requested `.m4s` file |
| `upload_init` | Upload handler opens a QUIC stream and writes `init.mp4` |
| `reboot` | Sends `ack`, flushes pending writes, calls `systemctl reboot` |
| `network_config` | Appends a WiFi network to `/var/ghostcam/networks.conf`, updates NetworkManager |
| `remove_network` | Removes a WiFi network from `/var/ghostcam/networks.conf`, updates NetworkManager |
| `list_networks` | Sends `networks` alert with current known network list |
| `update_available` | Sends `ack`, defers to next segment boundary if recording, then begins OTA update flow (see §11) |
| `cert_refresh` | Stores new cert and CA cert, sends `ack` (see `auth.md`) |
| `unregister` | Sends `ack`, clears all enrollment state, enters registration mode (see `auth.md`) |

### 8.1 Upload handler

On `upload_segment`:
1. Locate the `.m4s` file for the requested `segment_id`
2. If the file does not exist (evicted), send `segment_upload_failed` with `reason: "evicted"`
3. Open a new outbound QUIC unidirectional stream
4. Write the raw file bytes
5. Close the stream
6. Send `segment_uploaded` alert

On `upload_init`:
1. Open a new outbound QUIC unidirectional stream
2. Write the raw `init.mp4` bytes
3. Close the stream

No alert is sent on init upload completion — the server infers completion from stream close.

### 8.2 Network handler

On `network_config`:
1. Append the new SSID and PSK entry to `/var/ghostcam/networks.conf`
2. Add the network to NetworkManager via `nmcli`
3. No alert required — the server updates its local model optimistically

On `remove_network`:
1. Remove the matching SSID entry from `/var/ghostcam/networks.conf`
2. Remove the network from NetworkManager via `nmcli`

On `list_networks`:
1. Read `/var/ghostcam/networks.conf`
2. For each known network, check current signal strength if connected
3. Send `networks` alert with the full list

---

## 9. Persistent Storage

All runtime state is stored on the data partition (`/var/ghostcam/`) or in `/etc/ghostcam/`. The OS partition is never written to during normal operation.

### 9.1 Identity and trust (`/etc/ghostcam/`)

| File | Description | Cleared on unregister |
|------|-------------|----------------------|
| `device.crt` | Self-generated device identity certificate | No |
| `device.key` | Device identity private key | No |
| `user.crt` | User association certificate (issued by server) | Yes |
| `user.key` | User association private key (generated locally) | Yes |
| `ca.crt` | Server CA certificate (pinned during enrollment) | Yes |
| `server.pin` | Server TLS fingerprint (`server-solo` only) | Yes |
| `server.addr` | Server address learned during enrollment | Yes |

### 9.2 Runtime data (`/var/ghostcam/`)

| File / Directory | Description |
|-----------------|-------------|
| `segments/` | fMP4 ring buffer — `.m4s` segment files and `init.mp4` |
| `playlist.m3u8` | Current HLS manifest |
| `telemetry.buf` | On-disk telemetry buffer for offline periods |
| `networks.conf` | Known WiFi networks (TOML) |
| `firmware.prev` | Previous firmware binary (retained for OTA rollback) |
| `healthy` | Watchdog sentinel — written after successful post-update server connection |
| `rollback` | Written by watchdog wrapper after rollback — cleared on next successful boot |
| `audit.log` | Audit log (server-side only — not present on camera) |

---

## 10. Build and Deployment

The firmware is cross-compiled for `aarch64-unknown-linux-gnu` on a development machine and deployed to the Pi over SSH during development. Production deployment uses the Ghostcam camera image — see `deployment.md` §3.

```bash
# Cross-compile
cargo build --release -p camera --target aarch64-unknown-linux-gnu

# Deploy to Pi (development)
./scripts/pi.sh deploy
```

The deploy script copies the compiled binary to the Pi and restarts the systemd service. System dependencies (gpsd, NetworkManager cellular configuration) are configured separately via `./scripts/pi.sh setup` on first boot.

### 10.1 `ghostcam.conf`

Optional operator configuration file placed on the boot partition (`/boot/ghostcam.conf`) before first power-on. Never written by the firmware.

```toml
# Server address override (for server-solo deployments)
server_addr = "192.168.1.10:4433"

# Hardware toggles
no_audio = false
no_gps = false
```

### 10.2 CLI flags (development only)

| Flag | Default | Description |
|------|---------|-------------|
| `--server-addr` | — | Server QUIC address (overrides all other sources) |
| `--device-id` | — | Override device identifier for testing |
| `--segment-dir` | `/var/ghostcam/segments` | Directory for fMP4 ring buffer |
| `--no-audio` | off | Disable audio capture |
| `--no-gps` | off | Disable GPS even if `gpsd` is available |

### 10.3 Runtime dependencies

| Dependency | Purpose |
|------------|---------|
| `rpicam-vid` | H.264 video capture (live streaming and recording) |
| `rpicam-still` | Still frame capture (QR scanning in registration mode) |
| `rqrr` | QR code decoding library (pure Rust) |
| `gpsd` | GPS daemon (optional) |
| ALSA | Audio device access via `cpal` |
| NetworkManager | WiFi and cellular interface management |

---

## 11. Firmware Updates (OTA)

### 11.1 Update flow

On receipt of an `update_available` command:

1. Send `ack` immediately — acknowledges receipt, not success
2. If `force` is false and recording is active: wait for the current segment boundary (at most 10 seconds)
3. Download the firmware binary from `url` over HTTPS, verifying the server cert against the system CA bundle
4. Compute SHA-256 of the downloaded binary
5. If hash does not match `sha256`: send `update_failed` with `reason: "download_failed"` or `"hash_mismatch"` and abort
6. Write new binary to `/var/ghostcam/firmware.new`
7. Copy current binary to `/var/ghostcam/firmware.prev`
8. Atomically replace `/usr/bin/ghostcam-camera` with `firmware.new` via `rename(2)`
9. Delete `/var/ghostcam/healthy` (clears the watchdog sentinel)
10. Send `update_applying` alert
11. Call `systemctl reboot`

### 11.2 Watchdog supervisor

The systemd service runs a watchdog wrapper script rather than the firmware binary directly.

On each boot the wrapper:

1. Checks for `/var/ghostcam/healthy`:
   - **Present**: normal boot — exec the firmware binary directly
   - **Absent**: post-update boot — launch firmware with a 5-minute watchdog timer

2. If the watchdog fires (firmware did not write `healthy` within 5 minutes):
   - Copy `/var/ghostcam/firmware.prev` to `/usr/bin/ghostcam-camera`
   - Write `/var/ghostcam/rollback`
   - Call `systemctl reboot`

3. On the next boot after a rollback:
   - `healthy` is absent but `rollback` is present — launch previous firmware directly
   - Write `healthy` immediately (previous firmware was known good)
   - Delete `rollback`

### 11.3 Post-update success

On the first successful QUIC connection after an update boot:

1. Write `/var/ghostcam/healthy`
2. Send `update_succeeded` alert with the new version string
3. Delete `/var/ghostcam/firmware.prev` (no longer needed for rollback)

### 11.4 Air-gapped deployments

Cameras download firmware from `https://releases.ghostcam.io` by default. For deployments without internet access on the camera network, the operator can host firmware binaries on a local HTTP server and configure a custom base URL via the server's `--firmware-base-url` flag. The server includes this URL in the `update_available` command payload.

---

## 12. Open Questions

| Question | Notes |
|----------|-------|
| `network_config` ACK and rollback | The `network_config` command adds a WiFi network without acknowledgement. If a bad PSK is added the camera may attempt to connect to the network and fail, but won't lose connectivity to its current connection. However if the camera reboots after storing a bad credential for its only network it could be stranded. A connection validation step or rollback mechanism may be warranted. |
