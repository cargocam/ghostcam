# CLAUDE.md — Ghostcam Camera Development Guide

## What is this project?

This repo contains the open-source camera-side stack for [Ghostcam](https://ghostcam.fly.dev): a Go daemon that runs on a Raspberry Pi, the shared wire-contract types it uses to talk to a server, and the Debian / image-build glue that produces installable artefacts. The daemon captures H.264 + Opus via `rpicam-vid | ffmpeg`, uploads fMP4 HLS segments to S3 via server-presigned URLs, publishes live A/V via WHIP/WebRTC, and POSTs telemetry every 10 s. There's no server in this repo — the camera talks to one over HTTPS using the contract documented in `common/`.

The camera was Python from 2026-05-12 to 2026-05-14 (the `go-camera-rewrite` cutover). The Python port unblocked iteration on telemetry/upload/motion code but the live-relay slice (NAL framing + WebSocket transport) wanted to stay in Go, and once WHIP/pion was the chosen live wire format, Python was no longer earning its keep.

## Documentation Policy

When making changes to the codebase, **always update the relevant README and CLAUDE.md** to reflect those changes.

## Repository Layout

```
ghostcam/
├── camera/             Go camera daemon (package main). Single static linux/arm64
│   │                   binary; cross-compiled by release.yml. No cgo, no runtime
│   │                   deps beyond ffmpeg + rpicam-vid on the Pi.
│   ├── capture.go         ffmpeg orchestrator. rpicam-vid → tee → fMP4 segments +
│   │                      WHIP fanout. Synthetic mode uses testsrc2+sine.
│   ├── publisher.go       pion v4 WHIP client. Reads H.264 + OGG-Opus from named
│   │                      pipes, packetizes via media.Sample, posts to the
│   │                      configured server's WHIP endpoint. Multi-slice access
│   │                      units are coalesced via `first_mb_in_slice` detection.
│   ├── abr.go             Adaptive bitrate controller (opt-in via --abr). Samples
│   │                      pion's outbound packet loss, ratchets a 4-tier ladder
│   │                      (854×480 500k ↔ 1920×1080 4M) with fast-down /
│   │                      slow-up / cooldown, then trips requestPipelineRestart
│   │                      so capture respawns rpicam-vid at the new tier.
│   ├── firmware_stability.go Two-gate rollback (#106). After install, daemon
│   │                      touches /var/ghostcam/boot_ok on first healthy boot
│   │                      and increments healthy_minutes each minute thereafter;
│   │                      ExecStartPre rolls back to ghostcam-camera.prev when
│   │                      either gate is unmet after a fresh install.
│   ├── power_mode.go      Three power modes. `live` = always on; `standby` =
│   │                      capture runs but WHIP publisher only opens on a viewer
│   │                      Redis flag (saves ~50% cellular at idle); `sleep` =
│   │                      capture suspended, telemetry every 5 min for wake.
│   ├── battery_rules.go   Level-triggered evaluation of operator-supplied rules
│   │                      (lowest-threshold-wins) layered over the manual power
│   │                      mode.
│   ├── battery.go         BatteryReader interface + no-op default. Real drivers
│   │                      are registered at startup based on the
│   │                      GHOSTCAM_BATTERY_HAT env var.
│   ├── battery_pisugar_linux.go  PiSugar 3 / 3 Plus driver. Reads register 0x2A
│   │                      over /dev/i2c-1 at slave 0x57, polled every 30 s;
│   │                      cached %% feeds telemetry's battery_pct and the
│   │                      battery_rules evaluator.
│   ├── bt_onboarding_linux.go  GATT peripheral. Advertises `Ghostcam-<8hex>`,
│   │                      accepts the same provisioning JSON as the QR path.
│   │                      Raced with ScanQR in provisioning.go.
│   ├── sim_imsi_linux.go  Reads modem IMSI via `mmcli -m 0`, threads through
│   │                      ProvisionRequest so the server knows which SIM is
│   │                      bound to which camera.
│   ├── network_linux.go   nmcli wrapper. Connects WiFi from the BT-onboarding
│   │                      payload, sets autoconnect-retries=0 on every created
│   │                      connection so a single WPA rekey hiccup can't brick
│   │                      a headless wifi-only Pi.
│   └── sensors_*.go       Build-tag-gated. Real sensors on linux && !synthetic;
│   └── network_*.go       host-arch stubs everywhere else.
│   └── qr_*.go
├── common/             Shared Go types. Camera↔server contract: TelemetryEntry,
│                       QRPayload, ProvisionRequest/Response, PresignedURLs,
│                       CameraCommand, etc. The single source of truth for the
│                       wire. Other modules import as
│                       `github.com/cargocam/ghostcam/common`.
├── pi/                 Pi-side install glue.
│   ├── systemd/           ghostcam-camera.service unit
│   ├── debian/            postinst, prerm, udev rules, polkit rule (grants
│   │                      netdev blanket NM access so unattended BT-onboarded
│   │                      WiFi config works without an admin prompt)
│   └── image/             rpi-image-gen layer + firstboot script. Produces
│                          .img.xz artefacts via pi-images.yml.
├── scripts/            Developer tools.
│   ├── pi                 Thin docker wrapper around pi.sh — runs it
│   │                      inside the pi-tools image so the host doesn't
│   │                      need sshpass / rsync / Go installed.
│   └── pi.sh              Camera-manager CLI: cross-compile the daemon,
│                          scp to a real Pi, deploy/logs/status/etc.
│                          Reads .pi.env for PI_HOST / PI_USER / PI_PASSWORD.
├── docker/             Camera Docker entrypoints + pi-tools operator
│                       container (built by scripts/pi).
├── Dockerfile          Four stages: camera-builder, camera (synthetic),
│                       dummy-cameras (forks 2 synthetic cameras), camera-prod.
└── .github/workflows/  ci.yml, release.yml, pi-images.yml, rpi-image-gen.yml
```

## Build & Run

```bash
# Production binary (Pi target)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags="-s -w" -o ghostcam-camera ./camera

# Synthetic / dev (host-arch, no real sensors)
go build -tags synthetic -o ghostcam-camera-synthetic ./camera

# Tests
go test ./...
```

`-trimpath` + `-ldflags="-s -w"` is the same flag set release.yml uses; CI's reproducible-build gate asserts these produce byte-identical output across two consecutive builds. Adding non-deterministic state to the daemon (e.g., capturing a build timestamp via `init()`) will break that gate.

### Build tags

Two camera-side build tags select compile-time variants:

- `linux && !synthetic` — production. Real sensors (`sensors_linux.go`), real network (`network_linux.go`), real QR scan via `rpicam-still` (`qr_linux.go`), real Bluetooth peripheral (`bt_onboarding_linux.go`).
- Anything else (including `linux` with `-tags synthetic`) — synthetic. Test source via ffmpeg's `testsrc2` + `sine`, stub network, no QR, no BT.

The pattern across `camera/sensors_*.go`, `camera/network_*.go`, `camera/qr_*.go`, `camera/bt_onboarding_*.go`, `camera/battery_pisugar_*.go` is: one `_linux.go` file with the real impl behind `//go:build linux && !synthetic`, one `_other.go` with a no-op stub behind `//go:build !linux || synthetic`.

## Release flow

`release.yml` fires on every push to `main`:

1. Computes the next version: highest existing bare-semver tag (`vX.Y.Z`) + patch bump. No existing tag → seeds at `v0.1.0`.
2. Cross-compiles the camera binary for `linux/arm64` with `-X main.Version=$NEXT` (e.g. `v0.1.8`).
3. Generates a CycloneDX SBOM via `cyclonedx-gomod app`.
4. Packages the binary into a `.deb` named `ghostcam-camera_arm64.deb` with `Version: 0.1.8` (bare semver, no tilde/build-metadata suffix).
5. Tags HEAD with `$NEXT` (no force-move) and creates a fresh GitHub release with the .deb, SBOM, raw binary, and checksums.
6. The downstream server has a `release.published` webhook that reads the tag name directly from the event payload; cameras pull updates via that server's `/api/v1/firmware/latest` endpoint.

**Manual minor/major bumps**: operator runs locally `git tag v0.2.0 <sha> && git push --tags`, then the next push to main picks `v0.2.1`. There's no commit-message parsing; deliberate bumps go through git.

`pi-images.yml` is **manual** — `workflow_dispatch` triggered by an operator who passes a release tag. Builds `.img.xz` via rpi-image-gen for the requested device profiles (currently `zero2w`).

`rpi-image-gen.yml` builds the rpi-image-gen container image (used by pi-images.yml). Runs on the 1st of each month or manual dispatch.

## How the camera knows where to send data

1. **Operator-provided env**: `GHOSTCAM_SERVER_URL` in `/etc/ghostcam/env` (or `/etc/ghostcam/env.d/*.conf`). Set once by `pi-setup.sh`.
2. **Provisioning payload**: when the daemon has no `server_url` file, it enters provisioning mode and accepts a `QRPayload` (containing `server` + `token`) via either a QR code scanned by `rpicam-still` or a BT-GATT write to the advertised `Ghostcam-<id>` service. The payload also carries optional `wifi_ssid`/`wifi_password` for cellular-only cameras that want to drop onto WiFi at install time.

Once provisioned, the daemon signs every authenticated request with the ed25519 key at `/var/ghostcam/identity_key`. `device_id` is deterministic: SHA-256 of the public key, first 16 bytes hex. So a re-provisioned camera shows up as the same device on the same server.

### Cellular data uplink (APN)

Nothing in the stack creates a NetworkManager mobile-broadband connection on its own, so a SIM whose APN isn't in ModemManager's provider database will *enable* the modem but never *connect* a data bearer — the camera then has no cellular uplink despite healthy hardware. Set `GHOSTCAM_CELLULAR_APN` (env in `/etc/ghostcam/env`, `--cellular-apn`, or `cellular_apn` in the TOML config; optional `GHOSTCAM_CELLULAR_USER`/`GHOSTCAM_CELLULAR_PASS` for PAP/CHAP APNs) and the daemon provisions a `gsm` connection named `ghostcam-cellular` at startup (autoconnect on, infinite retries), via `network.EnsureCellular`. It runs as the non-root `ghostcam` user — permitted by the `netdev` polkit rule that already backs the WiFi path — so the fix rides firmware OTA in the binary, not just a reflash. Idempotent, and it won't clobber a pre-existing gsm connection the image/operator set up. Empty APN leaves cellular entirely to MM/NM auto-config. Check with `nmcli -t -f NAME,TYPE,STATE connection show` (look for `ghostcam-cellular:gsm:activated`) and `mmcli -m 0` (`state: connected` + a bearer with an IP).

The APN can also be delivered without touching the device's env, three ways that all funnel through `network.EnsureCellular` + `state.PersistCellular` (persisted to `{dataDir}/cellular_apn` so it survives reboots via the LoadConfig stored-file layer): (1) the `set_cellular` `CameraCommand` — server pushes it on a telemetry poll, so it reaches **already-deployed** cameras over the air; (2) the onboarding `QRPayload` fields `ca`/`cu`/`cp` — a cellular-only camera gets its APN at provisioning time alongside optional WiFi creds; (3) the env/flag/TOML above. Precedence at boot: CLI/env/TOML → stored file. EIOTCLUB SIMs (BICS backbone, roam AT&T+T-Mobile) use APN `america.bics`, no user/pass.

## Code Conventions

### Go

- `log/slog` for structured logging. Use `slog.Info` / `slog.Warn` / `slog.Error` with key-value attrs, not `Printf`. Examples: `slog.Info("rpicam-vid started", "pid", cmd.Process.Pid)`.
- Wrap errors via `fmt.Errorf("context: %w", err)`. The `%w` is load-bearing — many places use `errors.Is` / `errors.As` on the chain.
- `sync/atomic` for single-value cross-goroutine flags (e.g., `manualPowerMode`, `currentPowerMode`).
- Channels for goroutine-to-goroutine messaging that needs backpressure (`segmentCh`, `requestPipelineRestart`).
- Comments explain *why*, not *what*. Especially load-bearing: rollback gating logic, polkit/netdev hacks, the .deb Version `+sha` trick.

## Dependencies

External Go deps the camera relies on:

- `github.com/pion/webrtc/v4`, `pion/rtp`, `pion/rtcp` — WHIP publisher.
- `tinygo.org/x/bluetooth` — BLE GATT peripheral on Linux (uses BlueZ over D-Bus).
- `github.com/godbus/dbus/v5` — direct BlueZ D-Bus calls the tinygo API doesn't expose (powering on `hci0` before advertising, reading advertising-manager state for onboarding diagnostics).
- `github.com/makiuchi-d/gozxing` — QR decode (synthetic mode uses this; real mode currently relies on libzbar via rpicam-still glue but gozxing is the planned migration).
- `github.com/BurntSushi/toml` — config parsing.
- `github.com/google/uuid` — segment IDs.
- Standard library for everything else (HTTP, ed25519, JSON, slog).

Pi-side runtime deps (declared in the `.deb`'s `Depends:`):

- `ffmpeg`, `rpicam-apps`, `alsa-utils` (capture)
- `network-manager`, `wpasupplicant`, `modemmanager`, `libqmi-utils`, `usb-modeswitch` (connectivity)
- `gpsd`, `gpsd-clients` (GPS)
- `bluez`, `rfkill` (Bluetooth)
- `libzbar0`, `ca-certificates`, `adduser`, `init-system-helpers` (misc)

## Where issues + releases live

- Issues: https://github.com/cargocam/ghostcam/issues
- Releases (.deb + .img.xz): https://github.com/cargocam/ghostcam/releases
- Camera-firmware webhook source for the hosted server: the `release.published` event on this repo.

## Common debugging entry points

- **Service status on Pi**: `systemctl status ghostcam-camera`, `journalctl -u ghostcam-camera -f`.
- **Identity check**: `/var/ghostcam/identity_key` (private, mode 0600), `/var/ghostcam/identity_key.pub` (hex, mode 0644). Device ID = `sha256(identity_key.pub_bytes)[:16]` in hex.
- **What's the running version**: `dpkg-query -W -f='${Version}\n' ghostcam-camera` for the apt-level version, or `journalctl -u ghostcam-camera | grep 'camera identity'` for the daemon's own `main.Version` baked at build time.
- **WHIP not connecting**: check `journalctl -u ghostcam-camera | grep -iE "WHIP|publisher|state"`. Common causes: server URL unreachable, ed25519 signature mismatch (regenerated identity), or the WHIP endpoint returned 4xx.
- **BT onboarding: Pi doesn't show up as an available device**: the daemon only advertises the `Ghostcam-<id>` GATT peripheral while in provisioning mode (no `server_url`/token yet). It powers on the BlueZ controller itself before advertising — `ScanBT` sets `org.bluez.Adapter1.Powered=true` on `hci0` over D-Bus, because `rfkill unblock` alone leaves the controller un-powered and tinygo's `Advertisement.Start()` doesn't check/power it (unlike Scan/Connect). Check `journalctl -u ghostcam-camera | grep -iE "BT|adapter|advertis"` and `bluetoothctl show` (look for `Powered: yes`). `ghostcam` must be in the `bluetooth` group (deb postinst handles this) for the D-Bus power-on to be permitted.
- **Segment uploads stuck**: `ls -la /var/ghostcam/segments/` and `journalctl -u ghostcam-camera | grep -iE "presign|upload"`. The daemon writes segments to disk first, then uploads asynchronously — if disk fills up, the OS evicts old segments first.
- **No GPS fix (`cgps` shows NA)**: GPS on the SIM7600 is gated on the cellular modem — the GNSS engine is powered by ModemManager and NMEA only appears on `/dev/ttyUSB1` once the modem reaches `enabled`. Bring-up is a oneshot, `ghostcam-gps.service` → `/usr/local/bin/ghostcam-enable-gps.sh` (shipped in the `.deb` as of the GPS-self-update fix, so field cameras get changes via firmware update, not just reflash; `gpsd.conf`/`/etc/default/gpsd` stays image-side to avoid a dpkg conffile clash with the `gpsd` package). Check in order: `mmcli -m 0` (modem `state:` and the `ports:` line — the GPS port is tagged `(gps)`), `mmcli -m 0 --location-status` (want `gps-nmea`/`gps-raw` enabled), then `gpspipe -w -n 5` — if its `devices` list is empty, **gpsd never attached the port**. That's the classic failure: gpsd (`START_DAEMON`) starts before the modem finishes USB enumeration, can't open `/dev/ttyUSB1`, and with `USBAUTO="false"` never retries. `ghostcam-enable-gps.sh` fixes it by running `gpsdctl add /dev/ttyUSB1` after ModemManager is up; to recover a stuck unit by hand: `sudo gpsdctl add /dev/ttyUSB1` (or `sudo systemctl restart ghostcam-gps`). `journalctl -u ghostcam-gps -b` shows why boot-time bring-up failed. Note gpsd ≥ 3.20 dropped `cgps -a` — use plain `cgps`.

  **Durable OTA fallback (in the binary):** the shell oneshot only ships in the image/.deb, and firmware OTA swaps *just the daemon binary* (ExecStartPre extracts `/usr/local/bin/ghostcam-camera` and never runs the .deb's postinst), so script fixes never reach already-deployed cameras. To make the *attach* half self-heal over OTA, the daemon has a built-in fallback (`camera/internal/sensors/nmea_reader_linux.go` + `nmea.go`): it watches gpsd's WATCH-stream `DEVICES` messages and, **whenever gpsd has zero devices attached**, opens `/dev/ttyUSB1` directly (the udev rule ships it `MODE=0666`, so the non-root `ghostcam` daemon can read it) and parses NMEA (`$--GGA`/`$--GSA`) straight into the same fix cache `gpsdQuery` serves. The two never fight over the tty — the direct reader stands down the moment gpsd reports a device. This can't *power* the GNSS engine (that still needs the root `mmcli --location-enable`, which is why the oneshot stays in fresh images), but it recovers the common field failure where the engine is on and only gpsd's attach is missing. Look for `gps: reading NMEA directly (gpsd has no device)` in the daemon log.

If a Pi is offline after a routine WPA rekey, the fix landed in 2026-05-17: `connection.autoconnect-retries=0` on every NM connection. Old `.deb`s without this are vulnerable; apt-upgrade to the latest first if you suspect a stuck wifi state.
