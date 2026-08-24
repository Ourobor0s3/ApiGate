package middleware

import (
	"bytes"
	"compress/gzip"
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

	// X-Forwarded-For is client-supplied and must never be trusted blindly.
	r.Header.Set("X-Forwarded-For", "198.51.100.2, 10.0.0.1")
	if got, want := ClientIP(r), "203.0.113.5"; got != want {
		t.Errorf("ClientIP() must ignore spoofed X-Forwarded-For, got %q, want %q", got, want)
	}
}

func TestForwardedClientIP(t *testing.T) {
	resolve, err := ForwardedClientIP("10.0.0.0/8", "192.168.1.0/24")
	if err != nil {
		t.Fatalf("ForwardedClientIP() error: %v", err)
	}

	forward := func(remote, xff string) string {
		r, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return resolve(r)
	}

	// Trusted proxy: X-Forwarded-For wins.
	if got, want := forward("10.0.0.5:9000", "203.0.113.9"), "203.0.113.9"; got != want {
		t.Errorf("trusted proxy XFF = %q, want %q", got, want)
	}
	// Untrusted peer presenting a spoofed header: RemoteAddr wins.
	if got, want := forward("198.51.100.1:9000", "203.0.113.9"), "198.51.100.1"; got != want {
		t.Errorf("untrusted peer XFF = %q, want %q", got, want)
	}
}

func TestForwardedClientIPRejectsBadCIDR(t *testing.T) {
	if _, err := ForwardedClientIP("not-a-cidr"); err == nil {
		t.Error("ForwardedClientIP() accepted an invalid CIDR")
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
	RequestLogger(logger, ClientIP, inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/teapot", nil))

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

// TestRequestLoggerDefaultsStatusTo200 covers handlers that write nothing at
// all: the recorder status stays 0 and the log must still report a 200.
func TestRequestLoggerDefaultsStatusTo200(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	rec := httptest.NewRecorder()
	RequestLogger(logger, ClientIP, inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/empty", nil))

	if out := buf.String(); !strings.Contains(out, "status=200") {
		t.Errorf("RequestLogger() output %q missing status=200", out)
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

// TestStatusRecorderDefaultsTo200 covers handlers (e.g. SSE) that Write without
// an explicit WriteHeader: the recorder must still report 200.
func TestStatusRecorderDefaultsTo200(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &StatusRecorder{ResponseWriter: rec}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	inner.ServeHTTP(sr, httptest.NewRequest(http.MethodGet, "/", nil))

	if sr.Status != http.StatusOK {
		t.Errorf("Status = %d, want %d", sr.Status, http.StatusOK)
	}
	if sr.Bytes != 2 {
		t.Errorf("Bytes = %d, want 2", sr.Bytes)
	}
}

// Bodiless statuses (204, 304) must never grow a Content-Encoding header.
func TestGzipSkipsBodilessStatuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body []byte
	}{
		{"204 no content", http.StatusNoContent, nil},
		{"304 not modified", http.StatusNotModified, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.code)
				if len(tc.body) > 0 {
					_, _ = w.Write(tc.body)
				}
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			Gzip(inner).ServeHTTP(rec, req)
			if enc := rec.Header().Get("Content-Encoding"); enc != "" {
				t.Errorf("Content-Encoding = %q for %d, want none", enc, tc.code)
			}
		})
	}
}

func TestGzipCompressesJSON(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.Repeat([]byte(`{"k":1}`), 100))
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	Gzip(inner).ServeHTTP(rec, req)
	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not valid gzip: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil || len(got) == 0 {
		t.Fatalf("gzip round-trip failed: %v (%d bytes)", err, len(got))
	}
}
