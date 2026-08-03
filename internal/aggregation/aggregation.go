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

	"github.com/Ourobor0s3/ApiGate/internal/quota"
)

const (
	// DefaultWeatherURL and DefaultNewsURL are the upstream bases used when
	// no WEATHER_API_URL / NEWS_API_URL override is set.
	DefaultWeatherURL = "https://api.open-meteo.com/v1/forecast"
	// DefaultNewsURL uses /v2/everything (popular English articles from all
	// countries, not a single country's top headlines), capped at 20 so the
	// dashboard doesn't pull the default 100-article payload.
	DefaultNewsURL = "https://newsapi.org/v2/everything?language=en&sortBy=popularity&pageSize=20"

	defaultPlaceURL = "https://api.bigdatacloud.net/data/reverse-geocode-client"
	defaultRatesURL = "https://api.exchangerate-api.com/v4/latest"
	defaultTimeout  = 10 * time.Second
	// maxBodyBytes caps a single upstream response so a misbehaving or
	// compromised source can't balloon dashboard memory. All four upstreams
	// return well under 1 MiB.
	maxBodyBytes = 1 << 20
)

type DashboardData struct {
	Weather        interface{} `json:"weather,omitempty"`
	WeatherPlace   string      `json:"weatherPlace,omitempty"`
	News           interface{} `json:"news,omitempty"`
	Rates          interface{} `json:"rates,omitempty"`
	MissingSecrets []string    `json:"missingSecrets,omitempty"`
	Error          string      `json:"error,omitempty"`
}

// Handler aggregates weather, news and currency rates for /dashboard.
// getSecret resolves a setting by name (e.g. "NEWS_API_KEY", "WEATHER_LOCATION",
// "MAIN_CURRENCY") at request time; returning "" falls back to built-in defaults.
type Handler struct {
	httpClient *http.Client
	logger     *slog.Logger
	getSecret  func(context.Context, string) string
	newsQuota  NewsQuota
	weatherURL string
	newsURL    string
	placeURL   string
	ratesURL   string
}

// NewsQuota gates upstream newsapi consumption so the daily free-plan budget
// is shared between the /news route and the dashboard news block. A nil quota
// disables the check.
type NewsQuota interface {
	Allow(ctx context.Context) (bool, error)
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

func WithNewsQuota(q NewsQuota) Option {
	return func(h *Handler) { h.newsQuota = q }
}

func WithPlaceURL(u string) Option {
	return func(h *Handler) { h.placeURL = u }
}

func WithRatesURL(u string) Option {
	return func(h *Handler) { h.ratesURL = u }
}

func New(getSecret func(context.Context, string) string, opts ...Option) *Handler {
	h := &Handler{
		httpClient: &http.Client{Timeout: 8 * time.Second},
		logger:     slog.Default(),
		getSecret:  getSecret,
		weatherURL: DefaultWeatherURL,
		newsURL:    DefaultNewsURL,
		placeURL:   defaultPlaceURL,
		ratesURL:   defaultRatesURL,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), defaultTimeout)
	defer cancel()

	col := &collector{res: &DashboardData{}}
	lat, lon := parseLocation(h.getSecret(ctx, "WEATHER_LOCATION"))

	fetchNews := true
	if h.newsQuota != nil {
		allowed, err := h.newsQuota.Allow(ctx)
		switch {
		case err != nil:
			// Fail open on a broken quota backend: the budget check is a
			// courtesy, not a hard dependency of the dashboard.
			h.logger.Warn("dashboard: news quota check failed", "err", err)
		case !allowed:
			fetchNews = false
			col.set("news", newsQuotaError())
		}
	}

	col.wg.Add(3)
	go h.fetchJSON(ctx, col, "weather", h.weatherURLFor(lat, lon))
	go h.fetchJSON(ctx, col, "place", h.placeURLFor(lat, lon))
	go h.fetchJSON(ctx, col, "rates", h.ratesURLFor(ctx))
	if fetchNews {
		col.wg.Add(1)
		go h.fetchJSON(ctx, col, "news", h.newsURLFor(ctx))
	}
	col.wg.Wait()

	res := col.res
	if isKeyError(res.News) {
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
	k := h.getSecret(ctx, "NEWS_API_KEY")
	if k == "" {
		return h.newsURL
	}
	return h.addQueryOr(h.newsURL, url.Values{"apiKey": []string{k}})
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

// newsQuotaError is a synthetic newsapi-style error object rendered as an
// error card on the dashboard when the daily news budget is exhausted.
func newsQuotaError() map[string]interface{} {
	return map[string]interface{}{
		"status":  "error",
		"code":    quota.ExhaustedCode,
		"message": quota.ExhaustedMessage,
	}
}

// isKeyError reports whether an upstream error object indicates a missing or
// invalid API key (e.g. newsapi's {"status":"error","code":"apiKeyMissing"}).
func isKeyError(v interface{}) bool {
	m, ok := v.(map[string]interface{})
	if !ok || m["status"] != "error" {
		return false
	}
	code, _ := m["code"].(string)
	switch code {
	case "apiKeyMissing", "apiKeyInvalid", "apiKeyDisabled", "apiKeyExhausted", "apiKeyMissingOrInvalid":
		return true
	}
	return false
}

func (h *Handler) fetchJSON(ctx context.Context, col *collector, field, url string) {
	defer col.wg.Done()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		h.logger.Warn("dashboard: invalid upstream URL", "field", field, "err", err)
		return
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		h.logger.Warn("dashboard: fetch failed", "field", field, "err", err)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		h.logger.Warn("dashboard: read body", "field", field, "err", err)
		return
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.logger.Warn("dashboard: upstream returned error status", "field", field, "status", resp.StatusCode)
		if field != "news" {
			// Only news bodies are parsed on failure: newsapi reports key
			// problems as a 200/4xx error object we need for MissingSecrets.
			return
		}
	}

	var data interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		h.logger.Warn("dashboard: unmarshal response", "field", field, "status", resp.StatusCode, "err", err)
		return
	}

	col.set(field, data)
}

// collector merges concurrent upstream fetches into a single DashboardData.
type collector struct {
	mu  sync.Mutex
	wg  sync.WaitGroup
	res *DashboardData
}

// set writes field's value under the mutex. The "rates" and "place" fields
// consume the raw JSON object instead of storing it wholesale.
func (c *collector) set(field string, v interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch field {
	case "weather":
		c.res.Weather = v
	case "news":
		c.res.News = v
	case "rates":
		if m, ok := v.(map[string]interface{}); ok {
			c.res.Rates = m
		}
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
