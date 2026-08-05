// Package metrics exposes a tiny set of Redis-backed counters, surfaced on the
// /api/metrics endpoint. Keeping counters in Redis makes them live across
// process restarts and lets the dashboard render them with the same read path
// as cached data. Counters are bucketed per day (metric:<name>:<date>) and
// expire 48 hours after their last increment, so a downed Redis never loses
// the day's numbers and stale days clean themselves up.
package metrics

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Ourobor0s3/ApiGate/internal/middleware"
	"github.com/redis/go-redis/v9"
)

// KeyPrefix namespaces all counter keys so unrelated Redis data never leaks in.
const KeyPrefix = "metric:"

// counterTTL is how long a day bucket lives after its last increment. Two days
// keeps yesterday's numbers readable (e.g. to compare) while bounding the
// number of keys Redis holds.
const counterTTL = 48 * time.Hour

// Redis is the narrow subset of the Redis client the metrics store needs,
// satisfied by *redis.Client. Tests fake it to avoid a live Redis.
type Redis interface {
	IncrBy(ctx context.Context, key string, n int64) *redis.IntCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd
}

// Store owns the counter keys. The counter set is fixed (namedCounters) and
// each counter lives in a per-day bucket, so keys can't accumulate.
type Store struct {
	rdb    Redis
	logger *slog.Logger
}

// New returns a Store over rdb.
func New(rdb Redis) *Store {
	return &Store{rdb: rdb, logger: slog.Default()}
}

// Incr increments the named counter. Failures are logged, never returned, so
// metrics can't take down a request path.
func (s *Store) Incr(name string) {
	s.add(context.Background(), name, 1)
}

// Middleware counts HTTP responses grouped by 2xx/3xx/4xx/5xx, using a
// status-capturing wrapper so the count reflects the final status code. A
// handler that never calls WriteHeader (an empty 200) is counted as 2xx.
func (s *Store) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &middleware.StatusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.Status
		if status == 0 {
			status = http.StatusOK
		}
		switch {
		case status >= 500:
			s.add(r.Context(), "http_5xx", 1)
		case status >= 400:
			s.add(r.Context(), "http_4xx", 1)
		case status >= 300:
			s.add(r.Context(), "http_3xx", 1)
		case status >= 200:
			s.add(r.Context(), "http_2xx", 1)
		}
	})
}

// namedCounters lists every counter the dashboard knows how to render.
var namedCounters = []string{"http_2xx", "http_3xx", "http_4xx", "http_5xx", "quota_rejected"}

// NamedCounters returns the fixed counter names tracked by the store.
func NamedCounters() []string {
	return namedCounters
}

// Values returns the current (today's) value of each known counter,
// best-effort.
func (s *Store) Values(ctx context.Context) map[string]int64 {
	return s.values(ctx)
}

// values reads each counter's today bucket. The counter set is tiny, so plain
// GETs are fine (no pipeline needed).
func (s *Store) values(ctx context.Context) map[string]int64 {
	out := make(map[string]int64, len(namedCounters))
	day := time.Now().Format("2006-01-02")
	for _, n := range namedCounters {
		if v, err := s.rdb.Get(ctx, KeyPrefix+n+":"+day).Int64(); err == nil {
			out[n] = v
		}
	}
	return out
}

// add bumps a counter in its per-day bucket and refreshes the bucket TTL, so
// each day's numbers expire 48 hours after the last update.
func (s *Store) add(ctx context.Context, name string, n int64) {
	key := KeyPrefix + name + ":" + time.Now().Format("2006-01-02")
	if err := s.rdb.IncrBy(ctx, key, n).Err(); err != nil {
		s.logger.Warn("metrics: add failed", "name", name, "err", err)
		return
	}
	if err := s.rdb.Expire(ctx, key, counterTTL).Err(); err != nil {
		s.logger.Warn("metrics: set ttl failed", "name", name, "err", err)
	}
}
