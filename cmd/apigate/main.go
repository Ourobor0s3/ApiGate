package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Ourobor0s3/ApiGate/internal/aggregation"
	"github.com/Ourobor0s3/ApiGate/internal/cache"
	"github.com/Ourobor0s3/ApiGate/internal/checks"
	"github.com/Ourobor0s3/ApiGate/internal/interval"
	"github.com/Ourobor0s3/ApiGate/internal/middleware"
	"github.com/Ourobor0s3/ApiGate/internal/newsstore"
	"github.com/Ourobor0s3/ApiGate/internal/notify"
	"github.com/Ourobor0s3/ApiGate/internal/proxy"
	"github.com/Ourobor0s3/ApiGate/internal/quota"
	"github.com/Ourobor0s3/ApiGate/internal/ratelimit"
	"github.com/Ourobor0s3/ApiGate/internal/secrets"
	"github.com/redis/go-redis/v9"
)

func main() {
	if err := run(context.Background()); err != nil {
		slog.Error("apigate exited with error", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.Default()

	rdb := redis.NewClient(&redis.Options{
		Addr: envOrDefault("REDIS_ADDR", "localhost:6379"),
	})
	defer rdb.Close()

	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pingCancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		return fmt.Errorf("cannot reach Redis at %s: %w", rdb.Options().Addr, err)
	}

	store := secrets.New(rdb)
	if names, err := store.List(ctx); err != nil {
		logger.Warn("secrets: cannot list from Redis", "err", err)
	} else {
		logger.Info("secrets loaded from Redis", "names", names)
	}

	// getSecret resolves a setting at request time: the Redis value wins, the
	// env var of the same name is the fallback.
	getSecret := func(ctx context.Context, name string) string {
		if v, err := store.Get(ctx, name); err == nil && v != "" {
			return v
		}
		return os.Getenv(name)
	}

	// Empty NEWS_API_URL/WEATHER_API_URL disable only the public proxy routes;
	// the dashboard keeps its built-in upstreams and can still switch news
	// sources via Redis secrets at poll time (see the aggregation options).
	weatherAPI := os.Getenv("WEATHER_API_URL")
	newsAPIUpstreamEnv := os.Getenv("NEWS_API_URL")
	newsURL := envOrDefault("NEWS_API_URL", aggregation.DefaultNewsURL)
	newsURLRU := envOrDefault("NEWS_API_URL_RU", aggregation.DefaultNewsURLRU)

	webhook := notify.New(os.Getenv("WEBHOOK_URL"))

	p, err := proxy.New(proxy.Config{
		WeatherAPI: weatherAPI,
		NewsAPI:    newsAPIUpstreamEnv,
		NewsAPIKey: func(ctx context.Context) string { return getSecret(ctx, "NEWS_API_KEY") },
		Breaker: proxy.BreakerConfig{
			FailureThreshold: envIntOrDefault("UPSTREAM_BREAK_FAILURES", 5),
			Cooldown:         envDurationOrDefault("UPSTREAM_BREAK_COOLDOWN", 30*time.Second),
		},
	})
	if err != nil {
		return err
	}

	c := cache.New(rdb, cache.Config{
		DefaultTTL:   300,
		RouteTTLs:    map[string]int64{"/weather": 300, "/news": 60},
		NoCachePaths: []string{"/dashboard", "/api/secrets", "/api/checks", "/api/newsquota", "/", "/healthz"},
		// Stale-while-revalidate: serve a stale cached copy for up to 10
		// minutes past its TTL while refreshing in the background, so a cold
		// cache never cascades to the upstream.
		StaleWhileRevalidate: envDurationOrDefault("CACHE_STALE_WHILE_REVALIDATE", 10*time.Minute),
	})

	// One IP resolver for both the rate limiter and the access log, so the
	// logged client always matches the one the limits are applied against.
	clientIP := clientIPResolver(logger)

	rl := ratelimit.New(rdb, ratelimit.Config{
		Limit:            100,
		Window:           60,
		Logger:           logger,
		ClientIP:         clientIP,
		NoRateLimitPaths: []string{"/healthz"},
	})

	// newsQuota caps newsapi consumption at 100 requests/day (free plan),
	// shared by the /news route and the background news poller. Exhaustion
	// fires a webhook once per day when one is configured.
	newsQuota := quota.New(rdb, quota.Config{
		Name:        "news",
		Limit:       envInt64OrDefault("NEWS_DAILY_LIMIT", 100),
		OnExhausted: quota.ExhaustedNotifier(rdb, webhook),
	})

	agg := aggregation.New(getSecret,
		aggregation.WithWeatherURL(envOrDefault("WEATHER_API_URL", aggregation.DefaultWeatherURL)),
		aggregation.WithNewsURL(newsURL),
		aggregation.WithNewsURLRU(newsURLRU),
		aggregation.WithNewsQuota(newsQuota),
		aggregation.WithNewsStore(newsstore.New(rdb)),
		aggregation.WithNewsStoreRU(newsstore.NewLang(rdb, "ru")),
		// The weather/place/rates snapshots live in Redis too: the dashboard
		// reads them per request and the poller refreshes them on the news
		// cycle, so no dashboard request ever spends an upstream budget.
		aggregation.WithKV(snapshotStore{rdb}),
		aggregation.WithNewsPollInterval(func(ctx context.Context) time.Duration {
			return interval.Parse(getSecret(ctx, "NEWS_POLL_INTERVAL"), aggregation.DefaultPollInterval)
		}),
	)

	chk := checks.New(rdb, checks.Config{
		Interval: func(ctx context.Context) time.Duration {
			return interval.Parse(getSecret(ctx, "CHECK_INTERVAL"), checks.DefaultInterval)
		},
		OnStatusChange: func(ctx context.Context, url string, from, to checks.Status) {
			state := "down"
			if to.OK {
				state = "up"
			}
			payload := map[string]any{
				"event":     "checkStatusChange",
				"url":       url,
				"state":     state,
				"code":      to.Code,
				"latencyMs": to.LatencyMs,
				"checkedAt": to.CheckedAt,
			}
			// Fire-and-forget: a slow webhook endpoint must not stall the
			// probe loop or a "check now" request.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				webhook.Post(ctx, payload)
			}()
		},
		AllowPrivate: envBoolOrDefault("CHECKS_ALLOW_PRIVATE", false),
	})

	mux := http.NewServeMux()
	mux.Handle("GET /weather", p.Weather())
	mux.Handle("GET /news", newsQuota.Middleware(p.News()))
	mux.Handle("GET /dashboard", agg)
	mux.Handle("GET /healthz", middleware.Health(rdb, logger))
	mux.Handle("GET /api/newsquota", newsQuotaHandler(newsQuota))
	secrets.NewHandler(store, []secrets.Setting{
		{Name: "NEWS_API_KEY", Masked: true},
		{Name: "WEATHER_LOCATION", Default: "55.7558,37.6173", Env: os.Getenv("WEATHER_LOCATION")},
		{Name: "MAIN_CURRENCY", Default: "USD", Env: os.Getenv("MAIN_CURRENCY")},
		{Name: "NEWS_POLL_INTERVAL", Default: "60m", Env: os.Getenv("NEWS_POLL_INTERVAL")},
		{Name: "CHECK_INTERVAL", Default: "5m", Env: os.Getenv("CHECK_INTERVAL")},
		{Name: "NEWS_API_URL", Default: newsURL, Env: newsAPIUpstreamEnv},
		{Name: "NEWS_API_URL_RU", Default: newsURLRU, Env: os.Getenv("NEWS_API_URL_RU")},
	}, secrets.WithOnChange(func(name string) {
		// A data-affecting secret must reflect on the dashboard right away:
		// fire one out-of-cycle refresh instead of waiting for the next poll.
		if refreshOnSecret(name) {
			agg.PollNow()
		}
	})).Register(mux)
	checks.NewHandler(chk).Register(mux)
	mux.Handle("GET /", noCache(uiHandler(logger)))

	var h http.Handler = mux
	h = c.Middleware(h)
	h = rl.Middleware(h)
	h = middleware.Recover(logger, h)
	h = middleware.Gzip(h)
	h = middleware.SecureHeaders(os.Getenv("CORS_ORIGIN"))(h)
	h = middleware.RequestLogger(logger, clientIP, h)

	httpServer := &http.Server{
		Addr:              ":" + envOrDefault("PORT", "8080"),
		Handler:           h,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// WriteTimeout stays zero so slow-but-alive clients are never cut off;
		// ReadHeaderTimeout, ReadTimeout and IdleTimeout still bound idle and
		// slow connections.
		IdleTimeout: 60 * time.Second,
	}

	go chk.Run(ctx)
	go agg.Run(ctx)
	if paths := envOrDefault("CACHE_WARM_PATHS", ""); paths != "" {
		go warmCache(ctx, h, strings.Split(paths, ","), logger)
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("apigate listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
		logger.Info("shutting down gracefully")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("server stopped")
	return nil
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// uiHandler serves the built SPA from frontend/dist (gitignored build output)
// when present, and a stub page otherwise. The UI is not embedded anymore:
// run `npm run build` in frontend/ before `go run` to avoid the stub.
func uiHandler(logger *slog.Logger) http.Handler {
	const dir = "frontend/dist"
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return http.FileServer(http.Dir(dir))
	}
	logger.Warn("frontend/dist not found — UI not built (run `cd frontend && npm run build`)")
	return http.HandlerFunc(stubHandler)
}

const stubPage = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>ApiGate</title>
<style>body{margin:0;font:14px/1.6 -apple-system,system-ui,Segoe UI,Roboto,sans-serif;background:#f5f6f9;color:#303238;display:grid;place-items:center;height:100vh}
.card{background:#fff;border:1px solid #d9dce3;border-radius:4px;padding:28px 36px;text-align:center}
h1{font-size:18px;margin:0 0 8px}code{font-family:ui-monospace,Menlo,monospace;font-size:13px;background:#f5f6f9;border:1px solid #d9dce3;border-radius:3px;padding:2px 6px}
p{color:#6b7280;margin:8px 0 0}</style></head>
<body><div class="card"><h1>ApiGate UI is not built</h1>
<p>Run <code>cd frontend &amp;&amp; npm run build</code> and start the server again.</p></div></body></html>`

func stubHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, stubPage)
}

// snapshotStore adapts *redis.Client to aggregation.Store for the dashboard's
// weather/place/rates snapshots. A missing key surfaces as an empty value
// (redis.Nil is the normal "not polled yet" case, not an error worth logging).
type snapshotStore struct{ rdb *redis.Client }

func (s snapshotStore) Get(ctx context.Context, key string) (string, error) {
	v, err := s.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

func (s snapshotStore) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return s.rdb.Set(ctx, key, value, ttl).Err()
}

// refreshOnSecret reports whether a changed secret invalidates served data.
// A new location, currency, news upstream or API key must show up on the
// dashboard immediately, so writes to these names trigger an immediate
// background refresh; schedule-only settings pick up on their next cycle.
func refreshOnSecret(name string) bool {
	switch name {
	case "NEWS_API_KEY", "WEATHER_LOCATION", "MAIN_CURRENCY":
		return true
	}
	return false
}

// newsQuotaHandler serves today's NewsAPI budget usage for the dashboard's
// budget bar: the quota limiter state, not per-request metrics.
func newsQuotaHandler(newsQuota *quota.Limiter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		out := make(map[string]int64, 2)
		if used, limit, err := newsQuota.Usage(ctx); err == nil {
			out["news_quota_used"] = used
			out["news_quota_limit"] = limit
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

// warmCache issues GET requests through the full middleware chain shortly after
// startup so cacheable routes (weather/news) serve from Redis instead of the
// upstream on the first real visitor. Failures are logged, never fatal.
func warmCache(ctx context.Context, h http.Handler, paths []string, logger *slog.Logger) {
	client := &http.Client{Timeout: 15 * time.Second}
	// Give the server a moment to come up before firing the warm requests.
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+envOrDefault("HOST", "localhost")+":"+envOrDefault("PORT", "8080")+p, nil)
		if err != nil {
			continue
		}
		if _, err := client.Do(req); err != nil {
			logger.Warn("warm: prefetch failed", "path", p, "err", err)
		} else {
			logger.Info("warm: cache primed", "path", p)
		}
	}
}

// clientIPResolver builds the per-request client-IP resolver from the
// TRUSTED_PROXIES env var (comma-separated CIDRs), letting X-Forwarded-For be
// honored only when the connection comes from a trusted proxy. Without it the
// secure default (RemoteAddr only) is used.
func clientIPResolver(logger *slog.Logger) func(*http.Request) string {
	if v := os.Getenv("TRUSTED_PROXIES"); v != "" {
		parts := strings.Split(v, ",")
		proxies := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				proxies = append(proxies, p)
			}
		}
		if resolve, err := middleware.ForwardedClientIP(proxies...); err == nil {
			return resolve
		} else {
			logger.Warn("ignoring invalid TRUSTED_PROXIES, falling back to RemoteAddr", "err", err)
		}
	}
	return middleware.ClientIP
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt64OrDefault(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envIntOrDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDurationOrDefault(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envBoolOrDefault(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
