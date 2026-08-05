package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	"github.com/Ourobor0s3/ApiGate/internal/metrics"
	"github.com/Ourobor0s3/ApiGate/internal/middleware"
	"github.com/Ourobor0s3/ApiGate/internal/newsstore"
	"github.com/Ourobor0s3/ApiGate/internal/notify"
	"github.com/Ourobor0s3/ApiGate/internal/proxy"
	"github.com/Ourobor0s3/ApiGate/internal/quota"
	"github.com/Ourobor0s3/ApiGate/internal/ratelimit"
	"github.com/Ourobor0s3/ApiGate/internal/secrets"
	"github.com/redis/go-redis/v9"
)

//go:embed static
var staticFS embed.FS

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

	// Raw os.Getenv (not envOrDefault): an explicitly empty value is the way
	// to disable the proxy route while keeping the dashboard on its built-in
	// default upstreams.
	weatherAPI := os.Getenv("WEATHER_API_URL")
	newsAPI := os.Getenv("NEWS_API_URL")

	webhook := notify.New(os.Getenv("WEBHOOK_URL"))
	metricsStore := metrics.New(rdb)

	// Counters used to live under undated keys (metric:http_2xx); drop any
	// leftovers so old numbers can't show up as "today".
	for _, name := range metrics.NamedCounters() {
		if err := rdb.Del(ctx, metrics.KeyPrefix+name).Err(); err != nil {
			logger.Warn("metrics: legacy counter cleanup failed", "name", name, "err", err)
		}
	}

	p, err := proxy.New(proxy.Config{
		WeatherAPI: weatherAPI,
		NewsAPI:    newsAPI,
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
		NoCachePaths: []string{"/dashboard", "/api/secrets", "/api/checks", "/api/metrics", "/style.css", "/app.js", "/", "/healthz"},
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
		Metrics:     metricsStore,
	})

	agg := aggregation.New(getSecret,
		// The dashboard keeps its built-in default upstreams even when the env
		// var is set empty to disable the public proxy route.
		aggregation.WithWeatherURL(envOrDefault("WEATHER_API_URL", aggregation.DefaultWeatherURL)),
		aggregation.WithNewsURL(envOrDefault("NEWS_API_URL", aggregation.DefaultNewsURL)),
		aggregation.WithNewsURLRU(envOrDefault("NEWS_API_URL_RU", aggregation.DefaultNewsURLRU)),
		aggregation.WithNewsQuota(newsQuota),
		aggregation.WithNewsStore(newsstore.New(rdb)),
		aggregation.WithNewsStoreRU(newsstore.NewLang(rdb, "ru")),
		aggregation.WithNewsPollInterval(func(ctx context.Context) time.Duration {
			return aggregation.ParseInterval(getSecret(ctx, "NEWS_POLL_INTERVAL"))
		}),
	)

	chk := checks.New(rdb, checks.Config{
		Interval: func(ctx context.Context) time.Duration {
			return checks.ParseInterval(getSecret(ctx, "CHECK_INTERVAL"))
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
			// Fire-and-forget: the webhook is a side effect and a slow endpoint
			// must not stall the probe loop or a "check now" request.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				webhook.Post(ctx, payload)
			}()
		},
		AllowPrivate: envBoolOrDefault("CHECKS_ALLOW_PRIVATE", false),
	})

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("GET /weather", p.Weather())
	mux.Handle("GET /news", newsQuota.Middleware(p.News()))
	mux.Handle("GET /dashboard", agg)
	mux.Handle("GET /healthz", middleware.Health(rdb, logger))
	mux.Handle("GET /api/metrics", metricsHandler(metricsStore, newsQuota))
	secrets.NewHandler(store).Register(mux)
	checks.NewHandler(chk).Register(mux)
	mux.Handle("GET /", noCache(http.FileServer(http.FS(staticSub))))

	var h http.Handler = mux
	h = c.Middleware(h)
	h = rl.Middleware(h)
	h = middleware.Recover(logger, h)
	h = metricsStore.Middleware(h)
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

// metricsHandler serves HTTP status counters plus today's NewsAPI budget usage.
func metricsHandler(store *metrics.Store, newsQuota *quota.Limiter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		out := make(map[string]int64, len(metrics.NamedCounters())+2)
		for k, v := range store.Values(ctx) {
			out[k] = v
		}
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
