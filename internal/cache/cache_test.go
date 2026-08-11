package cache

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestCacheKey(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "http://example.com/weather?lat=1&lon=2", nil)
	if got, want := cacheKey(r), "cache:GET:/weather?lat=1&lon=2"; got != want {
		t.Errorf("cacheKey() = %q, want %q", got, want)
	}

	r, _ = http.NewRequest(http.MethodGet, "http://example.com/news", nil)
	if got, want := cacheKey(r), "cache:GET:/news"; got != want {
		t.Errorf("cacheKey() = %q, want %q", got, want)
	}
}

// TestCacheKeyIgnoresAPIKey guards against leaking the newsapi key into Redis
// keys: the key must be identical regardless of the apiKey value or presence.
func TestCacheKeyIgnoresAPIKey(t *testing.T) {
	with, _ := http.NewRequest(http.MethodGet, "http://example.com/news?apiKey=secret&q=hi", nil)
	without, _ := http.NewRequest(http.MethodGet, "http://example.com/news?q=hi", nil)
	reordered, _ := http.NewRequest(http.MethodGet, "http://example.com/news?q=hi&apiKey=other", nil)

	want := "cache:GET:/news?q=hi"
	for name, r := range map[string]*http.Request{"with": with, "without": without, "reordered": reordered} {
		if got := cacheKey(r); got != want {
			t.Errorf("cacheKey(%s) = %q, want %q", name, got, want)
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	resp := &cachedResponse{
		Status: 200,
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Custom":     []string{"a", "b"},
		},
		Body: []byte(`{"hello":"world"}`),
	}

	got, err := decodeResponse(encodeResponse(resp))
	if err != nil {
		t.Fatalf("decodeResponse() error: %v", err)
	}
	if got.Status != resp.Status {
		t.Errorf("Status = %d, want %d", got.Status, resp.Status)
	}
	if !reflect.DeepEqual(got.Headers, resp.Headers) {
		t.Errorf("Headers = %v, want %v", got.Headers, resp.Headers)
	}
	if string(got.Body) != string(resp.Body) {
		t.Errorf("Body = %q, want %q", got.Body, resp.Body)
	}
}

func TestShouldCache(t *testing.T) {
	c := New(nil, Config{NoCachePaths: []string{"/dashboard", "/api/secrets", "/"}})

	for _, path := range []string{"/dashboard", "/api/secrets", "/", "/assets/index-x.js", "/assets/x.css"} {
		if c.shouldCache(path) {
			t.Errorf("shouldCache(%q) = true, want false", path)
		}
	}
	for _, path := range []string{"/weather", "/news", "/dashboard/v2", "/api/secrets/v2"} {
		if !c.shouldCache(path) {
			t.Errorf("shouldCache(%q) = false, want true", path)
		}
	}
}

func TestTTLFor(t *testing.T) {
	c := New(nil, Config{
		DefaultTTL: 300,
		RouteTTLs:  map[string]int64{"/weather": 300, "/news": 60},
	})

	if got, want := c.ttlFor("/news"), int64(60); got != want {
		t.Errorf("ttlFor(/news) = %d, want %d", got, want)
	}
	if got, want := c.ttlFor("/weather"), int64(300); got != want {
		t.Errorf("ttlFor(/weather) = %d, want %d", got, want)
	}
	// Exact-match, not prefix: a path with a route-name prefix must not inherit
	// the route TTL.
	if got, want := c.ttlFor("/weather/v2"), int64(300); got != want {
		t.Errorf("ttlFor(/weather/v2) = %d, want default %d", got, want)
	}
	if got, want := c.ttlFor("/unknown"), int64(300); got != want {
		t.Errorf("ttlFor(/unknown) = %d, want default %d", got, want)
	}
}

func TestStorableHeadersStripsDate(t *testing.T) {
	in := http.Header{
		"Date":             []string{"Sun, 01 Jan 2026 00:00:00 GMT"},
		"X-Cache":          []string{"MISS"},
		"Content-Encoding": []string{"gzip"},
		"Content-Length":   []string{"42"},
		"Content-Type":     []string{"application/json"},
	}
	out := storableHeaders(in)

	for _, name := range []string{"Date", "X-Cache", "Content-Encoding", "Content-Length"} {
		if _, ok := out[name]; ok {
			t.Errorf("storableHeaders() kept %s: %v", name, out)
		}
	}
	if out.Get("Content-Type") != "application/json" {
		t.Errorf("storableHeaders() lost Content-Type: %v", out)
	}
}

// fakeRedis is an in-memory Redis stub implementing the cache's narrow
// interface. It also records the most recent write TTL so tests can assert
// how long entries are stored.
type fakeRedis struct {
	mu      sync.Mutex
	data    map[string][]byte
	lastTTL time.Duration
	writes  int
}

func newFakeRedis() *fakeRedis { return &fakeRedis{data: make(map[string][]byte)} }

func (f *fakeRedis) Get(ctx context.Context, key string) *redis.StringCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := redis.NewStringCmd(ctx)
	v, ok := f.data[key]
	if !ok {
		cmd.SetErr(redis.Nil)
		return cmd
	}
	cmd.SetVal(string(v))
	return cmd
}

func (f *fakeRedis) Set(ctx context.Context, key string, value any, ttl time.Duration) *redis.StatusCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch v := value.(type) {
	case []byte:
		f.data[key] = v
	case string:
		f.data[key] = []byte(v)
	}
	f.lastTTL = ttl
	f.writes++
	return redis.NewStatusCmd(ctx)
}

// TestMissPropagatesHandlerHeaders guards the captureResponse Header() fix:
// headers the handler sets after the middleware starts must reach the client,
// not only the cache.
func TestMissPropagatesHandlerHeaders(t *testing.T) {
	c := New(newFakeRedis(), Config{DefaultTTL: 300})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "value")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("body"))
	})

	rec := httptest.NewRecorder()
	c.Middleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/weather", nil))

	if got := rec.Header().Get("X-Custom"); got != "value" {
		t.Errorf("client X-Custom = %q, want value", got)
	}
	if got := rec.Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("client X-Cache = %q, want MISS", got)
	}
	if got := rec.Body.String(); got != "body" {
		t.Errorf("client body = %q, want body", got)
	}
}

// TestHitServesStoredHeaders checks that a second request hits the cache and
// serves the stored headers without leaking the recording-time X-Cache value.
func TestHitServesStoredHeaders(t *testing.T) {
	c := New(newFakeRedis(), Config{DefaultTTL: 300})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("body"))
	})

	req := httptest.NewRequest(http.MethodGet, "/weather", nil)
	c.Middleware(next).ServeHTTP(httptest.NewRecorder(), req)

	rec := httptest.NewRecorder()
	c.Middleware(next).ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("client X-Cache = %q, want HIT", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("client Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get("X-Custom"); got != "" {
		t.Errorf("client X-Custom = %q, want empty", got)
	}
	if got := rec.Body.String(); got != "body" {
		t.Errorf("client body = %q, want body", got)
	}
}

// staleEntry encodes a cached response whose X-Fetched-At predates the TTL by
// a wide margin, simulating an entry inside the stale-while-revalidate window.
func staleEntry() []byte {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	h.Set("X-Fetched-At", time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339))
	return encodeResponse(&cachedResponse{Status: 200, Headers: h, Body: []byte("stale")})
}

// TestStaleServedWhileRevalidating covers the SWR path: a past-TTL entry is
// served immediately as stale and re-fetched in the background.
func TestStaleServedWhileRevalidating(t *testing.T) {
	fake := newFakeRedis()
	c := New(fake, Config{DefaultTTL: 60, StaleWhileRevalidate: 10 * time.Minute})
	fake.data["cache:GET:/weather"] = staleEntry()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fresh"))
	})

	rec := httptest.NewRecorder()
	c.Middleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/weather", nil))

	if got := rec.Header().Get("X-Cache"); got != "STALE" {
		t.Errorf("X-Cache = %q, want STALE", got)
	}
	if got := rec.Body.String(); got != "stale" {
		t.Errorf("served body = %q, want the stale copy", got)
	}

	// The background refresh must re-store a fresh copy shortly after.
	deadline := time.Now().Add(time.Second)
	for !bytes.Contains(storedBody(fake, "cache:GET:/weather"), []byte("fresh")) {
		if time.Now().After(deadline) {
			t.Fatal("background refresh did not re-store a fresh entry")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func storedBody(f *fakeRedis, key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data[key]
}

// TestStoreTTLIncludesSWR guards the SWR storage contract: entries must live
// ttl+StaleWhileRevalidate in Redis, otherwise the stale window is unreachable
// because Redis would delete the entry at the base TTL.
func TestStoreTTLIncludesSWR(t *testing.T) {
	fake := newFakeRedis()
	c := New(fake, Config{DefaultTTL: 60, RouteTTLs: map[string]int64{"/news": 10}, StaleWhileRevalidate: 5 * time.Minute})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	c.Middleware(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/news", nil))

	fake.mu.Lock()
	got := fake.lastTTL
	fake.mu.Unlock()
	if want := 10*time.Second + 5*time.Minute; got != want {
		t.Errorf("stored TTL = %v, want %v", got, want)
	}
}

// TestOversizedBodyStreamedNotCached covers the maxCacheBody cap: the response
// must reach the client in full but never be stored in Redis, so memory stays
// bounded under many concurrent users.
func TestOversizedBodyStreamedNotCached(t *testing.T) {
	fake := newFakeRedis()
	c := New(fake, Config{DefaultTTL: 300})
	big := bytes.Repeat([]byte("x"), maxCacheBody+4096)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(big)
	})

	rec := httptest.NewRecorder()
	c.Middleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/weather", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), big) {
		t.Errorf("client received %d bytes, want the full %d-byte body", rec.Body.Len(), len(big))
	}

	fake.mu.Lock()
	writes := fake.writes
	fake.mu.Unlock()
	if writes != 0 {
		t.Errorf("cache stored an oversized response: %d writes", writes)
	}
}

// TestRedirectsNotCached guards the 3xx change: a redirect that falls through
// the handler must not be replayed from the cache to every caller.
func TestRedirectsNotCached(t *testing.T) {
	fake := newFakeRedis()
	c := New(fake, Config{DefaultTTL: 300})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	c.Middleware(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/weather", nil))

	fake.mu.Lock()
	writes := fake.writes
	fake.mu.Unlock()
	if writes != 0 {
		t.Errorf("cache stored a 3xx response: %d writes", writes)
	}
}
