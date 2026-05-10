# Pi Test Plan

End-to-end acceptance for the Python camera against real Pi hardware.
The unit suite under `camera/tests/` covers the contract; this plan
covers the bits that need a real sensor, real network, and real
viewer round-trip.

Run this whenever:

* The cutover commit lands (must-pass before merging).
* A `--version` bump on `main` ships through `release.yml`.
* The Pi `.deb` Depends line, the systemd unit, or the rpi-image-gen
  layer changes.
* Anything in `camera/ghostcam/capture.py`, `live_ws.py`,
  `provisioning.py`, or `firmware.py` changes meaningfully.

The plan is deliberately ordered so each phase exercises only what
prior phases have validated. If Phase 4 fails, don't skip to Phase 8;
fix Phase 4 first.

## Conventions

Each test item has the shape:

```
- [ ] Test name
      Steps:    1. ... 2. ...
      Expect:   what success looks like (server-side AND camera-side)
      On fail:  where to look first
```

Commands prefixed `pi$` run on the Pi (via `./scripts/pi.sh ssh` or
SSH directly). Commands prefixed `dev$` run on the dev/CI machine.
Server-side checks assume you're authenticated in the viewer UI as
admin.

## 0. Prerequisites

### Hardware

- [ ] Pi 4 (4 GB), Pi 5 (4 GB), or Pi Zero 2W. (At minimum: Pi 4 + Zero 2W.)
- [ ] Official Camera Module (v2 or v3) attached via CSI.
- [ ] Optional: INMP441 I2S microphone wired per `pi/image/files/asound.conf`.
- [ ] Optional: SIM7600G-H modem with active SIM (cellular failover testing).
- [ ] Optional: External GPS antenna with sky visibility (for 3D fix).
- [ ] microSD ≥ 16 GB.

### Server

- [ ] Server reachable from the Pi over the internet (or LAN with
      `GHOSTCAM_PUBLIC_IP` set, if testing on `docker compose`).
- [ ] MinIO / Tigris bucket has space (`storage_capped` test
      separately exercises the full state).
- [ ] Admin account exists; you can log in to the viewer.

### Dev machine

- [ ] `.pi.env` set with `PI_HOST`, `PI_USER`, `PI_PASSWORD`.
- [ ] `sshpass` installed (`brew install hudochenkov/sshpass/sshpass`
      or `apt install sshpass`).
- [ ] `python3 -m build --wheel` works in `camera/`
      (`pip install build` if not).
- [ ] Latest `main` (or the branch under test) checked out.

---

## Phase 1 — Fresh Pi onboarding

Goal: from a freshly-flashed OS to a running camera service in one
script invocation.

- [ ] **1.1 Flash a clean Pi OS image** (Bookworm, 64-bit, with SSH
      enabled in user-data).
      Steps:    Use Raspberry Pi Imager → Pi OS Lite (64-bit) →
                preconfigure user/SSH/WiFi if desired. Boot, wait
                for SSH.
      Expect:   `pi$ uname -a` shows `aarch64 GNU/Linux`.
      On fail:  Check imager logs; SSH not enabled?

- [ ] **1.2 First connection**
      Steps:    `dev$ ./scripts/pi.sh status`
      Expect:   "Cannot connect" warnings about ghostcam-camera /
                gpsd not yet existing — this is expected on a fresh
                Pi. The status command itself should connect.
      On fail:  `.pi.env` host/user/pass mismatch.

- [ ] **1.3 Run setup**
      Steps:    `dev$ ./scripts/pi.sh setup` and answer the
                "server address" prompt with your server URL
                (or set `GHOSTCAM_SERVER_ADDR` in env).
      Expect:   Setup completes without errors. Final lines:
                `=== Setup complete ===` and `Camera is running.`
      On fail:  Inspect output. The setup phase logs each step;
                most failures show up as apt errors (missing repo)
                or rpicam package detection.

- [ ] **1.4 Verify package install**
      Steps:    `pi$ dpkg -l python3 python3-venv libzbar0 ffmpeg gpsd modemmanager network-manager`
      Expect:   All listed `ii` (installed).
      On fail:  Re-run setup; check `apt-get` errors.

- [ ] **1.5 Verify systemd units enabled**
      Steps:    `pi$ systemctl is-enabled ghostcam-camera ghostcam-gps gpsd`
      Expect:   All three print `enabled`.
      On fail:  `systemctl daemon-reload && systemctl enable …`.

- [ ] **1.6 Verify camera identity created**
      Steps:    `pi$ sudo ls -la /var/ghostcam/identity_key{,.pub}`
      Expect:   Both files exist; `identity_key` has mode `0600`,
                `identity_key.pub` has `0644`.
      On fail:  Camera couldn't write to `/var/ghostcam`. Check
                ownership.

- [ ] **1.7 Verify `/usr/local/bin/ghostcam-camera`**
      Steps:    `pi$ ghostcam-camera --version`
      Expect:   `ghostcam-camera 0.1.0` (or current version).
      On fail:  Symlink missing — `ls -la /opt/ghostcam/bin/ghostcam-camera`
                should also exist; `ln -sf` if needed.

---

## Phase 2 — Provisioning

Goal: every supported provision path lands the camera with a stable
device_id matching the server's view.

- [ ] **2.1 QR provisioning** (the user-facing happy path)
      Steps:    1. In the viewer, click "Add Camera" → display the QR.
                2. Hold the QR in front of the camera lens (15–30 cm,
                   well-lit).
                3. Wait for `journalctl -u ghostcam-camera -f` to log
                   "QR code decoded: server=…".
      Expect:   "provisioning complete: device_id=…" within ~10 s
                of the QR being shown. Camera appears in the viewer's
                camera list within 30 s.
      On fail:  Check `pyzbar/Pillow not installed` (install
                `ghostcam[real]` extra). Check `rpicam-still not on
                PATH`. Reduce ambient glare.

- [ ] **2.2 Pre-shared token provisioning**
      Steps:    1. In the viewer admin → cameras → "Generate token"
                   (or `POST /api/v1/cameras` as admin).
                2. `pi$ echo "$TOKEN" | sudo tee /var/ghostcam/provision_token`
                3. `pi$ sudo systemctl restart ghostcam-camera`
      Expect:   "provisioning complete" without QR scan path. Same
                device_id as before (per identity persistence).
      On fail:  Token was consumed; generate a new one.

- [ ] **2.3 Server-URL change triggers re-provisioning**
      Steps:    1. Note current `device_id` from viewer.
                2. Change `GHOSTCAM_SERVER_URL` in `/etc/ghostcam/env`
                   to a different server URL (or use a token from a
                   second server).
                3. `pi$ sudo systemctl restart ghostcam-camera`
      Expect:   Camera re-enters provisioning. Same device_id ends
                up registered on the new server (the keypair is
                permanent).
      On fail:  `pi$ ls /var/ghostcam/identity_key*` — these MUST
                survive a server change. If they don't, that's a
                bug in `clear_credentials`.

- [ ] **2.4 Unenroll preserves identity**
      Steps:    `dev$ ./scripts/pi.sh unenroll`
      Expect:   Camera stops, files like `server_url`, `device_id`,
                `provision_token` are removed, but `identity_key`
                and `identity_key.pub` remain. On next start the
                camera enters provisioning with the same keypair.

- [ ] **2.5 QR with WiFi credentials**
      Steps:    Generate a QR that includes `w` (SSID) and `p` (PSK)
                fields. Wipe Pi WiFi config and unenroll. Show the
                QR.
      Expect:   Camera reads QR → calls `nmcli device wifi connect …`
                → waits for default route → POSTs `/provision`.
      On fail:  Check `nmcli` is on PATH; check the WiFi network is
                in range and the PSK is correct.

---

## Phase 3 — Telemetry and observability

Goal: every telemetry field that should be populated *is* populated,
on the cadence we expect.

- [ ] **3.1 Telemetry POST cadence**
      Steps:    Watch `pi$ journalctl -u ghostcam-camera -f` for one
                minute.
      Expect:   ~6 successful telemetry posts (10 s base interval).
                "boot_ok" marker file appears after first success
                (`pi$ ls /var/ghostcam/boot_ok`).
      On fail:  If the cadence is 30 s or 60 s, the camera is in
                backoff — server is rejecting POSTs. Inspect the
                response body via `journalctl`.

- [ ] **3.2 Telemetry visible in viewer**
      Steps:    Open the camera's detail view in the UI.
      Expect:   Live numbers update every ~10 s: CPU %, memory MB,
                temperature °C, uptime, WiFi RSSI (if on WiFi).
      On fail:  SSE stream not connected — check browser dev tools
                Network tab for `/events`.

- [ ] **3.3 GPS telemetry (no fix)**
      Steps:    Without a GPS antenna or with view of sky blocked,
                wait two telemetry cycles.
      Expect:   `lat`/`lon`/`alt`/`gps_fix` fields ARE NULL in the
                wire (`omitempty`). Viewer shows "No GPS" or hides
                the map marker.

- [ ] **3.4 GPS telemetry (3D fix)**
      Steps:    Place GPS antenna with sky visibility. Allow up to
                2 minutes for cold start.
      Expect:   `gps_fix=3`, `lat` and `lon` populated, `alt`
                populated (from `altHAE`). Map marker appears in
                viewer.
      On fail:  `pi$ gpspipe -w -n 5` should show TPV reports. If
                gpsd isn't responding, check `systemctl status gpsd`
                and `/dev/ttyUSB1` exists.

- [ ] **3.5 Server-unreachable backoff**
      Steps:    1. Block the server URL from the Pi
                   (`pi$ sudo iptables -A OUTPUT -d <server-ip> -j DROP`).
                2. Wait 90 s.
                3. Restore (`-D` instead of `-A`).
      Expect:   Logs show 3 failures in a row → interval grows to
                30 s, then 60 s. After restore, next post succeeds
                and interval resets to 10 s. Capture pipeline pauses
                ("server unreachable") during the outage and resumes
                when the server is reachable again.

---

## Phase 4 — Recording / video / audio

Goal: each `recording_mode` setting produces the expected on-disk
and on-S3 state, and audio actually rides along.

- [ ] **4.1 recording_mode=never (default for fresh enrollment)**
      Steps:    On a fresh enrollment, before issuing any
                `set_recording_mode` command, observe.
      Expect:   `pi$ ls /var/ghostcam/segments/` is empty after
                30 s. No S3 PUTs in MinIO/Tigris bucket. Live WS
                still active (Phase 5 covers that).
      On fail:  If segments are appearing, the default is wrong.
                Check `/var/ghostcam/recording_mode` doesn't exist
                and `GHOSTCAM_RECORDING_MODE` isn't in `/etc/ghostcam/env`.

- [ ] **4.2 set_recording_mode=constant**
      Steps:    1. In the viewer's camera settings, set Recording
                   Mode to `constant`.
                2. Wait for camera to receive the command on the
                   next telemetry poll (≤ 10 s).
                3. Camera process exits and systemd restarts it.
      Expect:   `/var/ghostcam/recording_mode` contains `constant`.
                `seg00000.ts` appears in `/var/ghostcam/segments/`
                within 10 s of restart, then a new segment every
                ~6 s. Each segment uploads to S3 and the local copy
                is deleted.
      On fail:  Check `journalctl` for ffmpeg errors. Check
                `df -h /var` for disk space.

- [ ] **4.3 set_recording_mode=motion**
      Steps:    Switch to motion mode in the UI. Wait 30 s of
                stillness (cover the lens with something static),
                then create motion (wave a hand for 6 s).
      Expect:   During stillness: segments still produced locally
                but `has_motion=false` in confirms. The viewer's
                timeline either omits them or marks them as
                no-motion. After the wave, the next segment carries
                `has_motion=true`.
      On fail:  ffprobe missing on PATH → falls back to file-size
                heuristic (less accurate). `pi$ which ffprobe`.

- [ ] **4.4 Audio capture on-segment (AAC)**
      Steps:    With recording_mode=constant, download a segment from
                MinIO/Tigris and inspect.
                `dev$ ffprobe -v quiet -show_streams ./seg00010.ts`
      Expect:   Two streams: `codec_name=h264` (video) and
                `codec_name=aac` (audio).
      On fail:  If audio stream is absent, `cfg.no_audio` was true
                or ALSA capture failed. Check `pi$ arecord -l` for
                the I2S mic.

- [ ] **4.5 Audio capture on-live (Opus)**
      Steps:    Open viewer's live tile. Speak into the mic.
                The viewer plays Opus over WebRTC via the SFU.
      Expect:   Audio is audible with sub-second latency.
      On fail:  Check the OGG/Opus side-channel: `pi$ journalctl |
                grep -i opus`. The fd-passing pattern documented in
                `camera/capture.py` (Spike 2 — `pipe:{wfd}`) is
                load-bearing here.

- [ ] **4.6 set_resolution=zero2w/480p**
      Steps:    Issue `set_resolution=zero2w` from the UI.
      Expect:   Camera restarts with 854×480 segments at lower bitrate.
                `pi$ ffprobe seg00100.ts` shows `coded_width=854`.
      On fail:  Check `/var/ghostcam/resolution` was written.

---

## Phase 5 — Live WebRTC streaming

Goal: viewer-driven on-demand live streaming works with sub-second
latency and tears down cleanly.

- [ ] **5.1 First viewer arrival**
      Steps:    Open viewer's live tile. Click play.
      Expect:   "LIVE" badge (not "DELAYED"). Frame visible within
                2 s. Audio audible if mic configured. Camera logs:
                "live relay: viewer connected, starting stream".
      On fail:  WebRTC ICE failure — check `GHOSTCAM_PUBLIC_IP` is
                reachable on UDP 50000–50200 from the viewer.

- [ ] **5.2 Stream stops when viewer leaves**
      Steps:    Close the viewer tab. Watch camera logs.
      Expect:   "live relay: no viewers, stopping stream" within
                ~5 s. Camera stops sending frames over the WS but
                keeps the WS connected.
      On fail:  Frames still flowing means the server isn't sending
                `stop_stream` JSON.

- [ ] **5.3 WS reconnects on server restart**
      Steps:    Restart the Go server (or kill + wait for it to
                come back).
      Expect:   Camera logs "live relay reconnecting in N s",
                eventually reconnects with backoff capped at 30 s.
      On fail:  Reconnect storm — check `live_ws.run_live_relay`
                backoff ladder.

- [ ] **5.4 Concurrent recording + live**
      Steps:    With recording_mode=constant, open the live tile
                and keep it open for 60 s.
      Expect:   Both run simultaneously: segments still uploading
                AND live frames flowing. Upload bandwidth roughly
                doubles during this window.

- [ ] **5.5 HLS fallback when WebRTC unavailable**
      Steps:    Block UDP 50000–50200 from viewer to server. Open
                live tile.
      Expect:   "DELAYED" badge appears (HLS fallback). 5–10 s
                latency expected. Audio still works (in the AAC
                segments).
      On fail:  If neither WebRTC nor HLS works, recording is broken.
                Go back to Phase 4.

---

## Phase 6 — VOD / timeline / clips

- [ ] **6.1 Timeline scrubbing**
      Steps:    Click into a recorded camera, scrub the timeline.
      Expect:   Frames render at scrub position within ~1 s of the
                seek. Coverage bars match what the camera uploaded.

- [ ] **6.2 Clip export**
      Steps:    Select a 10-second clip in the timeline, export.
      Expect:   `.mp4` downloads. Plays in any browser.

- [ ] **6.3 Footage deletion**
      Steps:    From camera settings, "Delete footage" with a
                from/to range.
      Expect:   The viewer issues `DELETE /api/v1/cameras/:id/footage`
                in batches until `has_more=false`. Coverage bars
                disappear; corresponding S3 keys are gone from
                MinIO/Tigris.

---

## Phase 7 — Server-issued commands

For each command, verify the camera honours it on the next telemetry
poll (≤ 10 s) and the resulting state is correct.

- [ ] **7.1 reboot**
      Trigger:  Issue `reboot` from viewer.
      Expect:   Camera process exits cleanly. systemd restarts it
                within 5 s. Telemetry resumes.

- [ ] **7.2 unregister**
      Trigger:  Issue `unregister` from viewer.
      Expect:   Camera clears `server_url`, exits. On restart it's
                in provisioning mode (no credentials). Identity
                preserved.

- [ ] **7.3 set_recording_mode** (covered in 4.2/4.3).

- [ ] **7.4 set_resolution** (covered in 4.6).

- [ ] **7.5 network_config**
      Trigger:  Issue `network_config` with a valid SSID + PSK.
      Expect:   Camera calls `nmcli` to add+activate the connection.
                On the next reboot, that network is the default.
      On fail:  `pi$ nmcli connection show` to inspect added
                connections.

- [ ] **7.6 update_firmware**
      Trigger:  Issue `update_firmware` (typically only after a
                release.yml run published a newer .deb).
      Expect:   Camera downloads `staged-update.deb`, sha256 verifies
                it, exits. systemd's ExecStartPre runs `dpkg -i`
                on next start. New version reports in telemetry.
      On fail:  See Phase 11.

- [ ] **7.7 Unknown command type**
      Trigger:  POST a synthetic `{"type":"made_up"}` to the
                command queue (manual DB insert or via admin tool).
      Expect:   Camera logs "unknown command type: made_up" and
                continues. No crash, no exit.

---

## Phase 8 — WiFi + cellular network handling

Goal: the camera survives realistic network transitions, especially
on a cellular-failover Pi.

- [ ] **8.1 WiFi-only happy path** (covered in 1.x).

- [ ] **8.2 WiFi drop with no cellular**
      Steps:    `dev$ ./scripts/pi.sh wifi-off 60`
      Expect:   For 60 s: camera logs presign failures, capture
                pauses, no crash. After 60 s: WiFi returns,
                pending confirms flush, capture resumes.
      On fail:  If the camera crashes or fills disk (no eviction
                during outage), there's a bug in the upload-paused
                path.

- [ ] **8.3 WiFi → cellular failover** (requires SIM7600)
      Steps:    `dev$ ./scripts/pi.sh wifi-off 120`. Watch for
                cellular taking over.
      Expect:   Default route shifts to `wwan0` (or equivalent)
                within ~30 s. Telemetry POSTs continue (slower).
                Live streaming may degrade in quality but doesn't
                disconnect.
      On fail:  `pi$ mmcli -L` to confirm the modem is detected;
                `pi$ ip route` to confirm the cellular default route.

- [ ] **8.4 Cellular → WiFi recovery**
      Steps:    Continuation of 8.3: WiFi comes back at the 120 s
                mark.
      Expect:   Default route migrates back to `wlan0`. The
                cellular dispatcher script (`99-keep-cellular-route`)
                preserves the cellular interface as a backup; it
                isn't torn down.

- [ ] **8.5 No connectivity check confusion**
      Steps:    Check `pi$ nmcli general status`.
      Expect:   `CONNECTIVITY` is `none` or `unknown` (NetworkManager's
                `no-connectivity-check.conf` disables the captive-portal
                probe so it doesn't false-positive over cellular).

---

## Phase 9 — Storage cap + retention

- [ ] **9.1 Local storage cap eviction**
      Steps:    Set `GHOSTCAM_LOCAL_STORAGE_CAP_MB=64` in
                `/etc/ghostcam/env`. Disconnect from the server
                (Phase 8.2 trick). Let segments accumulate.
      Expect:   Once total `.ts` size hits 64 MB, oldest files are
                evicted. `pi$ du -sh /var/ghostcam/segments/`
                stabilises near the cap.

- [ ] **9.2 Server storage cap (`storage_capped=true`)**
      Steps:    On the server side, push the user past their tier's
                storage limit. Camera continues recording.
      Expect:   Camera logs "storage capped by server, pausing
                uploads". Segments remain on disk (subject to local
                cap). When server-side storage frees, uploads resume
                and the backlog flushes.

- [ ] **9.3 Pending-confirm crash recovery**
      Steps:    1. Camera is uploading. `pi$ sudo kill -9
                   $(pidof ghostcam-camera)` mid-upload.
                2. systemd restarts.
      Expect:   `/var/ghostcam/pending_confirms.json` survives.
                On restart, the camera logs "resuming N pending
                upload confirmations" and confirms are sent on the
                next presign call.

---

## Phase 10 — Stability soak

- [ ] **10.1 Pi 4 — 1 hour soak**
      Steps:    recording_mode=constant, viewer connected, watch
                for 60 min.
      Expect:   No crashes, no leak. CPU well under 25 %, memory
                stable (no monotonic growth). Segment backlog is
                zero or near-zero.

- [ ] **10.2 Pi Zero 2W — 24 hour soak**
      This is the bar that decides whether the deferred Rust phase
      is mandatory.
      Steps:    recording_mode=motion, GPS antenna attached, viewer
                opened intermittently. Run for 24 h.
      Pass:     CPU < 50 % single-core average.
                Memory steady (RSS within 20 % of starting).
                No live-WS reconnect storm (look for backoff
                ladders maxing at 30 s — fine; rapid 2-s reconnects
                = problem).
                No segment backlog older than 5 minutes.
                No process restarts visible in `journalctl`
                (a clean run shows the same PID throughout).
      Fail policy: If any of the above fails, the Rust+pyo3 phase
      (`_native.find_nal_boundaries` swap) is mandatory before
      shipping a Zero 2W image.

---

## Phase 11 — Firmware self-update + rollback

Requires two .deb versions in the GitHub release:
`ghostcam-camera_0.1.0_arm64.deb` and a newer one.

- [ ] **11.1 Successful update**
      Steps:    Camera is running `0.1.0`. New `.deb` published.
                Issue `update_firmware` from viewer (or wait for the
                check-on-startup path).
      Expect:   `pi$ ls /var/ghostcam/staged-update.deb` after
                download. sha256 line in logs. systemd restarts.
                `dpkg -i` runs in ExecStartPre. New version logged.
      On fail:  sha256 mismatch → camera logs and discards. Check
                the release artifact's published sha256 matches.

- [ ] **11.2 Rollback on crash-loop**
      Steps:    Push a deliberately-broken `.deb` (e.g. a wheel
                that segfaults on import). Issue `update_firmware`.
      Expect:   New binary exits before writing `boot_ok`. systemd
                restarts → ExecStartPre sees missing `boot_ok` and
                a `.prev` backup → restores `.prev`. Old version
                runs again.
      On fail:  If the broken binary keeps restarting in a loop
                (no rollback), the ExecStartPre script in
                `pi/systemd/ghostcam-camera.service` is wrong.

- [ ] **11.3 Hash mismatch refusal**
      Steps:    Manipulate the release's sha256 to be wrong. Issue
                `update_firmware`.
      Expect:   Camera logs "firmware hash mismatch" and discards
                the staged file. No restart. Old version continues.

---

## Phase 12 — Crash recovery

- [ ] **12.1 ffmpeg crash**
      Steps:    `pi$ sudo pkill -9 ffmpeg`
      Expect:   Capture pipeline detects, logs "ffmpeg exited",
                restarts with exponential backoff (1 s, 2 s, …,
                cap 30 s). Stable after 5 minutes resets backoff
                to 1 s.

- [ ] **12.2 rpicam-vid crash**
      Steps:    `pi$ sudo pkill -9 rpicam-vid`
      Expect:   Same as 12.1. Crash counter increments; if it
                exceeds the threshold (5 in 5 min) you'll see
                "capture pipeline unstable" warnings.

- [ ] **12.3 Segment dir corruption**
      Steps:    `pi$ sudo dd if=/dev/urandom of=/var/ghostcam/segments/seg99999.ts bs=1024 count=1`
      Expect:   Watcher logs "skipping corrupt/partial segment:
                seg99999.ts" on next scan. File stays on disk
                (eventually evicted by the cap).

- [ ] **12.4 Disk full**
      Steps:    Fill `/var` to 99 %. Continue recording.
      Expect:   ffmpeg may fail to write, but the camera survives
                with crash-recovery + backoff. Storage cap eviction
                kicks in (Phase 9.1).

---

## Sign-off

Every box ticked above on the indicated hardware → cutover is safe.
The cutover commit can then `rm -rf legacy_camera/`, replace
`release.yml`'s Go camera build with the wheel + Python `.deb`, and
ship.

Document the result of each soak run with:

  * Hardware (Pi 4 / Pi 5 / Zero 2W).
  * Camera version (`ghostcam-camera --version`).
  * Server commit SHA.
  * Date and duration.
  * Any anomalies, even if they didn't fail a check.
