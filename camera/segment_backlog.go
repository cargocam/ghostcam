package main

import (
	"os"
	"strings"
	"sync/atomic"
)

// Disk-side segment backlog telemetry (#115 bug 2). The watcher's
// oldest-first eviction can only see the symptom (disk filling); the
// server can only see the consequence (stalled uploads). We bridge by
// counting files in the segmentDir on every telemetry poll and shipping
// the value as LocalSegmentBacklog. The server edge-detects crossings
// of a threshold and emits an `upload_stalled` SSE event the UI
// renders as a distinct alert.

// segmentDirForTelemetry is the directory ReadTelemetry walks to
// count .m4s files. Stored as *string so main() can seed it after
// LoadConfig without ReadTelemetry needing a parameter. Test contexts
// leave it unset; SampleLocalSegmentBacklog then returns ok=false.
var segmentDirForTelemetry atomic.Pointer[string]

// SetSegmentDirForTelemetry seeds the package-level atomic. Called
// once from main() after cfg load.
func SetSegmentDirForTelemetry(dir string) {
	d := dir
	segmentDirForTelemetry.Store(&d)
}

// SampleLocalSegmentBacklog returns the count of .m4s segment files
// currently in the seeded segment directory. Returns ok=false when the
// directory hasn't been seeded (test context) or can't be read.
func SampleLocalSegmentBacklog() (uint32, bool) {
	p := segmentDirForTelemetry.Load()
	if p == nil || *p == "" {
		return 0, false
	}
	entries, err := os.ReadDir(*p)
	if err != nil {
		return 0, false
	}
	var n uint32
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".m4s") {
			n++
		}
	}
	return n, true
}
