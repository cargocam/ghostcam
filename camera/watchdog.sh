#!/bin/bash
# Ghostcam camera watchdog
# Runs the camera binary and handles firmware rollback on failed updates.

set -euo pipefail

FIRMWARE_DIR="${GHOSTCAM_DATA_DIR:-/var/ghostcam}/firmware"
CURRENT="$FIRMWARE_DIR/current"
PREVIOUS="$FIRMWARE_DIR/previous"
SENTINEL="$FIRMWARE_DIR/healthy"
HEALTH_TIMEOUT="${GHOSTCAM_HEALTH_TIMEOUT:-60}"

# Use the firmware binary if it exists, otherwise use the system binary
if [ -x "$CURRENT" ]; then
    BINARY="$CURRENT"
else
    BINARY="$(which ghostcam-camera 2>/dev/null || echo /usr/local/bin/ghostcam-camera)"
fi

while true; do
    # Remove sentinel before starting
    rm -f "$SENTINEL"

    echo "WATCHDOG: Starting camera ($BINARY)"

    # Start camera in background
    "$BINARY" "$@" &
    PID=$!

    # Wait for health sentinel
    WAITED=0
    while [ ! -f "$SENTINEL" ] && [ "$WAITED" -lt "$HEALTH_TIMEOUT" ]; do
        sleep 1
        WAITED=$((WAITED + 1))

        # Check if process died
        if ! kill -0 "$PID" 2>/dev/null; then
            break
        fi
    done

    # If no sentinel after timeout and we have a previous version, rollback
    if [ ! -f "$SENTINEL" ] && [ -x "$PREVIOUS" ] && [ "$BINARY" = "$CURRENT" ]; then
        echo "WATCHDOG: Camera unhealthy after ${HEALTH_TIMEOUT}s — rolling back firmware"
        kill "$PID" 2>/dev/null || true
        wait "$PID" 2>/dev/null || true

        mv "$CURRENT" "${CURRENT}.failed"
        mv "$PREVIOUS" "$CURRENT"
        BINARY="$CURRENT"
        continue
    fi

    # Wait for camera to exit
    wait "$PID" || true
    EXIT_CODE=$?

    if [ "$EXIT_CODE" -eq 0 ]; then
        echo "WATCHDOG: Camera exited cleanly (likely firmware update) — restarting"
    else
        echo "WATCHDOG: Camera crashed (exit $EXIT_CODE) — restarting in 5s"
        sleep 5
    fi
done
