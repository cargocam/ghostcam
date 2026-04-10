.PHONY: generate-types check-types

# Regenerate ui/src/lib/api-types/ from the Go source of truth. Run after
# changing any struct in common/ or server/apitypes/, then commit the
# updated TypeScript files alongside the Go change. Uses the Go 1.24+
# `tool` directive pinned in go.mod.
generate-types:
	@go tool tygo generate
	@echo "Generated api-types. Review ui/src/lib/api-types/ and commit."

# CI guard: regenerate and fail if anything drifted from what's checked in.
# If this fails locally, run `make generate-types` and commit the diff.
# --intent-to-add tells git about any untracked generated files so they
# show up in the diff output (raw git diff ignores untracked files).
check-types: generate-types
	@git add --intent-to-add ui/src/lib/api-types/ 2>/dev/null || true
	@git diff --exit-code -- ui/src/lib/api-types/ \
		|| (echo "ui/src/lib/api-types/ is stale. Run 'make generate-types' and commit." && exit 1)
