package main

import (
	"context"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"

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
	rdb := redis.NewClient(&redis.Options{
		Addr: envOrDefault("REDIS_ADDR", "localhost:6379"),
	})

	store := secrets.New(rdb)
	getSecret := func(ctx context.Context, name string) string {
		if v, err := store.Get(ctx, name); err == nil && v != "" {
			return v
		}
		return os.Getenv(name)
	}

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("cannot reach Redis at %s: %v", rdb.Options().Addr, err)
	}
	if names, err := store.List(context.Background()); err != nil {
		log.Printf("secrets: cannot list from Redis: %v", err)
	} else {
		log.Printf("secrets loaded from Redis: %v", names)
	}

	p, err := proxy.New(proxy.Config{
		WeatherAPI: envOrDefault("WEATHER_API_URL", "https://api.open-meteo.com/v1/forecast"),
		NewsAPI:    envOrDefault("NEWS_API_URL", "https://newsapi.org/v2/top-headlines"),
		NewsAPIKey: func(ctx context.Context) string { return getSecret(ctx, "NEWS_API_KEY") },
	})
	if err != nil {
		log.Fatal(err)
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

	mux := http.NewServeMux()
	mux.Handle("/weather", p)
	mux.Handle("/news", p)
	mux.Handle("/dashboard", aggregation.Handler(getSecret))
	mux.Handle("/api/secrets", secrets.Handler(store))

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", noCache(http.FileServer(http.FS(staticSub))))

	var h http.Handler = mux
	h = c.Middleware(h)
	h = rl.Middleware(h)

	port := envOrDefault("PORT", "8080")
	log.Printf("ApiGate listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, h))
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
