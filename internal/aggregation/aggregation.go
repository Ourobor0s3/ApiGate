package aggregation

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ourobor0s3/ApiGate/internal/newsstore"
	"github.com/Ourobor0s3/ApiGate/internal/quota"
)

const (
	// DefaultWeatherURL and DefaultNewsURL are the upstream bases used when
	// no WEATHER_API_URL / NEWS_API_URL override is set.
	DefaultWeatherURL = "https://api.open-meteo.com/v1/forecast"
	// DefaultNewsURL pulls the day's top headlines from a curated list of
	// major, verified international sources instead of everything matching a
	// keyword, so the news card shows popular mainstream news. Cap pageSize at
	// 50 so the poller pulls a bounded payload; articles are persisted in the
	// news store and replayed across requests.
	DefaultNewsURL = "https://newsapi.org/v2/top-headlines?sources=bbc-news,cnn,reuters,associated-press,abc-news,nbc-news,cbs-news,al-jazeera-english,dw,the-guardian-uk,france-24,independent&pageSize=50"
	// DefaultNewsURLRU feeds the dashboard's Russian mode with Russian-language
	// headlines. country=ru and language=ru return empty articles on the
	// NewsAPI free plan, but the sources feed works (verified:
	// sources=lenta,rbc,rt returns the day's Russian headlines).
	DefaultNewsURLRU = "https://newsapi.org/v2/top-headlines?sources=lenta,rbc,rt&pageSize=50"

	defaultPlaceURL = "https://api.bigdatacloud.net/data/reverse-geocode-client"
	defaultRatesURL = "https://api.exchangerate-api.com/v4/latest"
	defaultTimeout  = 10 * time.Second
	// maxNewsAge is the oldest publication date shown in the news card; the
	// /everything upstream request also passes a matching `from` filter.
	maxNewsAge = 48 * time.Hour
	// maxBodyBytes caps a single upstream response so a misbehaving or
	// compromised source can't balloon dashboard memory. All four upstreams
	// return well under 1 MiB.
	maxBodyBytes = 1 << 20
	// DefaultPollInterval is how often the background poller refreshes the
	// news store from the upstream. The poll is the only dashboard-side news
	// fetch, so the NewsAPI daily quota lasts all day instead of being spent
	// by dashboard refreshes.
	DefaultPollInterval = 30 * time.Minute
)

type DashboardData struct {
	Weather        interface{} `json:"weather,omitempty"`
	WeatherPlace   string      `json:"weatherPlace,omitempty"`
	News           interface{} `json:"news,omitempty"`
	// NewsRU carries the Russian-language headline store; the UI picks the
	// block matching its language. Omitted when the RU poller/store are not
	// configured or still empty.
	NewsRU         interface{} `json:"newsRu,omitempty"`
	Rates          interface{} `json:"rates,omitempty"`
	MissingSecrets []string    `json:"missingSecrets,omitempty"`
	Error          string      `json:"error,omitempty"`
}

// Handler aggregates weather, news and currency rates for /dashboard.
// getSecret resolves a setting by name (e.g. "NEWS_API_KEY", "WEATHER_LOCATION",
// "MAIN_CURRENCY") at request time; returning "" falls back to built-in defaults.
// News is never fetched per request: /dashboard serves the stored history and
// the background poller (Run) refills it on a schedule.
type Handler struct {
	httpClient   *http.Client
	logger       *slog.Logger
	getSecret    func(context.Context, string) string
	newsQuota    NewsQuota
	newsStore    NewsStore
	newsStoreRU  NewsStore
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
// is shared between the /news route and the dashboard news block. A nil quota
// disables the check.
type NewsQuota interface {
	Allow(ctx context.Context) (bool, error)
}

// NewsStore persists and replays newsapi articles so the dashboard can show
// accumulated history instead of only the latest page. A nil store skips
// persistence and the fresh page is returned as-is.
type NewsStore interface {
	Store(ctx context.Context, articles []newsstore.Article) error
	All(ctx context.Context) ([]newsstore.Article, error)
}

// newsStatus records the outcome of the latest background news poll so the
// dashboard can surface key, quota and upstream problems without hitting the
// upstream itself. The zero value means the last poll succeeded.
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

// WithNewsPollInterval sets how often the background poller refreshes the news
// store from the upstream. The resolver is re-invoked each cycle, so a changed
// secret takes effect on the next poll. Defaults to DefaultPollInterval.
func WithNewsPollInterval(f func(context.Context) time.Duration) Option {
	return func(h *Handler) { h.pollInterval = f }
}

func New(getSecret func(context.Context, string) string, opts ...Option) *Handler {
	h := &Handler{
		httpClient:   &http.Client{Timeout: 8 * time.Second},
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

	// News never hits the upstream on a request: it is served from the store
	// and refreshed only by the background poller, so auto/manual refreshes
	// fetch weather and rates live while news stays free of the daily quota.
	col.wg.Add(5)
	go h.fetchJSON(ctx, col, "weather", h.weatherURLFor(lat, lon))
	go h.fetchJSON(ctx, col, "place", h.placeURLFor(lat, lon))
	go h.fetchJSON(ctx, col, "rates", h.ratesURLFor(ctx))
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

func (h *Handler) weatherURLFor(lat, lon float64) string {
	return h.addQueryOr(h.weatherURL, url.Values{
		"latitude":        []string{strconv.FormatFloat(lat, 'f', 4, 64)},
		"longitude":       []string{strconv.FormatFloat(lon, 'f', 4, 64)},
		"current_weather": []string{"true"},
		// Without timezone=auto open-meteo reports "GMT" and UTC times; with it
		// the response carries the location's real IANA zone (e.g.
		// "Europe/Moscow") and times in that zone's wall clock. The dashboard
		// uses the zone to render news/check times and treats the weather time
		// as a wall clock, so everything lines up with the location instead of
		// drifting into GMT.
		"timezone": []string{"auto"},
	})
}

func (h *Handler) placeURLFor(lat, lon float64) string {
	return h.addQueryOr(h.placeURL, url.Values{
		"latitude":         []string{strconv.FormatFloat(lat, 'f', 6, 64)},
		"longitude":        []string{strconv.FormatFloat(lon, 'f', 6, 64)},
		"localityLanguage": []string{"en"},
	})
}

func (h *Handler) newsURLFor(ctx context.Context) string {
	return h.newsURLForBase(ctx, h.newsURL)
}

func (h *Handler) newsURLForRU(ctx context.Context) string {
	return h.newsURLForBase(ctx, h.newsURLRU)
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
	return h.addQueryOr(base, params)
}

func (h *Handler) ratesURLFor(ctx context.Context) string {
	base := strings.ToUpper(strings.TrimSpace(h.getSecret(ctx, "MAIN_CURRENCY")))
	if base == "" {
		base = "USD"
	}
	return h.ratesURL + "/" + url.PathEscape(base)
}

// addQueryOr returns base with params merged in, or base unchanged when the
// URL is unparseable.
func (h *Handler) addQueryOr(base string, params url.Values) string {
	u, err := addQuery(base, params)
	if err != nil {
		return base
	}
	return u
}

// addQuery merges params into base's existing query string, preserving any
// params already present on the base URL.
func addQuery(base string, params url.Values) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// parseLocation accepts a "lat,lon" location string; invalid or empty values
// fall back to Moscow (55.7558, 37.6173).
func parseLocation(location string) (float64, float64) {
	lat, lon := 55.7558, 37.6173
	if parts := strings.Split(location, ","); len(parts) == 2 {
		if v, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64); err == nil {
			lat = v
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil {
			lon = v
		}
	}
	return lat, lon
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

// isKeyErrCode reports whether a newsapi error code indicates a missing or
// invalid API key.
func isKeyErrCode(code string) bool {
	switch code {
	case "apiKeyMissing", "apiKeyInvalid", "apiKeyDisabled", "apiKeyExhausted", "apiKeyMissingOrInvalid":
		return true
	}
	return false
}

// isKeyError reports whether an upstream error object indicates a missing or
// invalid API key (e.g. newsapi's {"status":"error","code":"apiKeyMissing"}).
func isKeyError(v interface{}) bool {
	m, ok := v.(map[string]interface{})
	if !ok || m["status"] != "error" {
		return false
	}
	code, _ := m["code"].(string)
	return isKeyErrCode(code)
}

// fetchBody performs an upstream GET with a bounded read and returns the body
// bytes plus the HTTP status. Errors cover URL construction, transport failures
// and body reads; callers decide how to interpret the status code.
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

func (h *Handler) fetchJSON(ctx context.Context, col *collector, field, url string) {
	defer col.wg.Done()

	body, status, err := h.fetchBody(ctx, url)
	if err != nil {
		h.logger.Warn("dashboard: fetch failed", "field", field, "err", err)
		return
	}
	if status < 200 || status >= 300 {
		h.logger.Warn("dashboard: upstream returned error status", "field", field, "status", status)
		return
	}

	var data interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		h.logger.Warn("dashboard: unmarshal response", "field", field, "status", status, "err", err)
		return
	}

	col.set(field, data)
}

// fetchStoreNews serves the news card from the news store alone. When the
// store is empty the last poll outcome is surfaced instead (missing key,
// exhausted quota, upstream failure), so error states survive without any
// on-request upstream call.
func (h *Handler) fetchStoreNews(ctx context.Context, col *collector) {
	h.fetchStoreNewsLang(ctx, col, false)
}

// fetchStoreNewsRU mirrors fetchStoreNews for the Russian-language store,
// filling the newsRu dashboard field (omitted when the RU poller is not
// configured).
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

// pollNews fetches one fresh newsapi page into the English store, stores any
// new articles and records the outcome for the dashboard. It is the only news
// fetch outside the /news route, so each poll consumes exactly one unit of the
// daily quota.
func (h *Handler) pollNews(ctx context.Context) {
	h.pollNewsLang(ctx, false)
}

// pollNewsRU fetches the Russian-language page (sources=lenta,rbc,rt) into the
// RU store. The poller alternates between the two languages, so adding Russian
// news does not grow the daily quota: still one upstream call per interval.
func (h *Handler) pollNewsRU(ctx context.Context) {
	h.pollNewsLang(ctx, true)
}

func (h *Handler) pollNewsLang(ctx context.Context, ru bool) {
	// RU polling is optional: without a RU store there is nothing to persist
	// and no dashboard field to fill, so skip the fetch (and the quota unit).
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
			// the poll must not spend it blindly — a poller overshoot would
			// exhaust the daily NewsAPI quota for everyone. Skip this cycle,
			// keep the last good status, and let the next cycle retry.
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

// Run polls the news upstream on a fixed cycle and stores new articles, so the
// dashboard serves news from Redis without spending the daily quota on every
// refresh. With a Russian-language store configured (WithNewsStoreRU) the two
// languages alternate on consecutive cycles — English, Russian, English, ...
// — so both stay fresh while the daily budget still burns only one request per
// interval (each language refreshes every two intervals). The interval is
// re-resolved each cycle (e.g. from a NEWS_POLL_INTERVAL secret); the first
// poll runs immediately.
func (h *Handler) Run(ctx context.Context) {
	ru := h.newsStoreRU != nil
	if h.newsStore == nil && !ru {
		h.logger.Warn("news: poller disabled, no store configured")
		return
	}
	interval := h.pollInterval(ctx)
	h.logger.Info("news: poller started", "interval", interval, "languages", pollerLangs(ru))
	h.pollNews(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	phaseRu := true
	for {
		select {
		case <-ctx.Done():
			h.logger.Info("news: poller stopped")
			return
		case <-ticker.C:
			interval = h.pollInterval(ctx)
			ticker.Reset(interval)
			if ru && phaseRu {
				h.pollNewsRU(ctx)
			} else {
				h.pollNews(ctx)
			}
			phaseRu = !phaseRu
		}
	}
}

func pollerLangs(ru bool) string {
	if ru {
		return "en+ru"
	}
	return "en"
}

// ParseInterval converts a Go duration string (e.g. "30m", "6m 30s") into a
// poll interval, falling back to DefaultPollInterval on empty or invalid input.
func ParseInterval(v string) time.Duration {
	if d, err := time.ParseDuration(strings.Join(strings.Fields(v), "")); err == nil && d > 0 {
		return d
	}
	return DefaultPollInterval
}

// filterRecentArticles drops articles older than maxNewsAge so the card stays
// focused on current headlines even when the store still holds older entries.
// Articles whose date can't be parsed are kept and treated as fresh: the store
// already scores them as "just now" (publishedScore), and several upstreams
// (e.g. lenta) omit publishedAt entirely — dropping them would hide real
// headlines for the sake of an unknown timestamp.
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

// newsResponseData wraps a stored article list in a newsapi-shaped response so
// the frontend renderer keeps working unchanged.
func newsResponseData(articles []newsstore.Article) map[string]interface{} {
	return map[string]interface{}{
		"status":       "ok",
		"totalResults": len(articles),
		"articles":     articles,
	}
}

// collector merges concurrent upstream fetches into a single DashboardData.
type collector struct {
	mu  sync.Mutex
	wg  sync.WaitGroup
	res *DashboardData
}

// set writes field's value under the mutex. The "place" field is special: it
// stores the first non-empty locality string instead of the raw JSON object.
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
		if m, ok := v.(map[string]interface{}); ok {
			for _, k := range []string{"city", "locality", "principalSubdivision"} {
				if s, _ := m[k].(string); s != "" {
					c.res.WeatherPlace = s
					break
				}
			}
		}
	}
}
