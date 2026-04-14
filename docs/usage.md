# Usage & Setup Guide

Everything you need to go from zero to a running camera you can watch in the browser — plus a walkthrough of the viewer.

- [Part 1: Camera Setup](#part-1-camera-setup) — flash, install, or run the camera binary
- [Part 2: Using the Viewer](#part-2-using-the-viewer) — log in, enroll cameras, playback, clips, billing

---

# Part 1: Camera Setup

There are three ways to set up a camera, from easiest to most flexible.

## Option 1: Flash a Pi Image (recommended for new hardware)

> **Easiest path:** log in to the web UI with no cameras enrolled and
> follow the built-in **Get Started** card — it picks the right Pi
> model, offers a presigned download of the image your server is
> hosting, renders the flashing instructions, and shows the enrollment
> QR code all in one place. The card watches for your camera to come
> online and dismisses itself automatically. Everything below is the
> manual equivalent.

Download the `.img.xz` for your Pi model — either from the Get Started
card (the server pulls images into S3 automatically on every GitHub
release via the `/api/v1/webhooks/github` webhook) or directly from the
[latest release](../../../releases/latest) — and flash it to an SD card:

```bash
# macOS
xzcat ghostcam-zero2w-v0.1.0.img.xz | sudo dd of=/dev/diskN bs=4M

# Linux
xzcat ghostcam-zero2w-v0.1.0.img.xz | sudo dd of=/dev/sdX bs=4M status=progress
```

The image comes pre-configured with all dependencies (ffmpeg, gpsd, modemmanager, ALSA, NetworkManager) and the camera service enabled. On first boot you can provision in two ways:

**QR code (recommended):** Generate a QR code from the web UI (see [Enrolling a Camera](#enrolling-a-camera)), including your WiFi credentials if the Pi isn't wired. Hold the QR code in front of the Pi camera. The camera scans it automatically on boot, joins WiFi if included, and provisions itself — no SSH needed.

**Manual (headless / no camera access):**
1. SSH in (user: `ghostcam`, password: `ghostcam`)
2. Set the server URL: `echo "GHOSTCAM_SERVER_URL=https://your-server.example.com" >> /etc/ghostcam/env`
3. Provision the camera from the web UI (generates a one-time token — see [Enrolling a Camera](#enrolling-a-camera) below), then write it: `echo "<token>" > /var/ghostcam/provision_token`
4. Restart: `sudo systemctl restart ghostcam-camera`

Either way, subsequent boots are automatic — credentials are persisted after the first successful provision.

## Option 2: Install the .deb Package (existing Pi with Raspberry Pi OS)

For Pis already running Raspberry Pi OS (Bookworm, arm64):

```bash
# Download and install
curl -LO https://github.com/<owner>/ghostcam/releases/latest/download/ghostcam-camera_<version>_arm64.deb
sudo dpkg -i ghostcam-camera_<version>_arm64.deb

# Install remaining dependencies
sudo apt install -y rpicam-apps gpsd gpsd-clients modemmanager alsa-utils

# Create data directory
sudo mkdir -p /var/ghostcam /etc/ghostcam
sudo chown $USER:$USER /var/ghostcam

# Configure environment
sudo tee /etc/ghostcam/env << EOF
GHOSTCAM_DATA_DIR=/var/ghostcam
GHOSTCAM_SERVER_URL=https://your-server.example.com
GHOSTCAM_VIDEO_PROFILE=pi4
EOF

# Install and enable the systemd service
sudo cp pi/systemd/ghostcam-camera.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now ghostcam-camera
```

Set `GHOSTCAM_VIDEO_PROFILE` to match your hardware: `zero2w` (480p), `pi4` (720p), or `pi5` (1080p).

Provision the camera the same way as Option 1 (write a provision token, or use `pi.sh setup` for full automated provisioning).

## Option 3: Deploy the Raw Binary (any Linux/arm64 or amd64)

Download the standalone binary from the [latest release](../../../releases/latest). This is useful for non-Pi Linux systems or custom setups:

```bash
curl -LO https://github.com/<owner>/ghostcam/releases/latest/download/ghostcam-camera-aarch64
chmod +x ghostcam-camera-aarch64
sudo mv ghostcam-camera-aarch64 /usr/local/bin/ghostcam-camera

# Requires ffmpeg on PATH
sudo apt install -y ffmpeg

# Create data directory
sudo mkdir -p /var/ghostcam
sudo chown $USER:$USER /var/ghostcam

# Run with environment variables
GHOSTCAM_SERVER_URL=https://your-server.example.com \
GHOSTCAM_DATA_DIR=/var/ghostcam \
GHOSTCAM_VIDEO_PROFILE=pi4 \
  ghostcam-camera
```

For production use, set up a systemd service (see `pi/systemd/ghostcam-camera.service` as a template).

## Pi Developer Workflow

For iterating on the camera binary against a test Pi over SSH:

```bash
./scripts/pi.sh setup    # First-time Pi provisioning (installs deps, deploys config + binary)
./scripts/pi.sh deploy   # Build + deploy camera binary (primary dev loop)
./scripts/pi.sh logs     # Stream camera logs
./scripts/pi.sh status   # Health check
./scripts/pi.sh unenroll # Clear credentials so the camera re-provisions on next start
```

Defaults are configured via `.pi.env` (gitignored) or passed as CLI args `[HOST] [USER] [PASS]`.

---

# Part 2: Using the Viewer

## 1. Log In

Open the Ghostcam URL in your browser. You'll see a login form with **Email** and **Password** fields.

- **Local dev**: `admin@ghostcam.dev` / `dev-password`
- **Production**: whatever email/password was set via `GHOSTCAM_ADMIN_EMAIL` / `GHOSTCAM_ADMIN_PASSWORD` on first run

Registration is disabled. The bootstrap user is seeded on first server start and granted admin via a row in the `admins` table. Additional admins can be granted at runtime by inserting into `admins (user_id, created_at)` — admin status is resolved per-request from the table, so grants and revocations take effect without a token rotation.

## 2. Enrolling a Camera

A fresh account has no cameras. The empty-state prompt directs you to click the **+** button in the sidebar.

### Generate a Provision Token (QR Code)

1. Click **+** in the sidebar to open the **Add Camera** dialog.
2. (Optional) Enter your **WiFi SSID** and **Password** — the camera will join this network on first boot.
3. Choose a **token expiry**: 1 hour / 24 hours / 7 days.
4. Click **Generate QR Code**.

The dialog shows:
- A QR code containing `{ server_url, token, wifi_ssid?, wifi_password? }`
- The raw provision token (click to copy — useful for headless CLI provisioning)

### Deliver the Token to the Camera

There are three ways to get the provision token to the camera, from easiest to most flexible:

**QR scan (recommended for Pi hardware):**
Simply hold the QR code in front of the Pi camera. On first boot without credentials, the camera automatically scans for a QR code using `rpicam-still` for up to 5 minutes. If the QR includes WiFi credentials, the camera joins the network first, then provisions. No SSH or file copying needed.

> **Tip:** Include WiFi credentials in the QR if the Pi isn't on a wired connection yet — the camera will configure WiFi before attempting to reach the server.

**Flat file (headless / SSH)**:
```bash
echo "<provision_token>" > /var/ghostcam/provision_token
echo "https://your-server.example.com" > /var/ghostcam/server_url
sudo systemctl restart ghostcam-camera
```

**Environment variable (dev/test)**:
```bash
GHOSTCAM_SERVER_URL=https://your-server.example.com \
GHOSTCAM_PROVISION_TOKEN=<paste token here> \
  ghostcam-camera
```

The camera resolves provisioning inputs in order: CLI/env → flat files → QR scan. The first source that provides both a token and server URL wins.

Once the camera connects, it appears in the sidebar. It shows **offline (gray dot)** until the first telemetry arrives (usually within 10 seconds), then turns **online (green dot)**.

## 3. Live View

The main view shows a responsive grid of camera tiles. Each tile displays:

- Live HLS stream (starts as soon as segments arrive)
- **Status badge**: LIVE / PLAYBACK / CLIP / OFFLINE
- **Status dot**: green (live), blue (playback), yellow (clip mode), gray (offline)
- Telemetry overlay: CPU %, memory, temperature

### Tile Actions (hover to reveal)

- **Camera icon** — download a PNG snapshot
- **PiP icon** — toggle picture-in-picture
- **Volume icon** — mute / unmute
- **Gear icon** — open camera settings

### Selection

- **Click** a tile to select it (blue ring). The timeline scrubber then highlights that camera's coverage and the map centers on it.
- **Double-click** to open the full-screen camera view.

### Layout

Layout is auto-responsive on desktop and stacks on mobile. A hamburger menu replaces the sidebar below 768px.

## 4. Playback & Timeline

The **timeline scrubber** at the bottom of the screen shows recorded coverage.

- **Green bars**: merged segment coverage (merged across gaps < 30s)
- **Amber dots**: motion events
- **Playhead**: green when live, blue when scrubbing playback

### Controls

| Action | How |
|--------|-----|
| Seek to a time | Click or drag the timeline |
| Return to live | Click the **Live** button (right side) |
| Zoom in | Click and hold ~1.8 seconds — opens a 6-minute window |
| Pan while zoomed | Drag near the left or right edge |
| Zoom out | Release the held click |

The scrubber shows a union bar (all cameras) overlaid with the selected camera's coverage in a darker shade.

## 5. Download Clips

To export a time range as MP4 or telemetry CSV/JSON:

1. Click the **scissors** button on the timeline to enter clip mode (button turns yellow).
2. Two yellow drag handles appear. Drag them to select a range.
3. The clip bar appears below the timeline showing duration and target camera.
4. Click one of:
   - **Video** — downloads MP4 (H.264, remuxed client-side via ffmpeg.wasm, `~25 MB` on first use to load wasm)
   - **CSV** — telemetry as comma-separated values
   - **JSON** — telemetry as structured JSON

Constraints:
- Minimum clip length: **10 seconds**
- Maximum clip length: **5 minutes**
- If no camera is selected, clips are exported for **all cameras** (separate MP4 per camera, merged CSV/JSON with `device_id` column)

Filenames use the format `clip-{CameraName}-YYYY-MM-DDTHH-MM-SS.mp4`.

Click the scissors button again to exit clip mode.

## 6. Camera Settings

Hover a camera tile and click the **gear icon**, or click the gear next to the camera name in the sidebar.

| Field | Options |
|-------|---------|
| **Name** | Free text |
| **Resolution** | 480p / 720p / 1080p (matches Pi video profiles) |
| **Recording Mode** | Streaming Only / On Motion / Continuous — see [Recording Modes](#recording-modes) |
| **Motion Alerts** | Toggle — emit SSE events on motion detection |

Saving resolution or recording mode issues a command to the camera, which persists it to disk and restarts via systemd. The camera reappears online within a few seconds.

**Delete Camera** removes the camera record but leaves existing recordings in S3 until retention deletes them.

### Recording Modes

New cameras default to **Streaming Only** — live viewing works but no
footage is saved. You can switch modes at any time from the settings
dialog; the camera persists the new mode to disk and restarts to apply.

| Mode | Live view | Timeline / clips | Storage | Upload bandwidth |
|------|-----------|------------------|---------|------------------|
| **Streaming Only** (`never`, default) | Yes | Empty | None | Only while a viewer is watching |
| **On Motion** (`motion`) | Yes | Motion-only segments | Low | Bursty around motion events |
| **Continuous** (`constant`) | Yes | Full coverage | High (~2–4 GB / camera / day at 720p) | Sustained |

Pick **Streaming Only** when you only care about live monitoring (no
evidence trail, no scrubbing back). Pick **On Motion** to keep clips of
interesting activity without paying for idle footage — motion detection
is heuristic (H.264 P-frame size heuristic, with a file-size fallback) so
very subtle changes may be missed. Pick **Continuous** when you need a
complete record and you've sized your tier's storage cap accordingly.

## 7. Alerts & Events

Click the **bell icon** in the header to open the alerts panel. You'll see:

- **Motion events** — when a segment with detected motion is uploaded
- **Disconnect / reconnect** — when a camera goes offline or comes back
- **Storage capped** — when uploads pause because you hit your tier limit

Unread counts appear as a red badge on the bell. Opening the panel marks alerts as read.

Alerts are clickable:

- **Motion** — jumps to the relevant camera's view and seeks the timeline to the motion timestamp.
- **Storage capped** — opens the global settings panel (Billing section) so you can upgrade.

## 8. Billing & Storage

Click the **gear icon** in the header to open global settings. The **Billing** section shows:

- Your current tier (Free / Starter / Pro / Enterprise)
- Camera count vs. tier limit
- Storage bar: green (normal), amber (>80%), red (capped)
- **Upgrade** button (free tier) → Stripe Checkout
- **Manage Subscription** button (paid tier) → Stripe Billing Portal

### Tier Limits

| Tier | Storage | Cameras |
|------|---------|---------|
| Free | 5 GB | 1 |
| Starter | 50 GB | 4 |
| Pro | 500 GB | 16 |
| Enterprise | unlimited | unlimited |

### What Happens at the Limit

- **Camera limit reached** — enrolling a new camera returns HTTP 402. Delete an existing camera or upgrade.
- **Storage limit reached** — HLS segment uploads pause (`storage_capped` event fires). **Live viewing is unaffected**: WebRTC live streaming runs independently of storage (no segments are persisted on that path), so cameras stay watchable in real time even while recording is paused. Existing recordings remain available for playback and download. Free up space (wait for retention to delete old segments) or upgrade. Segments older than `GHOSTCAM_SEGMENT_RETENTION_DAYS` (30 by default) are deleted hourly.
- **Downgrade past camera limit** — only the oldest N cameras (by enrollment date) may upload. The rest stay visible and playable but stop recording new footage.

### Storage-Cap Banner

A persistent banner appears at the top of the main view when storage usage
crosses key thresholds, so users aren't surprised by silent upload pauses:

- **At 85%+ (warning)** — an amber banner appears with an **Upgrade** button.
  The banner is dismissible for the session.
- **At 100% (capped)** — a red banner replaces the warning with copy
  clarifying that recording is paused but live viewing still works. The
  capped banner is not dismissible; it disappears automatically once
  storage drops below the limit or the user upgrades.

The same Upgrade button opens the normal settings → Billing flow.

## 9. Groups

If your admin has organized cameras into groups, pill buttons appear below the header view toggles. Click a group to filter the sidebar and live view.

## 10. Global Settings

The settings dialog (gear icon in header) also includes:

- **Theme** — Light / Dark / System
- **Connection status** — server URL, SSE state, last error

## 11. Keyboard Shortcuts (Full-Screen Camera View)

Double-click a camera tile to open full-screen view, then:

| Key | Action |
|-----|--------|
| **F** | Toggle fullscreen |
| **M** | Mute / unmute |
| **S** | Download snapshot |
| **P** | Toggle picture-in-picture |
| **Esc** | Exit back to grid |

## Troubleshooting

- **Camera stuck offline after enrollment** — Check that the provision token hasn't expired and that the camera can reach the server URL. Inspect logs with `./scripts/pi.sh logs`.
- **QR scan not working** — Ensure `rpicam-still` is installed and the camera module is connected. The scan runs for 5 minutes on boot; hold the QR steady and well-lit, ~15–30 cm from the lens. Check logs for "scanning for provisioning QR code" to confirm the scan started. QR scanning is only available on real Pi hardware (not synthetic/Docker builds).
- **No live stream** — Verify the camera is uploading segments: check the `segments` table or look for recent activity in camera logs. HLS playback needs at least one segment in the 90-second sliding window.
- **"Storage full. Camera uploads paused."** — You've hit your tier's storage limit. Wait for retention, delete cameras, or upgrade.
- **Video export fails in browser** — ffmpeg.wasm requires Cross-Origin Isolation (COOP/COEP headers) and SharedArrayBuffer support. Check browser compatibility (Chrome/Edge/Firefox recent versions).
- **Playback shows "No footage"** — The selected camera has no segments in the seek time range. Scrub to a time with green coverage.

For deeper troubleshooting see [debugging.md](debugging.md).
