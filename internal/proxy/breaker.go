package proxy

import (
	"errors"
	"net/http"
	"sync"
	"time"
)

// errCircuitOpen short-circuits requests while an upstream is down.
var errCircuitOpen = errors.New("circuit open")

// BreakerConfig configures an upstream circuit breaker.
type BreakerConfig struct {
	// FailureThreshold is the number of consecutive failures that opens the
	// circuit, failing fast instead of hammering a dead upstream.
	FailureThreshold int
	// Cooldown is how long the circuit stays open before a half-open probe.
	Cooldown time.Duration
}

// Breaker guards a single upstream with a closed → open → half-open state
// machine. It short-circuits requests while the circuit is open and lets one
// probe through during half-open to decide whether to recover.
type Breaker struct {
	mu        sync.Mutex
	cfg       BreakerConfig
	failures  int
	openUntil time.Time
	probing   bool
}

// NewBreaker returns a breaker; a zero threshold disables the breaker.
func NewBreaker(cfg BreakerConfig) *Breaker {
	return &Breaker{cfg: cfg}
}

// state returns "closed", "open" or "half-open" for the current moment.
func (b *Breaker) state() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cfg.FailureThreshold <= 0 {
		return "closed"
	}
	if b.openUntil.After(time.Now()) {
		return "open"
	}
	if !b.openUntil.IsZero() {
		return "half-open"
	}
	return "closed"
}

// allow reports whether the request may proceed. When the circuit is half-open
// exactly one request is let through; further ones fail fast until it closes.
func (b *Breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cfg.FailureThreshold <= 0 {
		return true
	}
	if b.openUntil.After(time.Now()) {
		return false // open
	}
	if !b.openUntil.IsZero() {
		if b.probing {
			return false // half-open, probe slot taken
		}
		b.probing = true // half-open, admit the single probe
		return true
	}
	return true // closed
}

// success records a healthy response, closing the circuit and resetting the
// failure count.
func (b *Breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.openUntil = time.Time{}
	b.probing = false
}

// failure records an upstream error; reaching the threshold trips the breaker
// open for the cooldown period. A failed half-open probe re-trips the circuit
// open instead of leaving the probe slot permanently taken.
func (b *Breaker) failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cfg.FailureThreshold <= 0 {
		return
	}
	if b.probing {
		b.failures = 0
		b.openUntil = time.Now().Add(b.cfg.Cooldown)
		b.probing = false
		return
	}
	b.failures++
	if b.failures >= b.cfg.FailureThreshold {
		b.failures = 0
		b.openUntil = time.Now().Add(b.cfg.Cooldown)
		b.probing = false
	}
}

// breakerRoundTripper wraps a transport and short-circuits requests while the
// breaker is open.
type breakerRoundTripper struct {
	next    http.RoundTripper
	breaker *Breaker
}

func (t *breakerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.breaker.allow() {
		return nil, errCircuitOpen
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		t.breaker.failure()
		return nil, err
	}
	if resp.StatusCode >= 500 {
		t.breaker.failure()
		return resp, nil
	}
	t.breaker.success()
	return resp, nil
}
