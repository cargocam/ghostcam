#!/bin/bash
# Enable GPS on SIM7600 modem via ModemManager.
# ModemManager doesn't persist location settings across reboots, so
# this script runs at boot via ghostcam-gps.service.
#
# Earlier versions only waited for `mmcli -L` to list the modem before
# issuing `--location-enable-gps-*`, but enabling GPS requires the
# modem to be in state `enabled` (or beyond — `registered`, `connected`)
# rather than merely `detected`. On a cold boot the script regularly
# raced ModemManager's enable step and exited with
#   error: modem not enabled yet
# leaving GPS off for the rest of the boot. Wait for the actual state
# transition and retry the enable call a few times to absorb the
# remaining stragglers.

# Wait for ModemManager to list a modem AND for it to reach state
# `enabled` or later. Up to 60s total — registration on a cold
# cellular boot can be slow.
IDX=""
for i in $(seq 1 60); do
    IDX=$(mmcli -L 2>/dev/null | grep -oP '/Modem/\K[0-9]+' | head -1)
    if [ -n "$IDX" ]; then
        STATE=$(mmcli -m "$IDX" 2>/dev/null \
            | sed -n 's/.*state: //p' \
            | head -1 \
            | tr -d ' ' \
            | sed -E 's/\x1b\[[0-9;]*m//g')
        case "$STATE" in
            enabled|searching|registered|connecting|connected)
                break
                ;;
        esac
    fi
    sleep 1
done

if [ -z "$IDX" ]; then
    echo "No modem found after 60s"
    exit 1
fi

echo "Enabling GPS on modem $IDX (state=$STATE)"

# Retry the enable command for a further 30s — even after `enabled`,
# ModemManager occasionally returns "operation not allowed" while it
# settles plugin state. Idempotent on success: re-enabling already-
# enabled location is a no-op.
enabled=0
for i in $(seq 1 15); do
    if mmcli -m "$IDX" --location-enable-gps-nmea --location-enable-gps-raw; then
        enabled=1
        break
    fi
    sleep 2
done

# Hand the SIM7600's NMEA port to gpsd explicitly.
#
# gpsd (START_DAEMON in gpsd.conf) comes up early in boot — before the
# SIM7600 finishes USB enumeration — tries to open /dev/ttyUSB1, finds
# it absent, and gives up. Because gpsd.conf sets USBAUTO="false" (the
# knob that would re-grab the port on hotplug), gpsd never retries and
# sits with *zero* attached devices for the rest of the uptime, so
# `cgps` reads NA no matter what state the GNSS engine is in. This unit
# runs After=ModemManager and only reaches here once the modem is
# enabled, so /dev/ttyUSB1 now exists (and, once GPS is enabled above,
# is emitting NMEA) — attach it. gpsdctl is idempotent (re-adding an
# already-attached device is a no-op) and socket-activates gpsd if it
# isn't already running. Run it even when the enable loop exhausted its
# retries: the engine may already be powered from a prior boot, and a
# bound-but-idle gpsd is strictly better than an unbound one.
if command -v gpsdctl >/dev/null 2>&1; then
    gpsdctl add /dev/ttyUSB1 || echo "gpsdctl add /dev/ttyUSB1 failed (non-fatal)"
fi

if [ "$enabled" = 1 ]; then
    exit 0
fi

echo "GPS enable failed after 30s of retries"
exit 1
