# Containerized runtime for scripts/pi.sh.
#
# Bundles everything pi.sh needs (sshpass, rsync, openssh, Go toolchain
# for cross-compile) so the host doesn't need any of those installed —
# `./scripts/pi` (the wrapper) builds this image lazily on first run
# and execs into it.
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        ca-certificates \
        curl \
        findutils \
        openssh-client \
        rsync \
        sshpass \
    && rm -rf /var/lib/apt/lists/*

# Go toolchain for cross-compiling the camera binary (linux/arm64) on
# pi.sh deploy. Pinned to a specific minor so the toolchain doesn't
# drift between contributor runs; rebuild the image after bumping.
ARG GO_VERSION=1.24.0
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
    | tar -C /usr/local -xz
ENV PATH=/usr/local/go/bin:$PATH \
    GOTOOLCHAIN=local

WORKDIR /repo

ENTRYPOINT ["/repo/scripts/pi.sh"]
