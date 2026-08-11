package cache

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type Config struct {
	DefaultTTL   int64
	RouteTTLs    map[string]int64
	NoCachePaths []string
	// StaleWhileRevalidate, when > 0, lets a stale (expired) entry serve for up
	// to this long while a background goroutine refreshes it, so repeated misses
	// don't cascade to the upstream. Entries are stored with a TTL of
	// ttl+StaleWhileRevalidate: past the base TTL Redis still holds them, and
	// requests in that window get the stale copy plus an async refresh.
	StaleWhileRevalidate time.Duration
}

type Cache struct {
	rdb Redis
	cfg Config

	// mu guards refreshing; it dedups concurrent background refreshes of the
	// same key so a burst of stale requests maps to a single upstream call.
	mu         sync.Mutex
	refreshing map[string]bool
}

// Redis is the narrow subset of the Redis client the cache needs, satisfied by
// *redis.Client. Tests fake it to avoid a live Redis.
type Redis interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
}

type cachedResponse struct {
	Status  int
	Headers http.Header
	Body    []byte
}

func New(rdb Redis, cfg Config) *Cache {
	return &Cache{rdb: rdb, cfg: cfg, refreshing: make(map[string]bool)}
}

// storeTTL is how long an entry lives in Redis: the route TTL plus the stale
// window, giving the SWR serving path a window to operate in. When
// StaleWhileRevalidate is 0 the two collapse to the plain TTL.
func (c *Cache) storeTTL(path string) time.Duration {
	return time.Duration(c.ttlFor(path))*time.Second + c.cfg.StaleWhileRevalidate
}

func (c *Cache) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !c.shouldCache(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		key := cacheKey(r)
		ctx := r.Context()

		if data, err := c.rdb.Get(ctx, key).Bytes(); err == nil {
			resp, derr := decodeResponse(data)
			if derr == nil {
				if resp.Header("X-Fetched-At") != "" {
					stale := isStale(resp, c.ttlFor(r.URL.Path))
					if stale && c.cfg.StaleWhileRevalidate > 0 {
						// Serve the stale copy now and refresh in the background
						// (deduplicated so a burst shares one upstream call).
						w.Header().Set("X-Cache", "STALE")
						serveCached(w, resp)
						c.refreshAsync(key, r, next)
						return
					}
				}
				w.Header().Set("X-Cache", "HIT")
				serveCached(w, resp)
				return
			}
		}

		w.Header().Set("X-Cache", "MISS")
		cr := &captureResponse{ResponseWriter: w, buf: &bytes.Buffer{}}
		next.ServeHTTP(cr, r)

		if cr.code == 0 {
			cr.code = http.StatusOK
		}
		// Cache only successful responses: a 3xx that fell through (e.g. an
		// inaccessible redirect) is not worth replaying to every caller, and an
		// oversized body was streamed but never buffered.
		if cr.code < 200 || cr.code >= 300 || cr.overflow {
			return
		}

		headers := storableHeaders(cr.Header())
		headers.Set("X-Fetched-At", time.Now().UTC().Format(time.RFC3339))

		resp := &cachedResponse{
			Status:  cr.code,
			Headers: headers,
			Body:    cr.buf.Bytes(),
		}
		if ttl := c.ttlFor(r.URL.Path); ttl > 0 {
			c.rdb.Set(ctx, key, encodeResponse(resp), c.storeTTL(r.URL.Path))
		}
	})
}

// serveCached writes a cached response onto w without touching X-Cache state.
func serveCached(w http.ResponseWriter, resp *cachedResponse) {
	for k, v := range resp.Headers {
		if k == "X-Fetched-At" {
			continue
		}
		w.Header()[k] = v
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// isStale reports whether the entry is older than ttl. The cached entry carries
// its fetch time in X-Fetched-At.
func isStale(resp *cachedResponse, ttl int64) bool {
	if ttl <= 0 {
		return false
	}
	t, err := time.Parse(time.RFC3339, resp.Header("X-Fetched-At"))
	if err != nil {
		return false
	}
	return time.Since(t) > time.Duration(ttl)*time.Second
}

// refresh re-fetches the origin for key and stores the result, so a stale
// service can start from a warm cache without blocking the caller. The work
// runs on a bounded background context: a hung upstream or Redis must not
// leave the goroutine alive forever.
func (c *Cache) refresh(key string, r *http.Request, next http.Handler) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r = r.Clone(ctx)
	// A header-only capture lets the background fetch record upstream headers
	// without a ResponseWriter.
	h := make(http.Header)
	cr := &captureResponse{Headers: h, buf: &bytes.Buffer{}}
	next.ServeHTTP(cr, r)
	if cr.code == 0 {
		cr.code = http.StatusOK
	}
	// Mirror the MISS path: only successful, bounded responses are worth
	// re-storing.
	if cr.code < 200 || cr.code >= 300 || cr.overflow {
		return
	}
	h.Set("X-Fetched-At", time.Now().UTC().Format(time.RFC3339))
	resp := &cachedResponse{Status: cr.code, Headers: storableHeaders(h), Body: cr.buf.Bytes()}
	if c.ttlFor(r.URL.Path) > 0 {
		c.rdb.Set(ctx, key, encodeResponse(resp), c.storeTTL(r.URL.Path))
	}
}

// refreshAsync starts a background refresh of key, skipping it entirely when a
// refresh of the same key is already in flight so a burst of stale requests
// maps to a single upstream call instead of one per request. Panics inside the
// refresh chain are contained: the cache sits inside the Recover middleware,
// but this goroutine calls the chain below the cache directly, so nothing else
// would catch them.
func (c *Cache) refreshAsync(key string, r *http.Request, next http.Handler) {
	c.mu.Lock()
	if c.refreshing[key] {
		c.mu.Unlock()
		return
	}
	c.refreshing[key] = true
	c.mu.Unlock()

	go func() {
		defer func() {
			if v := recover(); v != nil {
				slog.Default().Error("cache: background refresh panicked", "key", key, "panic", v)
			}
		}()
		defer func() {
			c.mu.Lock()
			delete(c.refreshing, key)
			c.mu.Unlock()
		}()
		c.refresh(key, r, next)
	}()
}

// Header returns the first value of the named header, or "".
func (resp *cachedResponse) Header(name string) string { return resp.Headers.Get(name) }

// storableHeaders clones response headers, dropping ones that are per-response
// or would corrupt a cached copy: Date (stale timestamp), X-Cache (recomputed
// on every serve), Content-Encoding and Content-Length (the stored body is the
// raw uncompressed bytes, gzip is applied per-request outside the cache).
func storableHeaders(h http.Header) http.Header {
	clone := h.Clone()
	clone.Del("Date")
	clone.Del("X-Cache")
	clone.Del("Content-Encoding")
	clone.Del("Content-Length")
	return clone
}

func (c *Cache) shouldCache(path string) bool {
	// Vite emits hashed, immutable bundles under /assets/; caching them in
	// Redis buys nothing over the embedded filesystem.
	if strings.HasPrefix(path, "/assets/") {
		return false
	}
	for _, p := range c.cfg.NoCachePaths {
		if path == p {
			return false
		}
	}
	return true
}

func (c *Cache) ttlFor(path string) int64 {
	if ttl, ok := c.cfg.RouteTTLs[path]; ok {
		return ttl
	}
	return c.cfg.DefaultTTL
}

// keyPrefix namespaces every cached entry so unrelated Redis data never leaks
// into the cache and vice versa.
const keyPrefix = "cache:"

func cacheKey(r *http.Request) string {
	q := r.URL.Query()
	// apiKey never belongs in the cache key: it's a per-request credential, not
	// part of the cached resource, and leaking it into Redis keys would both
	// embed a secret in long-lived data and fragment the cache per key value.
	q.Del("apiKey")
	// Values.Encode sorts keys, so equivalent requests with different parameter
	// order share one entry.
	if enc := q.Encode(); enc != "" {
		return keyPrefix + r.Method + ":" + r.URL.Path + "?" + enc
	}
	return keyPrefix + r.Method + ":" + r.URL.Path
}

// maxCacheBody caps the size of a response body buffered for caching (and for
// background revalidation). Larger responses still stream straight to the
// client but are not stored, so a huge or misbehaving upstream can't balloon
// memory on every cache miss or stale request under many concurrent users.
const maxCacheBody = 8 << 20

// captureResponse buffers the handler output so it can be cached or refreshed.
// For the normal request path Header() returns the live writer's map so handler
// headers reach the client; the refresh path (no ResponseWriter) uses an
// explicit snapshot instead. Bodies beyond maxCacheBody are passed through
// untouched and marked overflow so callers skip caching.
type captureResponse struct {
	http.ResponseWriter
	buf      *bytes.Buffer
	code     int
	Headers  http.Header
	overflow bool
}

func (cr *captureResponse) Header() http.Header {
	if cr.ResponseWriter != nil {
		return cr.ResponseWriter.Header()
	}
	if cr.Headers != nil {
		return cr.Headers
	}
	return make(http.Header)
}

func (cr *captureResponse) WriteHeader(code int) {
	cr.code = code
	if cr.ResponseWriter != nil {
		cr.ResponseWriter.WriteHeader(code)
	}
}

func (cr *captureResponse) Write(b []byte) (int, error) {
	if !cr.overflow {
		if cr.buf.Len()+len(b) > maxCacheBody {
			cr.overflow = true
			cr.buf.Reset()
		} else {
			cr.buf.Write(b)
		}
	}
	if cr.ResponseWriter != nil {
		return cr.ResponseWriter.Write(b)
	}
	return len(b), nil
}

// Flush lets wrapped handlers that advertise http.Flusher keep streaming
// (e.g. SSE or chunked responses).
func (cr *captureResponse) Flush() {
	if cr.ResponseWriter != nil {
		if f, ok := cr.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (cr *captureResponse) Unwrap() http.ResponseWriter {
	return cr.ResponseWriter
}

func encodeResponse(resp *cachedResponse) []byte {
	var buf bytes.Buffer
	buf.WriteByte(byte(resp.Status >> 8))
	buf.WriteByte(byte(resp.Status))
	writeHeaders(&buf, resp.Headers)
	buf.Write(resp.Body)
	return buf.Bytes()
}

func decodeResponse(data []byte) (*cachedResponse, error) {
	r := bufio.NewReader(bytes.NewReader(data))
	var status [2]byte
	if _, err := io.ReadFull(r, status[:]); err != nil {
		return nil, err
	}
	resp := &cachedResponse{
		Status:  int(status[0])<<8 | int(status[1]),
		Headers: make(http.Header),
	}
	if err := readHeaders(r, resp.Headers); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	resp.Body = body
	return resp, nil
}

func writeHeaders(buf *bytes.Buffer, h http.Header) {
	for k, v := range h {
		for _, vv := range v {
			buf.WriteString(k + ":" + vv + "\r\n")
		}
	}
	buf.WriteString("\r\n")
}

func readHeaders(r *bufio.Reader, h http.Header) error {
	for {
		line, err := r.ReadString('\n')
		if line == "\r\n" || line == "\n" {
			return nil
		}
		if err != nil {
			return err
		}
		line = strings.TrimSuffix(line, "\r\n")
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			h.Add(parts[0], parts[1])
		}
	}
}
