#!/bin/sh
# Manager entrypoint for the `dummy-cameras` compose profile. Runs two
# synthetic Go camera processes side-by-side in one container, each
# bound to a different account:
#
#   admin@ghostcam.dev   /var/dummy/admin   (Starter tier — 3-cam allowance)
#   user@ghostcam.dev    /var/dummy/user    (Free tier — 1-cam trial)
#
# Both processes are the real `ghostcam-camera` binary built with
# -tags synthetic, so every code path the production camera exercises
# (capture → ffmpeg → S3 upload, telemetry, WHIP publish, motion
# detection, power modes, battery rules) runs unchanged. The only
# substitution is the input source: ffmpeg's `testsrc2` + `sine` instead
# of rpicam-vid + ALSA.
#
# Reuses docker/camera-entrypoint.sh for the actual provisioning dance
# (login → mint token → enroll). The per-camera DATA_DIR isolates state
# so the two cameras have distinct identity_keys (and therefore distinct
# device IDs) — they look like two separate physical cameras to the
# server.
set -e

SERVER="${GHOSTCAM_SERVER_URL:-http://server:3000}"
ADMIN_EMAIL="${ADMIN_EMAIL:-admin@ghostcam.dev}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-dev-password}"
DEMO_USER_EMAIL="${DEMO_USER_EMAIL:-user@ghostcam.dev}"

ADMIN_DATA=/var/dummy/admin
USER_DATA=/var/dummy/user
# Cache the demo user's password across container restarts so we don't
# burn through reset-password churn on every `docker compose up`. The
# password is reset (and re-cached) only on first boot OR when this
# file is deleted (which `docker compose down -v` does for us).
USER_PASSWORD_FILE=/var/dummy/.user-password

mkdir -p "$ADMIN_DATA" "$USER_DATA"

# --- 1. Wait for the server's HTTP listener. --------------------------------

for i in $(seq 1 30); do
    if wget -q -O /dev/null "$SERVER/healthz" 2>/dev/null; then
        break
    fi
    echo "dummy-cameras: waiting for $SERVER ($i)"
    sleep 2
done

# --- 2. Ensure $DEMO_USER_EMAIL exists, cache its password ------------------
#
# The bootstrap admin (admin@ghostcam.dev / dev-password) is created by
# the server at startup. The demo user is not — we provision it on
# demand here so the dummy-cameras profile is self-contained.

if [ ! -f "$USER_PASSWORD_FILE" ]; then
    echo "dummy-cameras: setting up $DEMO_USER_EMAIL"
    COOKIE=$(mktemp)
    trap 'rm -f "$COOKIE"' EXIT

    # Login as admin.
    wget -q -O /dev/null --save-cookies "$COOKIE" --keep-session-cookies \
        --post-data "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" \
        --header="Content-Type: application/json" \
        "$SERVER/api/v1/auth/login"

    # Create the user (409 if it already exists — both are fine).
    wget -q -O /dev/null --load-cookies "$COOKIE" \
        --post-data "{\"email\":\"$DEMO_USER_EMAIL\",\"display_name\":\"Demo User\"}" \
        --header="Content-Type: application/json" \
        "$SERVER/api/v1/admin/users" 2>/dev/null || true

    # Look up the user_id. `jq` is added to this image specifically so
    # we don't have to parse arbitrarily-ordered JSON with sed.
    USER_ID=$(wget -q -O - --load-cookies "$COOKIE" "$SERVER/api/v1/admin/users" \
        | jq -r ".users[] | select(.email == \"$DEMO_USER_EMAIL\") | .user_id")

    if [ -z "$USER_ID" ] || [ "$USER_ID" = "null" ]; then
        echo "dummy-cameras: ERROR — could not look up $DEMO_USER_EMAIL"
        exit 1
    fi

    # Reset password — the endpoint returns a fresh plaintext we cache.
    NEW_PASSWORD=$(wget -q -O - --load-cookies "$COOKIE" \
        --post-data '{}' --header="Content-Type: application/json" \
        "$SERVER/api/v1/admin/users/$USER_ID/reset-password" \
        | jq -r '.generated_password')

    if [ -z "$NEW_PASSWORD" ] || [ "$NEW_PASSWORD" = "null" ]; then
        echo "dummy-cameras: ERROR — password reset failed for $DEMO_USER_EMAIL"
        exit 1
    fi

    printf '%s\n' "$NEW_PASSWORD" > "$USER_PASSWORD_FILE"
    chmod 0600 "$USER_PASSWORD_FILE"
    rm -f "$COOKIE"
    trap - EXIT
    echo "dummy-cameras: $DEMO_USER_EMAIL ready (password cached at $USER_PASSWORD_FILE)"
fi

USER_PASSWORD=$(cat "$USER_PASSWORD_FILE")

# --- 3. Fork two ghostcam-camera processes side-by-side. --------------------
#
# Each invocation reuses the existing camera-entrypoint.sh, which is
# already exercised by the `test` profile's three-camera fleet. The only
# difference here is that we run TWO entrypoints in one container, each
# pointing at its own DATA_DIR and its own owner.
#
# The entrypoint's env-var names (GHOSTCAM_ADMIN_EMAIL/PASSWORD) are
# historical — they're really "login creds for the user who'll own this
# camera," which is admin for the first instance and the demo user for
# the second. Renaming them would touch the test profile too; leaving
# as-is for compatibility.

echo "dummy-cameras: starting admin's camera at $ADMIN_DATA"
GHOSTCAM_DATA_DIR=$ADMIN_DATA \
GHOSTCAM_SERVER_URL=$SERVER \
GHOSTCAM_ADMIN_EMAIL="$ADMIN_EMAIL" \
GHOSTCAM_ADMIN_PASSWORD="$ADMIN_PASSWORD" \
GHOSTCAM_RECORDING_MODE=motion \
    /usr/local/bin/camera-entrypoint.sh --test-source &
ADMIN_PID=$!

echo "dummy-cameras: starting user's camera at $USER_DATA"
GHOSTCAM_DATA_DIR=$USER_DATA \
GHOSTCAM_SERVER_URL=$SERVER \
GHOSTCAM_ADMIN_EMAIL="$DEMO_USER_EMAIL" \
GHOSTCAM_ADMIN_PASSWORD="$USER_PASSWORD" \
GHOSTCAM_RECORDING_MODE=motion \
    /usr/local/bin/camera-entrypoint.sh --test-source &
USER_PID=$!

echo "dummy-cameras: admin=$ADMIN_PID user=$USER_PID — running"

# Forward signals to children so `docker stop` propagates cleanly.
trap 'echo "dummy-cameras: shutting down"; kill -TERM $ADMIN_PID $USER_PID 2>/dev/null || true; wait' INT TERM

# If either process exits, take the other one down and exit with its
# status — docker's restart policy will bring us back up. Keeping the
# container alive when one camera has died would mask failures.
wait -n
EXIT=$?
kill -TERM $ADMIN_PID $USER_PID 2>/dev/null || true
wait 2>/dev/null || true
exit "$EXIT"
