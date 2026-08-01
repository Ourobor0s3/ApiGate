package ratelimit

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type Config struct {
	Limit  int64
	Window int64
}

type RateLimiter struct {
	rdb *redis.Client
	cfg Config
}

func New(rdb *redis.Client, cfg Config) *RateLimiter {
	return &RateLimiter{rdb: rdb, cfg: cfg}
}

func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		now := time.Now().UnixMilli()
		windowStart := now - rl.cfg.Window*1000
		key := "ratelimit:" + ip

		pipeline := rl.rdb.Pipeline()
		pipeline.ZRemRangeByScore(r.Context(), key, "0", strconv.FormatInt(windowStart, 10))
		countCmd := pipeline.ZCard(r.Context(), key)
		pipeline.ZAdd(r.Context(), key, redis.Z{Score: float64(now), Member: now})
		pipeline.Expire(r.Context(), key, time.Duration(rl.cfg.Window)*time.Second)
		pipeline.Exec(r.Context())

		if countCmd.Val() >= rl.cfg.Limit {
			w.Header().Set("Retry-After", strconv.FormatInt(rl.cfg.Window, 10))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
