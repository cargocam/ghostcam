# Pre-built container with rpi-image-gen and all its dependencies.
# Used by the release workflow to skip the ~5 min dep install on every
# Pi image build. Rebuilt manually or on-demand when rpi-image-gen
# changes upstream.
#
# Build & push:
#   docker buildx build -f .github/rpi-image-gen.Dockerfile \
#     -t ghcr.io/cargocam/rpi-image-gen:latest --push .

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      git binfmt-support qemu-user-static ca-certificates \
      xz-utils && \
    git clone --depth 1 https://github.com/raspberrypi/rpi-image-gen.git /opt/rpi-image-gen && \
    /opt/rpi-image-gen/install_deps.sh && \
    apt-get clean && rm -rf /var/lib/apt/lists/*
