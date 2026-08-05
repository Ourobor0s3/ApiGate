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

Open http://localhost:8080. The 🔑 **API Secrets** card stores settings in Redis (applied on the next request, no restart); the 📡 **Site Checks** card monitors the reachability of any URL. The interface language (EN/RU) is switched in the header and remembered per browser — news follows it: English mode shows the English sources, Russian mode shows Russian-language headlines (lenta, RBC, RT).

## API keys

Only NewsAPI needs a key (free plan: 100 requests/24h). Register at https://newsapi.org, then save `NEWS_API_KEY` in the 🔑 API Secrets card, or set the `NEWS_API_KEY` env var. Without a key the news card shows an error. News requests are capped by a shared daily budget (`NEWS_DAILY_LIMIT`) — once spent, `/news` returns 429 and the dashboard news card shows an error while weather/rates keep working. The counter resets at midnight local time.

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `REDIS_ADDR` | `localhost:6379` | Redis address |
| `PORT` | `8080` | HTTP listen port |
| `WEATHER_API_URL` | `https://api.open-meteo.com/v1/forecast` | Weather upstream (empty = route disabled) |
| `NEWS_API_URL` | `https://newsapi.org/v2/top-headlines?sources=bbc-news,cnn,reuters,associated-press,abc-news,nbc-news,cbs-news,al-jazeera-english,dw,the-guardian-uk,france-24,independent&pageSize=50` | English news upstream (empty = route disabled) |
| `NEWS_API_URL_RU` | `https://newsapi.org/v2/top-headlines?sources=lenta,rbc,rt&pageSize=50` | Russian news upstream for the RU dashboard mode (empty = RU news disabled) |
| `NEWS_API_KEY` | — | NewsAPI key fallback (Redis `secret:NEWS_API_KEY` wins) |
| `NEWS_DAILY_LIMIT` | `100` | Shared daily news request budget |
| `CHECK_INTERVAL` | `5m` | Site-check interval, e.g. `1m` or `6m 30s` (Redis `secret:CHECK_INTERVAL` wins) |
| `NEWS_POLL_INTERVAL` | `30m` | How often the background poller refills the news store from NewsAPI (Redis `secret:NEWS_POLL_INTERVAL` wins). With RU news enabled the two languages alternate on consecutive cycles, so both refresh every two intervals without spending extra quota |
| `WEATHER_LOCATION` | `55.7558,37.6173` | Weather `lat,lon` (Redis `secret:WEATHER_LOCATION` wins) |
| `MAIN_CURRENCY` | `USD` | Rates base currency (Redis `secret:MAIN_CURRENCY` wins) |
| `TRUSTED_PROXIES` | — | Comma-separated CIDRs of trusted reverse proxies whose `X-Forwarded-For` is honored for per-IP rate limiting; unset = header ignored (unspoofable) |
| `WEBHOOK_URL` | — | Webhook URL receiving JSON on quota exhaustion (once/day) and site status changes; unset = no notifications |
| `CORS_ORIGIN` | — | Origin allowed via CORS headers (e.g. `https://app.example.com`); unset = same-origin only. `*` allows any |
| `CACHE_STALE_WHILE_REVALIDATE` | `10m` | Serve stale cached data for up to this long past TTL while refreshing in the background (entries are stored with a TTL of route-TTL + this value) |
| `CACHE_WARM_PATHS` | — | Comma-separated paths to prefetch through the cache shortly after startup (e.g. `/weather,/news`) |
| `UPSTREAM_BREAK_FAILURES` | `5` | Consecutive 5xx/errors that open the upstream circuit breaker (0 = disabled) |
| `UPSTREAM_BREAK_COOLDOWN` | `30s` | How long the breaker stays open before a single half-open probe |
| `CHECKS_ALLOW_PRIVATE` | `false` | Allow site-check targets that resolve to private/loopback/link-local addresses (SSRF guard) |

`WEATHER_API_URL`/`NEWS_API_URL` override both the proxy routes and the dashboard upstreams. Setting either to an empty string disables the corresponding proxy route (the dashboard keeps its built-in default upstreams). Secrets stored in Redis survive restarts (`redis.conf` uses RDB + AOF; `dump.rdb`/`appendonlydir/` are gitignored).

## Endpoints

- `/` — dashboard UI (static, not cached)
- `/dashboard` — aggregated JSON: `weather` (+`weatherPlace`), `news` + `newsRu` (stored history per UI language, newest first), `rates`, `missingSecrets`/`error`; `Cache-Control: no-store`. News never hits the upstream here — a background poller refills the stores on a schedule, so auto/manual refreshes only spend quota on weather/rates. Times are rendered in the weather location's timezone, not the viewer's
- `/weather`, `/news` — reverse-proxied GETs (Redis cache 300s/60s, rate-limited 100 req/min per IP, circuit-breaker on the upstream); `/news` enforces the daily quota → 429 when spent
- `/healthz` — Redis PING → 200/503 (exempt from cache and rate limiting)
- `/api/metrics` — JSON counters: `http_2xx`, `http_3xx`, `http_4xx`, `http_5xx`, `quota_rejected` (zero counters omitted), plus `news_quota_used`/`news_quota_limit` for the dashboard's budget bar. Bucketed per day (`metric:<name>:<date>`) and expire 48h after their last update, so stale days self-clean
- `/api/secrets` — `GET` lists names only, `POST {name,value}` upserts, `DELETE ?name=` removes (names match `[A-Za-z0-9_-]+`, values never returned)
- `/api/checks` — `GET` targets + statuses + uptime + interval, `POST {url}` adds one (http/https + public-only by default, 409 on duplicate, probes it immediately), `POST /api/checks/run` probes all now, `DELETE ?url=` removes

## Data sources

| Card | Source | Key required |
| --- | --- | --- |
| Weather | [open-meteo](https://open-meteo.com) | No |
| Place name | [BigDataCloud](https://www.bigdatacloud.com) reverse-geocoding | No |
| News (EN) | [NewsAPI](https://newsapi.org) (`/v2/top-headlines`, curated list of 12 major verified sources, 50 per fetch) | Yes |
| News (RU) | [NewsAPI](https://newsapi.org) (`/v2/top-headlines`, `sources=lenta,rbc,rt` — the free plan returns no articles for `country=ru`/`language=ru`, but the sources feed works) | Yes |
| Currency rates | [ExchangeRate-API](https://www.exchangerate-api.com) | No |

## News history

A background poller pulls the day's top headlines from NewsAPI on a schedule (`NEWS_POLL_INTERVAL`, default 30m) into Redis as separate English and Russian stores. Each article is stored under a SHA-1 digest of its URL (`news:article:<lang>:<40 hex>`), and the index ZSET (`news:index:<lang>`) orders them by publication time. Articles are kept for 4 days; the index outlives its newest article by one day and self-expires when the last article ages out. When RU news is enabled the two languages are fetched on alternate cycles — one upstream request per interval — so each refreshes every two intervals and the daily budget is spent exactly as before. Articles are deduplicated by URL digest and never overwritten, so the dashboard replies with the full stored history, newest first, and the news card pages through it 10 at a time (Prev/Next). Dashboard refreshes never call NewsAPI themselves — the daily budget is spent only by the poller and the `/news` route. When the budget is exhausted the poller skips (and, if the budget backend is unreachable, the poll fails closed rather than spend blindly) and the stored history keeps serving instead of an error card.

## Redis storage

Everything lives under a small set of namespaced key patterns, all self-cleaning (TTLs and score/rank pruning, no background sweeper):

| Pattern | Type | TTL | Owner |
| --- | --- | --- | --- |
| `cache:GET:/path?query` | string (binary response) | route TTL + `CACHE_STALE_WHILE_REVALIDATE` | `internal/cache` |
| `ratelimit:<ip>` | ZSET (sliding window) | 60s window, refreshed per request | `internal/ratelimit` |
| `quota:<name>:<date>` | string counter | 48h from first hit | `internal/quota` |
| `quota:notify:<name>:<date>` | string | 48h (webhook dedup, once/day) | `internal/quota` |
| `metric:<name>:<date>` | string counter | 48h from last increment | `internal/metrics` |
| `secret:<name>` | string | never (persistent config) | `internal/secrets` |
| `check:targets` | SET | never (persistent config) | `internal/checks` |
| `check:history:<sha1>` | ZSET (status JSON, score = probe ms) | 96h from last probe, refreshed on every write; legacy URL-keyed entries dropped on sight | `internal/checks` |
| `news:index:<lang>` | ZSET (SHA-1 digest → publishedAt) | 5 days from last addition (article TTL + 1 day margin); stale members dropped lazily on read | `internal/newsstore` |
| `news:article:<lang>:<sha1>` | string (article JSON) | 4 days per article | `internal/newsstore` |

No key ever embeds a secret (the news `apiKey` is stripped from cache keys) and no list-based structure grows unbounded: every accumulation is a score-pruned ZSET or a dated key with a TTL. Check history keys and the news index both have explicit TTLs; article keys expire on their own 4-day clock.

## Architecture

- `cmd/apigate` — composition root: request logger → security headers/CORS → gzip → metrics → recovery → rate limiting → cache → mux; graceful shutdown (`signal.NotifyContext` → `Shutdown`, 10s timeout); optional startup cache warm
- `internal/proxy` — `httputil.ReverseProxy` per upstream (`Weather()`/`News()`, not-found when the URL is empty); merges query params and appends the news `apiKey`; optional per-upstream circuit breaker
- `internal/cache` — GET-only, only 2xx cached; custom binary serialization; per-route TTLs; stale-while-revalidate with `X-Cache: HIT/MISS/STALE`, single-flight background refresh per key; cache keys never include the news `apiKey`
- `internal/ratelimit` — sliding window over a Redis ZSET per IP, pruned and admitted atomically in one Lua call (`/healthz` exempt); fails open when Redis is unreachable; honors `X-Forwarded-For` only from proxies in `TRUSTED_PROXIES`, otherwise the header is ignored as spoofable
- `internal/quota` — global daily budget as an atomic Lua counter, shared by `/news` and the dashboard; fails open; fires a once-a-day webhook when exhausted
- `internal/newsstore` — deduplicated news archive, one namespace per language (`news:index:<lang>` ZSET + `news:article:<lang>:<sha1>` keys); legacy bare `news:index` and URL-keyed article keys are dropped on sight; index TTL = article TTL + 1 day
- `internal/aggregation` — `/dashboard` fetches weather/place/rates in parallel per request and serves news from the stores; a background poller (`Run`) refills them on `NEWS_POLL_INTERVAL`, alternating EN/RU cycles when both are configured — the only dashboard-side news fetch, so refreshes don't spend the NewsAPI quota
- `internal/checks` — probes URLs on a schedule (`CHECK_INTERVAL` re-read each cycle) with a bounded worker pool (10 concurrent probes); per-target rolling history keyed by SHA-1 digest (`check:history:<hex>`, up to last 100 probes, records pruned after 48h, key TTL 96h from last probe); legacy URL-keyed entries dropped on sight; status-change webhooks, SSRF guard against private/loopback targets; REST API + `POST /api/checks/run`
- `internal/metrics` — Redis-backed counters, counted per status class via a capturing middleware; per-day buckets that expire 48h after their last increment; `/api/metrics` JSON handler
- `internal/middleware` — request logging (no query string), panic recovery, health, client IP, security headers/CORS, gzip
- `internal/notify` — fire-and-forget JSON webhook client (non-2xx and send errors are logged, never fatal)
- `internal/secrets` — Redis key store + REST API on `/api/secrets`

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

Unit tests for `proxy`, `cache`, `secrets`, `aggregation`, `checks`, `newsstore`, `middleware`, `ratelimit`, `metrics`, `notify`, `quota` run without a live Redis.
