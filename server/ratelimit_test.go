package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGlobalRateLimiter_BurstThenRejects(t *testing.T) {
	// 6/min → burst of 6 then refusal until tokens refill.
	g := NewGlobalRateLimiter(6)

	for i := 0; i < 6; i++ {
		ok, _ := g.Allow()
		if !ok {
			t.Fatalf("Allow #%d returned false inside burst window", i+1)
		}
	}
	ok, _ := g.Allow()
	if ok {
		t.Fatal("Allow #7 should be rejected — bucket should be empty after burst")
	}
}

func TestGlobalRateLimiter_RefillsOverTime(t *testing.T) {
	// 60/min → 1 token per second. Drain, wait, expect one back.
	g := NewGlobalRateLimiter(60)
	for i := 0; i < 60; i++ {
		g.Allow()
	}
	ok, _ := g.Allow()
	if ok {
		t.Fatal("expected empty bucket")
	}
	// Hand-advance the limiter's clock by 1.1s instead of sleeping.
	g.mu.Lock()
	g.lastCheck = g.lastCheck.Add(-1100 * time.Millisecond)
	g.mu.Unlock()

	ok, _ = g.Allow()
	if !ok {
		t.Fatal("expected at least one token to have refilled after 1.1s")
	}
}

func TestGlobalRateLimiter_ConcurrentAccountsForEveryRequest(t *testing.T) {
	// All requests share one bucket regardless of source IP. Run N goroutines
	// in parallel; total Allow() == true count must equal the burst size,
	// not the per-IP cap × N. This is the property the per-IP limiter
	// alone DOESN'T give us.
	g := NewGlobalRateLimiter(10)

	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := g.Allow(); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != 10 {
		t.Errorf("allowed = %d, want 10 — global limiter is leaking under concurrency", got)
	}
}

func TestGlobalRateLimiter_Middleware_Returns429(t *testing.T) {
	g := NewGlobalRateLimiter(1) // burst of 1
	called := atomic.Int32{}
	h := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Add(1)
		w.WriteHeader(http.StatusOK)
	}))

	// First request goes through.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/login", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("first request: code = %d, want 200", rec.Code)
	}

	// Second is throttled.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/login", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("second request: code = %d, want 429", rec.Code)
	}
	if called.Load() != 1 {
		t.Errorf("handler called %d times, want exactly 1", called.Load())
	}
	// Retry-After must be present, parse as a positive integer (seconds).
	ra := rec.Header().Get("Retry-After")
	if ra == "" {
		t.Error("Retry-After header missing on 429 response")
	} else if secs, err := strconv.Atoi(ra); err != nil || secs < 1 {
		t.Errorf("Retry-After = %q, want positive integer seconds", ra)
	}
}

func TestRateLimiter_Middleware_Returns429WithRetryAfter(t *testing.T) {
	// Per-IP limiter must also emit Retry-After. Use 1/min so the second
	// hit from the same IP is immediately rejected.
	rl := NewRateLimiter(1)
	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.Header.Set("Fly-Client-IP", "203.0.113.7")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: code = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: code = %d, want 429", rec.Code)
	}
	ra := rec.Header().Get("Retry-After")
	if ra == "" {
		t.Error("Retry-After header missing on 429 response")
	} else if secs, err := strconv.Atoi(ra); err != nil || secs < 1 {
		t.Errorf("Retry-After = %q, want positive integer seconds", ra)
	}
}

func TestSecondsUntilNextToken(t *testing.T) {
	// 10/min → 1/6s per token. Empty bucket needs ceil(6) = 6s.
	got := secondsUntilNextToken(0, 10.0/60.0)
	if got != 6*time.Second {
		t.Errorf("empty bucket at 10/min: got %v, want 6s", got)
	}
	// Half a token at 60/min → 0.5s to fill, rounded up to 1s minimum.
	got = secondsUntilNextToken(0.5, 1.0)
	if got != 1*time.Second {
		t.Errorf("0.5 tokens at 60/min: got %v, want 1s (floor)", got)
	}
	// Edge: rate=0 must not divide-by-zero or hang.
	got = secondsUntilNextToken(0, 0)
	if got != 1*time.Second {
		t.Errorf("zero rate: got %v, want 1s fallback", got)
	}
}
