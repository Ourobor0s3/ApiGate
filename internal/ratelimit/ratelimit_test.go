package ratelimit

import "testing"

func TestBypasses(t *testing.T) {
	rl := New(nil, Config{NoRateLimitPaths: []string{"/healthz"}})

	for _, path := range []string{"/healthz"} {
		if !rl.bypasses(path) {
			t.Errorf("bypasses(%q) = false, want true", path)
		}
	}
	for _, path := range []string{"/weather", "/news", "/dashboard", "/healthz/v2", "/api/secrets"} {
		if rl.bypasses(path) {
			t.Errorf("bypasses(%q) = true, want false", path)
		}
	}
}
