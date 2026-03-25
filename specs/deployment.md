# Ghostcam — Deployment and Operations

**Status:** Draft

---

## 1. Overview

This document covers the operational concerns for running Ghostcam: server installation and configuration for `server-solo`, camera provisioning and first-boot behaviour, health check semantics, audit logging, backup and restore, and the operator-facing side of camera firmware updates.

The PKI model (CA certificates, trust establishment) is specified in `pki.md`. The camera firmware internals (capture pipeline, recording, QUIC lifecycle) are specified in `camera-firmware.md`. The application database schema is specified in `database.md`.

---

## 2. `server-solo` Installation

`server-solo` is a single self-contained binary targeting Linux/amd64 and Linux/arm64. It requires no external dependencies beyond a running Redis instance.

### 2.1 Requirements

| Dependency | Version | Notes |
|------------|---------|-------|
| Linux | Any modern distribution | Tested on Ubuntu 24.04 LTS |
| Redis | 7.x | For telemetry history and certificate revocation list |
| Open ports | UDP 4433 (QUIC), TCP 3000 (HTTP) | Configurable |

No database server is required — SQLite is embedded in the binary.

### 2.2 Installation

Download the binary for your platform from the Ghostcam releases page and place it on your server:

```bash
curl -Lo ghostcam-server https://releases.ghostcam.io/server-solo/latest/ghostcam-server-linux-amd64
chmod +x ghostcam-server
sudo mv ghostcam-server /usr/local/bin/
```

Create the data directory:

```bash
sudo mkdir -p /var/ghostcam
```

### 2.3 First startup

On first startup `server-solo` performs the following initialisation steps automatically:

1. Generate the instance CA key pair and self-signed certificate (`/var/ghostcam/ca.crt`, `ca.key`)
2. Generate the server TLS key pair and self-signed certificate (`/var/ghostcam/server.crt`, `server.key`)
3. Generate the HMAC signing secret for API token verification (`/var/ghostcam/hmac.key`)
4. Run database migrations — creates `ghostcam.db` at `/var/ghostcam/ghostcam.db`
5. Generate a random initial operator password and print it once to stdout
6. Print a warning about backing up the CA key

```
============================================================
Ghostcam server-solo first run

Initial operator password: xK9mP2vL8nQr

Log in at http://<your-server-ip>:3000 and change this password.

IMPORTANT: Back up /var/ghostcam/ca.key
Losing this file requires re-enrolling all cameras.
============================================================
```

The initial password is not stored anywhere — it is generated, printed, and discarded. The operator must log in and change it before the password can be used for API token creation or camera enrollment.

### 2.4 Running as a systemd service

Create `/etc/systemd/system/ghostcam.service`:

```ini
[Unit]
Description=Ghostcam server-solo
After=network.target redis.service
Requires=redis.service

[Service]
ExecStart=/usr/local/bin/ghostcam-server
Restart=on-failure
RestartSec=5
User=ghostcam
Group=ghostcam
WorkingDirectory=/var/ghostcam
Environment=RUST_LOG=info
Environment=GHOSTCAM_REDIS_URL=redis://127.0.0.1:6379

[Install]
WantedBy=multi-user.target
```

Create a dedicated user and enable the service:

```bash
sudo useradd -r -s /bin/false ghostcam
sudo chown -R ghostcam:ghostcam /var/ghostcam
sudo systemctl daemon-reload
sudo systemctl enable --now ghostcam
```

### 2.5 Configuration

`server-solo` is configured via environment variables or CLI flags. Environment variables take precedence.

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--http-port` | `GHOSTCAM_HTTP_PORT` | `3000` | HTTP listener port |
| `--quic-port` | `GHOSTCAM_QUIC_PORT` | `4433` | QUIC listener port (UDP) |
| `--data-dir` | `GHOSTCAM_DATA_DIR` | `/var/ghostcam` | Data directory for database, certs, logs |
| `--redis-url` | `GHOSTCAM_REDIS_URL` | `redis://127.0.0.1:6379` | Redis connection URL |
| `--audit-log-retention-days` | `GHOSTCAM_AUDIT_RETENTION_DAYS` | `90` | Days to retain audit log entries |
| `--log-level` | `RUST_LOG` | `info` | Application log level |

### 2.6 Network requirements

`server-solo` is designed for LAN deployment. The server machine should have a stable local IP address (static assignment or DHCP reservation recommended). Cameras and the browser UI must be able to reach the server on both the HTTP port (TCP) and the QUIC port (UDP).

No port forwarding, no public domain, no TLS certificate from a public CA is required. The browser UI is served over plain HTTP. The QUIC connection between cameras and server uses a self-signed TLS certificate verified via camera-side fingerprint pinning (see `pki.md` §6.1).

---

## 3. Camera Provisioning

### 3.1 What's on the image

The Ghostcam camera image is a pre-built Raspberry Pi OS Lite-based image for the Pi Zero 2W containing:

- Ghostcam firmware binary (`/usr/bin/ghostcam-camera`)
- systemd service unit
- `rpicam-vid` and supporting camera stack
- `gpsd` (optional GPS support)
- `rqrr` library for QR code decoding
- NetworkManager for WiFi and cellular interface management
- First-boot initialisation script

The image is stateless — it contains no secrets, no device identity, and no server configuration. Every camera flashed from the same image is identical before first boot.

### 3.2 Flashing

**Kit customers:** the SD card is pre-flashed. Insert it, connect the camera module, and power on.

**Self-hosters and reflashing:** download the image from the Ghostcam releases page and flash it using Raspberry Pi Imager:

```
https://releases.ghostcam.io/camera/latest/ghostcam-camera-pi-zero-2w.img.xz
```

Select "Use custom image" in Raspberry Pi Imager, choose the downloaded file, and flash to the SD card.

### 3.3 `ghostcam.conf` — optional bootstrap configuration

For `server-solo` deployments, create a file named `ghostcam.conf` in the boot partition of the SD card before first power-on. The boot partition is the FAT32 partition visible when the SD card is inserted into a Mac or Windows machine.

`ghostcam.conf` format (TOML):

```toml
# Server address for this camera.
# If omitted, the camera defaults to the Ghostcam cloud server.
# Override this for server-solo deployments.
server_addr = "192.168.1.10:4433"

# Hardware toggles (all default to enabled)
no_audio = false
no_gps = false
```

`ghostcam.conf` is read-only — the firmware never modifies it. It is the operator's configuration layer, separate from the camera's runtime state.

### 3.4 First boot sequence

On first boot the firmware:

1. Reads `ghostcam.conf` from `/boot/ghostcam.conf` if present
2. Generates a device identity key pair and self-signed certificate, stores at `/etc/ghostcam/device.crt` and `/etc/ghostcam/device.key`
3. Enters registration mode — activates the camera sensor and begins scanning for an enrollment QR code

Steps 1 and 2 happen once only. On all subsequent boots the existing device certificate is loaded from disk and the firmware proceeds directly to step 3 if not yet enrolled, or to the normal startup sequence if enrolled.

### 3.5 Enrollment

Enrollment is initiated from the Ghostcam UI:

1. Open the Ghostcam UI and navigate to **Cameras → Enroll New Camera**
2. Optionally enter a display name for the camera
3. For `server-solo`: the app includes the server's local IP in the QR payload automatically
4. The server generates a signed enrollment JWT and displays it as a QR code
5. Hold your phone screen displaying the QR code up to the camera lens
6. The camera decodes the QR code, stores WiFi credentials and server address if present, and initiates an enrollment QUIC connection
7. The server verifies the JWT, creates the camera record, issues the user association certificate
8. The camera stores the certificate and enters normal operation

The QR code is valid for 10 minutes. If enrollment does not complete within this window, return to the UI and generate a new QR code.

### 3.6 Server address precedence

The camera resolves the server address in the following order:

1. `ghostcam.conf` `server_addr` (operator override — highest priority)
2. Server address stored during enrollment (`/etc/ghostcam/server.addr`)
3. Hardcoded default Ghostcam cloud address (fallback)

---

## 4. Health Checks

The server exposes two health endpoints:

```
GET /healthz    -> 200 OK, body: "ok"
GET /readyz     -> 200 OK or 503 Service Unavailable
```

### 4.1 `/healthz`

Always returns `200 OK` if the process is running. No dependency checks. Used by process supervisors and container orchestration to detect process death.

### 4.2 `/readyz`

Returns `200 OK` only when the server is fully ready to handle requests. Returns `503` with a JSON body describing the failing check if any required dependency is unavailable.

Ready conditions:
- Application database is reachable and migrations have completed
- QUIC listener is bound and accepting connections
- HTTP listener is bound and accepting connections

Redis is **not** a readiness condition — the server starts and operates in a degraded mode if Redis is unavailable at startup (see `ingest.md` §9 for the failure policy). The `/readyz` response body indicates Redis availability separately:

```json
{
  "status": "ready",
  "database": "ok",
  "redis": "unavailable",
  "quic": "ok"
}
```

### 4.3 Systemd watchdog integration

The systemd service unit should be extended with a watchdog to restart the server if it becomes unresponsive:

```ini
[Service]
WatchdogSec=30
NotifyAccess=main
```

The server sends `sd_notify(WATCHDOG=1)` on each successful `/healthz` cycle.

---

## 5. Audit Logging

### 5.1 Overview

The audit log is a structured record of security-relevant events. It is separate from the application log (stdout) and is never suppressed by log level configuration.

**Application log** — stdout, operational events, level controlled by `RUST_LOG`. Redirect to a file or log aggregator as needed.

**Audit log** — `/var/ghostcam/audit.log`, security events only, always written regardless of `RUST_LOG`.

### 5.2 Format

Newline-delimited JSON (NDJSON). One event per line.

```json
{"ts":"2026-03-18T14:23:01.234Z","level":"audit","event":"camera.enrolled","device_id":"a1b2c3d4-...","display_name":"Back Door","ip":"192.168.1.45"}
{"ts":"2026-03-18T14:25:11.891Z","level":"audit","event":"auth.login","ip":"192.168.1.2","user_agent":"Mozilla/5.0 ...","session_id":"e5f6..."}
```

### 5.3 Event catalogue

| Event | Key fields | Description |
|-------|-----------|-------------|
| `server.started` | `version`, `variant` | Server process started |
| `server.stopped` | `uptime_secs` | Graceful shutdown |
| `auth.login` | `ip`, `user_agent`, `session_id` | Successful login |
| `auth.login_failed` | `ip`, `user_agent` | Failed login attempt |
| `auth.logout` | `session_id` | Session invalidated |
| `auth.token_created` | `token_id`, `label` | API token created |
| `auth.token_revoked` | `token_id`, `label` | API token revoked |
| `camera.enrolled` | `device_id`, `display_name`, `ip` | Camera enrolled successfully |
| `camera.enrollment_failed` | `cert_fingerprint`, `reason`, `ip` | Enrollment attempt rejected |
| `camera.unregistered` | `device_id` | Camera unregistered, data purged |
| `camera.connected` | `device_id`, `ip` | Camera QUIC connection established |
| `camera.disconnected` | `device_id`, `duration_secs` | Camera QUIC connection lost |
| `camera.revocation_blocked` | `cert_serial`, `ip` | Connection rejected — revoked cert |
| `camera.storage_full` | `device_id` | Recording paused on camera |
| `camera.update_succeeded` | `device_id`, `version` | Firmware update applied |
| `camera.update_failed` | `device_id`, `version_attempted`, `reason` | Firmware update failed |

### 5.4 Retention and rotation

The audit log is rotated daily. Retention defaults to 90 days, configurable via `--audit-log-retention-days`. Rotated logs are compressed with gzip and named `audit.log.YYYY-MM-DD.gz`.

The server performs rotation at midnight UTC. Rotation does not require a process restart.

### 5.5 Consuming the audit log

```bash
# Tail live audit events
tail -f /var/ghostcam/audit.log | jq .

# Find all failed login attempts
cat /var/ghostcam/audit.log | jq 'select(.event == "auth.login_failed")'

# Find all events for a specific camera
cat /var/ghostcam/audit.log | jq 'select(.device_id == "a1b2c3d4-...")'

# Search compressed rotated logs
zcat /var/ghostcam/audit.log.2026-03-17.gz | jq 'select(.event == "camera.enrolled")'
```

For `server-multi`, ship audit logs to a central log aggregator (CloudWatch Logs, Datadog, Loki) using standard log forwarding from the container runtime. The NDJSON format is compatible with all major aggregators without transformation.

---

## 6. Backup and Restore (`server-solo`)

### 6.1 What to back up

Everything the server needs to restore to a fully working state lives in `/var/ghostcam/`. Back up this directory in its entirety.

| File | Consequence of loss |
|------|---------------------|
| `ghostcam.db` | All enrollments, display names, API tokens — cameras must be re-enrolled |
| `ca.crt` + `ca.key` | **Critical** — all enrolled cameras must be re-enrolled; their user association certs are unverifiable without the CA key |
| `server.crt` + `server.key` | Cameras using TOFU pinning will reject the server on next connection — must re-enroll |
| `hmac.key` | All existing API tokens are invalidated |
| `audit.log` + rotated logs | Loss of audit trail |

The CA key (`ca.key`) is the most critical file. Its loss cannot be recovered from without re-enrolling every camera. Back it up to a separate location from the server.

### 6.2 Backup procedure

**Minimal backup** — restores enrollments, certs, and tokens. Telemetry history is lost; segment index is reconstructed from camera reconnections.

```bash
sudo systemctl stop ghostcam
sudo tar -czf ghostcam-backup-$(date +%Y%m%d).tar.gz -C / var/ghostcam
sudo systemctl start ghostcam
```

**Full backup** — includes Redis telemetry history and segment index.

```bash
sudo systemctl stop ghostcam
redis-cli BGSAVE && sleep 5  # Wait for RDB snapshot
sudo tar -czf ghostcam-backup-$(date +%Y%m%d).tar.gz \
  -C / var/ghostcam \
  -C /var/lib/redis dump.rdb  # Path may vary by Redis installation
sudo systemctl start ghostcam
```

Redis persistence mode (RDB vs AOF) is the operator's choice — see the Redis documentation for configuration options. RDB snapshots are sufficient for telemetry history; AOF provides near-zero data loss but higher write overhead.

### 6.3 Restore procedure

1. Install `server-solo` on the new machine following §2
2. Stop the server before it completes first-run initialisation:
   ```bash
   sudo systemctl stop ghostcam
   ```
3. Restore the data directory:
   ```bash
   sudo tar -xzf ghostcam-backup-20260318.tar.gz -C /
   sudo chown -R ghostcam:ghostcam /var/ghostcam
   ```
4. Optionally restore Redis:
   ```bash
   sudo systemctl stop redis
   sudo cp dump.rdb /var/lib/redis/dump.rdb
   sudo chown redis:redis /var/lib/redis/dump.rdb
   sudo systemctl start redis
   ```
5. Start the server:
   ```bash
   sudo systemctl start ghostcam
   ```

The server detects existing initialisation files and skips first-run setup. Cameras reconnect automatically. Existing browser sessions are invalidated — the operator must log in again.

### 6.4 Automated backups

A simple cron job for nightly backups:

```bash
# /etc/cron.d/ghostcam-backup
0 2 * * * ghostcam tar -czf /backup/ghostcam-$(date +\%Y\%m\%d).tar.gz -C / var/ghostcam && find /backup -name 'ghostcam-*.tar.gz' -mtime +30 -delete
```

Adjust the backup destination and retention period as appropriate.

---

## 7. Firmware Updates

### 7.1 Triggering an update

Firmware updates are pushed from the server to cameras via the `update_available` command. In `server-solo` the operator triggers updates from the UI:

1. Navigate to **Settings → Firmware**
2. The server checks for available updates against the Ghostcam release server
3. Select cameras to update (individual or all)
4. Click **Push Update**

The server sends `update_available` commands to selected cameras. Cameras that are offline will receive the command on next reconnection.

**Staged rollout recommendation:** update one camera first and verify it reconnects successfully before updating the remainder. The UI supports selecting individual cameras for this purpose.

### 7.2 Update states

| State | Description |
|-------|-------------|
| Up to date | Camera is running the current firmware version |
| Update available | `update_available` command not yet sent |
| Pending | `update_available` sent, camera has not yet acknowledged |
| Downloading | Camera is downloading the new firmware |
| Applying | Camera has sent `update_applying` and is rebooting |
| Succeeded | Camera sent `update_succeeded` — new firmware is healthy |
| Failed | Camera sent `update_failed` — rolled back to previous version |
| Offline | Camera is offline — update will be sent on reconnect |

### 7.3 Recovery from a failed update

If a camera sends `update_failed` with reason `watchdog`, it has automatically rolled back to the previous firmware version and is running normally. No operator action is required unless the camera is stuck in a boot loop (which would manifest as repeated connect/disconnect cycles in the UI).

If a camera does not reconnect within 10 minutes of `update_applying`, assume the update has failed unrecoverably. Physical access is required to reflash the SD card.

### 7.4 Firmware download source

Cameras download firmware directly from the Ghostcam release server over HTTPS:

```
https://releases.ghostcam.io/firmware/{version}/ghostcam-camera-aarch64.bin
```

The server includes this URL in the `update_available` command. For air-gapped deployments, operators can host firmware binaries on a local HTTP server and configure a custom base URL via `--firmware-base-url`.

---

## 8. `server-multi` Operational Notes

`server-multi` is deployed on Fly.io as a containerised service. Operational concerns specific to `server-multi` (scaling, secrets management, Redis cluster configuration, ACME certificate management) are managed by Ghostcam infrastructure and are not documented here.

Operators of `server-multi` (i.e. Ghostcam staff) should refer to the internal infrastructure runbook.

The single-instance constraint documented in `overview.md` §9 and `ingest.md` §5 applies to `server-multi` in v1. Do not run multiple instances behind a load balancer until `multi-server.md` is implemented.
