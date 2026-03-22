# server-core/src/egress
Viewer-facing WebRTC egress path. Converts ingest slot broadcasts into per-viewer WebRTC media/data flows.

## Main components
- `handle.rs`: `EgressHandle` lifecycle and str0m integration.
- `data_channel.rs`: client command message schema (`client_mode`).
- `rtp.rs`: H.264 NAL parsing/accumulation helpers and timestamp conversion.
- `sessions.rs`: `SessionManager` for teardown/scoping.

## Session creation path
`/api/v1/watch` calls `EgressHandle::create(...)`:
1. Bind UDP socket.
2. Build ICE-lite str0m `Rtc` (H.264 + Opus enabled).
3. Add local host candidate using configured public IP + bound UDP port.
4. Accept browser SDP offer and generate answer.
5. Parse negotiated video/audio mids.
6. Create telemetry data channel.
7. Subscribe to slot broadcast channels (video/audio/telemetry).

The spawned handle event loop is then tracked in `SessionManager`.

## Runtime loop behavior (`EgressHandle::run`)
The loop multiplexes:
- str0m output polling (`Transmit`, `Timeout`, internal events),
- UDP receives from browser peer,
- slot video frames,
- slot audio frames,
- slot telemetry datagrams.

On shutdown, subscriber demand counters are decremented via `update_subscriber_demand`.

## Client mode protocol
Client can send JSON on commands data channel:
- `{"type":"client_mode","mode":"live|playback|map"}`

This feeds `ingest::demand` and controls camera-side start/stop command emission.

## Video packetization helpers
`rtp.rs` provides:
- `parse_annex_b` to split byte streams into NAL units,
- `NalAccumulator` to prepend buffered SPS/PPS/SEI before next VCL access unit,
- timestamp conversion helpers for 90kHz video and 48kHz audio clocks.

## Session management (`sessions.rs`)
`SessionManager` tracks active sessions by `session_id`:
- register,
- teardown by session,
- teardown by device (camera disconnect),
- teardown by user,
- ownership lookup for API authorization.
