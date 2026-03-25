#!/bin/sh
# Auto-enroll camera if not yet enrolled.
# Requires GHOSTCAM_SERVER_HTTP, GHOSTCAM_ADMIN_PASSWORD, and GHOSTCAM_CAMERA_NAME env vars.

DATA_DIR="${GHOSTCAM_DATA_DIR:-/var/ghostcam}"
USER_CERT="$DATA_DIR/user.crt"

if [ ! -f "$USER_CERT" ]; then
    echo "[entrypoint] No enrollment found — enrolling..."

    SERVER_HTTP="${GHOSTCAM_SERVER_HTTP:-http://server:3000}"
    ADMIN_PASSWORD="${GHOSTCAM_ADMIN_PASSWORD:-}"
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
    LOGIN_RESULT=$(wget -qO- --save-cookies "$COOKIE_JAR" --keep-session-cookies \
        --header "Content-Type: application/json" \
        --post-data "{\"password\":\"$ADMIN_PASSWORD\"}" \
        "$SERVER_HTTP/api/v1/auth/login" 2>&1)

    if echo "$LOGIN_RESULT" | grep -q '"error"'; then
        echo "[entrypoint] ERROR: Login failed: $LOGIN_RESULT"
        exit 1
    fi

    # Create enrollment token
    ENROLL_RESULT=$(wget -qO- --load-cookies "$COOKIE_JAR" \
        --header "Content-Type: application/json" \
        --post-data "{\"display_name\":\"$CAMERA_NAME\"}" \
        "$SERVER_HTTP/api/v1/cameras" 2>&1)

    JWT=$(echo "$ENROLL_RESULT" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')

    if [ -z "$JWT" ]; then
        echo "[entrypoint] ERROR: Failed to get enrollment token: $ENROLL_RESULT"
        exit 1
    fi

    rm -f "$COOKIE_JAR"
    echo "[entrypoint] Got enrollment token, enrolling camera..."
    exec ghostcam-camera --enrollment-jwt "$JWT" "$@"
else
    echo "[entrypoint] Already enrolled, connecting..."
    exec ghostcam-camera "$@"
fi
