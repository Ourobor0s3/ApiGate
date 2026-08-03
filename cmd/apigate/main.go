package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Ourobor0s3/ApiGate/internal/aggregation"
	"github.com/Ourobor0s3/ApiGate/internal/cache"
	"github.com/Ourobor0s3/ApiGate/internal/proxy"
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

	p, err := proxy.New(proxy.Config{
		WeatherAPI: envOrDefault("WEATHER_API_URL", "https://api.open-meteo.com/v1/forecast"),
		NewsAPI:    envOrDefault("NEWS_API_URL", "https://newsapi.org/v2/top-headlines"),
		NewsAPIKey: func(ctx context.Context) string { return getSecret(ctx, "NEWS_API_KEY") },
	})
	if err != nil {
		return err
	}

	c := cache.New(rdb, cache.Config{
		DefaultTTL:   300,
		RouteTTLs:    map[string]int64{"/weather": 300, "/news": 60},
		NoCachePaths: []string{"/dashboard", "/api/secrets", "/"},
	})

	rl := ratelimit.New(rdb, ratelimit.Config{
		Limit:  100,
		Window: 60,
	})

	aggOpts := []aggregation.Option{}
	if u := os.Getenv("WEATHER_API_URL"); u != "" {
		aggOpts = append(aggOpts, aggregation.WithWeatherURL(u))
	}
	if u := os.Getenv("NEWS_API_URL"); u != "" {
		aggOpts = append(aggOpts, aggregation.WithNewsURL(u))
	}
	agg := aggregation.New(getSecret, aggOpts...)

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("GET /weather", p.Weather())
	mux.Handle("GET /news", p.News())
	mux.Handle("GET /dashboard", agg)
	secrets.NewHandler(store).Register(mux)
	mux.Handle("GET /", noCache(http.FileServer(http.FS(staticSub))))

	var h http.Handler = mux
	h = c.Middleware(h)
	h = rl.Middleware(h)

	httpServer := &http.Server{
		Addr:              ":" + envOrDefault("PORT", "8080"),
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
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

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
