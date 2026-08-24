package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBreakerStateMachine(t *testing.T) {
	b := NewBreaker(BreakerConfig{FailureThreshold: 3, Cooldown: 30 * time.Second})
	if got := b.state(); got != "closed" {
		t.Fatalf("initial state = %q, want closed", got)
	}

	for i := 0; i < 3; i++ {
		b.failure()
	}
	if got := b.state(); got != "open" {
		t.Fatalf("after 3 failures state = %q, want open", got)
	}
	if b.allow() {
		t.Fatal("allow() during open must return false")
	}

	b.success()
	if got := b.state(); got != "closed" {
		t.Fatalf("after success state = %q, want closed", got)
	}
	if !b.allow() {
		t.Fatal("allow() after close must return true")
	}
}

func TestBreakerRoundTripper(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// Always-failing upstream opens the breaker, then requests fail fast.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()

	rt := &breakerRoundTripper{
		next:    http.DefaultTransport,
		breaker: NewBreaker(BreakerConfig{FailureThreshold: 2, Cooldown: time.Minute}),
	}

	// First request goes through and records a failure.
	req := httptest.NewRequest(http.MethodGet, failing.URL, nil)
	resp, err := rt.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first request: resp=%v err=%v", resp, err)
	}
	resp.Body.Close()
	// Second request records another failure and trips the breaker; it must
	// still reach the upstream and carry the upstream status through.
	resp, err = rt.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second request: resp=%v err=%v, want 503 from upstream", resp, err)
	}
	resp.Body.Close()
	// Third request is short-circuited.
	if _, err := rt.RoundTrip(req); err != errCircuitOpen {
		t.Fatalf("third request err = %v, want errCircuitOpen", err)
	}

	// A healthy upstream recovers the breaker.
	rt2 := &breakerRoundTripper{
		next:    http.DefaultTransport,
		breaker: NewBreaker(BreakerConfig{FailureThreshold: 2, Cooldown: time.Minute}),
	}
	req2 := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
	for i := 0; i < 2; i++ {
		if _, err := rt2.RoundTrip(req2); err != nil {
			t.Fatalf("healthy request %d failed: %v", i, err)
		}
	}
	if got := rt2.breaker.state(); got != "closed" {
		t.Fatalf("state after healthy requests = %q, want closed", got)
	}
}

func TestZeroThresholdDisablesBreaker(t *testing.T) {
	b := NewBreaker(BreakerConfig{})
	b.failure()
	b.failure()
	if got := b.state(); got != "closed" {
		t.Fatalf("disabled breaker state = %q, want closed", got)
	}
	if !b.allow() {
		t.Fatal("disabled breaker must always allow")
	}
}

func TestFailedProbeReopensCircuit(t *testing.T) {
	b := NewBreaker(BreakerConfig{FailureThreshold: 2, Cooldown: time.Minute})
	for i := 0; i < 2; i++ {
		b.failure()
	}
	// Force the cooldown to have elapsed so the next allow() is the half-open
	// probe, then fail it. The circuit must re-trip open rather than leaving
	// the probe slot taken forever.
	b.openUntil = time.Now().Add(-time.Second)

	if !b.allow() {
		t.Fatal("allow() after cooldown must admit the probe")
	}
	if b.allow() {
		t.Fatal("second allow() must fail while the probe is in flight")
	}
	b.failure()
	if got := b.state(); got != "open" {
		t.Fatalf("state after failed probe = %q, want open", got)
	}
	if b.allow() {
		t.Fatal("allow() must keep failing fast while re-opened")
	}
}
