# server-core/src/pki
PKI and enrollment primitives used by server deployments.

## Components
- `ca.rs`: in-memory CA manager (load/generate, CSR signing, enrollment JWT signing/verification, user-cert verification).
- `bootstrap.rs`: first-run PKI bootstrap (`ca.crt`, `ca.key`, `server.crt`, `server.key`) and reload path.
- `server_tls.rs`: self-signed server TLS cert generation/loading.
- `enrollment.rs`: enrollment JWT claim structures.
- `revocation.rs`: in-memory revoked-serial cache.
- `unregister.rs`: unregister workflow (revoke + DB delete + optional Redis purge).

## Enrollment trust chain
1. User requests enrollment token via API.
2. Server signs enrollment JWT (ES256) using CA keypair.
3. Camera submits token + CSR over enrollment connection.
4. Server verifies JWT and signs CSR with instance CA.
5. Camera receives `cert_refresh` payload and persists cert chain material.

## CA manager behavior (`ca.rs`)
`CaManager` supports:
- generation of a long-lived instance CA (`Ghostcam Instance CA`),
- CSR signing with `ClientAuth` EKU and CN set to `device_id`,
- ES256 JWT signing/verification for enrollment tokens,
- verification that presented user cert chains to this CA.

## Bootstrap behavior (`bootstrap.rs`)
On first run:
- create PKI directory,
- generate CA and server TLS cert/key files,
- return `is_first_run = true`.

On subsequent runs:
- load existing PEM files into runtime structs,
- preserve certificate fingerprints across restarts.

## Revocation model
- `RevocationCache` is an in-memory set checked during ingest accept.
- `unregister_camera()` updates cache and DB, and optionally persists revocation to Redis (`redis::revocation::revoke_cert`).
- Redis-driven refresh loops live under `server-core/src/redis/revocation.rs`.
