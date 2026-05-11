"""Integration tests for the Python camera against a real Go server.

These exercise the actual wire — every HTTP request and S3 PUT goes
through the network stack, hitting Postgres + Redis + MinIO + Go
server containers spun up via testcontainers-python.

They are gated behind the `--integration` pytest flag so the default
`pytest -q` (the unit-test suite) doesn't try to start Docker.

  pip install testcontainers
  pytest tests/integration/ --integration

Container layout:
  * ghcr.io/...:postgres-16, redis:7-alpine, minio/minio:latest
  * The Go server is built once via `go build` and run as a host
    subprocess pointing at the testcontainers ports — keeping the
    server in-process means we don't need a published server image.

What's covered:
  test_provision.py     — POST /api/v1/cameras/provision returns the
                          expected device_id; key registered server-side.
  test_telemetry.py     — POST /api/v1/cameras/<id>/telemetry persists
                          to Redis stream; signature verification path.
  test_presign_upload.py — request URLs, PUT segments to MinIO, confirm
                          via next presign; row appears in segments table.
  test_live_ws.py       — WebSocket upgrade with signed Authorization;
                          server sees the binary frame via its in-memory
                          live session.

What's NOT covered (handed off to on-Pi smoke):
  * rpicam-vid, real ALSA/Opus, gpsd, nmcli, real QR scan
  * libzbar0 (only matters at provisioning time on a Pi)
  * 24h soak — that's the bar for the deferred Rust phase
"""
