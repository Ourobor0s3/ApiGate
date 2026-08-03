# ApiGate

API Gateway in Go. Proxies `/weather` and `/news` to external APIs, caches GET responses in Redis, rate-limits per-IP with a sliding window, serves an aggregated `/dashboard` with a web UI, and periodically probes user-configured URLs to show site reachability.

## Screenshot

![ApiGate Dashboard](docs/dashboard.png)

## Quick start

Prerequisites: Go 1.22+ and a running Redis on `localhost:6379`.

```bash
redis-server redis.conf    # start Redis with persistence (run from repo root)
go run ./cmd/apigate
```

Open http://localhost:8080. The 🔑 **API Secrets** card stores settings in Redis (applied on the next request, no restart); the 📡 **Site Checks** card monitors the reachability of any URL.

## API keys

Only NewsAPI needs a key (free plan: 100 requests/24h). Register at https://newsapi.org, then save `NEWS_API_KEY` in the 🔑 API Secrets card, or set the `NEWS_API_KEY` env var. Without a key the news card shows an error. News requests are capped by a shared daily budget (`NEWS_DAILY_LIMIT`) — once spent, `/news` returns 429 and the dashboard news card shows an error while weather/rates keep working. The counter resets at midnight local time.

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `REDIS_ADDR` | `localhost:6379` | Redis address |
| `PORT` | `8080` | HTTP listen port |
| `WEATHER_API_URL` | `https://api.open-meteo.com/v1/forecast` | Weather upstream (empty = route disabled) |
| `NEWS_API_URL` | `https://newsapi.org/v2/everything?language=en&sortBy=popularity&pageSize=50` | News upstream (empty = route disabled) |
| `NEWS_API_KEY` | — | NewsAPI key fallback (Redis `secret:NEWS_API_KEY` wins) |
| `NEWS_DAILY_LIMIT` | `100` | Shared daily news request budget |
| `CHECK_INTERVAL` | `5m` | Site-check interval, e.g. `1m` or `6m 30s` (Redis `secret:CHECK_INTERVAL` wins) |
| `WEATHER_LOCATION` | `55.7558,37.6173` | Weather `lat,lon` (Redis `secret:WEATHER_LOCATION` wins) |
| `MAIN_CURRENCY` | `USD` | Rates base currency (Redis `secret:MAIN_CURRENCY` wins) |

`WEATHER_API_URL`/`NEWS_API_URL` override both the proxy routes and the dashboard upstreams. Secrets stored in Redis survive restarts (`redis.conf` uses RDB + AOF; `dump.rdb`/`appendonlydir/` are gitignored).

## Endpoints

- `/` — dashboard UI (static, not cached)
- `/dashboard` — aggregated JSON: `weather` (+`weatherPlace`), `news` (all stored articles, newest first), `rates`, `missingSecrets`/`error`; `Cache-Control: no-store`
- `/weather`, `/news` — reverse-proxied GETs (Redis cache 300s/60s, rate-limited 100 req/min per IP); `/news` enforces the daily quota → 429 when spent
- `/healthz` — Redis PING → 200/503 (exempt from cache and rate limiting)
- `/api/secrets` — `GET` lists names only, `POST {name,value}` upserts, `DELETE ?name=` removes (names match `[A-Za-z0-9_-]+`, values never returned)
- `/api/checks` — `GET` targets + statuses + interval, `POST {url}` adds one (http/https only, 409 on duplicate, probes it immediately), `POST /api/checks/run` probes all now, `DELETE ?url=` removes

## Data sources

| Card | Source | Key required |
| --- | --- | --- |
| Weather | [open-meteo](https://open-meteo.com) | No |
| Place name | [BigDataCloud](https://www.bigdatacloud.com) reverse-geocoding | No |
| News | [NewsAPI](https://newsapi.org) (`/v2/everything`, popular English, 50 per fetch) | Yes |
| Currency rates | [ExchangeRate-API](https://www.exchangerate-api.com) | No |

## News history

Every dashboard fetch pulls the latest 50 articles into Redis (`news:article:<url>`, each kept 4 days). Articles are deduplicated by URL and never overwritten, so the dashboard replies with the full stored history, newest first, and the news card pages through it 10 at a time (Prev/Next). When the daily budget is exhausted the stored history keeps serving instead of an error card.

## Architecture

- `cmd/apigate` — composition root: logging → recovery → rate limiting → cache → mux; graceful shutdown (`signal.NotifyContext` → `Shutdown`, 10s timeout)
- `internal/proxy` — `httputil.ReverseProxy` per upstream (`Weather()`/`News()`, not-found when the URL is empty); merges query params and appends the news `apiKey`
- `internal/cache` — GET-only, only 2xx cached; custom binary serialization; per-route TTLs
- `internal/ratelimit` — sliding window over a Redis ZSET per IP; `/healthz` exempt
- `internal/quota` — global daily budget as an atomic Lua counter, shared by `/news` and the dashboard; fails open
- `internal/newsstore` — deduplicated news archive (`news:article:<url>` keys + `news:index` ZSET)
- `internal/aggregation` — `/dashboard` fans out 4 upstream fetches in parallel; stores and replays news history
- `internal/checks` — probes URLs on a schedule (`CHECK_INTERVAL` re-read each cycle); REST API + `POST /api/checks/run`
- `internal/middleware` — request logging (no query string), panic recovery, health, client IP
- `internal/secrets` — Redis key store + REST API on `/api/secrets`

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

Unit tests for `proxy`, `cache`, `quota`, `secrets`, `aggregation`, `checks`, `newsstore`, `middleware` run without a live Redis.
