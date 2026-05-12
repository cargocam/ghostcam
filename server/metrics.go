package main

import (
	"context"
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
// O(connected cameras) entries.
//
// Stale entries are evicted two ways:
//   1. Opportunistically on `get()` — any reader that hits a stale
//      entry deletes it before returning miss. Handles the common
//      "camera still attached, just past TTL" case in steady state.
//   2. Periodically via the sweeper goroutine started from main()
//      under runHLSManifestCacheSweeper. Handles the long-tail case
//      where a camera disconnects forever; without this the map
//      would grow without bound by deviceID count.
type hlsManifestCache struct {
	entries sync.Map // deviceID → *hlsManifestCacheEntry
}

type hlsManifestCacheEntry struct {
	body      []byte
	expiresAt int64 // unix nanos
}

const hlsManifestCacheTTL = time.Second

// get returns the cached body for a device or nil if the entry is
// absent / stale. Stale entries are deleted on read so a connected
// camera that's no longer queried doesn't hold a reference to a
// stale body indefinitely.
func (c *hlsManifestCache) get(deviceID string) []byte {
	v, ok := c.entries.Load(deviceID)
	if !ok {
		return nil
	}
	entry := v.(*hlsManifestCacheEntry)
	if time.Now().UnixNano() > entry.expiresAt {
		c.entries.Delete(deviceID)
		return nil
	}
	return entry.body
}

// sweep evicts every entry whose TTL has expired. Called periodically
// by runHLSManifestCacheSweeper. Cheap: sync.Map.Range iterates a
// snapshot, and the entry count is bounded by O(connected cameras).
func (c *hlsManifestCache) sweep() int {
	now := time.Now().UnixNano()
	evicted := 0
	c.entries.Range(func(k, v any) bool {
		entry := v.(*hlsManifestCacheEntry)
		if now > entry.expiresAt {
			c.entries.Delete(k)
			evicted++
		}
		return true
	})
	return evicted
}

// runHLSManifestCacheSweeper periodically evicts expired entries. The
// `get()` path also evicts on access, but a deviceID that stops being
// queried (camera offline, viewer left) would otherwise sit in the map
// forever. Period chosen well above the 1 s TTL so we don't churn
// freshly-replaced entries — we're guarding against orphans, not
// optimising hit rate.
func runHLSManifestCacheSweeper(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hlsCache.sweep()
		}
	}
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

// goroutineSample packs count + expiry behind a single atomic pointer
// so a concurrent reader sees a consistent pair. Previously these were
// two independent atomic.Int64 — a reader could observe a new expiry
// with an old count. The drift was bounded for this use case (gauge,
// no percentile maths) but the pattern would bite us the moment we
// added a "report N to logs every K seconds" check that wanted both
// values from the same snapshot.
type goroutineSample struct {
	count    int
	expiryNs int64
}

var cachedGoroutineSample atomic.Pointer[goroutineSample]

// runtimeNumGoroutine returns a 1-second-cached count. Multiple
// concurrent readers may all observe a stale cache and call
// numGoroutineSlow; last-write-wins on the atomic pointer. Values are
// monotonically close so the resulting drift is negligible.
func runtimeNumGoroutine() int {
	now := time.Now().UnixNano()
	if s := cachedGoroutineSample.Load(); s != nil && s.expiryNs > now {
		return s.count
	}
	n := numGoroutineSlow()
	cachedGoroutineSample.Store(&goroutineSample{
		count:    n,
		expiryNs: now + int64(time.Second),
	})
	return n
}
