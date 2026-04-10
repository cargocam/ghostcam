#!/usr/bin/env bash
set -euo pipefail

# Ghostcam Pi Camera Management
#
# Single entry point for all Pi operations. Run from the dev machine.
#
# Usage:
#   ./scripts/pi.sh setup    [HOST] [USER] [PASS]          # Full provisioning (fresh Pi)
#   ./scripts/pi.sh deploy   [HOST] [USER] [PASS]          # Quick build + deploy binary
#   ./scripts/pi.sh logs     [HOST] [USER] [PASS]          # Stream camera logs
#   ./scripts/pi.sh status   [HOST] [USER] [PASS]          # Health check
#   ./scripts/pi.sh wifi-off [SECS] [HOST] [USER] [PASS]   # Toggle WiFi for failover testing
#   ./scripts/pi.sh restart  [HOST] [USER] [PASS]          # Restart camera service
#   ./scripts/pi.sh ssh      [HOST] [USER] [PASS]          # Interactive SSH session
#   ./scripts/pi.sh unenroll [HOST] [USER] [PASS]          # Clear enrollment state
#
# Defaults: HOST=10.0.0.229  USER=yurei  PASS=password
#
# Override defaults via .pi.env in the repo root (gitignored):
#   PI_HOST=10.0.0.229
#   PI_USER=yurei
#   PI_PASSWORD=password
#
# Config files deployed to the Pi live in pi/:
#   pi/gpsd.conf                               -> /etc/default/gpsd
#   pi/ghostcam-enable-gps.sh                  -> /usr/local/bin/ghostcam-enable-gps.sh
#   pi/systemd/ghostcam-camera.service         -> /etc/systemd/system/
#   pi/systemd/ghostcam-gps.service            -> /etc/systemd/system/
#   pi/networkmanager/no-connectivity-check.conf -> /etc/NetworkManager/conf.d/
#   pi/networkmanager/99-keep-cellular-route    -> /etc/NetworkManager/dispatcher.d/

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "${SCRIPT_DIR}")"
PI_DIR="${PROJECT_ROOT}/pi"
# Camera binary is cross-compiled with: GOOS=linux GOARCH=arm64 CGO_ENABLED=0

SSH_OPTS="-o StrictHostKeyChecking=accept-new -o ConnectTimeout=10"

# Source .pi.env defaults if it exists
if [ -f "${PROJECT_ROOT}/.pi.env" ]; then
    # shellcheck disable=SC1091
    source "${PROJECT_ROOT}/.pi.env"
fi

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
    local target_bin="${PROJECT_ROOT}/ghostcam-camera"

    echo "Cross-compiling Go camera for linux/arm64..."
    (cd "${PROJECT_ROOT}" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "${target_bin}" ./camera)

    if [ ! -f "${target_bin}" ]; then
        echo "ERROR: Build failed - binary not found at ${target_bin}"
        exit 1
    fi

    echo "Deploying binary to /usr/local/bin/ghostcam-camera..."
    pi_scp "${target_bin}" "/tmp/ghostcam-camera"
    pi_ssh "sudo mv /tmp/ghostcam-camera /usr/local/bin/ghostcam-camera && sudo chmod +x /usr/local/bin/ghostcam-camera"
    rm -f "${target_bin}"
    echo "Deployed."
}

# --- Commands ---

cmd_setup() {
    check_connection
    stop_camera

    # --- Install system packages ---
    echo ""
    echo "=== Installing system packages ==="

    local packages="libasound2 alsa-utils gpsd gpsd-clients modemmanager libqmi-utils usb-modeswitch network-manager curl jq htop"

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

    # --- Add user to groups ---
    echo ""
    echo "=== Configuring user groups ==="
    pi_ssh "sudo usermod -aG video,dialout,audio ${PI_USER}"
    echo "  Added ${PI_USER} to video, dialout, audio groups"

    # --- Deploy system configs ---
    echo ""
    echo "=== Deploying system configs ==="

    # NetworkManager
    pi_scp "${PI_DIR}/networkmanager/no-connectivity-check.conf" "/tmp/no-connectivity-check.conf"
    pi_ssh "sudo mv /tmp/no-connectivity-check.conf /etc/NetworkManager/conf.d/no-connectivity-check.conf"
    echo "  Installed no-connectivity-check.conf"

    pi_scp "${PI_DIR}/networkmanager/99-keep-cellular-route" "/tmp/99-keep-cellular-route"
    pi_ssh "sudo mv /tmp/99-keep-cellular-route /etc/NetworkManager/dispatcher.d/99-keep-cellular-route && sudo chmod 755 /etc/NetworkManager/dispatcher.d/99-keep-cellular-route"
    echo "  Installed 99-keep-cellular-route"

    pi_ssh "sudo systemctl reload NetworkManager 2>/dev/null || sudo systemctl restart NetworkManager"

    # GPS
    pi_scp "${PI_DIR}/gpsd.conf" "/tmp/gpsd.conf"
    pi_ssh "sudo mv /tmp/gpsd.conf /etc/default/gpsd"
    echo "  Installed gpsd.conf"

    pi_scp "${PI_DIR}/ghostcam-enable-gps.sh" "/tmp/ghostcam-enable-gps.sh"
    pi_ssh "sudo mv /tmp/ghostcam-enable-gps.sh /usr/local/bin/ghostcam-enable-gps.sh && sudo chmod 755 /usr/local/bin/ghostcam-enable-gps.sh"
    echo "  Installed ghostcam-enable-gps.sh"

    # Systemd services
    pi_scp "${PI_DIR}/systemd/ghostcam-gps.service" "/tmp/ghostcam-gps.service"
    pi_ssh "sudo mv /tmp/ghostcam-gps.service /etc/systemd/system/ghostcam-gps.service"
    echo "  Installed ghostcam-gps.service"

    pi_scp "${PI_DIR}/systemd/ghostcam-camera.service" "/tmp/ghostcam-camera.service"
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

    pi_ssh "
cat > /tmp/wifi-toggle-run.sh << 'SCRIPT'
#!/bin/bash
echo \"[\$(date -Iseconds)] Dropping WiFi (\$2) for \${1}s\" > \"\$3\"
sudo nmcli connection down \"\$2\" >> \"\$3\" 2>&1
sleep \"\$1\"
echo \"[\$(date -Iseconds)] Restoring WiFi (\$2)\" >> \"\$3\"
sudo nmcli connection up \"\$2\" >> \"\$3\" 2>&1
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
    echo "Clearing enrollment state..."
    pi_ssh "sudo systemctl stop ghostcam-camera 2>/dev/null || true; rm -f /var/ghostcam/api_key /var/ghostcam/device_id /var/ghostcam/server_url /var/ghostcam/provision_token /var/ghostcam/user.crt /var/ghostcam/user.key /var/ghostcam/server.addr /var/ghostcam/server_fingerprint; sudo systemctl start ghostcam-camera"
    echo "Done. Camera will re-enroll on next connection."
}

# --- Main ---

CMD="${1:-help}"
shift 2>/dev/null || true

case "${CMD}" in
    setup)
        PI_HOST="${1:-${PI_HOST:-10.0.0.229}}"
        PI_USER="${2:-${PI_USER:-yurei}}"
        PI_PASSWORD="${3:-${PI_PASSWORD:-password}}"
        check_sshpass
        cmd_setup
        ;;
    deploy)
        PI_HOST="${1:-${PI_HOST:-10.0.0.229}}"
        PI_USER="${2:-${PI_USER:-yurei}}"
        PI_PASSWORD="${3:-${PI_PASSWORD:-password}}"
        check_sshpass
        cmd_deploy
        ;;
    logs)
        PI_HOST="${1:-${PI_HOST:-10.0.0.229}}"
        PI_USER="${2:-${PI_USER:-yurei}}"
        PI_PASSWORD="${3:-${PI_PASSWORD:-password}}"
        check_sshpass
        cmd_logs
        ;;
    status)
        PI_HOST="${1:-${PI_HOST:-10.0.0.229}}"
        PI_USER="${2:-${PI_USER:-yurei}}"
        PI_PASSWORD="${3:-${PI_PASSWORD:-password}}"
        check_sshpass
        cmd_status
        ;;
    wifi-off)
        # First arg is duration, then host/user/pass
        DURATION="${1:-60}"
        PI_HOST="${2:-${PI_HOST:-10.0.0.229}}"
        PI_USER="${3:-${PI_USER:-yurei}}"
        PI_PASSWORD="${4:-${PI_PASSWORD:-password}}"
        check_sshpass
        cmd_wifi_off "${DURATION}"
        ;;
    restart)
        PI_HOST="${1:-${PI_HOST:-10.0.0.229}}"
        PI_USER="${2:-${PI_USER:-yurei}}"
        PI_PASSWORD="${3:-${PI_PASSWORD:-password}}"
        check_sshpass
        cmd_restart
        ;;
    ssh)
        PI_HOST="${1:-${PI_HOST:-10.0.0.229}}"
        PI_USER="${2:-${PI_USER:-yurei}}"
        PI_PASSWORD="${3:-${PI_PASSWORD:-password}}"
        check_sshpass
        cmd_ssh
        ;;
    unenroll)
        PI_HOST="${1:-${PI_HOST:-10.0.0.229}}"
        PI_USER="${2:-${PI_USER:-yurei}}"
        PI_PASSWORD="${3:-${PI_PASSWORD:-password}}"
        check_sshpass
        cmd_unenroll
        ;;
    *)
        echo "Ghostcam Pi Camera Management"
        echo ""
        echo "Usage:"
        echo "  ./scripts/pi.sh setup    [HOST] [USER] [PASS]          # Full provisioning (fresh Pi)"
        echo "  ./scripts/pi.sh deploy   [HOST] [USER] [PASS]          # Quick build + deploy binary"
        echo "  ./scripts/pi.sh logs     [HOST] [USER] [PASS]          # Stream camera logs"
        echo "  ./scripts/pi.sh status   [HOST] [USER] [PASS]          # Health check"
        echo "  ./scripts/pi.sh wifi-off [SECS] [HOST] [USER] [PASS]   # Toggle WiFi for failover testing"
        echo "  ./scripts/pi.sh restart  [HOST] [USER] [PASS]          # Restart camera service"
        echo "  ./scripts/pi.sh ssh      [HOST] [USER] [PASS]          # Interactive SSH session"
        echo "  ./scripts/pi.sh unenroll [HOST] [USER] [PASS]          # Clear enrollment state"
        echo ""
        echo "Defaults: HOST=10.0.0.229  USER=yurei  PASS=password"
        echo "Override via .pi.env in the repo root (gitignored)."
        exit 1
        ;;
esac
