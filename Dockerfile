# --- Go builder ---
FROM golang:1-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /ghostcam-server ./server

# --- UI builder ---
FROM oven/bun:1 AS ui-builder
WORKDIR /app
COPY ui/package.json ui/bun.lock ./
RUN bun install --frozen-lockfile
COPY ui/ .
RUN bun run build

# --- UI dev target (Vite HMR, used by docker-compose) ---
FROM oven/bun:1 AS ui-dev
WORKDIR /app
COPY ui/package.json ui/bun.lock ./
RUN bun install
COPY ui/ .
EXPOSE 5173
CMD ["bun", "run", "dev"]

# --- Python camera builder ---
FROM python:3.11-slim AS python-camera-builder
WORKDIR /build
COPY camera/pyproject.toml camera/README.md ./
COPY camera/ghostcam ./ghostcam
RUN pip install --no-cache-dir build \
    && python -m build --wheel --outdir /wheels

# --- Camera (test/Docker — Python, synthetic sensors) ---
# This is the canonical test camera image. docker-compose's --profile test
# fleet builds this stage. The auto-provisioning shell entrypoint is
# unchanged from the legacy era — `ghostcam-camera` is the console script
# the wheel installs at /usr/local/bin/ghostcam-camera.
FROM python:3.11-slim AS camera
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ffmpeg wget fonts-dejavu tzdata \
    && rm -rf /var/lib/apt/lists/*
COPY --from=python-camera-builder /wheels/*.whl /tmp/
RUN pip install --no-cache-dir /tmp/*.whl && rm -f /tmp/*.whl
ENV GHOSTCAM_SYNTHETIC=1
COPY docker/camera-entrypoint.sh /usr/local/bin/camera-entrypoint.sh
ENTRYPOINT ["camera-entrypoint.sh"]

# --- Camera (production — Python, real hardware sensors) ---
# For Pi production builds. libzbar0 enables the QR provisioning path.
# Real sensors come for free because GHOSTCAM_SYNTHETIC isn't set.
FROM python:3.11-slim AS camera-prod
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates ffmpeg wget libzbar0 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=python-camera-builder /wheels/*.whl /tmp/
RUN sh -c 'pip install --no-cache-dir "$(ls /tmp/*.whl | head -1)[real]"' \
    && rm -f /tmp/*.whl
COPY docker/camera-entrypoint.sh /usr/local/bin/camera-entrypoint.sh
ENTRYPOINT ["camera-entrypoint.sh"]

# --- Server target (default for Fly.io deploy) ---
FROM alpine:3.21 AS server
RUN apk add --no-cache ca-certificates
COPY --from=builder /ghostcam-server /usr/local/bin/ghostcam-server
COPY --from=ui-builder /app/build /app/static
ENTRYPOINT ["ghostcam-server"]

# --- Server dev target (air hot-reload, used by docker-compose) ---
# Source is bind-mounted from the repo at /app. Air watches *.go files
# and rebuilds the binary on change; the UI is still served by Vite at
# :5173, so the static-file handler in server/main.go no-ops when
# /app/static is missing.
FROM golang:1-alpine AS server-dev
RUN apk add --no-cache ca-certificates git
RUN go install github.com/air-verse/air@latest
WORKDIR /app
EXPOSE 3000
CMD ["air", "-c", ".air.toml"]
