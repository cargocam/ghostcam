//go:build tools

// Package tools pins tool dependencies used at development / build time but
// not imported by any production code. `go mod tidy` keeps them in go.sum
// and `go install` respects the pinned version.
//
// To regenerate ui/src/lib/api-types/ from the Go source of truth:
//
//	make generate-types
//
// CI refuses a PR whose checked-in generated file is out of sync.
package tools

import (
	_ "github.com/gzuidhof/tygo"
)
