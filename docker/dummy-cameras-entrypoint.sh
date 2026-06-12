#!/bin/sh
# Manager entrypoint for the `dummy-cameras` compose profile. Runs N
# synthetic Go camera processes side-by-side in one container. The first
# camera is bound to the demo user; any additional cameras are bound to
# admin:
#
#   user@ghostcam.dev    /var/dummy/user      (Free tier — 1-cam trial)
#   admin@ghostcam.dev   /var/dummy/admin[-i] (Starter tier — multi-cam)
#
# Count is controlled by DUMMY_CAMERA_COUNT (default 2). Anything above 1
# adds admin-owned cameras, so admin must be on a tier whose camera limit
# covers (DUMMY_CAMERA_COUNT - 1) — run scripts/seed-dev.sh to promote
# admin to Starter, otherwise the extra enrollments 402 at the tier cap.
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
# How many synthetic cameras to run. 1 → user only; 2 → user + admin (the
# historical default); N → user + (N-1) admin cameras.
DUMMY_CAMERA_COUNT="${DUMMY_CAMERA_COUNT:-2}"

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

# --- 3. Fork N ghostcam-camera processes side-by-side. ----------------------
#
# Each invocation reuses the existing camera-entrypoint.sh, which is
# already exercised by the `test` profile's camera fleet. We run one
# entrypoint per camera in this container, each with its own DATA_DIR
# (distinct identity_key → distinct device ID) and its own owner.
#
# The entrypoint's env-var names (GHOSTCAM_ADMIN_EMAIL/PASSWORD) are
# historical — they're really "login creds for the user who'll own this
# camera." Video tuning (GHOSTCAM_VIDEO_PROFILE / _FPS / _BITRATE) is
# inherited from this container's environment, so the compose profile can
# dial the synthetic fleet's footprint up or down without editing this
# script.
#
# Ownership: by default the LAST camera goes to the demo user (so the
# free-tier path stays exercised) and the rest go to admin — so logging
# in as admin shows an (N-1)-camera fleet in one dashboard. Set
# DUMMY_USER_CAMERAS=0 to put every camera under admin, or bump it to
# hand more cameras to the (Free, 1-cam-limited) demo user. Admin must be
# on a tier whose limit covers its share — run scripts/seed-dev.sh.
DUMMY_USER_CAMERAS="${DUMMY_USER_CAMERAS:-1}"

PIDS=""

# start_camera <data_dir> <owner_email> <owner_password> <label>
start_camera() {
    echo "dummy-cameras: starting $4 camera at $1"
    mkdir -p "$1"
    GHOSTCAM_DATA_DIR="$1" \
    GHOSTCAM_SERVER_URL=$SERVER \
    GHOSTCAM_ADMIN_EMAIL="$2" \
    GHOSTCAM_ADMIN_PASSWORD="$3" \
    GHOSTCAM_RECORDING_MODE=motion \
        /usr/local/bin/camera-entrypoint.sh --test-source &
    PIDS="$PIDS $!"
}

# admin_count = total minus the user's share (never negative).
admin_count=$((DUMMY_CAMERA_COUNT - DUMMY_USER_CAMERAS))
[ "$admin_count" -lt 0 ] && admin_count=0

# Admin cameras. First keeps the bare /var/dummy/admin dir for device-ID
# continuity across restarts; subsequent ones get a numbered suffix.
i=1
while [ "$i" -le "$admin_count" ]; do
    if [ "$i" -eq 1 ]; then dir="$ADMIN_DATA"; else dir="$ADMIN_DATA-$i"; fi
    start_camera "$dir" "$ADMIN_EMAIL" "$ADMIN_PASSWORD" "admin's #$i"
    i=$((i + 1))
done

# Demo user cameras (free-tier path). First keeps the bare /var/dummy/user
# dir; subsequent ones (only if the user tier allowed >1) get a suffix.
j=1
while [ "$j" -le "$DUMMY_USER_CAMERAS" ] && [ "$j" -le "$DUMMY_CAMERA_COUNT" ]; do
    if [ "$j" -eq 1 ]; then dir="$USER_DATA"; else dir="$USER_DATA-$j"; fi
    start_camera "$dir" "$DEMO_USER_EMAIL" "$USER_PASSWORD" "user's #$j"
    j=$((j + 1))
done

echo "dummy-cameras: running ${DUMMY_CAMERA_COUNT} camera(s) — pids$PIDS"

# Forward signals to children so `docker stop` propagates cleanly.
trap 'echo "dummy-cameras: shutting down"; kill -TERM $PIDS 2>/dev/null || true; wait' INT TERM

# If any process exits, take the rest down and exit with its status —
# docker's restart policy brings us back up. Keeping the container alive
# when one camera has died would mask failures.
wait -n
EXIT=$?
kill -TERM $PIDS 2>/dev/null || true
wait 2>/dev/null || true
exit "$EXIT"
