# Smoke-test image for the camera Debian package.
#
# Two stages:
#   1. builder — cross-compiles the camera Go binary for linux/arm64 and
#      assembles the .deb the same way release.yml does. Building inside
#      a debian container keeps dpkg-deb available so we can produce a
#      valid .deb on macOS hosts that don't have it.
#   2. installer — pristine debian:12 rootfs. The .deb gets apt-installed
#      so any missing Depends: surfaces here as a hard build failure —
#      same as a fresh Pi would see.

FROM debian:12 AS builder
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        dpkg-dev \
    && rm -rf /var/lib/apt/lists/*

ARG GO_VERSION=1.24.0
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
    | tar -C /usr/local -xz
ENV PATH=/usr/local/go/bin:$PATH

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -trimpath -ldflags='-s -w' -o /build/dist/ghostcam-camera_arm64 ./camera

# Assemble the .deb. Mirrors release.yml exactly — if these two ever
# drift, our prod release and our smoke test stop catching each other's
# bugs.
WORKDIR /build
RUN mkdir -p ghostcam-camera_0.1.0-alpha_arm64/DEBIAN \
              ghostcam-camera_0.1.0-alpha_arm64/usr/local/bin \
    && install -m 0755 dist/ghostcam-camera_arm64 \
       ghostcam-camera_0.1.0-alpha_arm64/usr/local/bin/ghostcam-camera

COPY <<'EOF' /build/ghostcam-camera_0.1.0-alpha_arm64/DEBIAN/control
Package: ghostcam-camera
Version: 0.1.0~alpha
Architecture: arm64
Maintainer: Ghostcam <noreply@ghostcam.dev>
Depends: ffmpeg, ca-certificates
Description: Ghostcam camera agent (Go)
 Captures video via rpicam-vid + ffmpeg, uploads HLS
 segments to S3 via presigned URLs, publishes live via
 WHIP/WebRTC to the broadcast hub.
EOF

RUN dpkg-deb --build --root-owner-group /build/ghostcam-camera_0.1.0-alpha_arm64

# Export-friendly layout — `docker build -o type=local,dest=./dist` lifts
# the .deb and binary out without us hand-cat'ing them.
FROM scratch AS export
COPY --from=builder /build/ghostcam-camera_*.deb /
COPY --from=builder /build/dist/ghostcam-camera_arm64 /

# Pristine installer — debian:12 stand-in for a fresh Pi (arm64).
# Build with `docker buildx build --platform=linux/arm64 --target=installer .`
# on an amd64 host (qemu-user-static handles the cross-arch exec).
FROM debian:12 AS installer
COPY --from=builder /build/ghostcam-camera_*.deb /tmp/
RUN apt-get update \
    && apt-get install -y /tmp/ghostcam-camera_*.deb \
    && rm -rf /var/lib/apt/lists/* /tmp/*.deb
# Smoke check: binary is present at the expected path. We can't exec it
# under qemu-user without setting up the full emulation hooks, so the
# Depends: check via apt-install is the real validation.
RUN test -x /usr/local/bin/ghostcam-camera
