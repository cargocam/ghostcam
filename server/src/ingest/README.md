# server-core/src/ingest
Camera-facing QUIC ingest pipeline.

## Responsibilities
- accept and classify incoming camera connections,
- run enrollment flow for unenrolled devices,
- verify enrolled camera identity and revocation status,
- host per-camera slot runtime with media/telemetry/alert channels,
- route command demand based on active viewer mode.

## Connection classification (`accept.rs`)
For each accepted QUIC connection:
1. Read peer cert chain.
2. If chain has only device cert: run enrollment path (`enrollment.rs`).
3. If chain includes user association cert:
   - check revocation cache (serial + fingerprint paths),
   - verify user cert was signed by instance CA,
   - extract `device_id` from cert CN,
   - match camera by device cert fingerprint in DB,
   - enforce cert CN == DB device_id.
4. Accept control stream, validate `InboundStreamTag::Alerts`, and require `Alert::Handshake`.
5. Spawn `IngestSlot`, register in `RoutingRegistry`, publish SSE online/offline events.

## Stream tags
Inbound unidirectional streams are typed by first byte (`frames.rs`):
- `0x10` alerts (persistent, established via bidi accept path),
- `0x11` video (persistent),
- `0x12` audio (persistent),
- `0x00` segment upload (one-shot),
- `0x01` init upload (one-shot),
- `0x02` manifest push (one-shot),
- `0x03` telemetry buffer upload (one-shot batch).

`slot.rs::stream_acceptor` dispatches these stream classes.

## Slot runtime (`slot.rs`)
Each slot owns:
- identity (`device_id`, `user_id`),
- broadcast channels for video/audio/telemetry,
- in-memory manifest + init segment,
- command queue to camera,
- subscriber counters for live-demand control,
- segment upload waiter map.

Spawned workers:
- alert reader,
- telemetry datagram reader,
- command writer,
- unidirectional stream acceptor (video/audio/uploads).

Any worker exit triggers slot cancellation and QUIC close.

## Alert handling (`alerts.rs`)
Alert dispatcher updates slot state and side effects:
- capability updates,
- recording segment metadata persistence,
- segment eviction tracking,
- upload waiter completion/failure propagation.

## Viewer demand coupling (`demand.rs`)
`ClientMode` transitions (`live` / `playback` / `map`) control command emission:
- first live subscriber sends `StartVideo` + `StartAudio`,
- last live subscriber leaving sends `StopVideo` + `StopAudio`.

This keeps camera-side live streaming demand-aligned with active viewers.

## Registry semantics (`registry.rs`)
`RoutingRegistry` maps `user_id -> device_id -> IngestSlot`.

Important invariants:
- new connection for same device replaces stale slot and shuts old slot down,
- unregister only removes matching `Arc` instance, preventing stale disconnect cleanup from removing newer sessions.
