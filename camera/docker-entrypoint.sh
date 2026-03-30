#!/bin/sh
# Docker entrypoint for camera containers.
#
# In the new enrollment flow, cameras connect with their device cert and are
# auto-registered as unclaimed. For Docker dev, we auto-claim by:
# 1. Starting the camera in the background (it connects and becomes unclaimed)
# 2. Logging in as admin, creating a claim token
# 3. The camera picks up the Active status on reconnect
#
# For production hardware, claiming happens via QR code in the web UI.

DATA_DIR="${GHOSTCAM_DATA_DIR:-/var/ghostcam}"
SERVER_HTTP="${GHOSTCAM_SERVER_HTTP:-http://server:3000}"
ADMIN_PASSWORD="${GHOSTCAM_ADMIN_PASSWORD:-}"
ADMIN_EMAIL="${GHOSTCAM_ADMIN_EMAIL:-admin@localhost}"
CAMERA_NAME="${GHOSTCAM_CAMERA_NAME:-Camera}"
AUTO_CLAIM="${GHOSTCAM_AUTO_CLAIM:-true}"

# Wait for server to be ready
until wget -qO- "$SERVER_HTTP/healthz" >/dev/null 2>&1; do
    echo "[entrypoint] Waiting for server..."
    sleep 2
done

if [ "$AUTO_CLAIM" = "true" ] && [ -n "$ADMIN_PASSWORD" ]; then
    # Auto-claim flow for Docker dev:
    # Start camera, wait for it to register, then claim it via API.

    # Start camera in background
    ghostcam-camera "$@" &
    CAMERA_PID=$!

    # Give the camera time to connect and register
    sleep 3

    # Login and capture session cookie
    COOKIE_JAR="$(mktemp)"
    wget -qO /dev/null --save-cookies "$COOKIE_JAR" --keep-session-cookies \
        --header "Content-Type: application/json" \
        --post-data "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" \
        "$SERVER_HTTP/api/v1/auth/login" 2>/dev/null

    # Create enrollment/claim token
    ENROLL_RESULT=$(wget -qO- --load-cookies "$COOKIE_JAR" \
        --header "Content-Type: application/json" \
        --post-data "{\"display_name\":\"$CAMERA_NAME\"}" \
        "$SERVER_HTTP/api/v1/cameras" 2>/dev/null)

    rm -f "$COOKIE_JAR"

    JWT=$(echo "$ENROLL_RESULT" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')

    if [ -z "$JWT" ]; then
        echo "[entrypoint] WARN: Failed to get claim token: $ENROLL_RESULT"
        echo "[entrypoint] Camera will run unclaimed — claim via web UI."
    else
        echo "[entrypoint] Claim token obtained. Camera will be claimed on next connect."
        # The camera will pick this up when it reconnects and the server validates
        # the token. For now, we just need to wait for the camera to reconnect.
    fi

    # Wait for camera process
    wait $CAMERA_PID
else
    # No auto-claim: camera connects as-is, must be claimed via QR or web UI
    echo "[entrypoint] Starting camera (no auto-claim)..."
    exec ghostcam-camera "$@"
fi
