#!/usr/bin/env bash
# Ghostcam camera installer for Raspberry Pi OS Lite (64-bit, Bookworm).
#
# Usage (on the Pi):
#   curl -sSL https://raw.githubusercontent.com/cargocam/ghostcam/main/scripts/install.sh | sudo bash
#
# Or with a specific version:
#   curl -sSL https://raw.githubusercontent.com/cargocam/ghostcam/main/scripts/install.sh | sudo VERSION=v0.1.0-alpha.1 bash

set -euo pipefail

REPO="cargocam/ghostcam-firmware"
VERSION="${VERSION:-latest}"
DATA_DIR="/var/ghostcam"
CONFIG_DIR="/etc/ghostcam"

echo "=== Ghostcam Camera Installer ==="
echo ""

# Detect architecture
ARCH=$(dpkg --print-architecture)
if [ "$ARCH" != "arm64" ]; then
    echo "ERROR: This installer is for arm64 (aarch64) only. Detected: $ARCH"
    exit 1
fi

# Resolve version
if [ "$VERSION" = "latest" ]; then
    echo "Fetching latest release..."
    VERSION=$(curl -sSL "https://api.github.com/repos/$REPO/releases/latest" | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')
    if [ -z "$VERSION" ]; then
        echo "ERROR: Could not determine latest version"
        exit 1
    fi
fi
echo "Installing Ghostcam $VERSION"
echo ""

# Install system dependencies
echo "=== Installing dependencies ==="
apt-get update -qq
apt-get install -y -qq \
    rpicam-apps \
    libasound2 alsa-utils \
    gpsd gpsd-clients \
    modemmanager network-manager libqmi-utils usb-modeswitch \
    curl jq

# Download and install camera binary
echo ""
echo "=== Installing camera binary ==="
DEB_URL="https://github.com/$REPO/releases/download/$VERSION/ghostcam-camera_${VERSION#v}_arm64.deb"
BINARY_URL="https://github.com/$REPO/releases/download/$VERSION/ghostcam-camera-aarch64"

# Try .deb first, fall back to raw binary
TMPDIR=$(mktemp -d)
if curl -sSL -o "$TMPDIR/ghostcam.deb" "$DEB_URL" 2>/dev/null && [ -s "$TMPDIR/ghostcam.deb" ]; then
    dpkg -i "$TMPDIR/ghostcam.deb" || apt-get install -f -y -qq
    echo "Installed from .deb"
elif curl -sSL -o "$TMPDIR/ghostcam-camera" "$BINARY_URL" 2>/dev/null && [ -s "$TMPDIR/ghostcam-camera" ]; then
    install -m 755 "$TMPDIR/ghostcam-camera" /usr/local/bin/ghostcam-camera
    echo "Installed from binary"
else
    echo "ERROR: Could not download camera binary"
    rm -rf "$TMPDIR"
    exit 1
fi
rm -rf "$TMPDIR"

# Create directories
echo ""
echo "=== Configuring system ==="
mkdir -p "$DATA_DIR" "$CONFIG_DIR"

# Detect the user (whoever called sudo, or 'ghostcam' if not found)
INSTALL_USER="${SUDO_USER:-ghostcam}"
if id "$INSTALL_USER" &>/dev/null; then
    chown "$INSTALL_USER:$INSTALL_USER" "$DATA_DIR"
fi

# Write default env file
if [ ! -f "$CONFIG_DIR/env" ]; then
    cat > "$CONFIG_DIR/env" << EOF
GHOSTCAM_DATA_DIR=$DATA_DIR
GHOSTCAM_VIDEO_PROFILE=zero2w
RUST_LOG=camera=info
EOF
fi

# Install systemd service
cat > /etc/systemd/system/ghostcam-camera.service << EOF
[Unit]
Description=Ghostcam Camera
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$INSTALL_USER
Group=$INSTALL_USER
SupplementaryGroups=video dialout audio
ExecStart=/usr/local/bin/ghostcam-camera
Restart=always
RestartSec=5
EnvironmentFile=-/etc/ghostcam/env
Environment=MALLOC_ARENA_MAX=2

[Install]
WantedBy=multi-user.target
EOF

# Install ALSA config for INMP441 I2S mic
cat > /etc/asound.conf << 'EOF'
pcm.!default {
    type asym
    playback.pcm "plughw:0,0"
    capture.pcm "plug_capture"
}
pcm.plug_capture {
    type plug
    slave.pcm "hw:sndrpigooglevoi,0"
}
ctl.!default {
    type hw
    card 0
}
EOF

# Configure I2S in boot config if not already
if ! grep -q "dtparam=i2s=on" /boot/firmware/config.txt 2>/dev/null; then
    echo "dtparam=i2s=on" >> /boot/firmware/config.txt
fi
if ! grep -q "googlevoicehat-soundcard" /boot/firmware/config.txt 2>/dev/null; then
    echo "dtoverlay=googlevoicehat-soundcard" >> /boot/firmware/config.txt
fi

# GPS setup (if SIM7600 modem present)
if [ -e /dev/ttyUSB1 ] || [ -e /dev/ttyUSB0 ]; then
    echo "Modem detected, configuring GPS..."
    cat > /etc/default/gpsd << 'EOF'
DEVICES="/dev/ttyUSB1"
GPSD_OPTIONS="-n"
USBAUTO="true"
EOF

    cat > /usr/local/bin/ghostcam-enable-gps.sh << 'GPSEOF'
#!/bin/bash
# Enable GPS on SIM7600 modem via ModemManager
for i in $(seq 1 30); do
    IDX=$(mmcli -L 2>/dev/null | grep -oP '/Modem/\K\d+' | head -1)
    if [ -n "$IDX" ]; then
        mmcli -m "$IDX" --location-enable-gps-nmea --location-enable-gps-raw 2>/dev/null && echo "GPS enabled on modem $IDX" && exit 0
    fi
    sleep 1
done
echo "WARNING: Could not enable GPS (modem not found after 30s)"
GPSEOF
    chmod +x /usr/local/bin/ghostcam-enable-gps.sh

    cat > /etc/systemd/system/ghostcam-gps.service << 'EOF'
[Unit]
Description=Enable GPS on SIM7600 modem
After=ModemManager.service
Requires=ModemManager.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/ghostcam-enable-gps.sh

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable gpsd ghostcam-gps
fi

# NetworkManager cellular failover
mkdir -p /etc/NetworkManager/conf.d /etc/NetworkManager/dispatcher.d
cat > /etc/NetworkManager/conf.d/no-connectivity-check.conf << 'EOF'
[connectivity]
enabled=false
EOF

cat > /etc/NetworkManager/dispatcher.d/99-keep-cellular-route << 'NMEOF'
#!/bin/bash
IFACE="$1"
ACTION="$2"
if [ "$IFACE" = "wlan0" ] && [ "$ACTION" = "down" ]; then
    GW=$(ip route show dev wwan0 2>/dev/null | grep default | awk '{print $3}')
    if [ -n "$GW" ]; then
        ip route add default via "$GW" dev wwan0 metric 700 2>/dev/null
        logger -t ghostcam "WiFi down — restored cellular route via $GW"
    fi
fi
NMEOF
chmod 755 /etc/NetworkManager/dispatcher.d/99-keep-cellular-route

# Configure swap (1GB)
if [ -f /etc/dphys-swapfile ]; then
    sed -i 's/^CONF_SWAPSIZE=.*/CONF_SWAPSIZE=1024/' /etc/dphys-swapfile
    dphys-swapfile swapoff 2>/dev/null || true
    dphys-swapfile setup 2>/dev/null || true
    dphys-swapfile swapon 2>/dev/null || true
fi

# Add user to required groups
usermod -aG video,dialout,audio "$INSTALL_USER" 2>/dev/null || true

# Enable and start the camera service
systemctl daemon-reload
systemctl enable ghostcam-camera
systemctl start ghostcam-camera

echo ""
echo "=== Installation complete ==="
echo ""
echo "Ghostcam camera is now running."
echo "It will connect to the default server and wait for a QR code to claim it."
echo ""
echo "View logs:  journalctl -u ghostcam-camera -f"
echo "Status:     systemctl status ghostcam-camera"
echo ""
echo "NOTE: If you added I2S mic config, reboot for it to take effect:"
echo "  sudo reboot"
