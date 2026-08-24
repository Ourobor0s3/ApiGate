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
	// NewsAPI's free plan serves articles with a roughly one-day ingestion
	// delay (verified empirically), and top-headlines rotates slowly on top
	// of that, so stored history is inherently day-old; pageSize=50 caps the
	// payload of a single poll.
	DefaultNewsURL = "https://newsapi.org/v2/top-headlines?sources=bbc-news,cnn,reuters,associated-press,abc-news,nbc-news,cbs-news,al-jazeera-english,dw,the-guardian-uk,france-24,independent&pageSize=50"
	// country=ru / language=ru return empty article lists on the free plan;
	// named sources are the verified working RU feed.
	DefaultNewsURLRU = "https://newsapi.org/v2/top-headlines?sources=lenta,rbc,rt,google-news-ru&pageSize=50"

	defaultPlaceURL = "https://api.bigdatacloud.net/data/reverse-geocode-client"
	defaultRatesURL = "https://api.exchangerate-api.com/v4/latest"
	defaultTimeout  = 10 * time.Second
	maxNewsAge      = 48 * time.Hour // card cutoff; also passed as `from` to /everything upstreams
	maxBodyBytes    = 1 << 20        // all four upstreams return well under 1 MiB
	// Two news requests per cycle at 60m = 48/day against the 100/day budget.
	DefaultPollInterval = 60 * time.Minute
)

// DashboardData is the aggregated /dashboard payload. Served entirely from
// Redis — a dashboard request never touches an upstream.
type DashboardData struct {
	Weather      interface{} `json:"weather,omitempty"`
	WeatherPlace string      `json:"weatherPlace,omitempty"`
	News         interface{} `json:"news,omitempty"`
	// NewsRU carries the Russian-language store; omitted from the JSON when
	// not configured or still empty. The UI picks the block matching its
	// language.
	NewsRU         interface{} `json:"newsRu,omitempty"`
	Rates          interface{} `json:"rates,omitempty"`
	MissingSecrets []string    `json:"missingSecrets,omitempty"`
	Error          string      `json:"error,omitempty"`
}

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

// NewsQuota gates upstream newsapi consumption shared by the /news route and
// the poller. Nil disables.
type NewsQuota interface {
	Allow(ctx context.Context) (bool, error)
}

type NewsStore interface {
	Store(ctx context.Context, articles []newsstore.Article) (int64, error)
	All(ctx context.Context) ([]newsstore.Article, error)
}

// Store is the narrow snapshot surface for weather/place/rates in Redis.
// Tests fake it.
type Store interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key string, value string, ttl time.Duration) error
}

// newsStatus records the outcome of the latest poll so the dashboard can
// surface key, quota and upstream problems without fetching per request.
// The zero value means success.
type newsStatus struct {
	missingKey bool
	exhausted  bool
	code       string // newsapi-style code, e.g. "upstreamError"
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

func WithNewsURLRU(u string) Option {
	return func(h *Handler) { h.newsURLRU = u }
}

func WithNewsQuota(q NewsQuota) Option {
	return func(h *Handler) { h.newsQuota = q }
}

func WithNewsStore(s NewsStore) Option {
	return func(h *Handler) { h.newsStore = s }
}

// Without a RU store the poller runs in single-language mode and /dashboard
// omits newsRu.
func WithNewsStoreRU(s NewsStore) Option {
	return func(h *Handler) { h.newsStoreRU = s }
}

func WithPlaceURL(u string) Option {
	return func(h *Handler) { h.placeURL = u }
}

func WithRatesURL(u string) Option {
	return func(h *Handler) { h.ratesURL = u }
}

func WithKV(s Store) Option {
	return func(h *Handler) { h.kv = s }
}

// The interval resolver is re-invoked every cycle, so a changed secret takes
// effect on the next poll.
func WithNewsPollInterval(f func(context.Context) time.Duration) Option {
	return func(h *Handler) { h.pollInterval = f }
}

func New(getSecret func(context.Context, string) string, opts ...Option) *Handler {
	h := &Handler{
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

	col.wg.Add(5)
	go h.serveKV(ctx, col, "weather", h.weatherKey(lat, lon))
	go h.serveKV(ctx, col, "place", h.placeKey(lat, lon))
	go h.serveKV(ctx, col, "rates", h.ratesKey(code, amount))
	go h.fetchStoreNews(ctx, col)
	go h.fetchStoreNewsRU(ctx, col)
	col.wg.Wait()

	res := col.res
	stEN, stRU := h.currentNewsStatus(), h.currentNewsStatusRU()
	if stEN.missingKey || stRU.missingKey {
		res.MissingSecrets = append(res.MissingSecrets, "NEWS_API_KEY")
	}
	if res.Weather == nil && res.News == nil && res.NewsRU == nil && res.Rates == nil {
		res.Error = "all upstream services failed"
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(res)
}

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
		// timezone=auto makes the dashboard render wall-clock times instead
		// of drifting into GMT.
		"timezone": []string{"auto"},
		"hourly":   []string{"temperature_2m,weathercode,precipitation_probability"},
		"daily":    []string{"sunrise,sunset"},
		// Two forecast days keep the hourly strip full past midnight
		// (open-meteo defaults to seven days: wasted payload and Redis TTL).
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

func (h *Handler) newsURLFor(ctx context.Context) string {
	return h.newsURLForBase(ctx, h.urlOrSecret(ctx, "NEWS_API_URL", h.newsURL))
}

func (h *Handler) newsURLForRU(ctx context.Context) string {
	return h.newsURLForBase(ctx, h.urlOrSecret(ctx, "NEWS_API_URL_RU", h.newsURLRU))
}

// guardRedirects keeps a public URL (which a Redis secret can set) from
// hopping into a private network through a redirect chain.
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

// urlOrSecret resolves a Redis secret with the compiled default as fallback,
// rejecting non-public hosts: otherwise a secret could turn the poller into
// an SSRF probe.
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

func (h *Handler) newsURLForBase(ctx context.Context, base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	// `from` is an /everything-only filter; match the parsed path rather than
	// a substring so query strings cannot fool it.
	if strings.HasSuffix(u.Path, "/everything") {
		q.Set("from", time.Now().Add(-maxNewsAge).Format("2006-01-02"))
	}
	if k := h.getSecret(ctx, "NEWS_API_KEY"); k != "" {
		// Set, never Add: a NEWS_API_URL secret may carry its own ?apiKey=,
		// and duplicate params would make the effective key ambiguous.
		q.Set("apiKey", k)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

var currencyAmount = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([A-Za-z]{3})$`)

// parseCurrency splits MAIN_CURRENCY into an ISO code and a base amount:
// "100RUB" shows every rate per 100 units. Invalid input falls back to USD x1.
func parseCurrency(v string) (code string, amount float64) {
	v = strings.TrimSpace(v)
	if m := currencyAmount.FindStringSubmatch(v); m != nil {
		if a, err := strconv.ParseFloat(m[1], 64); err == nil && a > 0 {
			return strings.ToUpper(m[2]), a
		}
	}
	return strings.ToUpper(v), 1
}

// baseCurrency mirrors the poller's resolution exactly, so the dashboard
// always reads the exact snapshot key the poller writes.
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

// parseLocation falls back to the default (Moscow) on any invalid input:
// NaN/Inf/out-of-range values must never reach an upstream.
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

// Snapshot keys hash their parts (location|currency): a changed secret starts
// writing a fresh key on the next poll while old keys self-expire by TTL.
const dashKeyPrefix = "dash:"

func dashKey(namespace string, parts ...string) string {
	sum := sha1.Sum([]byte(strings.Join(parts, "|")))
	return dashKeyPrefix + namespace + ":" + fmt.Sprintf("%x", sum[:8])
}

func (h *Handler) weatherKey(lat, lon float64) string {
	return dashKey("weather", strconv.FormatFloat(lat, 'f', 4, 64), strconv.FormatFloat(lon, 'f', 4, 64))
}

func (h *Handler) placeKey(lat, lon float64) string {
	return dashKey("place", strconv.FormatFloat(lat, 'f', 6, 64), strconv.FormatFloat(lon, 'f', 6, 64))
}

// The amount is part of the key so "RUB" and "100RUB" never share cached values.
func (h *Handler) ratesKey(code string, amount float64) string {
	return dashKey("rates", code, strconv.FormatFloat(amount, 'f', -1, 64))
}

func newsError(code, message string) map[string]interface{} {
	return map[string]interface{}{
		"status":  "error",
		"code":    code,
		"message": message,
	}
}

func isKeyErrCode(code string) bool {
	switch code {
	case "apiKeyMissing", "apiKeyInvalid", "apiKeyDisabled", "apiKeyExhausted", "apiKeyMissingOrInvalid":
		return true
	}
	return false
}

// fetchBody performs a GET with a bounded read; callers interpret the status.
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

func (h *Handler) fetchStoreNews(ctx context.Context, col *collector) {
	h.fetchStoreNewsLang(ctx, col, false)
}

func (h *Handler) fetchStoreNewsRU(ctx context.Context, col *collector) {
	h.fetchStoreNewsLang(ctx, col, true)
}

// fetchStoreNewsLang serves one news block from its store alone; when the
// store is empty, the last poll outcome (missing key, exhausted quota, failed
// upstream) is surfaced instead, so error states survive without any
// per-request fetches.
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

// Two poll intervals, floored at 2h and capped at 48h: a snapshot survives a
// missed cycle, yet a huge misconfigured interval cannot pin stale data
// forever.
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

// An open-meteo error payload ({"error":true,...} returned with a 2xx status)
// counts as a failed poll so it is never cached as a valid snapshot.
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

// scaleRates multiplies every rate by amount ("100RUB") and injects the
// amount for the UI label; amount <= 1 leaves the payload untouched.
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

func (h *Handler) pollNews(ctx context.Context) {
	h.pollNewsLang(ctx, false)
}

func (h *Handler) pollNewsRU(ctx context.Context) {
	h.pollNewsLang(ctx, true)
}

// pollNewsLang fetches one upstream page into its store; every call spends
// exactly one unit of the shared daily budget.
func (h *Handler) pollNewsLang(ctx context.Context, ru bool) {
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
			// Fail closed: with the budget unknown, the poll must not spend
			// it blindly — an overshoot would exhaust the daily quota for
			// everyone.
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
		added, err := store.Store(ctx, data.Articles)
		if err != nil {
			h.logger.Warn("news: store articles", "ru", ru, "err", err)
		} else {
			// added vs fetched shows feed turnover: the frozen pages served
			// by the free plan log added=0 cycle after cycle.
			h.logger.Info("news: poll", "ru", ru, "fetched", len(data.Articles), "added", added)
		}
	}
	setStatus(newsStatus{})
}

// Run polls everything on a fixed cycle, storing results in Redis so the
// dashboard never spends budget per request. The interval is re-resolved each
// cycle; the first poll runs immediately.
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

func (h *Handler) pollAll(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); h.pollNews(ctx) }()
	go func() { defer wg.Done(); h.pollNewsRU(ctx) }()
	go func() { defer wg.Done(); h.pollSnapshot(ctx) }()
	wg.Wait()
}

// PollNow is fire-and-forget and safe to run concurrently with the scheduled
// cycle; main triggers it after a data-affecting secret changes.
func (h *Handler) PollNow() {
	h.logger.Info("news: immediate refresh triggered")
	go h.pollAll(context.Background())
}

// Undated articles count as fresh: several upstreams (e.g. lenta) omit
// publishedAt entirely.
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

// newsResponseData wraps stored articles in a newsapi-shaped response so the
// frontend renderer keeps working unchanged.
func newsResponseData(articles []newsstore.Article) map[string]interface{} {
	return map[string]interface{}{
		"status":       "ok",
		"totalResults": len(articles),
		"articles":     articles,
	}
}

type collector struct {
	mu  sync.Mutex
	wg  sync.WaitGroup
	res *DashboardData
}

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
