# Ghostcam — Multi-Server Architecture

**Status:** Planned — not implemented in v1. The current system is explicitly single-instance. See `overview.md` §9 and `ingest.md` §5 for the documented single-instance constraint.

---

## 1. Overview

This document will specify the architecture for running multiple `server-multi` instances behind a load balancer, enabling horizontal scaling of the Ghostcam cloud service.

The core challenge is that the server's routing registry (`user_id → [IngestSlot]`, `user_id → [EgressHandle]`), manifest cache, and live subscriber counters are all process-local in-memory state. A camera connected to instance A is invisible to an observer connected to instance B. This document will specify how to resolve that.

---

## 2. Deployment Context

The Ghostcam cloud service runs on Fly.io. Fly.io's machine-level routing (Anycast, machine-specific DNS, `fly-prefer-region`) provides primitives that may make camera-to-instance affinity tractable without explicit client-side redirects. The multi-server design should be evaluated against Fly.io's capabilities before committing to a general-purpose solution.

---

## 3. Proposed Direction

**Redis as routing directory, not media relay.**

Routing actual media frames through Redis is not feasible at scale — frame volume at 2.5Mbps per camera with multiple observers would saturate a Redis cluster and add unacceptable latency.

The proposed model:

- Each server instance registers connected cameras in Redis: `camera:{device_id} → instance_addr` with a TTL tied to the QUIC connection lifetime
- When an observer requests a camera that is not connected to their instance, the server looks up the authoritative instance in Redis and either:
  - **Redirects** the observer's session to the correct instance (requires client awareness)
  - **Proxies** the WebRTC signaling to the correct instance (transparent to the client, more complex server-side)
- SSE events (`camera_online`, `camera_offline`) are fanned out via Redis pub/sub — each instance publishes events, all instances relay to their connected observers
- Live subscriber counts (`video_subscribers`, `audio_subscribers`) are maintained as Redis atomic counters rather than in-process atomics, ensuring `start_video`/`stop_video` decisions are correct across instances

**Camera affinity as an alternative.**

A simpler model: the load balancer routes each camera to a fixed instance based on consistent hashing of `device_id`. Observers are routed to the same instance as their cameras. This avoids cross-instance routing entirely but complicates failover — if an instance goes down, cameras and observers must be redistributed.

Fly.io's session affinity features may make this tractable without bespoke implementation.

---

## 4. Open Design Questions

| Question | Notes |
|----------|-------|
| Redis pub/sub vs camera affinity | Evaluate against Fly.io primitives before committing |
| Observer redirect vs proxy | Redirect is simpler server-side; proxy is transparent to the client |
| Manifest cache invalidation | How does instance B get an updated manifest when instance A's camera pushes one? Redis pub/sub notification + re-fetch, or push to all instances? |
| Subscriber count accuracy | Redis atomic counters are the natural answer; need to handle instance crash (counter not decremented) |
| Enrollment during multi-instance | Enrollment QUIC connections must hit the same instance as subsequent normal connections, or enrollment state must be fully in the database (it already is) |
| Fly.io-specific primitives | Evaluate `fly-prefer-region`, machine-specific routing, and Fly Machines API for camera affinity |

---

## 5. Prerequisites

This spec is blocked on:

- Validating the single-instance system at production load
- Confirming Fly.io deployment topology
- Evaluating Redis pub/sub throughput at expected camera and observer counts

Horizontal scaling is not needed until a single Fly.io instance is saturated. Vertical scaling (larger machine) should be exhausted first.

---

## 6. Dependencies

- `ingest.md` — routing registry must be extended to support cross-instance lookup
- `webrtc-client.md` — may require client-side redirect handling
- `telemetry.md` — Redis stream writes are already per-instance and naturally distributed; no changes expected
- `database.md` — enrollment state is already fully in the database; no changes expected
