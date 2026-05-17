#!/bin/sh
# Boot-time bootstrap for ghostcam Pi images. Runs before NetworkManager
# and bluetooth.service so its work is visible to both when they start.
#
# 1. Unblock rfkill. PiOS-style images frequently boot with the Bluetooth
#    (and sometimes Wi-Fi) adapter rfkill-soft-blocked, which makes
#    adapter.Enable() inside the camera daemon return "operation not
#    possible" and the BT onboarding peripheral never advertises. We can't
#    fix this from the .deb postinst because postinst runs in a chroot
#    during image build, where there's no kernel rfkill state to mutate.
#
# 2. Opt-in SSH bootstrap. The image ships with sshd installed but
#    disabled. Two boot-partition files turn it on:
#       /boot/firmware/ssh              — touchfile, enables sshd and is
#                                         consumed (deleted) on first boot.
#                                         Matches PiOS convention.
#       /boot/firmware/authorized_keys  — installed to ~ghostcam/.ssh/
#                                         authorized_keys (0600,
#                                         owned by ghostcam) and deleted
#                                         from the boot partition so the
#                                         key doesn't sit on plaintext FAT.
#    Either file alone is enough to enable sshd. Production images ship
#    with neither, so they have no remote shell.
#
# 3. Apply an optional Wi-Fi config from /boot/firmware/wifi.conf so the
#    image has an escape hatch when cellular hardware is unavailable
#    (broken antenna, no SIM, lab bring-up). User flow:
#       a. Mount the SD card's boot partition on any OS (FAT — readable
#          everywhere, no ext4 tooling needed).
#       b. Drop a wifi.conf with SSID= and PSK= (shell key=value form).
#       c. Boot. We translate that into a NetworkManager system-connection
#          profile and (on successful connect) delete wifi.conf so the
#          creds don't sit on a plaintext FAT partition forever. NM picks
#          up the new profile when it starts (we run Before=NetworkManager).
#    The primary onboarding path is still BT GATT with WiFi creds in the
#    payload; wifi.conf is a fallback for when BT itself is the thing
#    we're trying to debug.
#
# 4. Always write a status log to /boot/firmware/firstboot.log so the
#    operator can diagnose why WiFi didn't come up by re-mounting the SD
#    card. ALL stderr/stdout from this script + a tail of system state
#    (rfkill, nmcli) gets captured. On failure the wifi.conf is preserved
#    so the user can retry without writing it again.

LOG=/boot/firmware/firstboot.log
exec >>"$LOG" 2>&1
echo
echo "=== ghostcam-firstboot $(date -u +%FT%TZ) ==="

if command -v rfkill >/dev/null 2>&1; then
    echo "--- rfkill list (before) ---"
    rfkill list || true
    echo "--- rfkill unblock all ---"
    rfkill unblock all || true
    echo "--- rfkill list (after) ---"
    rfkill list || true
else
    echo "rfkill not installed"
fi

SSH_TOUCH=/boot/firmware/ssh
SSH_KEYS=/boot/firmware/authorized_keys
GHOSTCAM_SSH_DIR=/home/ghostcam/.ssh
SSH_REQUESTED=

if [ -e "$SSH_TOUCH" ]; then
    echo "found $SSH_TOUCH, consuming"
    rm -f "$SSH_TOUCH"
    SSH_REQUESTED=1
fi

if [ -f "$SSH_KEYS" ]; then
    echo "found $SSH_KEYS, installing for ghostcam user"
    # /home/ghostcam doesn't exist in the deb postinst layout (--no-create-home),
    # but the deb's postinst sets /var/ghostcam as the home. Use the actual
    # passwd entry so we don't get this wrong if the layout changes.
    HOME_DIR="$(getent passwd ghostcam | cut -d: -f6)"
    if [ -z "$HOME_DIR" ]; then
        echo "ghostcam user not found, skipping authorized_keys install"
    else
        install -d -o ghostcam -g ghostcam -m 0700 "$HOME_DIR/.ssh"
        install -m 0600 -o ghostcam -g ghostcam "$SSH_KEYS" "$HOME_DIR/.ssh/authorized_keys"
        rm -f "$SSH_KEYS"
        echo "installed authorized_keys at $HOME_DIR/.ssh/authorized_keys"
        SSH_REQUESTED=1
    fi
fi

if [ -n "$SSH_REQUESTED" ]; then
    systemctl enable --now ssh || systemctl enable --now sshd || true
    echo "sshd enabled"
fi

WIFI_CONF=/boot/firmware/wifi.conf
NM_DIR=/etc/NetworkManager/system-connections

if [ ! -f "$WIFI_CONF" ]; then
    echo "no $WIFI_CONF, skipping wifi bootstrap"
    exit 0
fi

SSID=""
PSK=""
# shellcheck disable=SC1090
. "$WIFI_CONF"

if [ -z "$SSID" ]; then
    echo "$WIFI_CONF present but SSID is empty; aborting"
    exit 0
fi

echo "ssid=$SSID, psk-set=$([ -n "$PSK" ] && echo yes || echo no)"

mkdir -p "$NM_DIR"
PROFILE="$NM_DIR/boot-wifi.nmconnection"
UUID="$(cat /proc/sys/kernel/random/uuid 2>/dev/null || echo 00000000-0000-0000-0000-000000000001)"

{
    echo "[connection]"
    echo "id=boot-wifi"
    echo "uuid=$UUID"
    echo "type=wifi"
    echo "interface-name=wlan0"
    echo "autoconnect=true"
    echo "autoconnect-priority=100"
    # 0 = retry forever. NM's default of 4 is fatal on a headless
    # camera: one bad WPA rekey or AP blip exhausts the cap and the
    # Pi stays off-network until reboot. Discovered when the dev Pi
    # at 10.0.0.229 went silent for ~45 min after a 4-way-handshake
    # failure during routine rekey ("no-secrets" → state=failed).
    echo "autoconnect-retries=0"
    echo ""
    echo "[wifi]"
    echo "mode=infrastructure"
    echo "ssid=$SSID"
    echo ""
    if [ -n "$PSK" ]; then
        echo "[wifi-security]"
        echo "key-mgmt=wpa-psk"
        echo "psk=$PSK"
        echo ""
    fi
    echo "[ipv4]"
    echo "method=auto"
    echo ""
    echo "[ipv6]"
    echo "method=auto"
} > "$PROFILE"
chmod 0600 "$PROFILE"
chown root:root "$PROFILE"

echo "wrote $PROFILE:"
sed 's/^psk=.*/psk=<redacted>/' "$PROFILE"

# Keep wifi.conf so the user can iterate if connection fails. A later
# tail-log unit (ghostcam-firstboot-finalize, if added) can verify the
# connection came up and then clean up. For now we leave it and let
# the user delete it manually after confirming SSH works.

exit 0
