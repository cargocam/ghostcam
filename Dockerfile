# --- Go builder ---
FROM golang:1-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /ghostcam-server ./cmd/ghostcam-server
RUN CGO_ENABLED=0 go build -o /ghostcam-camera ./cmd/ghostcam-camera
# Test camera: synthetic sensors (GPS, CPU, etc.) instead of real hardware
RUN CGO_ENABLED=0 go build -tags synthetic -o /ghostcam-camera-synthetic ./cmd/ghostcam-camera

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

# --- Camera target (test/Docker — synthetic sensors) ---
FROM alpine:3.21 AS camera
RUN apk add --no-cache ca-certificates ffmpeg wget
COPY --from=builder /ghostcam-camera-synthetic /usr/local/bin/ghostcam-camera
COPY camera-entrypoint.sh /usr/local/bin/camera-entrypoint.sh
ENTRYPOINT ["camera-entrypoint.sh"]

# --- Camera target (production — real hardware sensors) ---
FROM alpine:3.21 AS camera-prod
RUN apk add --no-cache ca-certificates ffmpeg wget
COPY --from=builder /ghostcam-camera /usr/local/bin/ghostcam-camera
COPY camera-entrypoint.sh /usr/local/bin/camera-entrypoint.sh
ENTRYPOINT ["camera-entrypoint.sh"]

# --- Server target (default for Fly.io deploy) ---
FROM alpine:3.21 AS server
RUN apk add --no-cache ca-certificates
COPY --from=builder /ghostcam-server /usr/local/bin/ghostcam-server
COPY --from=ui-builder /app/build /app/static
ENTRYPOINT ["ghostcam-server"]
