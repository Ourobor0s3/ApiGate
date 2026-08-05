package ratelimit

import (
	"testing"
)

func TestMemberUniqueness(t *testing.T) {
	rl := New(nil, Config{Limit: 1, Window: 60})

	// Members must stay unique even for requests in the same millisecond:
	// a ZSET dedupes by member, and colliding members would undercount.
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		m := rl.member(1234)
		if seen[m] {
			t.Fatalf("member %q repeated across calls in the same millisecond", m)
		}
		seen[m] = true
	}

	// Same sequence space keeps producing distinct members across windows too.
	if m := rl.member(5678); seen[m] {
		t.Fatalf("member %q collides across windows", m)
	}
}

func TestBypasses(t *testing.T) {
	rl := New(nil, Config{NoRateLimitPaths: []string{"/healthz"}})

	if !rl.bypasses("/healthz") {
		t.Error("bypasses(/healthz) = false, want true")
	}
	for _, p := range []string{"/weather", "/healthz/v2"} {
		if rl.bypasses(p) {
			t.Errorf("bypasses(%q) = true, want false", p)
		}
	}
}
