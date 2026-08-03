// Package quota enforces a daily budget over a Redis counter. Unlike
// ratelimit's per-IP sliding window, a quota is a global cap shared by all
// clients — used to stay inside NewsAPI's free-plan 100 requests/24h limit.
package quota

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Config configures a daily budget.
type Config struct {
	// Name is the budget name used in the Redis key "quota:<name>:<date>".
	Name string
	// Limit is the max number of requests allowed per local calendar day.
	Limit int64
}

// quotaKeyTTL gives the counter plenty of time to expire after midnight even
// if the key is created late in the day.
const quotaKeyTTL = 48 * time.Hour

// ExhaustedCode and ExhaustedMessage form the newsapi-style error object
// served once the daily budget runs out, shared by the /news 429 and the
// dashboard's news card.
const (
	ExhaustedCode    = "dailyQuotaExhausted"
	ExhaustedMessage = "Daily newsapi quota exceeded"
)

// incrScript atomically bumps the daily counter only while budget remains: it
// returns the new count when allowed, or a negative count when exhausted
// (without incrementing further, so rejected requests don't waste budget).
var incrScript = redis.NewScript(`
local count = tonumber(redis.call('GET', KEYS[1]) or '0')
if count >= tonumber(ARGV[1]) then
	return -count
end
count = redis.call('INCR', KEYS[1])
if count == 1 then
	redis.call('EXPIRE', KEYS[1], ARGV[2])
end
return count
`)

type Limiter struct {
	rdb *redis.Client
	cfg Config
}

func New(rdb *redis.Client, cfg Config) *Limiter {
	return &Limiter{rdb: rdb, cfg: cfg}
}

// Allow atomically consumes one unit of the daily budget and reports whether
// the request is within the limit. The counter resets per local calendar day.
func (l *Limiter) Allow(ctx context.Context) (bool, error) {
	n, err := incrScript.Run(ctx, l.rdb, []string{dayKey(l.cfg.Name, time.Now())}, l.cfg.Limit, int64(quotaKeyTTL/time.Second)).Int64()
	if err != nil {
		return false, err
	}
	return n >= 0, nil
}

// Middleware returns 429 with a newsapi-style error body once the daily budget
// is exhausted, otherwise it forwards the request unchanged. On a backend
// failure it fails open (like the ratelimit middleware) rather than
// interrupting the request.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed, err := l.Allow(r.Context())
		if err != nil || allowed {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.FormatInt(secondsUntilNextDay(time.Now()), 10))
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "error",
			"code":    ExhaustedCode,
			"message": ExhaustedMessage,
		})
	})
}

// dayKey returns the Redis key for a budget on a given day, e.g.
// "quota:news:2026-08-03".
func dayKey(name string, t time.Time) string {
	return "quota:" + name + ":" + t.Format("2006-01-02")
}

// secondsUntilNextDay reports how long until the daily counter rolls over,
// used for Retry-After on a 429.
func secondsUntilNextDay(t time.Time) int64 {
	next := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
	return int64(next.Sub(t).Seconds()) + 1
}
