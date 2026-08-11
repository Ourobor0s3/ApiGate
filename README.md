# ApiGate

API Gateway in Go: reverse-proxies `/weather` and `/news`, caches the responses in Redis, rate-limits per IP, and serves an aggregated `/dashboard` with a Vue 3 web UI — weather, currency rates, news (EN/RU) and site-reachability checks.

## Quick start

Prerequisites: Go 1.22+, Redis on `localhost:6379`, Node.

```bash
redis-server redis.conf            # start Redis with persistence (repo root)
cd frontend && npm install && npm run build   # build the UI once
go run ./cmd/apigate               # serves API + UI on :8080
```

Open http://localhost:8080. The sidebar splits the UI into pages: **Overview** (weather, currency rates), **News** (EN/RU headlines + the NewsAPI budget bar; follows the UI language), **Site Checks**, **API Secrets**. The EN/RU switch in the header is remembered per browser. Without the frontend build the server serves a stub page that says so.

## Docker

```bash
docker compose up --build -d    # builds the image, starts Redis + the gateway
docker compose logs -f apigate
docker compose down             # stop (Redis data volume persists)
docker compose down -v          # stop and wipe stored secrets/settings
```

Open http://localhost:8083. The image is multi-stage (Node-built SPA + static Go binary in a non-root alpine runtime; only binary + `frontend/dist` inside). The gateway starts only after Redis is healthy; secrets saved in the 🔑 API Secrets UI survive restarts via the `redis-data` volume.

## API keys

Only NewsAPI needs a key (free plan: 100 requests/24h). Register at https://newsapi.org and save `NEWS_API_KEY` in the 🔑 **API Secrets** page (stored in Redis, applied without a restart) or set the env var. Without a key the news card shows an error; when the shared daily budget (`NEWS_DAILY_LIMIT`, env-only) is spent, `/news` returns 429 and the news card shows an error — weather and rates keep working. The counter resets at midnight local time.

## Runtime settings

Everything below is changeable at runtime on the 🔑 **API Secrets** page (stored in Redis, applied without a restart; each also has an env fallback of the same name). Saving or clearing a data-affecting value (`NEWS_API_KEY`, `NEWS_API_URL`, `NEWS_API_URL_RU`, `WEATHER_LOCATION`, `MAIN_CURRENCY`) refreshes the dashboard immediately instead of waiting for the next poll.

| Setting | Default | Description |
| --- | --- | --- |
| `NEWS_API_KEY` | — | NewsAPI key (masked in the UI, never revealed) |
| `NEWS_API_URL` | `https://newsapi.org/v2/top-headlines?sources=bbc-news,cnn,reuters,associated-press,abc-news,nbc-news,cbs-news,al-jazeera-english,dw,the-guardian-uk,france-24,independent&pageSize=50` | English news upstream (empty = route disabled) |
| `NEWS_API_URL_RU` | `https://newsapi.org/v2/top-headlines?sources=lenta,rbc,rt,google-news-ru&pageSize=50` | Russian news upstream (empty = RU news disabled) |
| `WEATHER_LOCATION` | `55.7558,37.6173` | Weather `lat,lon` |
| `MAIN_CURRENCY` | `USD` | Rates base currency; `100RUB` shows every rate per 100 RUB |
| `NEWS_POLL_INTERVAL` | `60m` | Background refresh cycle: news EN + RU (**2 quota'd NewsAPI requests** per cycle — keep above ~29 min so the budget of 100 lasts) plus 3 unquota'd snapshots |
| `CHECK_INTERVAL` | `5m` | Site-check interval |

Everything else (`PORT`, `REDIS_ADDR`, `WEBHOOK_URL`, `CORS_ORIGIN`, `TRUSTED_PROXIES`, cache/circuit-breaker tuning, `CHECKS_ALLOW_PRIVATE`) is env-only.

## Endpoints

- `/` — the built SPA (not cached)
- `/dashboard` — aggregated JSON: weather (+place), news + newsRu (stored history), rates, `missingSecrets`/`error`. Served from Redis only — zero upstream calls per request. Times are rendered in the weather location's timezone
- `/weather`, `/news` — cached (300s/60s), rate-limited (100 req/min/IP) proxies with an upstream circuit breaker; `/news` → 429 when the daily budget is spent
- `/healthz` — Redis PING, exempt from cache/rate limiting
- `/api/newsquota` — today's NewsAPI budget usage
- `/api/secrets` — `GET` list + `POST {name,value}` + `DELETE ?name=`; masked values are never revealed
- `/api/checks` — `GET` statuses/uptime, `POST {url}` add (probes immediately), `POST /api/checks/run`, `DELETE ?url=`

## News history

A background poller (default every 60m) pulls headlines for both languages into Redis and stores them deduplicated for 4 days; the news card pages through the history 8 items at a time, newest first. When the budget is exhausted the poller skips and the stored history keeps serving instead of an error card. The dashboard never fetches news per request.

## Architecture

`cmd/apigate` is the composition root — one place wires Redis, the poller, caching and every middleware; graceful shutdown on SIGINT/SIGTERM. The logic lives in small `internal/` packages: `proxy` (reverse proxies + circuit breaker), `cache`, `ratelimit`, `quota` (daily news budget), `newsstore` (history), `aggregation` (dashboard + background poller), `checks` (site probing + webhooks), `secrets` (runtime settings), `middleware`, `notify`, `netguard` (SSRF protection).

## Security

Designed for a trusted local network: there is **no authentication**, and every endpoint (including `/api/secrets` and `/api/checks`) is open to anyone who can reach the port. Rate limiting is per IP and fails open when Redis is unreachable (availability over protection). Don't set `CORS_ORIGIN` to `*`.

## Development

```bash
go build ./...
go vet ./...
go test ./...      # unit tests run without a live Redis
```

For frontend work: `cd frontend && npm run dev` (Vite on :5173) against the running Go server — the SPA uses hash routing, so the Go server keeps serving only `/`. UI copy lives in `frontend/src/i18n.js` (EN/RU); cards are one component per feature in `frontend/src/components/`.