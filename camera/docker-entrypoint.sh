#!/bin/sh
# Auto-enroll camera if not yet enrolled.
# Requires GHOSTCAM_SERVER_HTTP, GHOSTCAM_ADMIN_PASSWORD env vars.

DATA_DIR="${GHOSTCAM_DATA_DIR:-/var/ghostcam}"
USER_CERT="$DATA_DIR/user.crt"

if [ ! -f "$USER_CERT" ]; then
    echo "[entrypoint] No enrollment found — enrolling..."

    SERVER_HTTP="${GHOSTCAM_SERVER_HTTP:-http://server:3000}"
    ADMIN_PASSWORD="${GHOSTCAM_ADMIN_PASSWORD:-}"
    ADMIN_EMAIL="${GHOSTCAM_ADMIN_EMAIL:-admin@localhost}"
    CAMERA_NAME="${GHOSTCAM_CAMERA_NAME:-Camera}"

    if [ -z "$ADMIN_PASSWORD" ]; then
        echo "[entrypoint] ERROR: GHOSTCAM_ADMIN_PASSWORD is required for auto-enrollment"
        exit 1
    fi

    # Wait for server to be ready
    until wget -qO- "$SERVER_HTTP/healthz" >/dev/null 2>&1; do
        echo "[entrypoint] Waiting for server..."
        sleep 2
    done

    # Login and capture session cookie
    COOKIE_JAR="$(mktemp)"
    wget -qO /dev/null --save-cookies "$COOKIE_JAR" --keep-session-cookies \
        --header "Content-Type: application/json" \
        --post-data "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" \
        "$SERVER_HTTP/api/v1/auth/login" 2>/dev/null

    # Create enrollment token
    ENROLL_RESULT=$(wget -qO- --load-cookies "$COOKIE_JAR" \
        --header "Content-Type: application/json" \
        --post-data "{\"display_name\":\"$CAMERA_NAME\"}" \
        "$SERVER_HTTP/api/v1/cameras" 2>/dev/null)

    JWT=$(echo "$ENROLL_RESULT" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')

    rm -f "$COOKIE_JAR"

    if [ -z "$JWT" ]; then
        echo "[entrypoint] ERROR: Failed to get enrollment token: $ENROLL_RESULT"
        echo "[entrypoint] Retrying in 5s..."
        sleep 5
        exec "$0" "$@"
    fi

    echo "[entrypoint] Got enrollment token, enrolling camera..."
    exec ghostcam-camera --enrollment-jwt "$JWT" "$@"
else
    echo "[entrypoint] Already enrolled, connecting..."
    exec ghostcam-camera "$@"
fi
