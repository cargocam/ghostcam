package main

import (
	"sync"
	"testing"
	"time"
)

func TestHLSRequestCounter_QPSOverWindow(t *testing.T) {
	c := &hlsRequestCounter{}
	for i := 0; i < 60; i++ {
		c.inc()
	}
	got := c.qps5m()
	// 60 in current bucket → 60 / 300s = 0.2 qps
	if got < 0.19 || got > 0.21 {
		t.Errorf("qps = %f, want ~0.2", got)
	}
}

func TestHLSRequestCounter_AgesBucketsForward(t *testing.T) {
	c := &hlsRequestCounter{}
	c.inc()
	// Pretend that 6 minutes elapsed since the bucket was last touched
	// — advance() should zero every bucket because the gap exceeds the
	// ring length (5 buckets × 1 min).
	c.mu.Lock()
	c.last -= 6
	c.mu.Unlock()
	if got := c.qps5m(); got != 0 {
		t.Errorf("after 6m gap qps = %f, want 0", got)
	}
}

func TestHLSManifestCache_GetPutTTL(t *testing.T) {
	c := &hlsManifestCache{}
	body := []byte("#EXTM3U\nfake-manifest\n")
	c.put("device-a", body)
	if got := c.get("device-a"); string(got) != string(body) {
		t.Errorf("cache miss: got %q, want %q", got, body)
	}
	// Force the entry stale by rewriting expiresAt to the past.
	if v, ok := c.entries.Load("device-a"); ok {
		entry := v.(*hlsManifestCacheEntry)
		entry.expiresAt = time.Now().Add(-time.Second).UnixNano()
	}
	if got := c.get("device-a"); got != nil {
		t.Errorf("stale entry returned: %q", got)
	}
}

func TestHLSManifestCache_GetEvictsStaleOnAccess(t *testing.T) {
	// Stale entries should be deleted on `get()` so a connected camera
	// that's no longer queried doesn't hold a stale body indefinitely.
	c := &hlsManifestCache{}
	c.put("device-a", []byte("body"))

	// Force expiry.
	if v, ok := c.entries.Load("device-a"); ok {
		v.(*hlsManifestCacheEntry).expiresAt = time.Now().Add(-time.Second).UnixNano()
	}

	if got := c.get("device-a"); got != nil {
		t.Errorf("expected stale miss, got %q", got)
	}

	// Map should no longer have the entry — opportunistic eviction.
	if _, ok := c.entries.Load("device-a"); ok {
		t.Error("stale entry was not deleted on get()")
	}
}

func TestHLSManifestCache_Sweep(t *testing.T) {
	c := &hlsManifestCache{}
	// Three entries: two stale, one fresh.
	c.put("stale-1", []byte("a"))
	c.put("stale-2", []byte("b"))
	c.put("fresh", []byte("c"))
	for _, k := range []string{"stale-1", "stale-2"} {
		v, _ := c.entries.Load(k)
		v.(*hlsManifestCacheEntry).expiresAt = time.Now().Add(-time.Second).UnixNano()
	}

	evicted := c.sweep()
	if evicted != 2 {
		t.Errorf("sweep evicted %d, want 2", evicted)
	}
	if _, ok := c.entries.Load("fresh"); !ok {
		t.Error("sweep evicted a fresh entry")
	}
	if _, ok := c.entries.Load("stale-1"); ok {
		t.Error("sweep left stale-1 in place")
	}
}

func TestHLSManifestCache_IsolatesPerDevice(t *testing.T) {
	c := &hlsManifestCache{}
	c.put("a", []byte("body-a"))
	c.put("b", []byte("body-b"))
	if string(c.get("a")) != "body-a" {
		t.Error("device-a returned wrong body")
	}
	if string(c.get("b")) != "body-b" {
		t.Error("device-b returned wrong body")
	}
	if c.get("c") != nil {
		t.Error("unknown device should miss")
	}
}

func TestRuntimeNumGoroutine_CacheConsistentPair(t *testing.T) {
	// The new packed-atomic-pointer cache returns (count, expiry) from
	// a single load; readers can't observe a new expiry with an old
	// count. Drive a cache miss + verify the stored sample is fresh.
	cachedGoroutineSample.Store(nil) // reset
	n1 := runtimeNumGoroutine()
	if n1 < 1 {
		t.Errorf("count = %d, want >= 1", n1)
	}
	sample := cachedGoroutineSample.Load()
	if sample == nil {
		t.Fatal("cache not populated after miss")
	}
	if sample.count != n1 {
		t.Errorf("cache count = %d, returned %d", sample.count, n1)
	}
	if sample.expiryNs <= time.Now().UnixNano() {
		t.Error("cache expiry not in the future")
	}
	if sample.expiryNs > time.Now().Add(2*time.Second).UnixNano() {
		t.Error("cache expiry too far in the future")
	}
}

func TestHLSRequestCounter_ConcurrentInc(t *testing.T) {
	c := &hlsRequestCounter{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.inc()
		}()
	}
	wg.Wait()
	// 100 in the current bucket → 100 / 300s ≈ 0.333 qps
	got := c.qps5m()
	if got < 0.32 || got > 0.34 {
		t.Errorf("qps = %f, want ~0.333", got)
	}
}
