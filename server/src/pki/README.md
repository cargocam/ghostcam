# pki

Certificate authority and PKI operations. The server acts as its own CA — it signs device certificates for cameras during enrollment and maintains a certificate revocation list (CRL).

## Bootstrap

On first start, `bootstrap::bootstrap_pki` checks `GHOSTCAM_DATA_DIR` for an existing CA keypair and server certificate. If absent:

1. Generates a CA keypair (`ca.key`, `ca.crt`) — self-signed, long-lived
2. Signs a server TLS certificate (`server.key`, `server.crt`) from the CA
3. Stores both on disk

The CA certificate is distributed to cameras during enrollment and used for mTLS verification.

## Enrollment Flow

```
camera sends Alert::Enrollment { enrollment_token }
    │
    ▼
pki::enrollment::sign_device_cert(ca, token, device_id)
    ├── verify enrollment JWT (issued by server, time-limited)
    ├── generate device keypair
    ├── sign device cert with CA
    └── return (device_cert_pem, device_key_pem)

server sends signed cert back to camera via alerts stream
camera stores cert, uses it for future mTLS connections
```

## Revocation

`RevocationCache` maintains an in-memory set of revoked fingerprints loaded from the DB at startup and updated on camera delete. The QUIC accept loop checks every connecting camera's cert fingerprint against this cache before proceeding.

## Files

| File | Purpose |
|------|---------|
| `bootstrap.rs` | `bootstrap_pki` — CA and server cert generation on first start |
| `ca.rs` | `CertificateAuthority` — signs device certificates |
| `enrollment.rs` | `sign_device_cert` — validates enrollment JWT, issues signed camera cert |
| `server_tls.rs` | Builds `rustls::ServerConfig` from the server cert for the QUIC endpoint |
| `revocation.rs` | `RevocationCache` — in-memory fingerprint blocklist |
| `unregister.rs` | Revoke a camera: add fingerprint to revocation list, delete DB record |
