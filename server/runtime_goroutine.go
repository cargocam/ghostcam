package main

import "runtime"

// numGoroutineSlow is the actual runtime call. Isolated so the metrics
// cache layer above can wrap it and tests can stub it.
func numGoroutineSlow() int { return runtime.NumGoroutine() }
