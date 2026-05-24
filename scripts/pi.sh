#!/usr/bin/env bash
set -euo pipefail

# Ghostcam Pi Camera Management
#
# Single entry point for all Pi operations. Run from the dev machine.
#
# Usage:
#   ./scripts/pi.sh setup    [HOST] [USER] [PASS]          # Full provisioning (fresh Pi)
#   ./scripts/pi.sh deploy   [HOST] [USER] [PASS]          # Cross-compile Go binary + deploy
#   ./scripts/pi.sh logs     [HOST] [USER] [PASS]          # Stream camera logs
#   ./scripts/pi.sh status   [HOST] [USER] [PASS]          # Health check
#   ./scripts/pi.sh wifi-off [SECS] [HOST] [USER] [PASS]   # Toggle WiFi for failover testing
#   ./scripts/pi.sh restart  [HOST] [USER] [PASS]          # Restart camera service
#   ./scripts/pi.sh ssh      [HOST] [USER] [PASS]          # Interactive SSH session
#   ./scripts/pi.sh unenroll [HOST] [USER] [PASS]          # Clear enrollment state
#   ./scripts/pi.sh point-at <local|prod|URL> [HOST] [USER] [PASS]   # Repoint at another server
#
# Camera install layout on the Pi:
#   /usr/local/bin/ghostcam-camera              (static Go binary)
#   /etc/systemd/system/ghostcam-camera.service (ExecStart=/usr/local/bin/ghostcam-camera)
#
# Configure your Pi host by copying .pi.env.example to .pi.env in the
# repo root (gitignored) and filling in your Pi's LAN IP / mDNS name.
# USER + PASSWORD default to the dev-image conventions (ghostcam /
# Ghostcam1!) — those match pi/image/config/ghostcam-*.yaml's user1 /
# user1pass and are safe to leave as-is for the dev image. SSH is only
# reachable on dev images flashed via `./scripts/pi.sh flash-dev` —
# production images ship with sshd disabled.
#
# All commands accept [HOST] [USER] [PASS] as positional args, which
# override the .pi.env values for a single invocation.
#
# Config files sourced from this repo and deployed to the Pi:
#   pi/image/files/gpsd.conf                          -> /etc/default/gpsd
#   pi/image/files/ghostcam-enable-gps.sh             -> /usr/local/bin/ghostcam-enable-gps.sh
#   pi/systemd/ghostcam-camera.service                -> /etc/systemd/system/
#   pi/image/files/ghostcam-gps.service               -> /etc/systemd/system/
#   pi/image/files/no-connectivity-check.conf         -> /etc/NetworkManager/conf.d/
#   pi/image/files/99-keep-cellular-route             -> /etc/NetworkManager/dispatcher.d/

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "${SCRIPT_DIR}")"
PI_FILES="${PROJECT_ROOT}/pi/image/files"
PI_SYSTEMD="${PROJECT_ROOT}/pi/systemd"
# The Go camera ships as a single static binary cross-compiled from
# camera/ for linux/arm64 and dropped at /usr/local/bin/ghostcam-camera.
# No venv, no Python, no system dependencies beyond ffmpeg + rpicam-vid.
CAMERA_PKG="${PROJECT_ROOT}/camera"
CAMERA_BIN_NAME="ghostcam-camera"

SSH_OPTS="-o StrictHostKeyChecking=accept-new -o ConnectTimeout=10"

# Source .pi.env defaults if it exists
if [ -f "${PROJECT_ROOT}/.pi.env" ]; then
    # shellcheck disable=SC1091
    source "${PROJECT_ROOT}/.pi.env"
fi

# require_pi_host bails out with a helpful message when PI_HOST hasn't
# been resolved by either the .pi.env file or a CLI positional arg.
# Called from every per-command branch in the case block below so any
# command — `deploy`, `logs`, etc. — fails fast with the same actionable
# error instead of trying to SSH to whatever a stale env var pointed at.
require_pi_host() {
    if [ -z "${PI_HOST:-}" ]; then
        cat >&2 <<'HINT'
ERROR: PI_HOST is not set.

  Copy the example file and fill in your Pi's LAN IP / mDNS name:

      cp .pi.env.example .pi.env
      $EDITOR .pi.env

  Or pass it inline (overrides .pi.env for this invocation):

      ./scripts/pi.sh <command> 192.168.1.42

HINT
        exit 2
    fi
}

# --- Helpers ---

pi_ssh() {
    sshpass -p "${PI_PASSWORD}" ssh ${SSH_OPTS} "${PI_USER}@${PI_HOST}" "$@"
}

pi_scp() {
    sshpass -p "${PI_PASSWORD}" scp ${SSH_OPTS} "$1" "${PI_USER}@${PI_HOST}:$2"
}

check_sshpass() {
    if ! command -v sshpass &> /dev/null; then
        echo "ERROR: sshpass not found. Install it:"
        echo "  macOS: brew install hudochenkov/sshpass/sshpass"
        echo "  Linux: sudo apt install sshpass"
        exit 1
    fi
}

check_connection() {
    echo "Connecting to ${PI_USER}@${PI_HOST}..."
    if ! pi_ssh "echo ok" > /dev/null 2>&1; then
        echo "ERROR: Cannot connect to ${PI_USER}@${PI_HOST}"
        exit 1
    fi
    echo "Connected."
}

stop_camera() {
    echo "Stopping camera..."
    pi_ssh "sudo systemctl stop ghostcam-camera 2>/dev/null || sudo pkill -f '[g]hostcam-camera' 2>/dev/null || true"
}

build_and_deploy() {
    # Pure-Go camera daemon. Cross-compile for linux/arm64, scp the binary,
    # drop it at /usr/local/bin/ghostcam-camera. No venv, no Python, no cgo.
    local dist_dir
    dist_dir="$(mktemp -d -t ghostcam-camera-build.XXXXXX)"
    local bin_path="${dist_dir}/${CAMERA_BIN_NAME}"
    trap 'rm -rf "${dist_dir}"' RETURN

    echo "Cross-compiling ${CAMERA_BIN_NAME} (linux/arm64)..."
    if ! (cd "${PROJECT_ROOT}" && \
          CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
          go build -trimpath -ldflags='-s -w' -o "${bin_path}" ./camera 2>&1 | tail -10); then
        echo "ERROR: go build failed"
        exit 1
    fi
    if [ ! -x "${bin_path}" ]; then
        echo "ERROR: build did not produce ${bin_path}"
        exit 1
    fi
    local size_mb
    size_mb=$(du -m "${bin_path}" | cut -f1)
    echo "Built ${CAMERA_BIN_NAME} (${size_mb} MB) — deploying to ${PI_HOST}..."

    pi_scp "${bin_path}" "/tmp/${CAMERA_BIN_NAME}"
    pi_ssh "
set -e
# Replace the binary atomically (mv is atomic on the same filesystem).
sudo install -m 0755 /tmp/${CAMERA_BIN_NAME} /usr/local/bin/${CAMERA_BIN_NAME}
rm -f /tmp/${CAMERA_BIN_NAME}
"
    echo "Deployed."
}

# --- Commands ---

cmd_setup() {
    check_connection
    stop_camera

    # --- Install system packages ---
    echo ""
    echo "=== Installing system packages ==="

    # ffmpeg: video encoding pipeline (rpicam-vid → ffmpeg → segments + WHIP).
    # The camera daemon is a statically linked Go binary with no runtime deps.
    local packages="libasound2 alsa-utils gpsd gpsd-clients modemmanager libqmi-utils usb-modeswitch network-manager curl jq htop ffmpeg"

    # Check for video capture tool
    local has_video
    has_video=$(pi_ssh "command -v rpicam-vid >/dev/null 2>&1 && echo rpicam || (command -v libcamera-vid >/dev/null 2>&1 && echo libcamera) || echo none")
    if [ "${has_video}" = "none" ]; then
        echo "  No video capture tool found, will try rpicam-apps then libcamera-apps"
        packages="${packages} rpicam-apps"
    else
        echo "  Video capture: ${has_video}"
    fi

    echo "  Packages: ${packages}"
    pi_ssh "sudo apt-get update -qq && sudo apt-get install -y -qq ${packages} 2>&1 | tail -5"

    # --- Swap setup (1GB) ---
    echo ""
    echo "=== Configuring swap (1GB) ==="
    pi_ssh "
if command -v dphys-swapfile >/dev/null 2>&1; then
    sudo sed -i 's/^CONF_SWAPSIZE=.*/CONF_SWAPSIZE=1024/' /etc/dphys-swapfile
    sudo dphys-swapfile setup
    sudo dphys-swapfile swapon
    echo 'Swap configured via dphys-swapfile'
elif [ ! -f /swapfile ] || [ \$(stat -c%s /swapfile 2>/dev/null || echo 0) -lt 1073741824 ]; then
    sudo swapoff /swapfile 2>/dev/null || true
    sudo rm -f /swapfile
    sudo fallocate -l 1G /swapfile
    sudo chmod 600 /swapfile
    sudo mkswap /swapfile
    sudo swapon /swapfile
    grep -q '/swapfile' /etc/fstab || echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
    echo 'Swap configured via swapfile'
else
    echo 'Swap already configured'
fi
free -h | grep -i swap
"

    # --- Create the ghostcam system user ---
    # The systemd unit runs the daemon as `ghostcam` (not the SSH user)
    # to match the .deb's postinst layout. Mirror its setup here so
    # `pi.sh deploy` continues to work on Pis that were never apt-installed.
    echo ""
    echo "=== Creating ghostcam system user ==="
    pi_ssh "
set -e
if ! getent group ghostcam >/dev/null; then sudo addgroup --system ghostcam; fi
if ! getent passwd ghostcam >/dev/null; then
    sudo adduser --system --ingroup ghostcam \
        --home /var/ghostcam --no-create-home \
        --shell /usr/sbin/nologin ghostcam
fi
for g in video audio dialout; do
    if getent group \"\$g\" >/dev/null; then sudo adduser ghostcam \"\$g\" >/dev/null 2>&1 || true; fi
done
sudo install -d -o ghostcam -g ghostcam -m 0750 /var/ghostcam
sudo chown -R ghostcam:ghostcam /var/ghostcam
"
    echo "  ghostcam user/group ready, /var/ghostcam owned by ghostcam"

    # --- Deploy system configs ---
    echo ""
    echo "=== Deploying system configs ==="

    # NetworkManager
    pi_scp "${PI_FILES}/no-connectivity-check.conf" "/tmp/no-connectivity-check.conf"
    pi_ssh "sudo mv /tmp/no-connectivity-check.conf /etc/NetworkManager/conf.d/no-connectivity-check.conf"
    echo "  Installed no-connectivity-check.conf"

    pi_scp "${PI_FILES}/99-keep-cellular-route" "/tmp/99-keep-cellular-route"
    pi_ssh "sudo mv /tmp/99-keep-cellular-route /etc/NetworkManager/dispatcher.d/99-keep-cellular-route && sudo chmod 755 /etc/NetworkManager/dispatcher.d/99-keep-cellular-route"
    echo "  Installed 99-keep-cellular-route"

    pi_ssh "sudo systemctl reload NetworkManager 2>/dev/null || sudo systemctl restart NetworkManager"

    # GPS
    pi_scp "${PI_FILES}/gpsd.conf" "/tmp/gpsd.conf"
    pi_ssh "sudo mv /tmp/gpsd.conf /etc/default/gpsd"
    echo "  Installed gpsd.conf"

    pi_scp "${PI_FILES}/ghostcam-enable-gps.sh" "/tmp/ghostcam-enable-gps.sh"
    pi_ssh "sudo mv /tmp/ghostcam-enable-gps.sh /usr/local/bin/ghostcam-enable-gps.sh && sudo chmod 755 /usr/local/bin/ghostcam-enable-gps.sh"
    echo "  Installed ghostcam-enable-gps.sh"

    # Systemd services
    pi_scp "${PI_FILES}/ghostcam-gps.service" "/tmp/ghostcam-gps.service"
    pi_ssh "sudo mv /tmp/ghostcam-gps.service /etc/systemd/system/ghostcam-gps.service"
    echo "  Installed ghostcam-gps.service"

    pi_scp "${PI_SYSTEMD}/ghostcam-camera.service" "/tmp/ghostcam-camera.service"
    pi_ssh "sudo mv /tmp/ghostcam-camera.service /etc/systemd/system/ghostcam-camera.service"
    echo "  Installed ghostcam-camera.service"

    # --- Create environment file ---
    echo ""
    echo "=== Configuring environment ==="
    local server_addr="${GHOSTCAM_SERVER_ADDR:-}"
    if [ -z "${server_addr}" ]; then
        echo -n "  Enter server address (host:port): "
        read -r server_addr
    fi
    # Sanitize server_addr — strip newlines, validate format
    server_addr="${server_addr//$'\n'/}"
    server_addr="${server_addr//$'\r'/}"
    if [ -n "${server_addr}" ] && ! [[ "${server_addr}" =~ ^[a-zA-Z0-9._:-]+$ ]]; then
        echo "  ERROR: Invalid server address format: ${server_addr}"
        exit 1
    fi
    if [ -n "${server_addr}" ]; then
        pi_ssh "sudo mkdir -p /etc/ghostcam && sudo tee /etc/ghostcam/env > /dev/null << EOF
GHOSTCAM_DATA_DIR=/var/ghostcam
GHOSTCAM_SERVER_ADDR=${server_addr}
RUST_LOG=camera=info
EOF"
        echo "  Written /etc/ghostcam/env (server: ${server_addr})"
    else
        echo "  WARNING: No server address set. Edit /etc/ghostcam/env on the Pi before starting."
        pi_ssh "sudo mkdir -p /etc/ghostcam && sudo tee /etc/ghostcam/env > /dev/null << EOF
GHOSTCAM_DATA_DIR=/var/ghostcam
# GHOSTCAM_SERVER_ADDR=<your-server>:4433
RUST_LOG=camera=info
EOF"
    fi

    # --- Enable services ---
    echo ""
    echo "=== Enabling services ==="
    pi_ssh "sudo systemctl daemon-reload"
    pi_ssh "sudo systemctl enable gpsd ghostcam-gps ghostcam-camera"
    echo "  Enabled: gpsd, ghostcam-gps, ghostcam-camera"

    # Start GPS services
    pi_ssh "sudo systemctl start gpsd 2>/dev/null || true"
    pi_ssh "sudo systemctl start ghostcam-gps 2>/dev/null || true"

    # --- Verify GPS ---
    echo ""
    echo "=== Verifying GPS ==="
    local gps_port
    gps_port=$(pi_ssh "ls /dev/ttyUSB1 2>/dev/null && echo /dev/ttyUSB1 || echo ''")
    if [ -n "${gps_port}" ]; then
        echo "  GPS serial port: ${gps_port}"
        echo "  Checking for GPS data..."
        local gps_check
        gps_check=$(pi_ssh "timeout 10 gpspipe -w 2>/dev/null | grep -m1 TPV || echo 'no TPV'") || true
        if echo "${gps_check}" | grep -q '"lat"'; then
            echo "  GPS fix acquired"
        elif echo "${gps_check}" | grep -q 'TPV'; then
            echo "  GPS connected, no fix yet (needs sky visibility)"
        else
            echo "  No GPS data yet. Check antenna and modem."
        fi
    else
        echo "  No GPS serial port (/dev/ttyUSB1), skipping"
    fi

    # --- Build and deploy ---
    echo ""
    echo "=== Building and deploying ==="
    build_and_deploy

    # --- Start camera ---
    echo ""
    echo "=== Starting camera ==="
    pi_ssh "sudo systemctl start ghostcam-camera"

    echo ""
    echo "=== Setup complete ==="
    echo ""
    echo "Camera is running. Check status:"
    echo "  ./scripts/pi.sh status"
    echo ""
    echo "Quick deploy after code changes:"
    echo "  ./scripts/pi.sh deploy"
    echo ""
    echo "View logs:"
    echo "  ./scripts/pi.sh logs"
}

cmd_deploy() {
    check_connection
    stop_camera
    build_and_deploy

    echo ""
    echo "Starting camera..."
    pi_ssh "sudo systemctl start ghostcam-camera"

    echo ""
    echo "Tailing logs (Ctrl+C to stop)..."
    pi_ssh "journalctl -u ghostcam-camera -f --no-pager"
}

cmd_logs() {
    check_connection
    pi_ssh "journalctl -u ghostcam-camera -f --no-pager"
}

cmd_status() {
    check_connection
    pi_ssh "
echo '=== Camera ==='
systemctl status ghostcam-camera --no-pager -l 2>/dev/null || echo 'ghostcam-camera: not found'
echo ''
echo '=== GPS ==='
systemctl status gpsd --no-pager 2>/dev/null || echo 'gpsd: not found'
timeout 3 gpspipe -w -n 1 2>/dev/null || echo 'no GPS fix'
echo ''
echo '=== Network ==='
nmcli device status 2>/dev/null || echo 'NetworkManager not available'
ip route | grep default
echo ''
echo '=== System ==='
uptime
free -h
df -h /
vcgencmd measure_temp 2>/dev/null || cat /sys/class/thermal/thermal_zone0/temp 2>/dev/null || echo 'temp: unknown'
"
}

cmd_wifi_off() {
    local duration="${1:-60}"

    check_connection

    local wifi_conn log="/tmp/wifi-toggle.log"
    wifi_conn=$(pi_ssh "nmcli -t -f NAME,DEVICE con show --active 2>/dev/null | grep wlan0 | cut -d: -f1 || echo ''")
    if [ -z "${wifi_conn}" ]; then
        echo "ERROR: No active WiFi connection on wlan0"
        exit 1
    fi

    echo "Dropping WiFi (${wifi_conn}) for ${duration}s with auto-recovery."
    echo "SSH will disconnect -- reconnect after ~$((duration + 10))s."
    echo ""

    # No sudo here — `nmcli connection up/down` is authorized via the
    # polkit rule shipped in pi/debian/polkit/49-ghostcam-nm.rules
    # (any netdev member, which ghostcam is, may control NetworkManager).
    # sudo would re-introduce the cargocam/ghostcam-server#28 silent
    # no-op: this script body is nohup'd with no TTY, and dev-image
    # sudo requires a password.
    pi_ssh "
cat > /tmp/wifi-toggle-run.sh << 'SCRIPT'
#!/bin/bash
echo \"[\$(date -Iseconds)] Dropping WiFi (\$2) for \${1}s\" > \"\$3\"
nmcli connection down \"\$2\" >> \"\$3\" 2>&1
sleep \"\$1\"
echo \"[\$(date -Iseconds)] Restoring WiFi (\$2)\" >> \"\$3\"
nmcli connection up \"\$2\" >> \"\$3\" 2>&1
echo \"[\$(date -Iseconds)] Done\" >> \"\$3\"
SCRIPT
chmod +x /tmp/wifi-toggle-run.sh
nohup /tmp/wifi-toggle-run.sh '${duration}' '${wifi_conn}' '${log}' </dev/null >/dev/null 2>&1 &
echo \"Launched (PID=\$!)\"
"

    echo ""
    echo "Monitor camera failover in another terminal:"
    echo "  ./scripts/pi.sh logs"
    echo ""
    echo "Check WiFi toggle results after ~$((duration + 10))s:"
    echo "  ./scripts/pi.sh ssh   # then: cat ${log}"
}

cmd_restart() {
    check_connection
    echo "Restarting ghostcam-camera..."
    pi_ssh "sudo systemctl restart ghostcam-camera"
    echo "Done. Check status with: ./scripts/pi.sh status"
}

cmd_ssh() {
    sshpass -p "${PI_PASSWORD}" ssh ${SSH_OPTS} "${PI_USER}@${PI_HOST}"
}

cmd_unenroll() {
    check_connection
    echo "Clearing enrollment state (identity keypair preserved)..."
    pi_ssh "sudo systemctl stop ghostcam-camera 2>/dev/null || true; rm -f /var/ghostcam/api_key /var/ghostcam/device_id /var/ghostcam/server_url /var/ghostcam/provision_token /var/ghostcam/user.crt /var/ghostcam/user.key /var/ghostcam/server.addr /var/ghostcam/server_fingerprint; sudo systemctl start ghostcam-camera"
    echo "Done. Camera will re-enroll on next connection (same keypair + device ID)."
}

# Resolve a "local" / "prod" / arbitrary-URL target into the URL the
# Pi should point at. "local" reads GHOSTCAM_PUBLIC_IP from .env so the
# Pi can reach the docker-compose server over LAN; falls back to
# `ipconfig getifaddr en0` (macOS) or `hostname -I` (Linux) when .env
# is missing the var.
resolve_target_url() {
    local target="$1"
    case "$target" in
        prod)
            echo "https://ghostcam.fly.dev"
            ;;
        local)
            local lan_ip=""
            if [ -f "${PROJECT_ROOT}/.env" ]; then
                lan_ip="$(grep '^GHOSTCAM_PUBLIC_IP=' "${PROJECT_ROOT}/.env" 2>/dev/null | head -1 | cut -d= -f2- | tr -d '"' || true)"
            fi
            if [ -z "$lan_ip" ] && [ -n "${GHOSTCAM_PUBLIC_IP:-}" ]; then
                lan_ip="${GHOSTCAM_PUBLIC_IP}"
            fi
            if [ -z "$lan_ip" ] && command -v ipconfig &>/dev/null; then
                lan_ip="$(ipconfig getifaddr en0 2>/dev/null || true)"
            fi
            if [ -z "$lan_ip" ] && command -v hostname &>/dev/null; then
                lan_ip="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
            fi
            if [ -z "$lan_ip" ]; then
                echo "ERROR: could not detect dev-machine LAN IP. Set GHOSTCAM_PUBLIC_IP in .env or pass a full URL instead of 'local'." >&2
                return 1
            fi
            echo "http://${lan_ip}:3000"
            ;;
        http://*|https://*)
            echo "$target"
            ;;
        *)
            echo "ERROR: unrecognized target '$target'. Use 'local', 'prod', or a full URL." >&2
            return 1
            ;;
    esac
}

cmd_point_at() {
    local target="${1:-}"
    if [ -z "$target" ]; then
        echo "Usage: $0 point-at <local|prod|URL> [HOST] [USER] [PASS]"
        echo "  local — http://<your-LAN-IP>:3000 (resolved from .env or ipconfig)"
        echo "  prod  — https://ghostcam.fly.dev"
        echo "  URL   — any full URL (e.g. https://staging.example.com)"
        exit 1
    fi
    local url
    url="$(resolve_target_url "$target")" || exit 1

    check_connection
    echo "Repointing ${PI_HOST} at ${url}"
    echo "(identity keypair preserved — you'll need to re-pair the camera in the target server's dashboard)"

    # 1. Stop the service before mutating any state.
    # 2. Update /var/ghostcam/server_url (the file the camera reads at
    #    startup when env doesn't set GHOSTCAM_SERVER_URL).
    # 3. Rewrite /etc/ghostcam/env.d/00-server.conf — the installer-
    #    managed drop-in introduced for the env-preservation fix. Older
    #    units only read /etc/ghostcam/env directly, so we ALSO patch
    #    GHOSTCAM_SERVER_URL there for back-compat with Pis still on
    #    the pre-drop-in systemd unit. Either way, operator-added keys
    #    in /etc/ghostcam/env stay intact.
    # 4. Clear the enrollment files so the camera enters provisioning
    #    mode against the new server. identity_key{,pub} stay because
    #    the device ID is derived from them and is server-agnostic.
    # 5. Restart. The camera will sit waiting for a provision token
    #    delivered via QR or BT in the target server's dashboard.
    pi_ssh "sudo bash -c '
        set -e
        systemctl stop ghostcam-camera 2>/dev/null || true
        install -d -m 0755 -o ghostcam -g ghostcam /var/ghostcam
        printf %s \"${url}\" > /var/ghostcam/server_url
        chown ghostcam:ghostcam /var/ghostcam/server_url
        chmod 0600 /var/ghostcam/server_url
        install -d -m 0755 /etc/ghostcam /etc/ghostcam/env.d
        # Drop-in for the new layout (preferred path; harmless on old units).
        cat > /etc/ghostcam/env.d/00-server.conf <<EOF
GHOSTCAM_SERVER_URL=${url}
GHOSTCAM_DATA_DIR=/var/ghostcam
EOF
        chmod 0644 /etc/ghostcam/env.d/00-server.conf
        # In-place patch for /etc/ghostcam/env, in case the unit is the
        # pre-drop-in version that only reads that single file. Add the
        # SERVER_URL line if missing; replace if present. Preserves all
        # other operator-added keys.
        touch /etc/ghostcam/env
        if grep -q \"^GHOSTCAM_SERVER_URL=\" /etc/ghostcam/env; then
            sed -i \"s|^GHOSTCAM_SERVER_URL=.*|GHOSTCAM_SERVER_URL=${url}|\" /etc/ghostcam/env
        else
            printf \"GHOSTCAM_SERVER_URL=%s\\n\" \"${url}\" >> /etc/ghostcam/env
        fi
        rm -f /var/ghostcam/api_key /var/ghostcam/device_id /var/ghostcam/provision_token /var/ghostcam/user.crt /var/ghostcam/user.key /var/ghostcam/server.addr /var/ghostcam/server_fingerprint
        systemctl daemon-reload
        systemctl start ghostcam-camera
    '"
    echo
    echo "Done. Next steps:"
    echo "  1. Open ${url}/ in your browser"
    echo "  2. Pair the camera via QR or Bluetooth in the dashboard"
    echo "  3. Tail logs to watch enrollment: ./scripts/pi.sh logs"
}

cmd_flash_dev() {
    # Flash a production .img(.xz) onto an SD card AND drop the boot-partition
    # touchfiles that ghostcam-firstboot.sh consumes to enable sshd. The
    # production image has sshd installed-but-disabled by default; this
    # wrapper is the dev-only path to make a unit you can SSH into.
    local img="${1:-}" device="${2:-}" key="${3:-${HOME}/.ssh/id_ed25519.pub}"

    if [ -z "${img}" ] || [ -z "${device}" ]; then
        echo "Usage: $0 flash-dev <IMG> <DEVICE> [KEY]"
        echo "  IMG     path to .img or .img.xz"
        echo "  DEVICE  raw SD device (e.g. /dev/disk6, /dev/sdb)"
        echo "  KEY     public key to bake into authorized_keys (default ~/.ssh/id_ed25519.pub)"
        exit 1
    fi
    if [ ! -f "${img}" ]; then
        echo "ERROR: image not found: ${img}"
        exit 1
    fi
    if [ ! -b "${device}" ] && [ ! -c "${device}" ]; then
        echo "ERROR: ${device} is not a block/char device. Pass the whole-disk node (e.g. /dev/disk6)."
        exit 1
    fi
    if [ ! -f "${key}" ]; then
        echo "WARNING: ${key} not found — SSH will only accept the password."
        key=""
    fi

    echo "Image:  ${img}"
    echo "Device: ${device}"
    [ -n "${key}" ] && echo "Key:    ${key}"
    echo ""
    echo "About to ERASE ${device} and flash ${img}. Press Ctrl+C within 5s to abort."
    sleep 5

    case "$(uname -s)" in
        Darwin)
            diskutil unmountDisk "${device}" || true
            ;;
        Linux)
            for p in "${device}"?*; do [ -e "$p" ] && sudo umount "$p" 2>/dev/null || true; done
            ;;
    esac

    echo "Flashing (this can take 5-10 min on USB 2.0)..."
    if [[ "${img}" == *.xz ]]; then
        if command -v pv >/dev/null 2>&1; then
            xz -dc "${img}" | pv | sudo dd of="${device}" bs=4m
        else
            xz -dc "${img}" | sudo dd of="${device}" bs=4m
        fi
    else
        sudo dd if="${img}" of="${device}" bs=4m status=progress 2>/dev/null \
            || sudo dd if="${img}" of="${device}" bs=4m
    fi
    sync
    echo "Flashed."

    case "$(uname -s)" in
        Darwin)
            diskutil unmountDisk "${device}" 2>/dev/null || true
            diskutil mountDisk "${device}"
            local boot_mount="/Volumes/bootfs"
            if [ ! -d "${boot_mount}" ]; then
                boot_mount="$(diskutil info "${device}s1" 2>/dev/null | awk -F': *' '/Mount Point/ {print $2}')"
            fi
            ;;
        Linux)
            local part="${device}1"
            [ -e "${part}" ] || part="${device}p1"
            local boot_mount="/mnt/ghostcam-boot.$$"
            sudo mkdir -p "${boot_mount}"
            sudo mount "${part}" "${boot_mount}"
            ;;
    esac

    if [ ! -d "${boot_mount}" ]; then
        echo "ERROR: could not locate boot partition mount point."
        exit 1
    fi
    echo "Boot partition mounted at ${boot_mount}"

    sudo touch "${boot_mount}/ssh"
    echo "  + wrote ${boot_mount}/ssh (enables sshd on first boot)"
    if [ -n "${key}" ]; then
        sudo cp "${key}" "${boot_mount}/authorized_keys"
        echo "  + wrote ${boot_mount}/authorized_keys (installs to ~ghostcam/.ssh/)"
    fi
    sync

    case "$(uname -s)" in
        Darwin) diskutil eject "${device}" ;;
        Linux)  sudo umount "${boot_mount}" && sudo rmdir "${boot_mount}" ;;
    esac

    echo ""
    echo "Done. Insert the SD into the Pi and power on. After ~60s:"
    echo "  ssh ghostcam@<pi-ip>   # key-based if a key was baked in, otherwise password 'Ghostcam1!'"
}

# --- Main ---

CMD="${1:-help}"
shift 2>/dev/null || true

case "${CMD}" in
    setup)
        PI_HOST="${1:-${PI_HOST:-}}"
        PI_USER="${2:-${PI_USER:-ghostcam}}"
        PI_PASSWORD="${3:-${PI_PASSWORD:-Ghostcam1!}}"
        require_pi_host
        check_sshpass
        cmd_setup
        ;;
    deploy)
        PI_HOST="${1:-${PI_HOST:-}}"
        PI_USER="${2:-${PI_USER:-ghostcam}}"
        PI_PASSWORD="${3:-${PI_PASSWORD:-Ghostcam1!}}"
        require_pi_host
        check_sshpass
        cmd_deploy
        ;;
    logs)
        PI_HOST="${1:-${PI_HOST:-}}"
        PI_USER="${2:-${PI_USER:-ghostcam}}"
        PI_PASSWORD="${3:-${PI_PASSWORD:-Ghostcam1!}}"
        require_pi_host
        check_sshpass
        cmd_logs
        ;;
    status)
        PI_HOST="${1:-${PI_HOST:-}}"
        PI_USER="${2:-${PI_USER:-ghostcam}}"
        PI_PASSWORD="${3:-${PI_PASSWORD:-Ghostcam1!}}"
        require_pi_host
        check_sshpass
        cmd_status
        ;;
    wifi-off)
        DURATION="${1:-60}"
        PI_HOST="${2:-${PI_HOST:-}}"
        PI_USER="${3:-${PI_USER:-ghostcam}}"
        PI_PASSWORD="${4:-${PI_PASSWORD:-Ghostcam1!}}"
        require_pi_host
        check_sshpass
        cmd_wifi_off "${DURATION}"
        ;;
    restart)
        PI_HOST="${1:-${PI_HOST:-}}"
        PI_USER="${2:-${PI_USER:-ghostcam}}"
        PI_PASSWORD="${3:-${PI_PASSWORD:-Ghostcam1!}}"
        require_pi_host
        check_sshpass
        cmd_restart
        ;;
    ssh)
        PI_HOST="${1:-${PI_HOST:-}}"
        PI_USER="${2:-${PI_USER:-ghostcam}}"
        PI_PASSWORD="${3:-${PI_PASSWORD:-Ghostcam1!}}"
        require_pi_host
        check_sshpass
        cmd_ssh
        ;;
    unenroll)
        PI_HOST="${1:-${PI_HOST:-}}"
        PI_USER="${2:-${PI_USER:-ghostcam}}"
        PI_PASSWORD="${3:-${PI_PASSWORD:-Ghostcam1!}}"
        require_pi_host
        check_sshpass
        cmd_unenroll
        ;;
    flash-dev)
        cmd_flash_dev "$@"
        ;;
    point-at)
        TARGET="${1:-}"
        shift || true
        PI_HOST="${1:-${PI_HOST:-}}"
        PI_USER="${2:-${PI_USER:-ghostcam}}"
        PI_PASSWORD="${3:-${PI_PASSWORD:-Ghostcam1!}}"
        require_pi_host
        check_sshpass
        cmd_point_at "$TARGET"
        ;;
    *)
        echo "Ghostcam Pi Camera Management"
        echo ""
        echo "Usage:"
        echo "  ./scripts/pi.sh setup    [HOST] [USER] [PASS]          # Full provisioning (fresh Pi)"
        echo "  ./scripts/pi.sh deploy   [HOST] [USER] [PASS]          # Cross-compile Go binary + deploy"
        echo "  ./scripts/pi.sh logs     [HOST] [USER] [PASS]          # Stream camera logs"
        echo "  ./scripts/pi.sh status   [HOST] [USER] [PASS]          # Health check"
        echo "  ./scripts/pi.sh wifi-off [SECS] [HOST] [USER] [PASS]   # Toggle WiFi for failover testing"
        echo "  ./scripts/pi.sh restart  [HOST] [USER] [PASS]          # Restart camera service"
        echo "  ./scripts/pi.sh ssh      [HOST] [USER] [PASS]          # Interactive SSH session"
        echo "  ./scripts/pi.sh unenroll [HOST] [USER] [PASS]          # Clear enrollment state"
        echo "  ./scripts/pi.sh point-at <local|prod|URL> [HOST] [USER] [PASS]"
        echo "                                                          # Repoint at another server"
        echo "  ./scripts/pi.sh flash-dev <IMG> <DEVICE> [KEY]         # Flash prod img + enable SSH for dev"
        echo ""
        echo "Configure your Pi: cp .pi.env.example .pi.env and fill in PI_HOST."
        echo "USER defaults to 'ghostcam' and PASS to 'Ghostcam1!' (dev-image conventions)."
        exit 1
        ;;
esac
