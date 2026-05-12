package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGlobalRateLimiter_BurstThenRejects(t *testing.T) {
	// 6/min → burst of 6 then refusal until tokens refill.
	g := NewGlobalRateLimiter(6)

	for i := 0; i < 6; i++ {
		if !g.Allow() {
			t.Fatalf("Allow #%d returned false inside burst window", i+1)
		}
	}
	if g.Allow() {
		t.Fatal("Allow #7 should be rejected — bucket should be empty after burst")
	}
}

func TestGlobalRateLimiter_RefillsOverTime(t *testing.T) {
	// 60/min → 1 token per second. Drain, wait, expect one back.
	g := NewGlobalRateLimiter(60)
	for i := 0; i < 60; i++ {
		g.Allow()
	}
	if g.Allow() {
		t.Fatal("expected empty bucket")
	}
	// Hand-advance the limiter's clock by 1.1s instead of sleeping.
	g.mu.Lock()
	g.lastCheck = g.lastCheck.Add(-1100 * time.Millisecond)
	g.mu.Unlock()

	if !g.Allow() {
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
			if g.Allow() {
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
}
