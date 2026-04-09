#!/bin/sh
set -e

# Auto-provision: if no credentials exist, get a token from the server and provision.
# Requires GHOSTCAM_SERVER_URL, GHOSTCAM_ADMIN_EMAIL, GHOSTCAM_ADMIN_PASSWORD env vars.
DATA_DIR="${GHOSTCAM_DATA_DIR:-/var/ghostcam}"
mkdir -p "$DATA_DIR/segments"

if [ ! -f "$DATA_DIR/api_key" ]; then
    echo "Auto-provisioning camera..."
    SERVER="${GHOSTCAM_SERVER_URL:-http://server:3000}"

    # Wait for server to be ready
    for i in $(seq 1 30); do
        if wget -q -O /dev/null "$SERVER/healthz" 2>/dev/null; then
            break
        fi
        echo "Waiting for server... ($i)"
        sleep 2
    done

    # Login to get session cookie
    COOKIE_FILE=$(mktemp)
    wget -q -O /dev/null --save-cookies "$COOKIE_FILE" --keep-session-cookies \
        --post-data "{\"email\":\"${GHOSTCAM_ADMIN_EMAIL}\",\"password\":\"${GHOSTCAM_ADMIN_PASSWORD}\"}" \
        --header="Content-Type: application/json" \
        "$SERVER/api/v1/auth/login" 2>/dev/null || true

    # Generate provision token
    TOKEN=$(wget -q -O - --load-cookies "$COOKIE_FILE" \
        --post-data '{}' --header="Content-Type: application/json" \
        "$SERVER/api/v1/cameras" 2>/dev/null | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')

    if [ -z "$TOKEN" ]; then
        echo "Failed to get provision token"
        rm -f "$COOKIE_FILE"
        exit 1
    fi

    # Write token + server URL for the camera binary
    printf '%s' "$TOKEN" > "$DATA_DIR/provision_token"
    printf '%s' "$SERVER" > "$DATA_DIR/server_url"
    rm -f "$COOKIE_FILE"
    echo "Provision token ready: $TOKEN"
fi

exec ghostcam-camera "$@"
