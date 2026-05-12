# Smoke-test image for the camera Debian package.
#
# Two stages worth using:
#   1. builder — builds the wheel + .deb the same way release.yml does
#      on the self-hosted runner. dpkg-deb is available here, so unlike
#      macOS we can produce a valid .deb on the host.
#   2. installer — pristine debian:12 rootfs. The .deb gets installed
#      via `apt install ./pkg.deb` so missing system deps (libzbar0,
#      ffmpeg, python3-venv) get resolved against the real repos.
#      Anything the .deb forgot to declare surfaces here as a hard
#      build failure — same as a fresh Pi would see.

FROM debian:12 AS builder
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        python3 python3-venv python3-pip \
        dpkg-dev \
        curl \
    && rm -rf /var/lib/apt/lists/*

# uv brings its own wheel builder so we don't need `pip install build`.
RUN curl -LsSf https://astral.sh/uv/install.sh | sh \
    && cp /root/.local/bin/uv /usr/local/bin/uv

WORKDIR /build
COPY camera/ ./camera/

WORKDIR /build/camera
RUN uv build --wheel --out-dir /build/dist

# Assemble the .deb. Mirrors release.yml exactly — if these two ever
# drift, our prod release and our smoke test stop catching each other's
# bugs.
WORKDIR /build
RUN mkdir -p ghostcam-camera_0.1.0-alpha_all/DEBIAN \
              ghostcam-camera_0.1.0-alpha_all/opt/ghostcam-camera \
    && cp dist/ghostcam-*.whl ghostcam-camera_0.1.0-alpha_all/opt/ghostcam-camera/

COPY <<'EOF' /build/ghostcam-camera_0.1.0-alpha_all/DEBIAN/postinst
#!/bin/sh
set -e
if [ ! -x /opt/ghostcam/bin/python3 ]; then
    python3 -m venv /opt/ghostcam
fi
WHEEL="$(ls /opt/ghostcam-camera/ghostcam-*.whl | head -1)"
/opt/ghostcam/bin/pip install --quiet --upgrade --force-reinstall "$WHEEL[real]"
ln -sf /opt/ghostcam/bin/ghostcam-camera /usr/local/bin/ghostcam-camera
exit 0
EOF

COPY <<'EOF' /build/ghostcam-camera_0.1.0-alpha_all/DEBIAN/control
Package: ghostcam-camera
Version: 0.1.0~alpha
Architecture: all
Maintainer: Ghostcam <noreply@ghostcam.dev>
Depends: ffmpeg, ca-certificates, python3, python3-venv, libzbar0
Description: Ghostcam camera agent (Python)
 Captures video via rpicam-vid + ffmpeg and uploads
 HLS segments to S3 via presigned URLs.
EOF

RUN chmod 755 /build/ghostcam-camera_0.1.0-alpha_all/DEBIAN/postinst \
    && dpkg-deb --build --root-owner-group /build/ghostcam-camera_0.1.0-alpha_all

# Export-friendly layout — `docker build -o type=local,dest=./dist` lifts
# the .deb + wheel out without us hand-cat'ing them.
FROM scratch AS export
COPY --from=builder /build/ghostcam-camera_*.deb /
COPY --from=builder /build/dist/ghostcam-*.whl /

# Pristine installer — debian:12 stand-in for a fresh Pi.
FROM debian:12 AS installer
COPY --from=builder /build/ghostcam-camera_*.deb /tmp/
RUN apt-get update \
    && apt-get install -y /tmp/ghostcam-camera_*.deb \
    && rm -rf /var/lib/apt/lists/* /tmp/*.deb
# Smoke checks (network-free): entrypoint exists, --version works,
# no missing modules at import time.
RUN /usr/local/bin/ghostcam-camera --version \
    && /opt/ghostcam/bin/python3 -c "import ghostcam.main; import pyzbar.pyzbar"
