package main

import (
	"net"
	"net/http"
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

// Allow checks whether a request from the given IP is allowed.
func (rl *RateLimiter) Allow(ip string) bool {
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
		return false
	}
	entry.tokens--
	return true
}

// Middleware returns an HTTP middleware that rate-limits by client IP.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow(clientIP(r)) {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
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
