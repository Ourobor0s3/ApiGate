package ratelimit

import (
	"net/http"
	"testing"
)

func TestClientIP(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	r.RemoteAddr = "203.0.113.5:56789"
	if got, want := clientIP(r), "203.0.113.5"; got != want {
		t.Errorf("clientIP() = %q, want %q", got, want)
	}

	r.Header.Set("X-Forwarded-For", "198.51.100.2, 10.0.0.1")
	if got, want := clientIP(r), "198.51.100.2"; got != want {
		t.Errorf("clientIP() with X-Forwarded-For = %q, want %q", got, want)
	}

	r.Header.Set("X-Forwarded-For", " 198.51.100.2 , 10.0.0.1")
	if got, want := clientIP(r), "198.51.100.2"; got != want {
		t.Errorf("clientIP() trims whitespace = %q, want %q", got, want)
	}
}
