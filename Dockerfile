# Camera-side container images for the ghostcam Pi camera daemon.
#
# Stages:
#   camera-builder  — Go cross-compile (synthetic + production binaries)
#   camera          — Alpine runtime, synthetic sensors. Base for dummy-cameras
#                     and for one-off synthetic camera containers.
#   dummy-cameras   — Manager wrapping the synthetic camera, used by the
#                     server repo's docker-compose `demo` profile. Forks two
#                     ghostcam-camera processes (one per demo user) so a
#                     single container appears to the server as two distinct
#                     physical cameras.
#   camera-prod     — Alpine runtime, real (Pi) sensors. The actual Pi .deb
#                     install path is the canonical production deploy; this
#                     stage exists for parity testing and for the rare
#                     case where you want to run the production binary in a
#                     container against a real Pi-side ffmpeg + libcamera.

# --- Go builder ---
# Build-tag-gated files (sensors_*, network_*, qr_*) select synthetic vs
# real sensors at compile time — see CLAUDE.md "Build tags".
FROM golang:1-alpine AS camera-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -tags synthetic -o /ghostcam-camera-synthetic ./camera \
 && CGO_ENABLED=0 go build              -o /ghostcam-camera-prod      ./camera

# --- Synthetic camera runtime ---
# font-dejavu provides DejaVuSansMono.ttf which the test-source pipeline's
# drawtext filter expects (camera/capture.go:127). Without it, ffmpeg
# fails with "Cannot find a valid font for the family Sans" and exits
# 254 before producing a single segment.
FROM alpine:3.21 AS camera
RUN apk add --no-cache ca-certificates ffmpeg wget tzdata font-dejavu
COPY --from=camera-builder /ghostcam-camera-synthetic /usr/local/bin/ghostcam-camera
COPY docker/camera-entrypoint.sh /usr/local/bin/camera-entrypoint.sh
ENTRYPOINT ["camera-entrypoint.sh"]

# --- Dummy cameras (demo profile — one container, two synthetic cameras) ---
# Adds `jq` to the regular camera image and replaces the entrypoint with
# a manager script. The manager:
#   * ensures user@ghostcam.dev exists (admin API), caches its password,
#   * forks two ghostcam-camera processes in parallel (one as admin, one
#     as the demo user) reusing camera-entrypoint.sh for each,
#   * forwards SIGTERM/SIGINT so `docker stop` tears down both children.
# Each child has its own DATA_DIR, so identity_keys (and therefore
# device IDs) are distinct — to the server they look like two separate
# physical units.
FROM alpine:3.21 AS dummy-cameras
RUN apk add --no-cache ca-certificates ffmpeg wget tzdata font-dejavu jq
COPY --from=camera-builder /ghostcam-camera-synthetic /usr/local/bin/ghostcam-camera
COPY docker/camera-entrypoint.sh /usr/local/bin/camera-entrypoint.sh
COPY docker/dummy-cameras-entrypoint.sh /usr/local/bin/dummy-cameras-entrypoint.sh
ENTRYPOINT ["dummy-cameras-entrypoint.sh"]

# --- Camera (production — real hardware sensors) ---
# For Pi production builds. Real sensors via the !synthetic build tag.
# The Pi has libcamera + gpsd + nmcli in the system image, so the camera
# binary itself stays statically linked with no runtime cgo deps.
FROM alpine:3.21 AS camera-prod
RUN apk add --no-cache ca-certificates ffmpeg wget
COPY --from=camera-builder /ghostcam-camera-prod /usr/local/bin/ghostcam-camera
COPY docker/camera-entrypoint.sh /usr/local/bin/camera-entrypoint.sh
ENTRYPOINT ["camera-entrypoint.sh"]
