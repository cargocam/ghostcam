package server

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// rateLimitEntry tracks token bucket state for a single IP.
type rateLimitEntry struct {
	tokens    float64
	lastCheck time.Time
}

// RateLimiter is a simple per-IP token bucket rate limiter.
type RateLimiter struct {
	mu       sync.Mutex
	entries  map[string]*rateLimitEntry
	rate     float64 // tokens per second
	maxBurst float64 // max tokens (bucket size)
}

// NewRateLimiter creates a rate limiter allowing reqsPerMin requests per minute per IP.
func NewRateLimiter(reqsPerMin int) *RateLimiter {
	rl := &RateLimiter{
		entries:  make(map[string]*rateLimitEntry),
		rate:     float64(reqsPerMin) / 60.0,
		maxBurst: float64(reqsPerMin),
	}
	// Periodically clean up stale entries
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			rl.cleanup()
		}
	}()
	return rl
}

func (rl *RateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Add(-10 * time.Minute)
	for ip, entry := range rl.entries {
		if entry.lastCheck.Before(cutoff) {
			delete(rl.entries, ip)
		}
	}
}

// Allow checks whether a request from the given IP is allowed.
func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
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

// Middleware returns an HTTP middleware that rate-limits requests by client IP.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !rl.Allow(ip) {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP extracts the client IP from the request. Prefers Fly-Client-IP
// (trusted, set by Fly.io proxy and cannot be spoofed by clients) over
// X-Forwarded-For (can be forged when not behind a reverse proxy).
func clientIP(r *http.Request) string {
	// Fly.io's trusted client IP header — cannot be spoofed
	if fci := r.Header.Get("Fly-Client-IP"); fci != "" {
		return fci
	}
	// X-Forwarded-For — only safe behind a trusted proxy, but use as fallback
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
