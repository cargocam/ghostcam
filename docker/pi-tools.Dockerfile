# Containerized runtime for scripts/pi.sh.
#
# Bundles everything pi.sh needs (sshpass, rsync, python+build, openssh)
# so the host doesn't need any of those installed — `./scripts/pi`
# (the wrapper) builds this image lazily on first run and execs into it.
#
# Stays at debian-slim because pi.sh already targets debian-based Pi
# hosts; this keeps the toolchain consistent with what we're deploying.
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        ca-certificates \
        findutils \
        openssh-client \
        python3 \
        python3-pip \
        python3-venv \
        rsync \
        sshpass \
    && rm -rf /var/lib/apt/lists/*

# `python -m build` is invoked by pi.sh build_and_deploy. --break-system-packages
# is fine inside a throwaway container image.
RUN pip install --break-system-packages --no-cache-dir build

WORKDIR /repo

ENTRYPOINT ["/repo/scripts/pi.sh"]
