# QR Code Enrollment — Design Document

## Overview

Add QR code scanning as an enrollment method so cameras can be paired with a server by simply pointing the camera at a QR code displayed in the web UI or printed. This eliminates the need to manually pass enrollment JWTs via environment variables or CLI flags.

## Prerequisites

- Real video capture must be implemented first (rpicam-vid) — QR scanning uses raw camera frames
- Server enrollment API already exists and issues JWTs

## User Flow

### Web UI (Server Side)

1. Admin navigates to "Add Camera" in the UI
2. Server generates an enrollment JWT (short-lived, ~5 minutes)
3. UI displays a QR code containing the enrollment payload
4. Admin points the camera at the screen (or prints the QR code for field deployment)

### Camera Side

1. Camera boots without enrollment (`user.crt` doesn't exist)
2. Camera enters **enrollment mode**: captures raw frames and scans for QR codes
3. QR code detected → decode payload → extract server address + enrollment JWT
4. Camera connects to server, completes enrollment handshake (existing `enrollment.rs` flow)
5. Camera stores certs + server address, exits enrollment mode, begins normal operation

## QR Code Payload

JSON encoded, kept small for reliable scanning:

```json
{
  "s": "https://cam.example.com:3000",
  "t": "eyJhbGciOiJIUzI1NiI..."
}
```

| Field | Description |
|-------|-------------|
| `s` | Server HTTP base URL (for firmware check + enrollment QUIC address derivation) |
| `t` | Enrollment JWT (same token as `--enrollment-jwt` CLI flag) |

The server's QUIC address is derived from the HTTP URL: same host, port from `GHOSTCAM_QUIC_PORT` (encoded in the JWT claims, same as today).

Total payload: ~200-400 bytes, fits comfortably in a QR code version 10-15 (easily scannable from a phone screen or printout).

## Camera-Side Implementation

### Enrollment Mode Detection

On startup, if no `user.crt` exists and no `--enrollment-jwt` is provided:

```rust
if !enrolled && cli.enrollment_jwt.is_none() {
    tracing::info!("no enrollment found — entering QR scan mode");
    match qr_enrollment::scan_and_enroll(&config).await {
        Ok(()) => tracing::info!("enrollment via QR complete"),
        Err(e) => {
            tracing::error!("QR enrollment failed: {e}");
            // Fall back to waiting for manual JWT
        }
    }
}
```

### QR Scanning Pipeline

QR scanning needs **raw video frames** (not H.264 encoded). This requires a different `rpicam-vid` invocation or using `rpicam-still` / `libcamera-still` for frame grabs:

```
rpicam-still \
  --width 640 \
  --height 480 \
  -n \
  -t 0 \
  --timelapse 500 \        # capture every 500ms
  --codec yuv420 \         # raw YUV (or use --codec png for simpler parsing)
  -o -                     # stdout
```

Alternative: use `rpicam-vid` with `--codec yuv420` for a continuous raw stream, but `rpicam-still` with timelapse is simpler for periodic frame grabs.

For QR decoding, use the `rqrr` crate (pure Rust, no C dependencies):

```rust
use rqrr::PreparedImage;
use image::GrayImage;

fn try_decode_qr(yuv_data: &[u8], width: u32, height: u32) -> Option<String> {
    // Y plane is the first width*height bytes of YUV420
    let gray = GrayImage::from_raw(width, height, yuv_data[..width * height].to_vec())?;
    let mut prepared = PreparedImage::prepare(gray);
    let grids = prepared.detect_grids();
    for grid in grids {
        if let Ok((_, content)) = grid.decode() {
            return Some(content);
        }
    }
    None
}
```

### Scan Loop

```rust
pub async fn scan_and_enroll(config: &CameraConfig) -> Result<()> {
    let mut child = Command::new("rpicam-still")
        .args(["--width", "640", "--height", "480", "-n", "-t", "0",
               "--timelapse", "500", "--codec", "yuv420", "-o", "-"])
        .stdout(Stdio::piped())
        .spawn()?;

    let stdout = child.stdout.take().unwrap();
    let frame_size = 640 * 480 * 3 / 2; // YUV420
    let mut buf = vec![0u8; frame_size];

    loop {
        // Read one complete frame
        stdout.read_exact(&mut buf).await?;

        if let Some(payload) = try_decode_qr(&buf, 640, 480) {
            child.kill().await?;

            let qr: QrPayload = serde_json::from_str(&payload)
                .context("invalid QR payload")?;

            // Derive server QUIC address from HTTP URL
            // Use existing enrollment flow
            enrollment::enroll_with_jwt(&qr.t, &qr.s, config).await?;
            return Ok(());
        }
    }
}
```

### Timeout and Fallback

- QR scanning runs for up to 5 minutes
- If no QR code is detected, the camera logs an error and waits for manual enrollment (`--enrollment-jwt` on next restart)
- A physical button press (GPIO, future enhancement) could re-trigger QR scan mode

### LED/Status Indication

During enrollment mode, the camera should indicate it's waiting for a QR code. For v1, this is log output only. Future: GPIO LED blinking pattern (e.g., slow blink = scanning, solid = enrolled).

## Server-Side Implementation

### Enrollment QR Generation

The server already generates enrollment JWTs. Add an endpoint that returns a QR code image:

```
GET /api/v1/cameras/enroll/qr
```

Returns SVG (or PNG) QR code containing the JSON payload. The enrollment JWT is embedded in the QR code and has a 5-minute expiry.

The web UI renders this QR code in the "Add Camera" dialog.

### UI Component

```svelte
<script>
  let qrSvg = $state('');
  async function generateQr() {
    const res = await fetch('/api/v1/cameras/enroll/qr');
    qrSvg = await res.text();
  }
</script>

{@html qrSvg}
```

Server-side QR generation using the `qrcode` crate:

```rust
use qrcode::{QrCode, render::svg};

fn generate_enrollment_qr(server_url: &str, jwt: &str) -> String {
    let payload = serde_json::json!({"s": server_url, "t": jwt});
    let code = QrCode::new(payload.to_string()).unwrap();
    code.render::<svg::Color>()
        .min_dimensions(200, 200)
        .build()
}
```

## Dependencies

### Camera (new)

```toml
rqrr = "0.7"      # QR code detection + decoding (pure Rust)
image = "0.25"     # GrayImage for QR decoder input
```

### Server (new)

```toml
qrcode = "0.14"    # QR code generation (SVG output)
```

## Enrollment Methods Summary (Post-Implementation)

| Method | Use Case | How |
|--------|----------|-----|
| **QR code** | Normal deployment | Point camera at QR from web UI |
| **JWT flag** | Scripted/Docker | `--enrollment-jwt <token>` or `GHOSTCAM_ENROLLMENT_JWT` |
| **Docker entrypoint** | Dev/test | Auto-enrolls via HTTP API (existing `docker-entrypoint.sh`) |

All three methods feed into the same `enrollment.rs` handshake — they only differ in how the JWT reaches the camera.
