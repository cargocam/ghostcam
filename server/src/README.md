# server-core/src
This directory contains the backend runtime modules shared by all server deployments.

## Module map
- `api/`: HTTP API handlers and auth middleware.
- `ingest/`: QUIC camera ingest pipeline.
- `egress/`: WebRTC viewer sessions and RTP/data-channel bridging.
- `pki/`: CA/bootstrap/enrollment/revocation primitives.
- `redis/`: optional external persistence and cache refresh loops.
- `db.rs`: storage trait and domain record types.
- `auth.rs`: password + token cryptography utilities.
- `frames.rs`: stream-tag definitions for camera inbound uni streams.
- `sse.rs`: per-user in-memory SSE bus.
- `audit.rs`: tamper-evident audit logger primitives.

## Primary data flow
1. Camera connects to QUIC endpoint and is processed by `ingest::accept`.
2. Accepted camera streams are attached to an `IngestSlot` and registered in `RoutingRegistry`.
3. API handlers (`/api/v1/watch`) locate the slot, create an `EgressHandle`, and start WebRTC forwarding.
4. Redis (if configured) receives telemetry and segment metadata writes from ingest path.
5. SSE events fan out camera online/offline transitions to authenticated viewers.

## Ownership boundaries
- Deployment crates provide concrete DB implementation + process lifecycle.
- `server-core` owns protocol correctness and runtime coordination between ingest/api/egress/pki/redis modules.
