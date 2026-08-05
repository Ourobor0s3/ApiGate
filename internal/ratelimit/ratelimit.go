package ratelimit

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Ourobor0s3/ApiGate/internal/middleware"
	"github.com/redis/go-redis/v9"
)

// windowScript prunes the sliding window, counts it and admits the request in
// one atomic step, so concurrent requests from the same IP can't both read the
// same pre-request count and slip past the limit. Returns the new count when
// admitted, or a negative count when the limit is already reached (and the
// attempt is not recorded, so rejected requests don't consume budget).
var windowScript = redis.NewScript(`
local windowStart = ARGV[1]
local limit = tonumber(ARGV[2])
local score = ARGV[3]
local member = ARGV[4]
local ttl = tonumber(ARGV[5])

redis.call('ZREMRANGEBYSCORE', KEYS[1], '0', windowStart)
local count = redis.call('ZCARD', KEYS[1])
if count >= limit then
	return -count
end
redis.call('ZADD', KEYS[1], score, member)
redis.call('EXPIRE', KEYS[1], ttl)
return count + 1
`)

// keyPrefix namespaces the sliding-window ZSETs so per-IP counters never mix
// with other Redis data.
const keyPrefix = "ratelimit:"

type Config struct {
	Limit  int64
	Window int64
	// Logger receives warnings about Redis pipeline failures; nil uses
	// slog.Default().
	Logger *slog.Logger
	// ClientIP resolves the request's client IP. It defaults to
	// middleware.ClientIP; wire middleware.ForwardedClientIP here when running
	// behind a trusted reverse proxy.
	ClientIP func(*http.Request) string
	// NoRateLimitPaths are exact-match paths exempt from rate limiting
	// (e.g. "/healthz" so orchestration probes never consume budget).
	NoRateLimitPaths []string
}

type RateLimiter struct {
	rdb      *redis.Client
	cfg      Config
	logger   *slog.Logger
	clientIP func(*http.Request) string
	// seq disambiguates ZSET members added within the same millisecond.
	seq atomic.Uint64
}

func New(rdb *redis.Client, cfg Config) *RateLimiter {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clientIP := cfg.ClientIP
	if clientIP == nil {
		clientIP = middleware.ClientIP
	}
	return &RateLimiter{rdb: rdb, cfg: cfg, logger: logger, clientIP: clientIP}
}

// member returns a unique member for the window at now. A ZSET dedupes by
// member, so using the raw millisecond timestamp would collapse concurrent
// same-millisecond requests into one entry and let the limit be bypassed under
// load; appending a monotonic sequence keeps every request in the window.
func (rl *RateLimiter) member(now int64) string {
	return strconv.FormatInt(now, 10) + "-" + strconv.FormatUint(rl.seq.Add(1), 10)
}

func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rl.bypasses(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		ip := rl.clientIP(r)
		now := time.Now().UnixMilli()
		windowStart := now - rl.cfg.Window*1000
		key := keyPrefix + ip

		n, err := windowScript.Run(r.Context(), rl.rdb, []string{key},
			windowStart, rl.cfg.Limit, now, rl.member(now), rl.cfg.Window).Int64()
		if err != nil {
			// Fail open: rate limiting is a courtesy, so an unreachable backend
			// must not take the route down. Log so the dropout is visible.
			rl.logger.Warn("ratelimit: redis failed", "ip", ip, "err", err)
			next.ServeHTTP(w, r)
			return
		}
		if n < 0 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", strconv.FormatInt(rl.cfg.Window, 10))
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":  "error",
				"code":    "rateLimitExceeded",
				"message": "rate limit exceeded",
			})
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (rl *RateLimiter) bypasses(path string) bool {
	for _, p := range rl.cfg.NoRateLimitPaths {
		if path == p {
			return true
		}
	}
	return false
}
