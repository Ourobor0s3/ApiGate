package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeRedis is an in-memory Redis stub implementing the narrow interface.
type fakeRedis struct {
	mu   sync.Mutex
	data map[string]int64
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{data: make(map[string]int64)}
}

func (f *fakeRedis) IncrBy(ctx context.Context, key string, n int64) *redis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[key] += n
	cmd := redis.NewIntCmd(ctx)
	cmd.SetVal(f.data[key])
	return cmd
}

func (f *fakeRedis) Get(ctx context.Context, key string) *redis.StringCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := redis.NewStringCmd(ctx)
	v, ok := f.data[key]
	if !ok {
		cmd.SetErr(redis.Nil)
		return cmd
	}
	cmd.SetVal(strconv.FormatInt(v, 10))
	return cmd
}

func (f *fakeRedis) Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd {
	return redis.NewBoolCmd(ctx)
}

// todayKey mirrors the store's day bucket: metric:<name>:<date>.
func (f *fakeRedis) counter(name string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := KeyPrefix + name + ":" + time.Now().Format("2006-01-02")
	return f.data[key]
}

func TestMiddlewareCountsByStatus(t *testing.T) {
	fake := newFakeRedis()
	s := New(fake)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // 4xx
	})
	other := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // 2xx
	})
	err := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 5xx
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	s.Middleware(next).ServeHTTP(rec, req)
	s.Middleware(other).ServeHTTP(rec, req)
	s.Middleware(other).ServeHTTP(rec, req)
	s.Middleware(err).ServeHTTP(rec, req)

	if got := fake.counter("http_4xx"); got != 1 {
		t.Errorf("http_4xx = %d, want 1", got)
	}
	if got := fake.counter("http_2xx"); got != 2 {
		t.Errorf("http_2xx = %d, want 2", got)
	}
	if got := fake.counter("http_5xx"); got != 1 {
		t.Errorf("http_5xx = %d, want 1", got)
	}
}

func TestValuesReadsToday(t *testing.T) {
	fake := newFakeRedis()
	s := New(fake)
	s.Incr("http_2xx")
	s.Incr("http_2xx")
	s.Incr("quota_rejected")

	got := s.Values(context.Background())
	if got["http_2xx"] != 2 {
		t.Errorf("http_2xx = %d, want 2", got["http_2xx"])
	}
	if got["quota_rejected"] != 1 {
		t.Errorf("quota_rejected = %d, want 1", got["quota_rejected"])
	}
}

func TestMiddlewareCountsEmptyHandlerAs2xx(t *testing.T) {
	fake := newFakeRedis()
	s := New(fake)

	// A handler that never calls WriteHeader still counts as a 200.
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	rec := httptest.NewRecorder()
	s.Middleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := fake.counter("http_2xx"); got != 1 {
		t.Errorf("http_2xx = %d, want 1 (status 0 normalized to 200)", got)
	}
}
