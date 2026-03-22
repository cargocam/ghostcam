# camera/src/recording
This subsystem maintains a local rolling fMP4 archive on the camera and serves upload requests from the server.

## What it owns
- fMP4 init segment generation (`init.rs`)
- segment writing (`segment.rs`)
- segment rotation and muxing (`muxer.rs`)
- on-device ring-buffer indexing + eviction (`ring_buffer.rs`)
- HLS manifest generation + push (`manifest.rs`, `manifest_push.rs`)
- on-demand segment/init uploads (`uploads.rs`)
- optional storage/recovery helpers (`storage.rs`, `recovery.rs`)

## Segment lifecycle
1. `Muxer` consumes broadcast video/audio frames.
2. SPS/PPS are cached from video NALs.
3. First IDR with SPS+PPS produces `SegmentEvent::InitReady`.
4. Segment starts once init is available (`segment_id = "<device_fingerprint>:<unix_ms>"`).
5. On IDR boundary + duration threshold, current segment finalizes to `.m4s`.
6. Segment metadata is registered in the ring buffer.
7. `SegmentEvent::Finalized` and `SegmentEvent::ManifestUpdated` are emitted.

## Ring buffer behavior
`RingBuffer` scans existing `.m4s` files at startup and tracks:
- `segment_id`
- `start_ts`, `end_ts`
- file size + path

Eviction APIs:
- `ensure_space(needed_bytes)`: evict oldest until enough bytes freed.
- `emergency_evict(count)`: drop oldest N segments immediately.

Evictions emit `SegmentEvent::Evicted` for upstream alerting.

## Upload command handling
`uploads.rs` handles:
- `UploadCommand::Segment { seq, segment_id }`
- `UploadCommand::Init { seq }`

Segment upload stream format:
1. stream tag `InboundStreamTag::Segment`
2. segment id length (`u16`, big-endian)
3. segment id bytes
4. segment payload bytes

Failure paths emit `Alert::SegmentUploadFailed` (evicted / I/O error).

## Manifest format
`manifest.rs` emits rolling HLS-style playlist text with:
- `#EXTM3U`
- `#EXT-X-VERSION:7`
- `#EXT-X-TARGETDURATION:10`
- `#EXT-X-MAP:URI="init.mp4?v=<latest_start_ts>"`
- relative segment URIs (`./<segment_id>.m4s`)

The explicit `./` prefix avoids browser URL parsing issues for IDs containing `:`.

## Storage and recovery helpers
- `storage.rs`: storage-full pause/recovery policy hooks.
- `recovery.rs`: directory scan + empty-file cleanup + manifest rebuild.

These modules are available for durability/ops flows, while the active runtime path primarily depends on `muxer + ring_buffer + uploads + manifest_push`.
