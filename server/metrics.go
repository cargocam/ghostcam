package main

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cargocam/ghostcam/server/auth"
)

// serverStartedAt is set once in main.go (init in metrics.go would
// race with config loading) — read by the metrics endpoint to report
// process uptime alongside the gauges.
var serverStartedAt = time.Now()

// hlsRequestCounter is a wall-time sliding 5-minute counter. Used by
// the admin metrics endpoint and lazily incremented by GetLiveManifest /
// GetVodManifest. 60-second buckets keep the bookkeeping cheap.
type hlsRequestCounter struct {
	mu      sync.Mutex
	buckets [5]uint64 // 1-minute buckets, indexed by minute-of-window
	last    int64     // monotonic minute that buckets[0] represents
}

func (c *hlsRequestCounter) inc() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advance(time.Now().Unix() / 60)
	c.buckets[0]++
}

// advance rolls the ring forward to a new "current minute" key, zeroing
// any buckets that the cursor has lapped. Idempotent if called on the
// same key twice.
func (c *hlsRequestCounter) advance(nowMin int64) {
	if c.last == 0 {
		c.last = nowMin
		return
	}
	gap := nowMin - c.last
	if gap <= 0 {
		return
	}
	if gap >= int64(len(c.buckets)) {
		for i := range c.buckets {
			c.buckets[i] = 0
		}
	} else {
		copy(c.buckets[gap:], c.buckets[:int64(len(c.buckets))-gap])
		for i := int64(0); i < gap; i++ {
			c.buckets[i] = 0
		}
	}
	c.last = nowMin
}

// qps5m returns the per-second rate over the last 5 minutes. Reading
// also rolls the ring forward so stale buckets don't inflate the rate.
func (c *hlsRequestCounter) qps5m() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advance(time.Now().Unix() / 60)
	var total uint64
	for _, n := range c.buckets {
		total += n
	}
	return float64(total) / float64(len(c.buckets)*60)
}

// hlsManifestCache caches the rendered live.m3u8 body for ~1 s. Reads
// take a single map lookup; only the writer holds the lock. Sized for
// O(connected cameras) entries which is small enough that an unbounded
// sync.Map is fine. Entries are reaped opportunistically by the next
// reader after their TTL elapses — no background sweeper.
type hlsManifestCache struct {
	entries sync.Map // deviceID → *hlsManifestCacheEntry
}

type hlsManifestCacheEntry struct {
	body      []byte
	expiresAt int64 // unix nanos
}

const hlsManifestCacheTTL = time.Second

// get returns the cached body for a device or nil if the entry is
// absent / stale. A stale entry is left in place so the next miss can
// race-replace it; no need to evict.
func (c *hlsManifestCache) get(deviceID string) []byte {
	v, ok := c.entries.Load(deviceID)
	if !ok {
		return nil
	}
	entry := v.(*hlsManifestCacheEntry)
	if time.Now().UnixNano() > entry.expiresAt {
		return nil
	}
	return entry.body
}

func (c *hlsManifestCache) put(deviceID string, body []byte) {
	c.entries.Store(deviceID, &hlsManifestCacheEntry{
		body:      body,
		expiresAt: time.Now().Add(hlsManifestCacheTTL).UnixNano(),
	})
}

// Process-global instances. main.go's router wires them to handlers.
// HLS counter doubles as the cache's "hot" signal — the cache only
// helps when QPS is non-trivial.
var (
	hlsCounter = &hlsRequestCounter{}
	hlsCache   = &hlsManifestCache{}
)

// AdminMetricsResponse is the admin-only health snapshot. JSON output
// because the UI consumes it directly; if we ever need Prometheus
// scraping we can layer a /metrics handler over the same gauges.
type AdminMetricsResponse struct {
	ServerUptimeSeconds        uint64  `json:"server_uptime_seconds"`
	Argon2SemaphoreDepth       int     `json:"argon2_semaphore_depth"`
	Argon2SemaphoreCapacity    int     `json:"argon2_semaphore_capacity"`
	GlobalRateLimiterTokens    float64 `json:"global_rate_limiter_tokens"`
	GlobalRateLimiterCapacity  float64 `json:"global_rate_limiter_capacity"`
	HLSManifestQPS5m           float64 `json:"hls_manifest_qps_5m"`
	HLSManifestCacheSize       int     `json:"hls_manifest_cache_size"`
	WHEPActiveSessions         int     `json:"whep_active_sessions"`
	LiveWSConnectedCameras     int     `json:"live_ws_connected_cameras"`
	GoroutineCount             int     `json:"goroutine_count"`
}

// AdminMetrics handles GET /api/v1/admin/metrics.
func (a *App) AdminMetrics(w http.ResponseWriter, r *http.Request) {
	a.WHEP.mu.Lock()
	whepCount := len(a.WHEP.sessions)
	a.WHEP.mu.Unlock()

	a.Live.mu.Lock()
	liveCount := len(a.Live.sessions)
	a.Live.mu.Unlock()

	resp := AdminMetricsResponse{
		ServerUptimeSeconds:       uint64(time.Since(serverStartedAt).Seconds()),
		Argon2SemaphoreDepth:      auth.Argon2SemaphoreDepth(),
		Argon2SemaphoreCapacity:   auth.Argon2SemaphoreCapacity(),
		GlobalRateLimiterTokens:   a.Argon2GlobalRL.Tokens(),
		GlobalRateLimiterCapacity: a.Argon2GlobalRL.MaxBurst(),
		HLSManifestQPS5m:          hlsCounter.qps5m(),
		HLSManifestCacheSize:      hlsManifestCacheSize(),
		WHEPActiveSessions:        whepCount,
		LiveWSConnectedCameras:    liveCount,
		GoroutineCount:            runtimeGoroutineCount(),
	}
	writeJSON(w, http.StatusOK, resp)
}

func hlsManifestCacheSize() int {
	var n int
	hlsCache.entries.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// runtimeGoroutineCount is split out so tests can monkey-patch by
// replacing the package-level var. Atomic Int64 over int because we
// want to allow zero-cost concurrent reads from the metrics endpoint
// even though the underlying function is itself cheap.
var goroutineCountFn = func() int { return runtimeNumGoroutine() }

func runtimeGoroutineCount() int { return goroutineCountFn() }

// runtimeNumGoroutine is a thin wrapper so we can stub it in tests
// without dragging in the full runtime package at call sites. Marked
// with an atomic.Int64 cache to keep the cost truly trivial — only
// refreshed once a second.
var (
	cachedGoroutineCount  atomic.Int64
	cachedGoroutineExpiry atomic.Int64
)

func runtimeNumGoroutine() int {
	now := time.Now().UnixNano()
	if cachedGoroutineExpiry.Load() > now {
		return int(cachedGoroutineCount.Load())
	}
	n := numGoroutineSlow()
	cachedGoroutineCount.Store(int64(n))
	cachedGoroutineExpiry.Store(now + int64(time.Second))
	return n
}
