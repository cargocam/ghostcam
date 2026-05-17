# Ghostcam

The camera daemon, wire-contract types, and Pi packaging for [Ghostcam](https://ghostcam.fly.dev) — a Raspberry Pi surveillance system.

This repo is the camera-side stack: the Go daemon that runs on the Pi, the shared Go types it uses to talk to a server, and the Debian / image-build glue that turns a fresh PiOS install into a working camera. The hosted server at `ghostcam.fly.dev` accepts these cameras out of the box, and the wire contract under `common/` is documented well enough to talk to your own server instead if you want to self-host.

```
┌────────────────────────────────┐         ┌──────────────────────────┐
│ ghostcam (this repo)           │         │ Any server speaking the  │
│                                │  HTTPS  │ wire contract in common/ │
│  Pi camera daemon (Go)         │ ──────► │                          │
│   • rpicam-vid + ffmpeg capture│  WHIP   │  • presigns S3 segments  │
│   • fMP4 segments → S3         │ ──────► │  • accepts WHIP video    │
│   • pion WHIP → server         │         │  • verifies ed25519 sigs │
│   • ed25519 identity           │         │                          │
└────────────────────────────────┘         └──────────────────────────┘
```

## Quick install (Raspberry Pi)

Already have a Pi running PiOS Lite (bookworm, arm64)? One-liner against the hosted server:

```bash
curl -sSL https://ghostcam.fly.dev/install.sh | bash -s -- <pi-host> [<pi-user>]
```

This SSHes to the Pi, downloads the latest signed `.deb` from this repo's [releases](https://github.com/cargocam/ghostcam/releases), verifies the sha256, and `apt install`s it. The daemon comes up in provisioning mode and advertises a Bluetooth GATT service named `Ghostcam-<id>`; pair via the web app to enrol it.

Or flash a pre-built image — find the latest `.img.xz` for your device (zero2w / pi4 / pi5) on the [releases page](https://github.com/cargocam/ghostcam/releases) and write it with [Raspberry Pi Imager](https://www.raspberrypi.com/software/).

## Repo layout

| Path | What lives there |
|---|---|
| `camera/` | Go camera daemon. Single static `linux/arm64` binary. Cross-compiles with `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./camera`. Build tags `linux && !synthetic` for real Pi sensors, otherwise synthetic stubs for host-arch dev. |
| `common/` | Camera ↔ server wire-contract types. Other Go modules consume these via `github.com/cargocam/ghostcam/common`. |
| `pi/` | Pi-side glue: systemd units, Debian package scripts, udev / polkit / NetworkManager configs, rpi-image-gen layer for flashable `.img.xz` builds. |
| `tools/sigverify/` | Cross-language ed25519 signature parity harness (regression check). |
| `docker/` | Camera Docker entrypoints + a `pi-tools` operator container. |
| `Dockerfile` | Four camera-side stages: `camera-builder` (Go cross-compile), `camera` (synthetic-sensor runtime), `dummy-cameras` (manager that forks two synthetic cameras for demo use), `camera-prod` (real-sensor runtime). |
| `.github/workflows/` | `release.yml` (rolling pre-release `.deb`), `pi-images.yml` (manual `.img.xz` builds), `rpi-image-gen.yml` (base-image container builder), `ci.yml` (vet + build + test + reproducible-build gate). |

## Building from source

```bash
# Production binary (Pi target)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags="-s -w" -o ghostcam-camera ./camera

# Synthetic / dev (host-arch, no real sensors)
go build -tags synthetic -o ghostcam-camera-synthetic ./camera

# Tests
go test ./...
```

CI runs `go vet`, both build targets, the full test suite, and a reproducible-build gate that asserts byte-identical output across two consecutive builds with the same `-X main.Version=$SHA` ldflag.

## Wire contract

The camera and server speak HTTP for provisioning, telemetry, and segment-presign exchanges, and WHIP/WebRTC for live video. All payload types live under `common/`:

- `common.QRPayload` — provisioning payload delivered via QR scan or BLE GATT write
- `common.TelemetryEntry` — 10-second heartbeat with GPS, battery, modem RAT, etc.
- `common.ProvisionRequest` / `ProvisionResponse` — initial enrolment exchange
- `common.PresignedURLsResponse` — server-issued S3 upload URLs for HLS segments

Public types are documented inline with the rationale for each field and how it's used at runtime.

## Talking to your own server

The hosted server at `ghostcam.fly.dev` is the easy path; if you want to point the camera elsewhere, set `GHOSTCAM_SERVER_URL` in `/etc/ghostcam/env` (or supply it via the BT-onboarding payload), and your server needs to:

- Implement the HTTP endpoints documented by the types in `common/`.
- Provision S3-compatible storage (Tigris, MinIO, R2) and hand the camera presigned URLs.
- Run a WHEP endpoint for live viewing if you want a UI.
- Verify ed25519 signatures on every authenticated request (the camera signs its `device_id` + a nonce with the keypair from `/var/ghostcam/identity_key`).

## License

[Apache License 2.0](LICENSE).

## Contributing

PRs welcome — especially Pi hardware fixes, build-system improvements, and additional `pi/image/config/` device profiles. Larger features are best discussed in an issue first.
