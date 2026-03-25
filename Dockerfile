# --- Chef stage (dependency caching) ---
FROM rust:1-bookworm AS chef
RUN cargo install cargo-chef --locked
WORKDIR /app

FROM chef AS planner
COPY . .
RUN cargo chef prepare --recipe-path recipe.json

FROM chef AS builder
COPY --from=planner /app/recipe.json recipe.json
RUN apt-get update && apt-get install -y libasound2-dev libopus-dev
RUN cargo chef cook --release --recipe-path recipe.json
COPY . .
RUN cargo build --release -p server -p camera

# --- UI build ---
FROM oven/bun:1 AS ui-builder
WORKDIR /app/ui
COPY ui/package.json ui/bun.lock ./
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

# --- server target ---
FROM debian:bookworm-slim AS server
RUN apt-get update && apt-get install -y ca-certificates libopus0 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/target/release/server /usr/local/bin/ghostcam-server
COPY --from=ui-builder /app/ui/build /app/static
EXPOSE 3000 4433/udp
ENTRYPOINT ["ghostcam-server"]

# --- ui-dev target (Vite dev server with HMR) ---
FROM oven/bun:1 AS ui-dev
WORKDIR /app/ui
COPY ui/package.json ui/bun.lock ./
RUN bun install --frozen-lockfile
COPY ui/ .
EXPOSE 5173
CMD ["bun", "run", "dev"]

# --- camera target ---
FROM debian:bookworm-slim AS camera
RUN apt-get update && apt-get install -y ca-certificates libopus0 wget && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/target/release/camera /usr/local/bin/ghostcam-camera
COPY --from=test-data /data/test.h264 /app/test-data/test.h264
COPY camera/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["--test-source", "--no-tofu"]
