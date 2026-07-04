#!/bin/sh
# Diagnostic snapshot. Dumps current system state (network, BT, camera
# daemon) to /boot/firmware/diag.log so the operator can read it by pulling
# the SD card out and mounting it on any OS. Pairs with
# /boot/firmware/firstboot.log (early-boot bootstrap actions).
#
# Driven by ghostcam-diag.timer: first fire ~90s after boot, then every few
# minutes *while the camera is un-onboarded*. Re-running matters for BT
# onboarding debugging — an un-onboardable camera can't phone home, and the
# operator scanning with their phone can't be timed to a single boot-time
# snapshot, so we keep a fresh one available across the whole onboarding
# window (and the daemon's ~5-min re-advertise restart cycles).
LOG=/boot/firmware/diag.log

# Bound the writes. The timer fires every few minutes; without a stop
# condition an un-onboarded unit that sits powered forever (DOA / warehoused /
# failed onboard) would rewrite the FAT boot partition indefinitely. We stop
# rewriting — but always keep the last snapshot — once EITHER:
#   * the camera is provisioned ([ -s ]: a non-empty server_url; a zero-byte
#     file written mid-provision must NOT count as done), because server-side
#     telemetry then supersedes this file; OR
#   * the onboarding window has clearly passed (uptime > 30 min). An operator
#     onboards shortly after power-on, and a retry is a fresh boot (uptime
#     resets), so 30 min of per-boot snapshots covers every real attempt.
# The first run always writes (no $LOG yet).
if [ -f "$LOG" ]; then
    if [ -s /var/ghostcam/server_url ]; then
        exit 0
    fi
    up=$(cut -d. -f1 /proc/uptime 2>/dev/null || echo 0)
    case "$up" in ''|*[!0-9]*) up=0 ;; esac
    if [ "$up" -gt 1800 ]; then
        exit 0
    fi
fi

# Write to a temp file and rename over $LOG so a reader that pulls the SD card
# mid-snapshot never sees a half-written (truncated) diag.log — rename is
# atomic on the FAT boot partition.
TMP="$LOG.tmp"

{
    echo "=== ghostcam-diag $(date -u +%FT%TZ) ==="
    echo
    echo "--- ip a ---"
    ip a
    echo
    echo "--- ip route ---"
    ip route
    echo
    echo "--- nmcli device status ---"
    nmcli device status 2>&1
    echo
    echo "--- nmcli connection show ---"
    nmcli connection show 2>&1
    echo
    echo "--- nmcli connection show --active ---"
    nmcli connection show --active 2>&1
    echo
    echo "--- nmcli device wifi list ---"
    nmcli device wifi list 2>&1
    echo
    echo "--- rfkill list ---"
    rfkill list 2>&1 || true
    echo
    echo "--- bluetoothctl show ---"
    bluetoothctl show 2>&1 || true
    echo
    # BT onboarding forensics. An un-onboardable camera can't phone home,
    # so this snapshot (pulled off the SD card's FAT boot partition) is the
    # only trail. The load-bearing values:
    #   Adapter1.Powered        — is the controller up at all?
    #   LEAdvertisingManager1.ActiveInstances — is an advert actually on the
    #                             air? 0 while the daemon claims to advertise
    #                             is the silent-failure "Pi doesn't show up".
    echo "--- busctl org.bluez /org/bluez/hci0 Adapter1 (Powered/Discoverable/Alias) ---"
    for p in Powered Discoverable Discovering Alias Address; do
        printf '%s=' "$p"
        busctl --system get-property org.bluez /org/bluez/hci0 org.bluez.Adapter1 "$p" 2>&1 || true
    done
    echo
    echo "--- busctl org.bluez /org/bluez/hci0 LEAdvertisingManager1 (Active/Supported) ---"
    for p in ActiveInstances SupportedInstances; do
        printf '%s=' "$p"
        busctl --system get-property org.bluez /org/bluez/hci0 org.bluez.LEAdvertisingManager1 "$p" 2>&1 || true
    done
    echo
    echo "--- dmesg | grep -iE 'bluetooth|hci|brcm|firmware' (BT driver/firmware load) ---"
    dmesg 2>/dev/null | grep -iE 'bluetooth|hci|brcm|firmware' | tail -40 2>&1 || true
    echo
    echo "--- camera journal, BT lines only (whole boot) ---"
    journalctl -b -u ghostcam-camera --no-pager 2>/dev/null \
        | grep -iE 'BT |bluetooth|advertis|adapter|provision|onboard' | tail -80 2>&1 || true
    echo
    echo "--- systemctl status NetworkManager bluetooth ssh ghostcam-camera --no-pager ---"
    systemctl status NetworkManager bluetooth ssh ghostcam-camera --no-pager 2>&1 || true
    echo
    echo "--- journalctl -b -u ghostcam-camera --no-pager -n 200 ---"
    journalctl -b -u ghostcam-camera --no-pager -n 200 2>&1 || true
    echo
    echo "--- journalctl -b -u bluetooth --no-pager -n 50 ---"
    journalctl -b -u bluetooth --no-pager -n 50 2>&1 || true
    echo
    echo "--- end ---"
} > "$TMP" 2>&1
mv -f "$TMP" "$LOG"
