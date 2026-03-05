# ── cargo-chef base ──────────────────────────────────────────────
FROM rust:1.82-bookworm AS chef
RUN cargo install cargo-chef
WORKDIR /app

# ── plan dependencies ────────────────────────────────────────────
FROM chef AS planner
COPY Cargo.toml Cargo.lock ./
COPY ghostcam/Cargo.toml ghostcam/Cargo.toml
COPY camera/Cargo.toml camera/Cargo.toml
COPY server/Cargo.toml server/Cargo.toml
COPY ghostcam/src ghostcam/src
COPY camera/src camera/src
COPY server/src server/src
RUN cargo chef prepare --recipe-path recipe.json

# ── build Rust binaries ─────────────────────────────────────────
FROM chef AS builder
RUN apt-get update && apt-get install -y --no-install-recommends \
    libasound2-dev libopus-dev pkg-config \
    && rm -rf /var/lib/apt/lists/*
COPY --from=planner /app/recipe.json recipe.json
RUN cargo chef cook --release --recipe-path recipe.json
COPY Cargo.toml Cargo.lock ./
COPY ghostcam ghostcam
COPY camera camera
COPY server server
RUN cargo build --release -p server -p camera

# ── build viewer SPA ────────────────────────────────────────────
FROM oven/bun:1 AS ui-builder
WORKDIR /app/ui
COPY ui/package.json ui/bun.lock ./
RUN bun install --frozen-lockfile
COPY ui/ ./
RUN bun run build

# ── generate test H.264 file ────────────────────────────────────
FROM debian:bookworm-slim AS test-data
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg \
    && rm -rf /var/lib/apt/lists/*
RUN mkdir -p /test-data && \
    ffmpeg -f lavfi -i testsrc2=duration=10:size=640x480:rate=30 \
      -c:v libx264 -profile:v baseline -x264-params keyint=60:min-keyint=60 \
      -f h264 /test-data/test.h264

# ── bridge (server) target ──────────────────────────────────────
FROM debian:bookworm-slim AS bridge
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/target/release/server /usr/local/bin/server
COPY --from=ui-builder /app/ui/build /app/ui
COPY --from=test-data /test-data /app/test-data
EXPOSE 4433/udp 3000/tcp
ENTRYPOINT ["server"]

# ── agent (camera) target ───────────────────────────────────────
FROM debian:bookworm-slim AS agent
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates libasound2 libopus0 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/target/release/camera /usr/local/bin/camera
COPY --from=test-data /test-data /app/test-data
ENTRYPOINT ["camera"]
