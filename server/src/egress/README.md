# egress

WebRTC egress pipeline. Each viewer × camera pair is one `EgressHandle` running a str0m WebRTC session on its own UDP socket. The server operates in ICE-lite mode — it never initiates connectivity checks, only responds to STUN binding requests from the browser.

## Flow

```
POST /api/v1/watch { device_id, sdp_offer }
    │
    ▼
EgressHandle::create()
    ├── bind UDP socket (0.0.0.0:0)
    ├── build str0m Rtc (ICE-lite, H.264 + Opus only)
    ├── add local candidate: public_ip:bound_port
    ├── accept_offer(sdp_offer) → SdpAnswer
    ├── parse video mid, audio mid, telemetry channel from answer SDP
    └── subscribe to IngestSlot broadcast channels

EgressHandle::run()
    loop {
        poll str0m output (Transmit → send UDP, Event → handle)
        recv UDP packet → rtc.handle_input(Receive)
        recv timeout → rtc.handle_input(Timeout)
        recv VideoFrame → packetize H.264 → rtc writer
        recv AudioFrame → rtc writer (one RTP per Opus frame)
        recv TelemetryDatagram → send JSON on data channel
    }
```

ICE candidate note: browser SDP offers may include mDNS-obfuscated candidates (Firefox) that str0m cannot parse. The viewer strips all `a=candidate` lines before posting the offer — safe because the server is ICE-lite and never uses the browser's candidates anyway.

## Files

| File | Purpose |
|------|---------|
| `handle.rs` | `EgressHandle` — one per viewer×camera: WebRTC session lifecycle, UDP event loop, frame dispatch |
| `sessions.rs` | `SessionManager` — concurrent map of `session_id → EgressHandle`, creation and teardown, per-user session counting (for MAX_SESSIONS_PER_USER limit) |
| `rtp.rs` | H.264 NAL → RTP packetization (Single NAL ≤ 1188 bytes, FU-A fragmentation for larger). Timestamp conversion (µs → 90 kHz video, 48 kHz audio). |
| `data_channel.rs` | `ClientMessage` — JSON messages sent to the viewer on the WebRTC data channel (telemetry, camera events) |
