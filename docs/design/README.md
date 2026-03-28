# Design Documents

Implementation designs for camera firmware features. Each doc covers architecture, implementation details, dependencies, and failure modes.

## Documents

| Document | Status | Description |
|----------|--------|-------------|
| [firmware-releases.md](firmware-releases.md) | Design complete | OTA updates via GitHub Releases, server webhook, staggered reboots |
| [video-capture.md](video-capture.md) | Design complete | Real video capture via rpicam-vid, ABR preset profiles |
| [audio-capture.md](audio-capture.md) | Design complete | Real audio capture via cpal + Opus encoding |
| [gps.md](gps.md) | Design complete | GPS via gpsd TCP client, SIM7600G-H modem |
| [cellular-failover.md](cellular-failover.md) | Design complete | WiFi→cellular failover, NetworkManager routing, network monitor |
| [qr-enrollment.md](qr-enrollment.md) | Design complete | QR code scanning for camera enrollment, rqrr decoder |
| [camera-manager.md](camera-manager.md) | Design complete | `scripts/pi.sh` CLI for managing Pi hardware over SSH |

## Implementation Order

```
1. Camera Manager (scripts/pi.sh)     — unblocks all hardware testing
2. Real Video Capture (rpicam-vid)     — core functionality, unblocks QR
3. Real Audio Capture (cpal + Opus)    — independent of video
4. GPS (gpsd client)                   — independent, straightforward
5. Cellular Failover (network monitor) — needs hardware for testing
6. QR Enrollment (rqrr)               — depends on video capture
7. Firmware Releases (OTA)             — depends on release workflow
```

Camera manager comes first because every subsequent feature needs real hardware to test.
