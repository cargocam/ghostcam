# server-multi
Multi-tenant deployment crate with Postgres-backed `Database` implementation.

## Current status on rewrite branch
`server-multi` currently initializes schema and DB layer, but does not yet start HTTP/QUIC runtime services. The process exits after successful DB init.

`src/main.rs` currently:
1. reads `DATABASE_URL`,
2. connects to Postgres,
3. runs migrations and initialization,
4. logs readiness of DB layer.

## What is implemented
`src/db.rs` implements `server_core::db::Database` for Postgres:
- user management (`create_user`, lookup, update),
- camera CRUD scoped by user UUID,
- enrollment token management,
- session CRUD,
- API token CRUD + last-used updates,
- password verification/change,
- HMAC secret persistence,
- migration lock via Postgres advisory lock.

## Schema
`migrations/001_initial.sql` creates:
- `config`
- `users`
- `sessions`
- `cameras`
- `enrollment_tokens`
- `api_tokens`

UUIDs and timestamp semantics are native Postgres (`UUID`, `TIMESTAMPTZ`, `INET`).

## Tests
Integration tests under `tests/db_tests.rs` are `#[ignore]` by default and expect a reachable Postgres instance:
- set `DATABASE_URL`,
- run with `cargo test -p server-multi -- --ignored`.

## Intended role
This crate is the foundation for a future full multi-user deployment that reuses `server-core` runtime modules while swapping SQLite assumptions for tenant-aware Postgres behavior.
