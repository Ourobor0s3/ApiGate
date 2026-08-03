package middleware

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestClientIP(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	r.RemoteAddr = "203.0.113.5:56789"
	if got, want := ClientIP(r), "203.0.113.5"; got != want {
		t.Errorf("ClientIP() = %q, want %q", got, want)
	}

	r.Header.Set("X-Forwarded-For", "198.51.100.2, 10.0.0.1")
	if got, want := ClientIP(r), "198.51.100.2"; got != want {
		t.Errorf("ClientIP() with X-Forwarded-For = %q, want %q", got, want)
	}

	r.Header.Set("X-Forwarded-For", " 198.51.100.2 , 10.0.0.1")
	if got, want := ClientIP(r), "198.51.100.2"; got != want {
		t.Errorf("ClientIP() trims whitespace = %q, want %q", got, want)
	}
}

func TestRequestLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		io.WriteString(w, "teapot")
	})

	rec := httptest.NewRecorder()
	RequestLogger(logger, inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/teapot", nil))

	out := buf.String()
	for _, want := range []string{"msg=request", "method=GET", "path=/teapot", "status=418", "bytes=6"} {
		if !strings.Contains(out, want) {
			t.Errorf("RequestLogger() output %q missing %q", out, want)
		}
	}
	if strings.Contains(out, "query=") {
		t.Errorf("RequestLogger() should not log the query string, got %q", out)
	}
}

func TestRecover(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	rec := httptest.NewRecorder()
	Recover(logger, inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panic", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Recover() code = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(buf.String(), "panic recovered") {
		t.Errorf("Recover() output %q missing panic log", buf.String())
	}
}

func TestHealthUnreachable(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})

	rec := httptest.NewRecorder()
	Health(rdb, slog.Default()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("Health() code = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
