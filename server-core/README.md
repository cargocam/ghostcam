# server-core
`server-core` is the shared backend runtime library used by deployment binaries (`server-solo`, `server-multi`).

It contains the full protocol and request-handling logic for:
- camera ingest over QUIC,
- viewer APIs over HTTP,
- WebRTC egress for live viewing,
- PKI/enrollment/revocation flows,
- optional Redis persistence for telemetry/segment metadata.

## What this crate is (and is not)
- **Is**: reusable backend engine + domain logic.
- **Is not**: a standalone executable (`main` lives in deployment crates).

## Main subsystem directories
- `src/api/`: Axum routes and HTTP handlers.
- `src/ingest/`: camera connection accept loop, slot runtime, stream ingestion.
- `src/egress/`: WebRTC watch sessions and session tracking.
- `src/pki/`: instance CA, enrollment JWT signing/verification, cert refresh/revocation helpers.
- `src/redis/`: optional Redis integration for telemetry, segments, and revocation sync.

See `src/README.md` and subsystem READMEs for deeper details.

## Core shared interfaces
- `db.rs`: `Database` trait used by both SQLite (`server-solo`) and Postgres (`server-multi`) implementations.
- `auth.rs`: password hashing/verification, session IDs, API token HMAC logic.
- `frames.rs`: inbound camera stream tags used on QUIC unidirectional streams.
- `sse.rs`: per-user event fanout used by `/events`.

## Runtime model (at a glance)
1. Deployment crate builds shared `AppState` with DB, registry, sessions, PKI, optional Redis.
2. QUIC accept loop (`ingest::accept`) classifies each camera as enrollment or normal ingest.
3. Connected cameras register `IngestSlot`s, providing broadcast channels for media/telemetry.
4. HTTP `/api/v1/watch` creates `EgressHandle` sessions that subscribe to those broadcasts and publish WebRTC media/data channels.
5. Optional Redis stores telemetry history and segment metadata for playback/map/dashboard use cases.
