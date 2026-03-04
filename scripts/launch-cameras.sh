#!/usr/bin/env bash
# Launch N test camera instances pointing at the bridge.
# Usage: ./scripts/launch-cameras.sh [count] [group_id]

set -e

COUNT=${1:-4}
GROUP=${2:-default}
BRIDGE=${BRIDGE_ADDR:-127.0.0.1:4433}
FPS=${FPS:-30}
TEST_FILE=${TEST_FILE:-test-data/test.h264}

echo "Launching $COUNT test cameras in group '$GROUP' -> $BRIDGE"

PIDS=()

cleanup() {
    echo "Stopping cameras..."
    for pid in "${PIDS[@]}"; do
        kill "$pid" 2>/dev/null || true
    done
    wait
}
trap cleanup EXIT INT TERM

for i in $(seq 1 "$COUNT"); do
    DEVICE_ID="${GROUP}-cam-$(printf '%02d' "$i")"
    echo "  Starting $DEVICE_ID"
    cargo run -p ghostcam-agent -- \
        --bridge-addr "$BRIDGE" \
        --device-id "$DEVICE_ID" \
        --group-id "$GROUP" \
        --test-file "$TEST_FILE" \
        --fps "$FPS" &
    PIDS+=($!)
    sleep 0.2
done

echo "All $COUNT cameras running. Press Ctrl+C to stop."
wait
