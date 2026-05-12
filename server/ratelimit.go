package main

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type rateLimitEntry struct {
	tokens    float64
	lastCheck time.Time
}

// RateLimiter is a per-IP token bucket rate limiter. Stale entries are
// evicted opportunistically on access instead of by a background sweeper,
// so there is no dedicated cleanup goroutine to manage.
type RateLimiter struct {
	mu         sync.Mutex
	entries    map[string]*rateLimitEntry
	rate       float64 // tokens per second
	maxBurst   float64 // bucket size
	idleExpiry time.Duration
}

// NewRateLimiter creates a rate limiter allowing reqsPerMin per IP per minute.
func NewRateLimiter(reqsPerMin int) *RateLimiter {
	return &RateLimiter{
		entries:    make(map[string]*rateLimitEntry),
		rate:       float64(reqsPerMin) / 60.0,
		maxBurst:   float64(reqsPerMin),
		idleExpiry: 10 * time.Minute,
	}
}

// Allow checks whether a request from the given IP is allowed. When
// rejected, retryAfter is the time until at least one token will be
// available; clients can echo it in the HTTP Retry-After header.
func (rl *RateLimiter) Allow(ip string) (allowed bool, retryAfter time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	// Opportunistic eviction: if the map has grown, drop entries whose last
	// check is older than idleExpiry. Amortizes cleanup across normal calls.
	if len(rl.entries) > 128 {
		cutoff := now.Add(-rl.idleExpiry)
		for k, v := range rl.entries {
			if v.lastCheck.Before(cutoff) {
				delete(rl.entries, k)
			}
		}
	}

	entry, ok := rl.entries[ip]
	if !ok {
		entry = &rateLimitEntry{tokens: rl.maxBurst, lastCheck: now}
		rl.entries[ip] = entry
	}

	elapsed := now.Sub(entry.lastCheck).Seconds()
	entry.tokens += elapsed * rl.rate
	if entry.tokens > rl.maxBurst {
		entry.tokens = rl.maxBurst
	}
	entry.lastCheck = now

	if entry.tokens < 1 {
		return false, secondsUntilNextToken(entry.tokens, rl.rate)
	}
	entry.tokens--
	return true, 0
}

// Middleware returns an HTTP middleware that rate-limits by client IP.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, retryAfter := rl.Allow(clientIP(r))
		if !ok {
			writeTooManyRequests(w, retryAfter)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GlobalRateLimiter is a single shared token bucket — no per-IP keying.
// Used in front of argon2-heavy endpoints to bound the *total* rate of
// expensive password verifications across all clients, defending against
// distributed credential stuffing where each individual IP stays under
// the per-IP cap but the aggregate would still saturate the server.
//
// The argon2 semaphore in `server/auth` bounds peak memory; this bounds
// the queue length feeding into it. Without the global cap, a botnet
// large enough to evade the per-IP limit could park thousands of
// goroutines at the semaphore — small per-request cost, but a real
// availability hit for legitimate users.
type GlobalRateLimiter struct {
	mu        sync.Mutex
	tokens    float64
	lastCheck time.Time
	rate      float64
	maxBurst  float64
}

// NewGlobalRateLimiter caps total requests at reqsPerMin across all
// clients. Burst size equals reqsPerMin (one minute's worth).
func NewGlobalRateLimiter(reqsPerMin int) *GlobalRateLimiter {
	return &GlobalRateLimiter{
		tokens:    float64(reqsPerMin),
		lastCheck: time.Now(),
		rate:      float64(reqsPerMin) / 60.0,
		maxBurst:  float64(reqsPerMin),
	}
}

// Allow returns whether a token is available (and consumes it). When
// rejected, retryAfter is the time until at least one token will be
// available — surfaced via the HTTP Retry-After header.
func (g *GlobalRateLimiter) Allow() (allowed bool, retryAfter time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := time.Now()
	g.tokens += now.Sub(g.lastCheck).Seconds() * g.rate
	if g.tokens > g.maxBurst {
		g.tokens = g.maxBurst
	}
	g.lastCheck = now

	if g.tokens < 1 {
		return false, secondsUntilNextToken(g.tokens, g.rate)
	}
	g.tokens--
	return true, 0
}

// Middleware rate-limits the wrapped handler at the global rate.
func (g *GlobalRateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, retryAfter := g.Allow()
		if !ok {
			writeTooManyRequests(w, retryAfter)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// secondsUntilNextToken computes how long until the bucket holds ≥1 token.
// Rounded up to the nearest whole second and minimum 1, since Retry-After
// is integer-seconds and 0 means "retry immediately" — which would defeat
// the rate-limit.
func secondsUntilNextToken(currentTokens, ratePerSec float64) time.Duration {
	if ratePerSec <= 0 {
		return time.Second
	}
	needed := 1.0 - currentTokens
	if needed <= 0 {
		return time.Second
	}
	secs := math.Ceil(needed / ratePerSec)
	if secs < 1 {
		secs = 1
	}
	return time.Duration(secs) * time.Second
}

// writeTooManyRequests writes a 429 with a Retry-After header. The header
// uses RFC 7231 delta-seconds form. Clients (browsers, fetch retry libs,
// CLI tools like `curl --retry`) respect it and pace their retries.
func writeTooManyRequests(w http.ResponseWriter, retryAfter time.Duration) {
	secs := int(retryAfter.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
}

// clientIP extracts the client IP from the request. Prefers Fly-Client-IP
// (trusted, set by Fly.io proxy and cannot be spoofed) over X-Forwarded-For.
func clientIP(r *http.Request) string {
	if fci := r.Header.Get("Fly-Client-IP"); fci != "" {
		return fci
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return xff[:i]
			}
		}
		return xff
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}
