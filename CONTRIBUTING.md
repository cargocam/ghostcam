# Contributing

Thanks for the interest. A note about the shape of this project:

**Ghostcam is open source for reading, learning, self-hosting, and
forking — but it is not, today, accepting external pull requests.**

The team is small and PR review is its own job. We've made the choice
to keep the source code public so people can:

- Audit the camera firmware they're running on their own hardware.
- Hack on it for personal use, fork it, ship something else built on top.
- Learn from how the wire protocol, the provisioning flow, the live
  WebRTC relay, and the codegen pipeline are put together.

If you find something broken, please **file an issue** with a clear
repro and the commit SHA. We do read every issue. We just may not
merge a PR you open against it — we'll likely fix it ourselves and
credit you in the commit message.

If you find a **security issue**, see [SECURITY.md](SECURITY.md). Do
not open a public issue.

## Forking

If you want to ship a fork:

- The wire-contract surface lives in `common/types.go` +
  `common/telemetry.go` (Go server) and is mirrored to
  `camera/ghostcam/wire/` (Python via `tools/pydanticgen`) and
  `ui/src/lib/api-types/` (TS via `tygo`). `go generate ./...`
  regenerates both.
- The camera library's stable public API is locked in
  `camera/tests/test_public_api.py` — the `EXPECTED_API` set is the
  contract. Adding to it is fine across minor versions; renaming or
  removing requires a major bump.
- The cross-language ed25519 signing harness lives in
  `tools/sigverify`. Both camera and server must produce
  byte-identical signatures for the same inputs; the test suite
  enforces this.

## License

MIT — see [LICENSE](LICENSE) (or the `license` field in
`camera/pyproject.toml` for the camera package). Forks are welcome
to relicense compatibly.
