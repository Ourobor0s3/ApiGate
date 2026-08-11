package aggregation

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ourobor0s3/ApiGate/internal/netguard"
	"github.com/Ourobor0s3/ApiGate/internal/newsstore"
	"github.com/Ourobor0s3/ApiGate/internal/quota"
)

const (
	DefaultWeatherURL = "https://api.open-meteo.com/v1/forecast"
	// DefaultNewsURL pulls top headlines from a curated list of major
	// international sources. pageSize is capped at 50 so the poller pulls a
	// bounded payload; articles are persisted in the news store and replayed
	// across requests.
	DefaultNewsURL = "https://newsapi.org/v2/top-headlines?sources=bbc-news,cnn,reuters,associated-press,abc-news,nbc-news,cbs-news,al-jazeera-english,dw,the-guardian-uk,france-24,independent&pageSize=50"
	// DefaultNewsURLRU feeds the dashboard's Russian mode. country=ru / language=ru
	// return empty articles on the free plan, but the sources feed works
	// (sources=lenta,rbc,rt,google-news-ru; verified).
	DefaultNewsURLRU = "https://newsapi.org/v2/top-headlines?sources=lenta,rbc,rt,google-news-ru&pageSize=50"

	defaultPlaceURL = "https://api.bigdatacloud.net/data/reverse-geocode-client"
	defaultRatesURL = "https://api.exchangerate-api.com/v4/latest"
	defaultTimeout  = 10 * time.Second
	// maxNewsAge is the oldest publication date shown in the news card; the
	// /everything request also passes a matching `from` filter.
	maxNewsAge = 48 * time.Hour
	// maxBodyBytes caps a single upstream response so a misbehaving source
	// can't balloon dashboard memory. All four upstreams return well under 1 MiB.
	maxBodyBytes = 1 << 20
	// DefaultPollInterval keeps the daily NewsAPI budget (100) well covered:
	// two requests per cycle at 60m = 48/day. The poll is the only
	// dashboard-side news fetch, so the quota lasts all day.
	DefaultPollInterval = 60 * time.Minute
)

// DashboardData is the aggregated dashboard payload. Everything is served
// from Redis: weather/place/rates are written by the background poller and
// news comes from the news stores, so a dashboard request never touches an
// upstream.
type DashboardData struct {
	Weather      interface{} `json:"weather,omitempty"`
	WeatherPlace string      `json:"weatherPlace,omitempty"`
	News         interface{} `json:"news,omitempty"`
	// NewsRU carries the Russian-language headline store; the UI picks the
	// block matching its language. Omitted when the RU poller/store are not
	// configured or still empty.
	NewsRU         interface{} `json:"newsRu,omitempty"`
	Rates          interface{} `json:"rates,omitempty"`
	MissingSecrets []string    `json:"missingSecrets,omitempty"`
	Error          string      `json:"error,omitempty"`
}

// Handler aggregates weather, news and currency rates for /dashboard.
// Nothing is fetched per request: weather/place/rates come from the Redis
// snapshot store (kv, refreshed by the background poller) and news from the
// news stores. getSecret resolves a setting by name at request time; returning
// "" falls back to defaults.
type Handler struct {
	httpClient   *http.Client
	logger       *slog.Logger
	getSecret    func(context.Context, string) string
	newsQuota    NewsQuota
	newsStore    NewsStore
	newsStoreRU  NewsStore
	kv           Store
	weatherURL   string
	newsURL      string
	newsURLRU    string
	placeURL     string
	ratesURL     string
	pollInterval func(context.Context) time.Duration
	statusMu     sync.Mutex
	status       newsStatus
	statusRU     newsStatus
}

// NewsQuota gates upstream newsapi consumption so the daily free-plan budget
// is shared between the /news route and the dashboard news block. Nil disables.
type NewsQuota interface {
	Allow(ctx context.Context) (bool, error)
}

// NewsStore persists and replays newsapi articles. Nil skips persistence.
type NewsStore interface {
	Store(ctx context.Context, articles []newsstore.Article) error
	All(ctx context.Context) ([]newsstore.Article, error)
}

// Store is the narrow Redis-backed surface the dashboard reads weather, place
// and rates from. Nil disables the snapshot poller. Tests use an in-memory fake.
type Store interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key string, value string, ttl time.Duration) error
}

// newsStatus records the outcome of the latest background news poll so the
// dashboard can surface key, quota and upstream problems. Zero value = success.
type newsStatus struct {
	missingKey bool   // upstream reported a missing/invalid API key
	exhausted  bool   // daily quota spent, poll skipped
	code       string // newsapi-style error code, e.g. "upstreamError"
	message    string
}

type Option func(*Handler)

func WithHTTPClient(c *http.Client) Option {
	return func(h *Handler) { h.httpClient = c }
}

func WithWeatherURL(u string) Option {
	return func(h *Handler) { h.weatherURL = u }
}

func WithNewsURL(u string) Option {
	return func(h *Handler) { h.newsURL = u }
}

// WithNewsURLRU sets the Russian-language news upstream used by the RU poller.
func WithNewsURLRU(u string) Option {
	return func(h *Handler) { h.newsURLRU = u }
}

func WithNewsQuota(q NewsQuota) Option {
	return func(h *Handler) { h.newsQuota = q }
}

func WithNewsStore(s NewsStore) Option {
	return func(h *Handler) { h.newsStore = s }
}

// WithNewsStoreRU enables the Russian-language news store and poller. Without
// it the poller runs in single-language mode and /dashboard omits newsRu.
func WithNewsStoreRU(s NewsStore) Option {
	return func(h *Handler) { h.newsStoreRU = s }
}

func WithPlaceURL(u string) Option {
	return func(h *Handler) { h.placeURL = u }
}

func WithRatesURL(u string) Option {
	return func(h *Handler) { h.ratesURL = u }
}

// WithKV wires the Redis-backed snapshot store for weather/place/rates. Without
// it the poller skips the snapshot fetch and /dashboard serves empty cards.
func WithKV(s Store) Option {
	return func(h *Handler) { h.kv = s }
}

// WithNewsPollInterval sets how often the background poller refreshes the news
// store from the upstream. The resolver is re-invoked each cycle, so a changed
// secret takes effect on the next poll. Defaults to DefaultPollInterval.
func WithNewsPollInterval(f func(context.Context) time.Duration) Option {
	return func(h *Handler) { h.pollInterval = f }
}

func New(getSecret func(context.Context, string) string, opts ...Option) *Handler {
	h := &Handler{
		// Dial guard + redirect guard: a public URL (which a Redis secret can
		// set) can't turn the poller into an SSRF relay, and DNS rebinding
		// between the URL check and the connect can't slip a private address
		// past the dial-time check.
		httpClient: &http.Client{
			Timeout:       8 * time.Second,
			CheckRedirect: guardRedirects,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           netguard.RestrictedDialContext(&net.Dialer{Timeout: 5 * time.Second}),
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          20,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       30 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
				TLSHandshakeTimeout:   5 * time.Second,
			},
		},
		logger:       slog.Default(),
		getSecret:    getSecret,
		weatherURL:   DefaultWeatherURL,
		newsURL:      DefaultNewsURL,
		newsURLRU:    DefaultNewsURLRU,
		placeURL:     defaultPlaceURL,
		ratesURL:     defaultRatesURL,
		pollInterval: func(context.Context) time.Duration { return DefaultPollInterval },
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func (h *Handler) setNewsStatus(st newsStatus) {
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	h.status = st
}

func (h *Handler) currentNewsStatus() newsStatus {
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	return h.status
}

func (h *Handler) setNewsStatusRU(st newsStatus) {
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	h.statusRU = st
}

func (h *Handler) currentNewsStatusRU() newsStatus {
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	return h.statusRU
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), defaultTimeout)
	defer cancel()

	col := &collector{res: &DashboardData{}}
	lat, lon := parseLocation(h.getSecret(ctx, "WEATHER_LOCATION"))
	code, amount := h.baseCurrency(ctx)

	// No upstream calls here: snapshots come from Redis, news from its store.
	col.wg.Add(5)
	go h.serveKV(ctx, col, "weather", h.weatherKey(lat, lon))
	go h.serveKV(ctx, col, "place", h.placeKey(lat, lon))
	go h.serveKV(ctx, col, "rates", h.ratesKey(code, amount))
	go h.fetchStoreNews(ctx, col)
	go h.fetchStoreNewsRU(ctx, col)
	col.wg.Wait()

	res := col.res
	if h.currentNewsStatus().missingKey || h.currentNewsStatusRU().missingKey {
		res.MissingSecrets = append(res.MissingSecrets, "NEWS_API_KEY")
	}
	if res.Weather == nil && res.News == nil && res.Rates == nil {
		res.Error = "all upstream services failed"
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(res)
}

// serveKV reads one snapshot from the Redis store. Missing keys are skipped.
func (h *Handler) serveKV(ctx context.Context, col *collector, field, key string) {
	defer col.wg.Done()
	if h.kv == nil {
		return
	}
	s, err := h.kv.Get(ctx, key)
	if err != nil || s == "" {
		if err != nil {
			h.logger.Warn("dashboard: read snapshot", "field", field, "err", err)
		}
		return
	}
	switch field {
	case "place":
		var p string
		if err := json.Unmarshal([]byte(s), &p); err == nil && p != "" {
			col.set(field, p)
		}
	default:
		var v interface{}
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			col.set(field, v)
		}
	}
}

func (h *Handler) weatherURLFor(lat, lon float64) string {
	return addQuery(h.weatherURL, url.Values{
		"latitude":        []string{strconv.FormatFloat(lat, 'f', 4, 64)},
		"longitude":       []string{strconv.FormatFloat(lon, 'f', 4, 64)},
		"current_weather": []string{"true"},
		// timezone=auto gives the location's real IANA zone so the dashboard
		// renders wall-clock times instead of drifting into GMT.
		"timezone": []string{"auto"},
		// Forecast over today and tomorrow: hourly temperature, weather code
		// and precipitation chance feed the next-N-hours strip. forecast_days=2
		// lets the evening strip run past midnight (open-meteo defaults to 7
		// days, a waste of payload and Redis TTL).
		"hourly": []string{"temperature_2m,weathercode,precipitation_probability"},
		// Sunrise/sunset for the same days, shown on the weather card.
		"daily":         []string{"sunrise,sunset"},
		"forecast_days": []string{"2"},
	})
}

func (h *Handler) placeURLFor(lat, lon float64) string {
	return addQuery(h.placeURL, url.Values{
		"latitude":         []string{strconv.FormatFloat(lat, 'f', 6, 64)},
		"longitude":        []string{strconv.FormatFloat(lon, 'f', 6, 64)},
		"localityLanguage": []string{"en"},
	})
}

// newsURLFor returns the effective English news upstream: a Redis secret
// NEWS_API_URL overrides the compiled/env default at request and poll time.
func (h *Handler) newsURLFor(ctx context.Context) string {
	return h.newsURLForBase(ctx, h.urlOrSecret(ctx, "NEWS_API_URL", h.newsURL))
}

// newsURLForRU mirrors newsURLFor for the Russian-language upstream.
func (h *Handler) newsURLForRU(ctx context.Context) string {
	return h.newsURLForBase(ctx, h.urlOrSecret(ctx, "NEWS_API_URL_RU", h.newsURLRU))
}

// guardRedirects is the CheckRedirect for the aggregation HTTP client. A
// public URL that redirects into a private network would bypass the pre-fetch
// host check, so every hop onto a non-public address is rejected and the
// chain length is capped.
func guardRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if req.URL.Hostname() == "" {
		return nil
	}
	if netguard.PrivateHost(req.Context(), strings.Trim(req.URL.Hostname(), "[]")) {
		return errors.New("upstream redirect to a private address blocked")
	}
	return nil
}

// urlOrSecret resolves a Redis secret of name, falling back to the compiled
// default. Secret values are checked before use: a value pointed at an internal
// address would turn the poller into an SSRF probe, so non-public hosts are
// rejected and the default is used instead.
func (h *Handler) urlOrSecret(ctx context.Context, name, fallback string) string {
	v := h.getSecret(ctx, name)
	if v == "" {
		return fallback
	}
	if !h.isPublicUpstream(ctx, v) {
		h.logger.Warn("news: upstream from secret rejected as non-public", "name", name)
		return fallback
	}
	return v
}

// isPublicUpstream reports whether an upstream URL resolves to public
// addresses only (fail-closed on parse/DNS errors). This keeps the news
// poller — whose upstream a Redis secret can redirect — from ever contacting
// private networks.
func (h *Handler) isPublicUpstream(ctx context.Context, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return false
	}
	if u.User != nil {
		return false
	}
	return !netguard.PrivateHost(ctx, strings.Trim(u.Hostname(), "[]"))
}

// newsURLForBase merges the shared params (apiKey, the /everything-only `from`
// filter) into any news upstream URL.
func (h *Handler) newsURLForBase(ctx context.Context, base string) string {
	params := url.Values{}
	// `from` is an /everything-only filter; top-headlines ignores it. Match on
	// the parsed path rather than a substring so query strings can't fool it.
	if u, err := url.Parse(base); err == nil && strings.HasSuffix(u.Path, "/everything") {
		params.Set("from", time.Now().Add(-maxNewsAge).Format("2006-01-02"))
	}
	k := h.getSecret(ctx, "NEWS_API_KEY")
	if k != "" {
		params.Set("apiKey", k)
	}
	return addQuery(base, params)
}

// currencyAmount matches a MAIN_CURRENCY value with an optional amount
// prefix: "RUB", "100RUB", "100 RUB" or "12.5 EUR".
var currencyAmount = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([A-Za-z]{3})$`)

// parseCurrency splits a MAIN_CURRENCY value into an ISO 4217 code and a base
// amount: "100RUB" means rates are shown per 100 units, plain "RUB" per 1.
// Empty or invalid input falls back to USD with amount 1.
func parseCurrency(v string) (code string, amount float64) {
	v = strings.TrimSpace(v)
	if m := currencyAmount.FindStringSubmatch(v); m != nil {
		if a, err := strconv.ParseFloat(m[1], 64); err == nil && a > 0 {
			return strings.ToUpper(m[2]), a
		}
	}
	return strings.ToUpper(v), 1
}

// baseCurrency resolves MAIN_CURRENCY with the same USD fallback as the
// poller, so the dashboard always reads the exact snapshot key the poller
// writes.
func (h *Handler) baseCurrency(ctx context.Context) (code string, amount float64) {
	code, amount = parseCurrency(h.getSecret(ctx, "MAIN_CURRENCY"))
	if code == "" {
		code = "USD"
	}
	return code, amount
}

func (h *Handler) ratesURLFor(ctx context.Context) string {
	code, _ := h.baseCurrency(ctx)
	return h.ratesURL + "/" + url.PathEscape(code)
}

// addQuery merges params into base's existing query string. An unparseable
// base is returned unchanged.
func addQuery(base string, params url.Values) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// parseLocation accepts a "lat,lon" location string. Invalid values — empty,
// unparseable, NaN/Inf or out-of-range — fall back to the default (Moscow):
// a bad location must never be sent upstream.
func parseLocation(location string) (float64, float64) {
	lat, lon := 55.7558, 37.6173
	if parts := strings.Split(location, ","); len(parts) == 2 {
		f := func(s string) (float64, bool) {
			v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				return 0, false
			}
			return v, true
		}
		if v, ok := f(parts[0]); ok {
			lat = v
		}
		if v, ok := f(parts[1]); ok {
			lon = v
		}
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 55.7558, 37.6173
	}
	return lat, lon
}

// ---- snapshot keys: one Redis key per (location, currency) so a changed
// secret picks up fresh data on the next poll while old keys self-expire. ----

const dashKeyPrefix = "dash:"

func dashKey(namespace string, parts ...string) string {
	sum := sha1.Sum([]byte(strings.Join(parts, "|")))
	return dashKeyPrefix + namespace + ":" + fmt.Sprintf("%x", sum[:8])
}

// weatherKey returns the snapshot key for a location pair.
func (h *Handler) weatherKey(lat, lon float64) string {
	return dashKey("weather", strconv.FormatFloat(lat, 'f', 4, 64), strconv.FormatFloat(lon, 'f', 4, 64))
}

// placeKey mirrors weatherKey for the reverse-geocoded place name.
func (h *Handler) placeKey(lat, lon float64) string {
	return dashKey("place", strconv.FormatFloat(lat, 'f', 6, 64), strconv.FormatFloat(lon, 'f', 6, 64))
}

// ratesKey returns the snapshot key for a base currency and amount (the amount
// is part of the key so "RUB" and "100RUB" never share cached values).
func (h *Handler) ratesKey(code string, amount float64) string {
	return dashKey("rates", code, strconv.FormatFloat(amount, 'f', -1, 64))
}

// newsError builds a newsapi-style error object, rendered as an error card on
// the dashboard for exhausted budgets and upstream key problems.
func newsError(code, message string) map[string]interface{} {
	return map[string]interface{}{
		"status":  "error",
		"code":    code,
		"message": message,
	}
}

// isKeyErrCode reports whether a newsapi error code means a missing/invalid
// API key.
func isKeyErrCode(code string) bool {
	switch code {
	case "apiKeyMissing", "apiKeyInvalid", "apiKeyDisabled", "apiKeyExhausted", "apiKeyMissingOrInvalid":
		return true
	}
	return false
}

// fetchBody performs an upstream GET with a bounded read and returns the body
// plus the HTTP status. Callers decide how to interpret the status code.
func (h *Handler) fetchBody(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// fetchStoreNews serves the news card from the news store alone. When the
// store is empty the last poll outcome is surfaced instead (missing key,
// exhausted quota, upstream failure), so error states survive without any
// on-request upstream call.
func (h *Handler) fetchStoreNews(ctx context.Context, col *collector) {
	h.fetchStoreNewsLang(ctx, col, false)
}

// fetchStoreNewsRU mirrors fetchStoreNews for the Russian-language store,
// filling the newsRu dashboard field.
func (h *Handler) fetchStoreNewsRU(ctx context.Context, col *collector) {
	h.fetchStoreNewsLang(ctx, col, true)
}

func (h *Handler) fetchStoreNewsLang(ctx context.Context, col *collector, ru bool) {
	defer col.wg.Done()

	field := "news"
	store := h.newsStore
	st := h.currentNewsStatus()
	if ru {
		field = "newsRu"
		store = h.newsStoreRU
		st = h.currentNewsStatusRU()
	}
	if store != nil {
		if all, err := store.All(ctx); err != nil {
			h.logger.Warn("dashboard: read news history", "lang", field, "err", err)
		} else if recent := filterRecentArticles(all); len(recent) > 0 {
			col.set(field, newsResponseData(recent))
			return
		}
	}
	switch {
	case st.missingKey:
		col.set(field, newsError("apiKeyMissing", "Your API key is missing"))
	case st.exhausted:
		col.set(field, newsError(quota.ExhaustedCode, quota.ExhaustedMessage))
	case st.code != "":
		col.set(field, newsError(st.code, st.message))
	}
}

// pollSnapshot refreshes the weather, place and rates snapshot keys in Redis,
// on the same cycle as the news polls. Failures keep the previous value (the
// keys live on with their TTL) and are logged, never fatal.
func (h *Handler) pollSnapshot(ctx context.Context) {
	if h.kv == nil {
		return
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); h.pollWeather(ctx) }()
	go func() { defer wg.Done(); h.pollPlace(ctx) }()
	go func() { defer wg.Done(); h.pollRates(ctx) }()
	wg.Wait()
}

// snapshotTTL bounds how long a snapshot key survives without a refresh: two
// poll intervals, floored at 2h and capped at 48h so a misconfigured huge
// interval can't pin stale data forever.
func (h *Handler) snapshotTTL(ctx context.Context) time.Duration {
	ttl := 2 * h.pollInterval(ctx)
	if ttl < 2*time.Hour {
		ttl = 2 * time.Hour
	}
	if ttl > 48*time.Hour {
		ttl = 48 * time.Hour
	}
	return ttl
}

func (h *Handler) setSnapshot(ctx context.Context, key, value string) {
	if h.kv == nil || value == "" {
		return
	}
	if err := h.kv.Set(ctx, key, value, h.snapshotTTL(ctx)); err != nil {
		h.logger.Warn("news: store snapshot", "key", key, "err", err)
	}
}

// pollWeather stores the open-meteo snapshot for the configured location. An
// error payload ({"error":true,...} with a 2xx status) is treated like any
// other failed poll so it never gets cached as a valid snapshot.
func (h *Handler) pollWeather(ctx context.Context) {
	lat, lon := parseLocation(h.getSecret(ctx, "WEATHER_LOCATION"))
	body, status, err := h.fetchBody(ctx, h.weatherURLFor(lat, lon))
	if err != nil || status < 200 || status >= 300 {
		h.logger.Warn("dashboard: weather poll failed", "status", status, "err", err)
		return
	}
	var m struct {
		Error bool `json:"error"`
	}
	if err := json.Unmarshal(body, &m); err != nil || m.Error {
		h.logger.Warn("dashboard: weather poll returned an error payload", "err", err)
		return
	}
	h.setSnapshot(ctx, h.weatherKey(lat, lon), string(body))
}

func (h *Handler) pollPlace(ctx context.Context) {
	lat, lon := parseLocation(h.getSecret(ctx, "WEATHER_LOCATION"))
	body, status, err := h.fetchBody(ctx, h.placeURLFor(lat, lon))
	if err != nil || status < 200 || status >= 300 {
		h.logger.Warn("dashboard: place poll failed", "status", status, "err", err)
		return
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		h.logger.Warn("dashboard: place poll returned invalid JSON")
		return
	}
	place := ""
	for _, k := range []string{"city", "locality", "principalSubdivision"} {
		if s, _ := m[k].(string); s != "" {
			place = s
			break
		}
	}
	if place == "" {
		return
	}
	if b, err := json.Marshal(place); err == nil {
		h.setSnapshot(ctx, h.placeKey(lat, lon), string(b))
	}
}

func (h *Handler) pollRates(ctx context.Context) {
	code, amount := h.baseCurrency(ctx)
	body, status, err := h.fetchBody(ctx, h.ratesURLFor(ctx))
	if err != nil || status < 200 || status >= 300 {
		h.logger.Warn("dashboard: rates poll failed", "status", status, "err", err)
		return
	}
	if scaled, err := scaleRates(body, amount); err == nil {
		h.setSnapshot(ctx, h.ratesKey(code, amount), scaled)
	} else {
		h.logger.Warn("dashboard: rates poll invalid JSON")
	}
}

// scaleRates multiplies every rate in an upstream exchange-rate payload by
// amount (e.g. 100 for MAIN_CURRENCY=100RUB) and injects the amount so the UI
// can label it. amount <= 1 leaves the payload untouched.
func scaleRates(raw []byte, amount float64) (string, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	if amount > 1 {
		if rates, ok := m["rates"].(map[string]interface{}); ok {
			for code, rate := range rates {
				if f, ok := rate.(float64); ok {
					rates[code] = f * amount
				}
			}
		}
		m["amount"] = amount
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// pollNews fetches one fresh newsapi page into the English store, stores any
// new articles and records the outcome for the dashboard. It is the only news
// fetch outside the /news route, so each poll consumes exactly one unit of the
// daily quota.
func (h *Handler) pollNews(ctx context.Context) {
	h.pollNewsLang(ctx, false)
}

// pollNewsRU mirrors pollNews for the Russian-language store.
func (h *Handler) pollNewsRU(ctx context.Context) {
	h.pollNewsLang(ctx, true)
}

func (h *Handler) pollNewsLang(ctx context.Context, ru bool) {
	// RU polling is optional: without a RU store there is nothing to persist.
	if ru && h.newsStoreRU == nil {
		return
	}

	url := h.newsURLFor(ctx)
	store := h.newsStore
	setStatus := h.setNewsStatus
	if ru {
		url = h.newsURLForRU(ctx)
		store = h.newsStoreRU
		setStatus = h.setNewsStatusRU
	}

	st := newsStatus{}
	if h.newsQuota != nil {
		allowed, err := h.newsQuota.Allow(ctx)
		if err != nil {
			// Fail closed on a broken quota backend: with the budget unknown
			// the poll must not spend it blindly — an overshoot would exhaust
			// the daily NewsAPI quota for everyone.
			h.logger.Warn("news: poll skipped, quota check failed", "ru", ru, "err", err)
			return
		}
		if !allowed {
			st.exhausted = true
			st.code = quota.ExhaustedCode
			st.message = quota.ExhaustedMessage
			setStatus(st)
			h.logger.Info("news: poll skipped, daily quota exhausted")
			return
		}
	}

	body, status, err := h.fetchBody(ctx, url)
	if err != nil {
		h.logger.Warn("news: poll fetch failed", "ru", ru, "err", err)
		st.code = "upstreamError"
		st.message = "news upstream fetch failed"
		setStatus(st)
		return
	}

	var data struct {
		Status   string              `json:"status"`
		Code     string              `json:"code"`
		Message  string              `json:"message"`
		Articles []newsstore.Article `json:"articles"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		// A non-2xx body that isn't newsapi JSON (e.g. a gateway HTML page) is
		// an upstream failure, not news.
		if status < 200 || status >= 300 {
			st.message = "news upstream returned " + http.StatusText(status)
		} else {
			st.message = "news upstream returned invalid JSON"
		}
		st.code = "upstreamError"
		setStatus(st)
		return
	}

	if data.Status == "error" {
		st.code = data.Code
		st.message = data.Message
		st.missingKey = isKeyErrCode(data.Code)
		setStatus(st)
		return
	}

	if status < 200 || status >= 300 {
		h.logger.Warn("news: poll upstream returned error status", "ru", ru, "status", status)
		st.code = "upstreamError"
		st.message = "news upstream returned " + http.StatusText(status)
		setStatus(st)
		return
	}

	if store != nil {
		if err := store.Store(ctx, data.Articles); err != nil {
			h.logger.Warn("news: store articles", "ru", ru, "err", err)
		}
	}
	setStatus(newsStatus{})
	h.logger.Info("news: poll stored articles", "ru", ru, "count", len(data.Articles))
}

// Run polls the upstreams on a fixed cycle: news (EN, and RU when a store is
// configured) plus the weather/place/rates snapshot, all stored in Redis so
// the dashboard serves everything without spending the daily quota or
// upstream budget per request. The interval is re-resolved each cycle (e.g.
// from a NEWS_POLL_INTERVAL secret); the first poll runs immediately.
func (h *Handler) Run(ctx context.Context) {
	if h.newsStore == nil && h.kv == nil {
		h.logger.Warn("news: poller disabled, no store configured")
		return
	}
	interval := h.pollInterval(ctx)
	h.logger.Info("news: poller started", "interval", interval)
	h.pollAll(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			h.logger.Info("news: poller stopped")
			return
		case <-ticker.C:
			interval = h.pollInterval(ctx)
			ticker.Reset(interval)
			h.pollAll(ctx)
		}
	}
}

// pollAll refreshes both language stores and the weather/rates snapshot
// concurrently. Each news language still consumes one unit of the shared
// daily quota via its own Allow call.
func (h *Handler) pollAll(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); h.pollNews(ctx) }()
	go func() { defer wg.Done(); h.pollNewsRU(ctx) }()
	go func() { defer wg.Done(); h.pollSnapshot(ctx) }()
	wg.Wait()
}

// PollNow refreshes the news stores and the weather/place/rates snapshots in
// the background, outside the scheduled cycle. The API Secrets UI calls it
// after a data-affecting secret changes so the dashboard reflects the new
// value immediately. It is fire-and-forget and safe to run concurrently with
// the scheduled cycle.
func (h *Handler) PollNow() {
	h.logger.Info("news: immediate refresh triggered")
	go h.pollAll(context.Background())
}

// filterRecentArticles drops articles older than maxNewsAge so the card stays
// focused on current headlines. Articles whose date can't be parsed are kept
// and treated as fresh (several upstreams, e.g. lenta, omit publishedAt).
func filterRecentArticles(articles []newsstore.Article) []newsstore.Article {
	cutoff := time.Now().Add(-maxNewsAge)
	out := make([]newsstore.Article, 0, len(articles))
	for _, a := range articles {
		t, err := time.Parse(time.RFC3339, a.PublishedAt)
		if err != nil {
			out = append(out, a)
			continue
		}
		if t.Before(cutoff) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// newsResponseData wraps a stored article list in a newsapi-shaped response
// so the frontend renderer keeps working unchanged.
func newsResponseData(articles []newsstore.Article) map[string]interface{} {
	return map[string]interface{}{
		"status":       "ok",
		"totalResults": len(articles),
		"articles":     articles,
	}
}

// collector merges concurrent Redis snapshot reads into a single DashboardData.
type collector struct {
	mu  sync.Mutex
	wg  sync.WaitGroup
	res *DashboardData
}

// set writes field's value under the mutex. "place" takes the pre-extracted
// locality string from the snapshot read.
func (c *collector) set(field string, v interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch field {
	case "weather":
		c.res.Weather = v
	case "news":
		c.res.News = v
	case "newsRu":
		c.res.NewsRU = v
	case "rates":
		c.res.Rates = v
	case "place":
		if s, ok := v.(string); ok && s != "" {
			c.res.WeatherPlace = s
		}
	}
}
