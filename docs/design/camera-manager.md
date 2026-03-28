# Camera Manager CLI — Design Document

## Overview

A developer CLI (`scripts/pi.sh`) for managing real camera hardware over SSH during development. Replaces the local docker-compose test camera workflow when working with physical Pi devices. Inspired by Kodama's `scripts/pi.sh`.

## Why

Docker test cameras are great for UI and server development, but real hardware is needed for:
- Testing real video/audio capture (rpicam-vid, cpal)
- GPS and cellular failover
- Firmware updates and watchdog behavior
- Performance profiling on Pi hardware
- QR enrollment flow

SSH-ing manually and running ad-hoc commands is slow and error-prone. The camera manager wraps the common operations into a single script.

## Commands

### `./scripts/pi.sh setup [HOST] [USER] [PASS]`

Full provisioning of a fresh Pi. Run once after flashing the OS.

1. **Verify SSH connectivity** (fail fast if unreachable)
2. **Install system packages**:
   - `rpicam-apps` (or `libcamera-apps` fallback) — video capture
   - `libasound2` `alsa-utils` — audio
   - `gpsd` `gpsd-clients` — GPS
   - `modemmanager` `libqmi-utils` `usb-modeswitch` — cellular modem
   - `network-manager` — connectivity management
   - `curl` `jq` `htop` — utilities
3. **Add user to groups**: `video`, `dialout`, `audio`
4. **Deploy system configs**:
   - `pi/networkmanager/no-connectivity-check.conf` → `/etc/NetworkManager/conf.d/`
   - `pi/networkmanager/99-keep-cellular-route` → `/etc/NetworkManager/dispatcher.d/` (chmod 755)
   - `pi/gpsd.conf` → `/etc/default/gpsd`
   - `pi/ghostcam-enable-gps.sh` → `/usr/local/bin/` (chmod 755)
   - `pi/systemd/ghostcam-gps.service` → `/etc/systemd/system/`
   - `pi/systemd/ghostcam-camera.service` → `/etc/systemd/system/`
5. **Enable services**: `gpsd`, `ghostcam-gps`, `ghostcam-camera`
6. **Verify GPS** (if `/dev/ttyUSB1` detected): `timeout 10 gpspipe -w`
7. **Build and deploy** firmware binary (same as `deploy`)
8. **Print** manual startup instructions

### `./scripts/pi.sh deploy [HOST] [USER] [PASS]`

Quick build + deploy after code changes. The primary dev loop command.

1. **Check SSH connectivity**
2. **Stop running camera**: `sudo systemctl stop ghostcam-camera` (or `pkill`)
3. **Cross-compile**: `cargo build --release --target aarch64-unknown-linux-gnu -p camera`
4. **SCP binary** to Pi: `scp target/.../camera pi:/usr/local/bin/ghostcam-camera`
5. **Start camera**: `sudo systemctl start ghostcam-camera`
6. **Tail logs**: `journalctl -u ghostcam-camera -f` (streams until Ctrl+C)

### `./scripts/pi.sh logs [HOST] [USER] [PASS]`

View camera logs.

```bash
ssh pi "journalctl -u ghostcam-camera -f"
```

### `./scripts/pi.sh status [HOST] [USER] [PASS]`

Quick health check of all relevant services and hardware:

```bash
echo "=== Camera ==="
systemctl status ghostcam-camera --no-pager -l
echo "=== GPS ==="
systemctl status gpsd --no-pager
timeout 3 gpspipe -w -n 1 2>/dev/null || echo "no GPS fix"
echo "=== Network ==="
nmcli device status
ip route | grep default
echo "=== System ==="
uptime
free -h
df -h /
vcgencmd measure_temp 2>/dev/null || cat /sys/class/thermal/thermal_zone0/temp
```

### `./scripts/pi.sh wifi-off [DURATION] [HOST] [USER] [PASS]`

Toggle WiFi off for cellular failover testing.

1. Find active WiFi connection name on `wlan0`
2. Create and execute a remote script (via `nohup`) that:
   - Brings WiFi down: `nmcli connection down "$WIFI_CONN"`
   - Waits `DURATION` seconds (default: 60)
   - Brings WiFi back up: `nmcli connection up "$WIFI_CONN"`
3. Log to `/tmp/wifi-toggle.log` on Pi
4. Print instructions for monitoring failover

### `./scripts/pi.sh restart [HOST] [USER] [PASS]`

Restart the camera service:

```bash
ssh pi "sudo systemctl restart ghostcam-camera"
```

### `./scripts/pi.sh ssh [HOST] [USER] [PASS]`

Open an interactive SSH session to the Pi:

```bash
sshpass -p "$PASS" ssh -o StrictHostKeyChecking=accept-new "$USER@$HOST"
```

### `./scripts/pi.sh unenroll [HOST] [USER] [PASS]`

Clear enrollment state so the camera re-enters enrollment mode on next start:

```bash
ssh pi "sudo systemctl stop ghostcam-camera && rm -f /var/ghostcam/user.crt /var/ghostcam/user.key /var/ghostcam/server.addr /var/ghostcam/server_fingerprint && sudo systemctl start ghostcam-camera"
```

## Script Structure

```bash
#!/usr/bin/env bash
set -euo pipefail

# Defaults
PI_HOST="${2:-10.0.0.100}"
PI_USER="${3:-pi}"
PI_PASSWORD="${4:-ghostcam}"
SSH_OPTS="-o StrictHostKeyChecking=accept-new -o ConnectTimeout=10"
TARGET="aarch64-unknown-linux-gnu"

# Helpers
pi_ssh()  { sshpass -p "$PI_PASSWORD" ssh $SSH_OPTS "$PI_USER@$PI_HOST" "$@"; }
pi_scp()  { sshpass -p "$PI_PASSWORD" scp $SSH_OPTS "$1" "$PI_USER@$PI_HOST:$2"; }
check_sshpass() { command -v sshpass >/dev/null || { echo "install sshpass: brew install sshpass / apt install sshpass"; exit 1; }; }
check_connection() { pi_ssh "true" || { echo "cannot reach $PI_HOST"; exit 1; }; }

# Commands
cmd_setup()    { ... }
cmd_deploy()   { ... }
cmd_logs()     { ... }
cmd_status()   { ... }
cmd_wifi_off() { ... }
cmd_restart()  { ... }
cmd_ssh()      { ... }
cmd_unenroll() { ... }

# Dispatch
case "${1:-help}" in
    setup)    check_sshpass; cmd_setup ;;
    deploy)   check_sshpass; cmd_deploy ;;
    logs)     check_sshpass; cmd_logs ;;
    status)   check_sshpass; cmd_status ;;
    wifi-off) check_sshpass; cmd_wifi_off "${2:-60}" ;;
    restart)  check_sshpass; cmd_restart ;;
    ssh)      check_sshpass; cmd_ssh ;;
    unenroll) check_sshpass; cmd_unenroll ;;
    *)        echo "Usage: $0 {setup|deploy|logs|status|wifi-off|restart|ssh|unenroll} [HOST] [USER] [PASS]" ;;
esac
```

## Cross-Compilation Setup

### `.cargo/config.toml`

```toml
[target.aarch64-unknown-linux-gnu]
linker = "aarch64-linux-gnu-gcc"

[env]
PKG_CONFIG_SYSROOT_DIR = { value = "/usr/aarch64-linux-gnu", force = false }
PKG_CONFIG_ALLOW_CROSS = { value = "1", force = false }
```

### Dev Machine Dependencies

**macOS**:
```bash
brew install aarch64-linux-gnu-gcc sshpass
rustup target add aarch64-unknown-linux-gnu
```

**Linux (Ubuntu/Debian)**:
```bash
sudo apt install gcc-aarch64-linux-gnu libasound2-dev:arm64 libopus-dev:arm64 sshpass
rustup target add aarch64-unknown-linux-gnu
```

## Pi System Files

New directory `pi/` in the repo:

```
pi/
├── gpsd.conf                          # /etc/default/gpsd
├── ghostcam-enable-gps.sh            # /usr/local/bin/ — enables GPS via mmcli
├── networkmanager/
│   ├── no-connectivity-check.conf    # /etc/NetworkManager/conf.d/
│   └── 99-keep-cellular-route        # /etc/NetworkManager/dispatcher.d/
└── systemd/
    ├── ghostcam-camera.service       # /etc/systemd/system/ (already exists, move here)
    └── ghostcam-gps.service          # /etc/systemd/system/
```

The existing `camera/ghostcam-camera.service` moves to `pi/systemd/ghostcam-camera.service` so all Pi system files live together.

## Environment File

The camera manager deploys `/etc/ghostcam/env` which the systemd service reads via `EnvironmentFile=`:

```bash
GHOSTCAM_DATA_DIR=/var/ghostcam
GHOSTCAM_SERVER_ADDR=cam.example.com:4433
RUST_LOG=camera=info
```

The `setup` command prompts for the server address (or reads from `$GHOSTCAM_SERVER_ADDR` if set).

## Dev Workflow

### First-time setup (fresh Pi)

```bash
# Flash Raspberry Pi OS Lite (64-bit) to SD card
# Boot Pi, note its IP address

./scripts/pi.sh setup 10.0.0.100 pi ghostcam
# Installs everything, deploys binary, prints status
```

### Daily dev loop

```bash
# Start infra (no test cameras)
docker compose up -d

# Make camera code changes...
./scripts/pi.sh deploy
# Cross-compiles, deploys, restarts, tails logs

# In another terminal:
./scripts/pi.sh status    # check health
./scripts/pi.sh wifi-off 30  # test failover
```

### Server/UI only (no hardware)

```bash
docker compose up -d --profile test
# Includes test cameras — no Pi needed
```

### Debugging

```bash
./scripts/pi.sh logs      # stream camera logs
./scripts/pi.sh ssh       # interactive shell for manual inspection
./scripts/pi.sh unenroll  # reset enrollment state
```

## Integration with Docker Compose

Docker compose always runs the infrastructure (postgres, redis, server, UI). Test cameras are behind a `test` profile so they don't start when working with real hardware.

### docker-compose.yml Changes

Test cameras (camera-1, camera-2, camera-3) get `profiles: [test]`:

```yaml
camera-1:
  profiles: [test]
  build: ...
```

This means:

| Command | What starts |
|---------|-------------|
| `docker compose up -d` | postgres, redis, server, UI only |
| `docker compose up -d --profile test` | Everything including test cameras |

### Dev workflows

**Server/UI development (no hardware)**:
```bash
docker compose up -d --profile test
# Server + UI + 3 test cameras
```

**Camera firmware on real hardware**:
```bash
docker compose up -d
# Server + UI (no test cameras)

./scripts/pi.sh deploy
# Pi camera connects to Docker server at <LAN_IP>:4433
```

**Mixed (test cameras + real hardware simultaneously)**:
```bash
docker compose up -d --profile test
./scripts/pi.sh deploy
# Both test cameras and real Pi camera connect to same server
```

The Pi camera connects to the Docker server via the host's LAN IP. The `GHOSTCAM_PUBLIC_IP` in `.env` is already the LAN IP, and the server's QUIC port (4433) and HTTP port (3000) are exposed on the host. The Pi's `/etc/ghostcam/env` just needs `GHOSTCAM_SERVER_ADDR=<LAN_IP>:4433`.

## Defaults Configuration

Default Pi host/user/pass can be configured via environment variables or a `.pi.env` file in the repo root (gitignored):

```bash
# .pi.env (optional, gitignored)
PI_HOST=10.0.0.100
PI_USER=pi
PI_PASSWORD=ghostcam
```

The script sources `.pi.env` if it exists, with CLI args taking precedence.
