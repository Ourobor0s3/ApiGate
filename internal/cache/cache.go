package cache

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type Config struct {
	DefaultTTL   int64
	RouteTTLs    map[string]int64
	NoCachePaths []string
}

type Cache struct {
	rdb *redis.Client
	cfg Config
}

type cachedResponse struct {
	Status  int
	Headers http.Header
	Body    []byte
}

func New(rdb *redis.Client, cfg Config) *Cache {
	return &Cache{rdb: rdb, cfg: cfg}
}

func (c *Cache) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !c.shouldCache(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		key := cacheKey(r)
		ctx := r.Context()

		data, err := c.rdb.Get(ctx, key).Bytes()
		if err == nil {
			resp, derr := decodeResponse(data)
			if derr == nil {
				for k, v := range resp.Headers {
					w.Header()[k] = v
				}
				w.WriteHeader(resp.Status)
				_, _ = w.Write(resp.Body)
				return
			}
		}

		cr := &captureResponse{ResponseWriter: w, buf: &bytes.Buffer{}}
		next.ServeHTTP(cr, r)

		if cr.code == 0 {
			cr.code = http.StatusOK
		}
		if cr.code < 200 || cr.code >= 400 {
			return
		}

		resp := &cachedResponse{
			Status:  cr.code,
			Headers: w.Header().Clone(),
			Body:    cr.buf.Bytes(),
		}
		ttl := c.ttlFor(r.URL.Path)
		c.rdb.Set(ctx, key, encodeResponse(resp), time.Duration(ttl)*time.Second)
	})
}

func (c *Cache) shouldCache(path string) bool {
	for _, p := range c.cfg.NoCachePaths {
		if path == p {
			return false
		}
	}
	return true
}

func (c *Cache) ttlFor(path string) int64 {
	for prefix, ttl := range c.cfg.RouteTTLs {
		if strings.HasPrefix(path, prefix) {
			return ttl
		}
	}
	return c.cfg.DefaultTTL
}

func cacheKey(r *http.Request) string {
	q := r.URL.RawQuery
	if q != "" {
		return "cache:" + r.Method + ":" + r.URL.Path + "?" + q
	}
	return "cache:" + r.Method + ":" + r.URL.Path
}

type captureResponse struct {
	http.ResponseWriter
	buf  *bytes.Buffer
	code int
}

func (cr *captureResponse) WriteHeader(code int) {
	cr.code = code
	cr.ResponseWriter.WriteHeader(code)
}

func (cr *captureResponse) Write(b []byte) (int, error) {
	cr.buf.Write(b)
	return cr.ResponseWriter.Write(b)
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
