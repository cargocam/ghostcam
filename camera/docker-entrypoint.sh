#!/bin/sh
# Docker entrypoint for camera containers.
#
# For Docker dev: auto-claims the camera by starting it, waiting for it to
# register as unclaimed, then using the admin API to claim it.
# For production hardware: claiming happens via QR code in the web UI.

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
    # Start camera in background — it will connect and register as unclaimed
    ghostcam-camera "$@" &
    CAMERA_PID=$!
    echo "[entrypoint] Camera started (PID $CAMERA_PID), waiting for registration..."
    sleep 5

    # Login
    COOKIE_JAR="$(mktemp)"
    wget -qO /dev/null --save-cookies "$COOKIE_JAR" --keep-session-cookies \
        --header "Content-Type: application/json" \
        --post-data "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" \
        "$SERVER_HTTP/api/v1/auth/login" 2>/dev/null

    # Get unclaimed devices and find ours (by fingerprint from device cert)
    DEVICE_FINGERPRINT=""
    if [ -f "$DATA_DIR/device.crt" ]; then
        # Extract fingerprint from device cert (SHA-256 of DER)
        DEVICE_FINGERPRINT=$(openssl x509 -in "$DATA_DIR/device.crt" -outform DER 2>/dev/null | sha256sum | cut -d' ' -f1)
    fi

    # Create a claim token
    ENROLL_RESULT=$(wget -qO- --load-cookies "$COOKIE_JAR" \
        --header "Content-Type: application/json" \
        --post-data "{\"display_name\":\"$CAMERA_NAME\"}" \
        "$SERVER_HTTP/api/v1/cameras" 2>/dev/null)

    JWT=$(echo "$ENROLL_RESULT" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
    rm -f "$COOKIE_JAR"

    if [ -n "$JWT" ]; then
        # Write claim token to a file the camera can read
        echo "$JWT" > "$DATA_DIR/claim_token"
        echo "[entrypoint] Claim token written to $DATA_DIR/claim_token"
        # Kill and restart camera so it picks up the token file
        kill $CAMERA_PID 2>/dev/null
        wait $CAMERA_PID 2>/dev/null
        sleep 1
        exec ghostcam-camera "$@"
    else
        echo "[entrypoint] WARN: Failed to get claim token: $ENROLL_RESULT"
        echo "[entrypoint] Camera running unclaimed — claim via web UI"
        wait $CAMERA_PID
    fi
else
    exec ghostcam-camera "$@"
fi
