// Package quota enforces a global daily budget over a Redis counter — used to
// stay inside NewsAPI's free-plan 100 requests/24h limit.
package quota

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Ourobor0s3/ApiGate/internal/notify"
	"github.com/redis/go-redis/v9"
)

// Config configures a daily budget under "quota:<name>:<date>".
type Config struct {
	Name  string
	Limit int64
	// OnExhausted, if set, fires when a request is rejected; use
	// quota.ExhaustedNotifier for a deduplicated webhook.
	OnExhausted func(ctx context.Context, name string)
}

const quotaKeyTTL = 48 * time.Hour

// ExhaustedCode and ExhaustedMessage form the newsapi-style error object
// served once the daily budget runs out, shared by the /news 429 and the
// dashboard's news card.
const (
	ExhaustedCode    = "dailyQuotaExhausted"
	ExhaustedMessage = "Daily newsapi quota exceeded"
)

// incrScript bumps the counter only while budget remains; a negative return
// means the budget is exhausted and nothing was incremented, so rejected
// requests never waste budget.
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

// Allow atomically consumes one unit; the counter resets per local day.
func (l *Limiter) Allow(ctx context.Context) (bool, error) {
	n, err := incrScript.Run(ctx, l.rdb, []string{dayKey(l.cfg.Name, time.Now())}, l.cfg.Limit, int64(quotaKeyTTL/time.Second)).Int64()
	if err != nil {
		return false, err
	}
	if n < 0 {
		if l.cfg.OnExhausted != nil {
			l.cfg.OnExhausted(ctx, l.cfg.Name)
		}
		return false, nil
	}
	return true, nil
}

// Usage reports today's counted upstream calls (0 when none recorded).
func (l *Limiter) Usage(ctx context.Context) (used, limit int64, err error) {
	limit = l.cfg.Limit
	used, err = l.rdb.Get(ctx, dayKey(l.cfg.Name, time.Now())).Int64()
	if err == redis.Nil {
		return 0, limit, nil
	}
	return used, limit, err
}

// ExhaustedNotifier posts a webhook at most once per local day per budget
// (SETNX dedup). Returns nil when no webhook client is configured.
func ExhaustedNotifier(rdb *redis.Client, c *notify.Client) func(context.Context, string) {
	if c == nil || !c.Enabled() {
		return nil
	}
	return func(ctx context.Context, name string) {
		day := time.Now().Format("2006-01-02")
		key := notifyKey(name, day)
		ok, err := rdb.SetNX(ctx, key, "1", quotaKeyTTL).Result()
		if err != nil || !ok {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c.Post(ctx, map[string]any{
				"event":   "quotaExhausted",
				"name":    name,
				"date":    day,
				"message": ExhaustedMessage,
			})
		}()
	}
}

// Middleware returns 429 once the budget is spent; on a backend failure it
// fails open like the ratelimit middleware.
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

func dayKey(name string, t time.Time) string {
	return "quota:" + name + ":" + t.Format("2006-01-02")
}

func notifyKey(name, day string) string {
	return "quota:notify:" + name + ":" + day
}

// secondsUntilNextDay feeds the Retry-After header on a 429.
func secondsUntilNextDay(t time.Time) int64 {
	next := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
	return int64(next.Sub(t).Seconds()) + 1
}
