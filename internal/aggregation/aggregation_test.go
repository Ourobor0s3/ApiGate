package aggregation

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Ourobor0s3/ApiGate/internal/newsstore"
)

func TestParseLocation(t *testing.T) {
	cases := map[string]struct{ wantLat, wantLon float64 }{
		"":               {55.7558, 37.6173},
		"garbage":        {55.7558, 37.6173},
		"10.5,20.25":     {10.5, 20.25},
		" 10.5 , 20.25 ": {10.5, 20.25},
		"10.5,bad":       {10.5, 37.6173},
	}
	for in, want := range cases {
		lat, lon := parseLocation(in)
		if lat != want.wantLat || lon != want.wantLon {
			t.Errorf("parseLocation(%q) = (%v, %v), want (%v, %v)", in, lat, lon, want.wantLat, want.wantLon)
		}
	}
}

func TestIsKeyError(t *testing.T) {
	if !isKeyError(map[string]interface{}{"status": "error", "code": "apiKeyMissing"}) {
		t.Error("apiKeyMissing not detected")
	}
	if isKeyError(map[string]interface{}{"status": "error", "code": "other"}) {
		t.Error("unrelated error code detected as key error")
	}
	if isKeyError(map[string]interface{}{"status": "ok"}) {
		t.Error("ok status detected as key error")
	}
	if isKeyError("not a map") {
		t.Error("non-map detected as key error")
	}
}

func TestAddQueryPreservesExisting(t *testing.T) {
	got, err := addQuery("https://example.com/path?a=1", url.Values{"b": []string{"2"}})
	if err != nil {
		t.Fatalf("addQuery() error: %v", err)
	}
	if !strings.Contains(got, "a=1") || !strings.Contains(got, "b=2") {
		t.Errorf("addQuery() = %q, want both a=1 and b=2", got)
	}
}

func TestServeHTTP(t *testing.T) {
	weather := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("current_weather") != "true" {
			t.Errorf("weather request missing current_weather param")
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"latitude": 10.0,
			"weather":  "ok",
		})
	}))
	defer weather.Close()

	place := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"city": "Rome"})
	}))
	defer place.Close()

	news := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if k := r.URL.Query().Get("apiKey"); k != "" {
			t.Errorf("news request should not include apiKey, got %q", k)
		}
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "error",
			"code":    "apiKeyMissing",
			"message": "Your API key is missing",
		})
	}))
	defer news.Close()

	rates := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"base":  "EUR",
			"rates": map[string]interface{}{"USD": 1.1, "GBP": 0.85},
		})
	}))
	defer rates.Close()

	h := New(
		func(ctx context.Context, name string) string {
			switch name {
			case "WEATHER_LOCATION":
				return "10,20"
			case "MAIN_CURRENCY":
				return "EUR"
			default:
				return ""
			}
		},
		WithHTTPClient(weather.Client()),
		WithWeatherURL(weather.URL),
		WithPlaceURL(place.URL),
		WithNewsURL(news.URL),
		WithRatesURL(rates.URL),
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("ServeHTTP() code = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	var data DashboardData
	if err := json.NewDecoder(rec.Body).Decode(&data); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if data.Weather == nil {
		t.Error("Weather not populated")
	}
	if data.WeatherPlace != "Rome" {
		t.Errorf("WeatherPlace = %q, want Rome", data.WeatherPlace)
	}
	if data.News == nil {
		t.Error("News not populated")
	}
	if data.Rates == nil {
		t.Error("Rates not populated")
	}
	if len(data.MissingSecrets) != 1 || data.MissingSecrets[0] != "NEWS_API_KEY" {
		t.Errorf("MissingSecrets = %v, want [NEWS_API_KEY]", data.MissingSecrets)
	}
	if data.Error != "" {
		t.Errorf("Error = %q, want empty", data.Error)
	}
}

func TestServeHTTPNewsQuotaExhausted(t *testing.T) {
	news := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("news upstream hit despite exhausted quota")
	}))
	defer news.Close()

	h := testDashboard(t, WithNewsURL(news.URL), WithNewsQuota(denyQuota{}))

	data := decodeDash(t, h)
	m, ok := data.News.(map[string]interface{})
	if !ok || m["status"] != "error" || m["code"] != "dailyQuotaExhausted" {
		t.Errorf("News = %v, want dailyQuotaExhausted error object", data.News)
	}
	if data.Error != "" {
		t.Errorf("Error = %q, want empty", data.Error)
	}
	if data.Weather == nil {
		t.Error("Weather not populated")
	}
}

func TestServeHTTPNewsQuotaAllowed(t *testing.T) {
	newsHit := false
	news := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		newsHit = true
		json.NewEncoder(w).Encode(map[string]interface{}{"articles": []interface{}{}})
	}))
	defer news.Close()

	h := testDashboard(t, WithNewsURL(news.URL), WithNewsQuota(allowQuota{}))

	data := decodeDash(t, h)
	if !newsHit {
		t.Error("news upstream not hit despite available quota")
	}
	if data.News == nil {
		t.Error("News not populated")
	}
}

type denyQuota struct{}

func (denyQuota) Allow(context.Context) (bool, error) { return false, nil }

type allowQuota struct{}

func (allowQuota) Allow(context.Context) (bool, error) { return true, nil }

func TestServeHTTPAllFailures(t *testing.T) {
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer fail.Close()

	h := testDashboard(t, WithWeatherURL(fail.URL), WithPlaceURL(fail.URL), WithNewsURL(fail.URL), WithRatesURL(fail.URL))

	data := decodeDash(t, h)
	if data.Error != "all upstream services failed" {
		t.Errorf("Error = %q, want %q", data.Error, "all upstream services failed")
	}
	if len(data.MissingSecrets) != 0 {
		t.Errorf("MissingSecrets = %v, want none", data.MissingSecrets)
	}
}

// newsArticle is a minimal newsapi article for building upstream fixtures.
func newsArticle(title, url, publishedAt string) map[string]interface{} {
	return map[string]interface{}{
		"title":       title,
		"url":         url,
		"publishedAt": publishedAt,
		"source":      map[string]interface{}{"name": "Src"},
	}
}

// newsArticles decodes the articles array from a served DashboardData.
func newsArticles(t *testing.T, data DashboardData) []map[string]interface{} {
	t.Helper()
	m, ok := data.News.(map[string]interface{})
	if !ok {
		t.Fatalf("News is %T, want map", data.News)
	}
	raw, ok := m["articles"].([]interface{})
	if !ok {
		t.Fatalf("news.articles is %T, want []interface{}", m["articles"])
	}
	var out []map[string]interface{}
	for _, a := range raw {
		am, ok := a.(map[string]interface{})
		if !ok {
			t.Fatalf("article is %T, want map", a)
		}
		out = append(out, am)
	}
	return out
}

// memStore is an in-memory NewsStore mimicking the dedup + newest-first
// behavior of the Redis-backed store, for tests that don't need Redis.
type memStore struct {
	mu    sync.Mutex
	data  map[string]newsstore.Article
	order []string
}

func newMemStore() *memStore {
	return &memStore{data: map[string]newsstore.Article{}}
}

func (m *memStore) Store(_ context.Context, arts []newsstore.Article) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range arts {
		if a.URL == "" {
			continue
		}
		if _, ok := m.data[a.URL]; ok {
			continue
		}
		m.data[a.URL] = a
		m.order = append(m.order, a.URL)
	}
	return nil
}

func (m *memStore) All(_ context.Context) ([]newsstore.Article, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]newsstore.Article, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		out = append(out, m.data[m.order[i]])
	}
	return out, nil
}

func TestServeHTTPNewsStoredAndDeduped(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	news := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		articles := []interface{}{
			newsArticle("A", "https://x.com/a", "2026-08-01T00:00:00Z"),
			newsArticle("B", "https://x.com/b", "2026-08-02T00:00:00Z"),
		}
		if call > 1 {
			// Second fetch overlaps with the first: B returns again and must
			// not duplicate or overwrite, while C is brand new.
			articles = []interface{}{
				newsArticle("B", "https://x.com/b", "2026-08-02T00:00:00Z"),
				newsArticle("C", "https://x.com/c", "2026-08-03T00:00:00Z"),
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "articles": articles})
	}))
	defer news.Close()

	store := newMemStore()
	h := testDashboard(t, WithNewsURL(news.URL), WithNewsStore(store))

	for i := 0; i < 2; i++ {
		arts := newsArticles(t, decodeDash(t, h))
		if len(arts) != i+2 {
			t.Fatalf("request %d: %d articles, want %d", i+1, len(arts), i+2)
		}
		want := []string{"B", "C"}
		if got := arts[0]["title"]; got != want[i] {
			t.Errorf("request %d: newest title = %v, want %s", i+1, got, want[i])
		}
	}

	arts := newsArticles(t, decodeDash(t, h))
	if len(arts) != 3 {
		t.Fatalf("after overlapping fetch: %d articles, want 3", len(arts))
	}
	want := []string{"C", "B", "A"}
	for i, w := range want {
		if got := arts[i]["title"]; got != w {
			t.Errorf("article %d title = %v, want %s", i, got, w)
		}
	}
}

func TestServeHTTPNewsQuotaExhaustedServesStored(t *testing.T) {
	newsHit := false
	news := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		newsHit = true
	}))
	defer news.Close()

	store := newMemStore()
	store.Store(context.Background(), []newsstore.Article{{
		Title:       "Stored",
		URL:         "https://x.com/stored",
		PublishedAt: "2026-08-02T00:00:00Z",
		Source:      &newsstore.Source{Name: "Src"},
	}})

	h := testDashboard(t, WithNewsURL(news.URL), WithNewsQuota(denyQuota{}), WithNewsStore(store))

	data := decodeDash(t, h)
	if newsHit {
		t.Error("news upstream hit despite exhausted quota")
	}
	arts := newsArticles(t, data)
	if len(arts) != 1 {
		t.Fatalf("articles = %d, want 1 from the store", len(arts))
	}
	if got := arts[0]["title"]; got != "Stored" {
		t.Errorf("stored article title = %v, want Stored", got)
	}
}

func TestServeHTTPNewsQuotaExhaustedEmptyStore(t *testing.T) {
	news := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("news upstream hit despite exhausted quota")
	}))
	defer news.Close()

	h := testDashboard(t, WithNewsURL(news.URL), WithNewsQuota(denyQuota{}), WithNewsStore(newMemStore()))

	data := decodeDash(t, h)
	m, ok := data.News.(map[string]interface{})
	if !ok || m["status"] != "error" || m["code"] != "dailyQuotaExhausted" {
		t.Errorf("News = %v, want dailyQuotaExhausted error object", data.News)
	}
}

// testDashboard builds a Handler backed by throwaway httptest servers for the
// weather/place/rates upstreams, plus any extra options. The news upstream is
// provided by the caller via WithNewsURL.
func testDashboard(t *testing.T, opts ...Option) *Handler {
	t.Helper()
	upstream := func(body string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		}))
		t.Cleanup(s.Close)
		return s
	}
	weather := upstream(`{"weather":"ok"}`)
	place := upstream(`{"city":"Rome"}`)
	rates := upstream(`{"base":"USD","rates":{"EUR":0.9}}`)
	return New(
		func(context.Context, string) string { return "" },
		append([]Option{
			WithHTTPClient(weather.Client()),
			WithWeatherURL(weather.URL),
			WithPlaceURL(place.URL),
			WithRatesURL(rates.URL),
		}, opts...)...,
	)
}

// decodeDash serves one dashboard request and returns the decoded payload.
func decodeDash(t *testing.T, h http.Handler) DashboardData {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	var data DashboardData
	if err := json.NewDecoder(rec.Body).Decode(&data); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return data
}
