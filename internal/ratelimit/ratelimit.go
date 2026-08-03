package ratelimit

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Ourobor0s3/ApiGate/internal/middleware"
	"github.com/redis/go-redis/v9"
)

type Config struct {
	Limit  int64
	Window int64
	// NoRateLimitPaths are exact-match paths exempt from rate limiting
	// (e.g. "/healthz" so orchestration probes never consume budget).
	NoRateLimitPaths []string
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
		if rl.bypasses(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		ip := middleware.ClientIP(r)
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
