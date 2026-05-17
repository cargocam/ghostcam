#!/bin/sh
# Late-boot diagnostic snapshot. Runs once after everything else has had
# a chance to settle, dumps current system state (network, BT, camera
# daemon) to /boot/firmware/diag.log so the operator can read it by
# pulling the SD card out and mounting it on any OS. Pairs with
# /boot/firmware/firstboot.log (early-boot bootstrap actions).
LOG=/boot/firmware/diag.log

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
} > "$LOG" 2>&1
