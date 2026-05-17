# Ghostcam

The camera daemon, wire-contract types, and Pi packaging for [Ghostcam](https://ghostcam.fly.dev) — a Raspberry Pi surveillance system.

This repo contains the open-source camera-side stack. The server, viewer UI, and managed infra are private; the camera talks to them over a documented HTTP + WebRTC contract that you can re-implement against if you want to self-host the whole thing.

```
┌─────────────────────────────────┐         ┌─────────────────────────────┐
│ ghostcam (this repo)            │         │ ghostcam-server (private)   │
│                                 │  HTTPS  │                             │
│  Pi camera daemon (Go)          │ ──────► │  Go server + Svelte viewer  │
│   • rpicam-vid + ffmpeg capture │  WHIP   │  • Postgres / Redis / S3    │
│   • fMP4 segments → S3          │ ──────► │  • pion WebRTC SFU          │
│   • pion WHIP → server          │         │  • Stripe / Soracom         │
│   • ed25519 identity            │         │                             │
└─────────────────────────────────┘         └─────────────────────────────┘
```

## Quick install (Raspberry Pi)

Already have a Pi running PiOS Lite (bookworm, arm64)?

```bash
curl -sSL https://ghostcam.fly.dev/install.sh | bash -s -- <pi-host> [<pi-user>]
```

This SSHes to the Pi, downloads the latest signed `.deb` from this repo's [releases](https://github.com/cargocam/ghostcam/releases), verifies the sha256, and `apt install`s it. The daemon comes up in provisioning mode and advertises a Bluetooth GATT service named `Ghostcam-<id>`; pair via the web app to enrol it against the hosted server.

Or flash a pre-built image — find the latest `.img.xz` for your device (zero2w / pi4 / pi5) on the [releases page](https://github.com/cargocam/ghostcam/releases) and write it with [Raspberry Pi Imager](https://www.raspberrypi.com/software/).

## Repo layout

| Path | What lives there |
|---|---|
| `camera/` | Go camera daemon. Single static `linux/arm64` binary. Cross-compiles with `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./camera`. Build tags `linux && !synthetic` for real Pi sensors, otherwise synthetic stubs. |
| `common/` | Camera ↔ server wire-contract types. Imported by the server module as `github.com/cargocam/ghostcam/common`. |
| `pi/` | Pi-side glue: systemd units, Debian package scripts, udev / polkit / NetworkManager configs, rpi-image-gen layer for flashable `.img.xz` builds. |
| `tools/sigverify/` | Cross-language ed25519 signature parity harness (kept around as a regression check). |
| `docker/` | Camera Docker entrypoints + Pi-tools container used by the `pi-tools` profile in the server repo's docker-compose. |
| `.github/workflows/` | `release.yml` (rolling-pre-release `.deb`), `pi-images.yml` (manual `.img.xz` builds), `rpi-image-gen.yml` (base-image container). |

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

The camera tests live alongside the source under `camera/*_test.go`. CI runs `go vet`, both build targets, the full test suite, and a reproducible-build gate that asserts byte-identical output across two consecutive builds.

## Wire contract

The camera and server speak HTTP for provisioning, telemetry, and segment-presign exchanges, and WHIP/WebRTC for live video. All payload types live under `common/`:

- `common.QRPayload` — provisioning payload delivered via QR scan or BLE GATT write
- `common.TelemetryEntry` — 10-second heartbeat with GPS, battery, modem RAT, etc
- `common.ProvisionRequest` / `ProvisionResponse` — initial enrolment exchange
- `common.PresignedURLsResponse` — server-issued S3 upload URLs for HLS segments

Public types are documented inline with the rationale for each field and how it's used at runtime.

## Self-hosting the server

The hosted server at `ghostcam.fly.dev` is the easy path. If you want to run your own:

- Implement the HTTP endpoints documented in `common/` and serve `/install.sh` + `/pi-setup.sh` (the install script is templated server-side).
- Provision S3-compatible storage (Tigris, MinIO, R2).
- Run a pion WHEP endpoint for live viewing.
- Issue ed25519-signed bearer tokens accepted by the camera.

There's no plug-and-play OSS server in this repo. The Go server (and Svelte viewer) live in a private repo; reach out if you need access for a real integration.

## License

[Apache License 2.0](LICENSE).

## Contributing

PRs welcome — especially Pi hardware fixes, build-system improvements, and additional `pi/image/config/` device profiles. Larger features are best discussed in an issue first.
