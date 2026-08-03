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
)

const (
	defaultWeatherURL = "https://api.open-meteo.com/v1/forecast"
	defaultNewsURL    = "https://newsapi.org/v2/top-headlines?country=us"
	defaultPlaceURL   = "https://api.bigdatacloud.net/data/reverse-geocode-client"
	defaultRatesURL   = "https://api.exchangerate-api.com/v4/latest"
	defaultTimeout    = 10 * time.Second
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
	weatherURL string
	newsURL    string
	placeURL   string
	ratesURL   string
}

type Option func(*Handler)

func WithHTTPClient(c *http.Client) Option {
	return func(h *Handler) { h.httpClient = c }
}

func WithLogger(l *slog.Logger) Option {
	return func(h *Handler) { h.logger = l }
}

func WithWeatherURL(u string) Option {
	return func(h *Handler) { h.weatherURL = u }
}

func WithNewsURL(u string) Option {
	return func(h *Handler) { h.newsURL = u }
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
		weatherURL: defaultWeatherURL,
		newsURL:    defaultNewsURL,
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

	var mu sync.Mutex
	result := &DashboardData{}
	var wg sync.WaitGroup

	lat, lon := parseLocation(h.getSecret(ctx, "WEATHER_LOCATION"))

	wg.Add(4)
	go h.fetchJSON(ctx, &mu, &wg, result, "weather", h.weatherURLFor(lat, lon))
	go h.fetchJSON(ctx, &mu, &wg, result, "place", h.placeURLFor(lat, lon))
	go h.fetchJSON(ctx, &mu, &wg, result, "news", h.newsURLFor(ctx))
	go h.fetchJSON(ctx, &mu, &wg, result, "rates", h.ratesURLFor(ctx))
	wg.Wait()

	if isKeyError(result.News) {
		result.MissingSecrets = append(result.MissingSecrets, "NEWS_API_KEY")
	}

	if result.Weather == nil && result.News == nil && result.Rates == nil {
		result.Error = "all upstream services failed"
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(result)
}

func (h *Handler) weatherURLFor(lat, lon float64) string {
	u, err := addQuery(h.weatherURL, url.Values{
		"latitude":        []string{strconv.FormatFloat(lat, 'f', 4, 64)},
		"longitude":       []string{strconv.FormatFloat(lon, 'f', 4, 64)},
		"current_weather": []string{"true"},
	})
	if err != nil {
		return h.weatherURL
	}
	return u
}

func (h *Handler) placeURLFor(lat, lon float64) string {
	u, err := addQuery(h.placeURL, url.Values{
		"latitude":         []string{strconv.FormatFloat(lat, 'f', 6, 64)},
		"longitude":        []string{strconv.FormatFloat(lon, 'f', 6, 64)},
		"localityLanguage": []string{"en"},
	})
	if err != nil {
		return h.placeURL
	}
	return u
}

func (h *Handler) newsURLFor(ctx context.Context) string {
	if k := h.getSecret(ctx, "NEWS_API_KEY"); k != "" {
		if u, err := addQuery(h.newsURL, url.Values{"apiKey": []string{k}}); err == nil {
			return u
		}
	}
	return h.newsURL
}

func (h *Handler) ratesURLFor(ctx context.Context) string {
	base := strings.ToUpper(strings.TrimSpace(h.getSecret(ctx, "MAIN_CURRENCY")))
	if base == "" {
		base = "USD"
	}
	return h.ratesURL + "/" + url.PathEscape(base)
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

func (h *Handler) fetchJSON(ctx context.Context, mu *sync.Mutex, wg *sync.WaitGroup, dst *DashboardData, field, url string) {
	defer wg.Done()

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

	body, err := io.ReadAll(resp.Body)
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

	mu.Lock()
	switch field {
	case "weather":
		dst.Weather = data
	case "news":
		dst.News = data
	case "rates":
		if m, ok := data.(map[string]interface{}); ok {
			dst.Rates = m
		}
	case "place":
		if m, ok := data.(map[string]interface{}); ok {
			for _, k := range []string{"city", "locality", "principalSubdivision"} {
				if s, _ := m[k].(string); s != "" {
					dst.WeatherPlace = s
					break
				}
			}
		}
	}
	mu.Unlock()
}
