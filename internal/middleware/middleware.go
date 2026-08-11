// Package middleware provides HTTP platform helpers: shared client-IP
// extraction, request logging, panic recovery and a liveness endpoint.
package middleware

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ClientIP returns the client's IP from RemoteAddr. X-Forwarded-For is
// deliberately ignored: it is client-controlled and trivially spoofable, and
// trusting it would let anyone bypass per-IP rate limits or make their requests
// counted against a different client. Behind a trusted reverse proxy, use
// ForwardedClientIP instead.
func ClientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// ForwardedClientIP returns a client-IP resolver that honors X-Forwarded-For
// only when the direct peer is one of the given trusted proxies (typically a
// load balancer that overwrites the header). Every other peer falls back to
// RemoteAddr, so the header can't be spoofed. Proxies are IPv4/IPv6 CIDR
// blocks; an invalid block is reported as an error rather than silently
// weakening the check.
func ForwardedClientIP(trustedProxies ...string) (func(*http.Request) string, error) {
	blocks := make([]*net.IPNet, 0, len(trustedProxies))
	for _, p := range trustedProxies {
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q is not a valid CIDR: %w", p, err)
		}
		blocks = append(blocks, ipnet)
	}
	return func(r *http.Request) string {
		if isTrustedPeer(r.RemoteAddr, blocks) {
			if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
				return strings.TrimSpace(strings.Split(fwd, ",")[0])
			}
		}
		return ClientIP(r)
	}, nil
}

// isTrustedPeer reports whether the connection's remote host falls inside any
// of the trusted proxy blocks.
func isTrustedPeer(remoteAddr string, blocks []*net.IPNet) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, b := range blocks {
		if b.Contains(ip) {
			return true
		}
	}
	return false
}

// StatusRecorder wraps a ResponseWriter to capture the status code and body
// size for logging.
type StatusRecorder struct {
	http.ResponseWriter
	Status int
	Bytes  int
}

func (r *StatusRecorder) WriteHeader(code int) {
	r.Status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *StatusRecorder) Write(b []byte) (int, error) {
	if r.Status == 0 {
		r.Status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.Bytes += n
	return n, err
}

// Flush keeps streaming responses (SSE) working through the recorder.
func (r *StatusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *StatusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// RequestLogger logs every request with method, path, status, size, latency
// and client IP. The query string is deliberately not logged — it may carry
// API keys. clientIP lets the caller pass the same trusted-proxy-aware
// resolver the rate limiter uses, so the logged IP always matches the one the
// limits are applied against.
func RequestLogger(logger *slog.Logger, clientIP func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &StatusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.Status
		if status == 0 {
			status = http.StatusOK
		}
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"bytes", rec.Bytes,
			"dur", time.Since(start),
			"ip", clientIP(r),
		)
	})
}

// Recover turns handler panics into a 500 response and logs the stack trace
// instead of letting net/http drop the connection.
func Recover(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				logger.Error("panic recovered",
					"method", r.Method,
					"path", r.URL.Path,
					"panic", v,
					"stack", string(debug.Stack()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Health reports liveness by pinging Redis. 200 when reachable, 503 otherwise.
func Health(rdb *redis.Client, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := rdb.Ping(ctx).Err(); err != nil {
			logger.Warn("health: redis ping failed", "err", err)
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok\n")
	})
}

// SecureHeaders sets security headers on every response and, when corsOrigin is
// non-empty, applies CORS headers for that origin (or "*"), including preflight
// handling. It must sit outermost so every response is covered.
func SecureHeaders(corsOrigin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("X-XSS-Protection", "0")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			h.Set("Content-Security-Policy",
				"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
					"script-src 'self'; connect-src 'self'; base-uri 'self'; frame-ancestors 'none'")

			origin := r.Header.Get("Origin")
			if corsOrigin != "" && origin != "" {
				switch {
				case corsOrigin == "*":
					h.Set("Access-Control-Allow-Origin", "*")
				case origin == corsOrigin:
					h.Set("Access-Control-Allow-Origin", origin)
					h.Add("Vary", "Origin")
				}
				h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Content-Type")
			}

			// CORS preflight requests are handled at the middleware level rather
			// than being routed to a handler.
			if r.Method == http.MethodOptions && origin != "" && corsOrigin != "" &&
				r.Header.Get("Access-Control-Request-Method") != "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Gzip compresses responses for clients that accept gzip, skipping streaming
// responses (SSE must stay uncompressed) and non-compressible content types.
func Gzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r) || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		gz := &gzipResponseWriter{ResponseWriter: w}
		defer gz.Close()
		next.ServeHTTP(gz, r)
	})
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.TrimSpace(strings.SplitN(part, ";", 2)[0]) == "gzip" {
			return true
		}
	}
	return false
}

// gzipResponseWriter compresses the response body when the content type is
// compressible. Compression is decided at the first WriteHeader, by which point
// handlers have set Content-Type.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz       *gzip.Writer
	written  bool
	compress bool
	status   int
}

// shouldCompress reports whether a response of the given status and
// Content-Type is worth gzip-encoding.
func (g *gzipResponseWriter) shouldCompress(code int) bool {
	if code < 200 || code >= 400 {
		return false
	}
	if g.Header().Get("Content-Encoding") != "" {
		return false
	}
	ct := g.Header().Get("Content-Type")
	switch {
	case ct == "text/event-stream": // never buffer/slow down a stream
		return false
	case strings.HasPrefix(ct, "text/"):
		return true
	case strings.HasPrefix(ct, "application/json"),
		ct == "application/javascript",
		strings.Contains(ct, "javascript"),
		strings.HasPrefix(ct, "application/xml"):
		return true
	}
	return false
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.written {
		return
	}
	g.written = true
	g.status = code
	g.compress = g.shouldCompress(code)
	if g.compress {
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Add("Vary", "Accept-Encoding")
		g.Header().Del("Content-Length")
		g.gz = gzip.NewWriter(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.written {
		g.WriteHeader(http.StatusOK)
	}
	if g.compress {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

// Flush keeps streaming responses (and the underlying flusher) working through
// the gzip wrapper.
func (g *gzipResponseWriter) Flush() {
	if g.compress {
		g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Close flushes the gzip stream once the handler has finished.
func (g *gzipResponseWriter) Close() {
	if g.compress {
		g.gz.Close()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (g *gzipResponseWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }
