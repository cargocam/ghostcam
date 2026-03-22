# --- Chef stage (dependency caching) ---
FROM rust:1.82-bookworm AS chef
RUN cargo install cargo-chef
WORKDIR /app

FROM chef AS planner
COPY . .
RUN cargo chef prepare --recipe-path recipe.json

FROM chef AS builder
COPY --from=planner /app/recipe.json recipe.json
RUN apt-get update && apt-get install -y libasound2-dev libopus-dev
RUN cargo chef cook --release --recipe-path recipe.json
COPY . .
RUN cargo build --release -p server-solo -p server-multi -p camera

# --- UI build ---
FROM oven/bun:1 AS ui-builder
WORKDIR /app/ui
COPY ui/package.json ui/bun.lockb ./
RUN bun install --frozen-lockfile
COPY ui/ .
RUN bun run build

# --- Test data ---
FROM debian:bookworm-slim AS test-data
RUN apt-get update && apt-get install -y ffmpeg
WORKDIR /data
RUN ffmpeg -f lavfi -i testsrc2=duration=10:size=640x480:rate=30 \
  -c:v libx264 -profile:v baseline -x264-params keyint=60:min-keyint=60 \
  -f h264 test.h264

# --- server-solo target ---
FROM debian:bookworm-slim AS server-solo
RUN apt-get update && apt-get install -y ca-certificates libopus0 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/target/release/server-solo /usr/local/bin/ghostcam-server-solo
COPY --from=ui-builder /app/ui/build /app/static
COPY --from=test-data /data/test.h264 /app/test-data/test.h264
EXPOSE 3000 4433/udp
ENTRYPOINT ["ghostcam-server-solo"]

# --- server-multi target ---
FROM debian:bookworm-slim AS server-multi
RUN apt-get update && apt-get install -y ca-certificates libopus0 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/target/release/server-multi /usr/local/bin/ghostcam-server-multi
COPY --from=ui-builder /app/ui/build /app/static
EXPOSE 3000 4433/udp
ENTRYPOINT ["ghostcam-server-multi"]

# --- camera target (for testing) ---
FROM debian:bookworm-slim AS camera
RUN apt-get update && apt-get install -y ca-certificates libopus0 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/target/release/camera /usr/local/bin/ghostcam-camera
COPY --from=test-data /data/test.h264 /app/test-data/test.h264
ENTRYPOINT ["ghostcam-camera"]
