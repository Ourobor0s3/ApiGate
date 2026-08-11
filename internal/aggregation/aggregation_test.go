package aggregation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ourobor0s3/ApiGate/internal/newsstore"
)

func TestParseLocation(t *testing.T) {
	cases := map[string]struct{ wantLat, wantLon float64 }{
		"":               {55.7558, 37.6173},
		"garbage":        {55.7558, 37.6173},
		"10.5,20.25":     {10.5, 20.25},
		" 10.5 , 20.25 ": {10.5, 20.25},
		"10.5,bad":       {10.5, 37.6173},
		// NaN, Inf and out-of-range coordinates are never sent upstream: an
		// invalid part falls back to the default, and a mix that lands outside
		// real-world ranges falls back entirely.
		"NaN,10":  {55.7558, 10},
		"10,+Inf": {10, 37.6173},
		"90.5,10": {55.7558, 37.6173},
		"10,181":  {55.7558, 37.6173},
	}
	for in, want := range cases {
		lat, lon := parseLocation(in)
		if lat != want.wantLat || lon != want.wantLon {
			t.Errorf("parseLocation(%q) = (%v, %v), want (%v, %v)", in, lat, lon, want.wantLat, want.wantLon)
		}
	}
}

func TestParseCurrency(t *testing.T) {
	cases := map[string]struct {
		wantCode   string
		wantAmount float64
	}{
		"":         {"", 1},
		"EUR":      {"EUR", 1},
		" rub ":    {"RUB", 1},
		"100RUB":   {"RUB", 100},
		"100 RUB":  {"RUB", 100},
		"12.5 EUR": {"EUR", 12.5},
		"0":        {"0", 1},
		"garbage":  {"GARBAGE", 1},
		"RUB 100":  {"RUB 100", 1},
	}
	for in, want := range cases {
		code, amount := parseCurrency(in)
		if code != want.wantCode || amount != want.wantAmount {
			t.Errorf("parseCurrency(%q) = (%q, %v), want (%q, %v)", in, code, amount, want.wantCode, want.wantAmount)
		}
	}
}

func TestServeHTTPRatesScaled(t *testing.T) {
	h := testDashboard(t, withSecret("MAIN_CURRENCY", "100RUB"))
	h.pollSnapshot(context.Background())
	data := decodeDash(t, h)

	m, ok := data.Rates.(map[string]interface{})
	if !ok {
		t.Fatalf("Rates = %#v, want map", data.Rates)
	}
	if m["base"] != "USD" {
		t.Errorf("base = %v, want USD", m["base"])
	}
	if got, ok := m["amount"].(float64); !ok || got != 100 {
		t.Errorf("amount = %v, want 100", m["amount"])
	}
	if got, ok := m["rates"].(map[string]interface{})["EUR"].(float64); !ok || got != 90 {
		t.Errorf("EUR rate = %v, want 90", m["rates"].(map[string]interface{})["EUR"])
	}
}

func TestAddQueryPreservesExisting(t *testing.T) {
	got := addQuery("https://example.com/path?a=1", url.Values{"b": []string{"2"}})
	if !strings.Contains(got, "a=1") || !strings.Contains(got, "b=2") {
		t.Errorf("addQuery() = %q, want both a=1 and b=2", got)
	}
}

func TestFilterRecentArticles(t *testing.T) {
	recent := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	old := time.Now().Add(-72 * time.Hour).Format(time.RFC3339)
	got := filterRecentArticles([]newsstore.Article{
		{Title: "fresh", PublishedAt: recent},
		{Title: "stale", PublishedAt: old},
		{Title: "unknown date"},
	})
	// unknown-date articles count as fresh (mirroring publishedScore), so both
	// "fresh" and the undated headline survive the 48h cut.
	if len(got) != 2 || got[0].Title != "fresh" || got[1].Title != "unknown date" {
		t.Fatalf("filterRecentArticles() = %+v, want fresh + unknown-date kept, stale dropped", got)
	}
}

func TestServeHTTP(t *testing.T) {
	weather := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("current_weather") != "true" {
			t.Errorf("weather request missing current_weather param")
		}
		if q.Get("hourly") != "temperature_2m,weathercode,precipitation_probability" {
			t.Errorf("weather request missing hourly vars, got %q", q.Get("hourly"))
		}
		if q.Get("daily") != "sunrise,sunset" {
			t.Errorf("weather request missing daily vars, got %q", q.Get("daily"))
		}
		if q.Get("forecast_days") != "2" {
			t.Errorf("weather request must cover two days, got %q", q.Get("forecast_days"))
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
		WithKV(newMemKV()),
	)

	h.pollNews(context.Background())
	h.pollSnapshot(context.Background())

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
	m, ok := data.News.(map[string]interface{})
	if !ok || m["status"] != "error" || m["code"] != "apiKeyMissing" {
		t.Errorf("News = %v, want apiKeyMissing error object", data.News)
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
	h.pollNews(context.Background())
	h.pollSnapshot(context.Background())

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
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "ok",
			"articles": []interface{}{
				newsArticle("Fresh", "https://x.com/fresh", recentPublished(3)),
			},
		})
	}))
	defer news.Close()

	h := testDashboard(t, WithNewsURL(news.URL), WithNewsQuota(allowQuota{}), WithNewsStore(newMemStore()))
	h.pollNews(context.Background())

	data := decodeDash(t, h)
	if !newsHit {
		t.Error("news upstream not hit by the poller despite available quota")
	}
	arts := newsArticles(t, data)
	if len(arts) != 1 || arts[0]["title"] != "Fresh" {
		t.Errorf("News articles = %v, want the polled article", arts)
	}
}

func TestServeHTTPNewsUpstreamError(t *testing.T) {
	news := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "<html>Bad Gateway</html>")
	}))
	defer news.Close()

	h := testDashboard(t, WithNewsURL(news.URL))
	h.pollNews(context.Background())
	h.pollSnapshot(context.Background())

	data := decodeDash(t, h)
	m, ok := data.News.(map[string]interface{})
	if !ok || m["status"] != "error" || m["code"] != "upstreamError" {
		t.Errorf("News = %v, want upstreamError error object", data.News)
	}
	if len(data.MissingSecrets) != 0 {
		t.Errorf("MissingSecrets = %v, want none", data.MissingSecrets)
	}
	if data.Weather == nil {
		t.Error("Weather not populated")
	}
}

func TestServeHTTPNewsRUStoredSeparately(t *testing.T) {
	en := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "ok",
			"articles": []interface{}{
				newsArticle("English headline", "https://x.com/en", recentPublished(2)),
			},
		})
	}))
	defer en.Close()

	ru := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "ok",
			"articles": []interface{}{
				newsArticle("Русский заголовок", "https://x.com/ru", recentPublished(1)),
			},
		})
	}))
	defer ru.Close()

	enStore, ruStore := newMemStore(), newMemStore()
	h := testDashboard(t,
		WithNewsURL(en.URL),
		WithNewsStore(enStore),
		WithNewsURLRU(ru.URL),
		WithNewsStoreRU(ruStore),
	)

	h.pollNews(context.Background())
	h.pollNewsRU(context.Background())

	data := decodeDash(t, h)
	arts := newsArticles(t, data)
	if len(arts) != 1 || arts[0]["title"] != "English headline" {
		t.Fatalf("News articles = %v, want only the English headline", arts)
	}
	ruArts := newsArticlesRU(t, data)
	if len(ruArts) != 1 || ruArts[0]["title"] != "Русский заголовок" {
		t.Fatalf("NewsRu articles = %v, want only the Russian headline", ruArts)
	}
	if len(enStore.order) != 1 || len(ruStore.order) != 1 {
		t.Errorf("stores polluted: en=%d ru=%d articles, want 1 each", len(enStore.order), len(ruStore.order))
	}
}

func TestPollNewsRUSkippedWithoutStore(t *testing.T) {
	enHit, ruHit := false, false
	en := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enHit = true
	}))
	defer en.Close()
	ru := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ruHit = true
	}))
	defer ru.Close()

	h := testDashboard(t, WithNewsURL(en.URL), WithNewsURLRU(ru.URL))
	h.pollNewsRU(context.Background())

	if ruHit {
		t.Error("RU upstream hit even though no RU store is configured")
	}
	if enHit {
		t.Error("EN upstream unexpectedly hit by the RU poll")
	}
}

func newsArticlesRU(t *testing.T, data DashboardData) []map[string]interface{} {
	t.Helper()
	m, ok := data.NewsRU.(map[string]interface{})
	if !ok {
		t.Fatalf("NewsRU is %T, want map", data.NewsRU)
	}
	raw, ok := m["articles"].([]interface{})
	if !ok {
		t.Fatalf("newsRu.articles is %T, want []interface{}", m["articles"])
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

func TestPollNewsQuotaBackendErrorSkipsFetch(t *testing.T) {
	newsHit := false
	news := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		newsHit = true
	}))
	defer news.Close()

	store := newMemStore()
	store.Store(context.Background(), []newsstore.Article{{
		Title:       "Stored",
		URL:         "https://x.com/stored",
		PublishedAt: recentPublished(12),
		Source:      &newsstore.Source{Name: "Src"},
	}})

	h := testDashboard(t, WithNewsURL(news.URL), WithNewsQuota(errQuota{}), WithNewsStore(store))
	h.pollNews(context.Background())

	if newsHit {
		t.Error("news upstream hit despite an unknown (errored) quota — the poll must fail closed")
	}
	// The last good status is preserved, so the stored history keeps serving.
	arts := newsArticles(t, decodeDash(t, h))
	if len(arts) != 1 || arts[0]["title"] != "Stored" {
		t.Errorf("articles = %v, want the stored history", arts)
	}
}

type errQuota struct{}

func (errQuota) Allow(context.Context) (bool, error) { return false, errors.New("redis down") }

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
	h.pollNews(context.Background())

	data := decodeDash(t, h)
	// The news card reports its own failure instead of being folded into a
	// blanket "all upstream services failed" banner, so no top-level Error.
	if data.Error != "" {
		t.Errorf("Error = %q, want empty (news shows its own upstreamError card)", data.Error)
	}
	if len(data.MissingSecrets) != 0 {
		t.Errorf("MissingSecrets = %v, want none", data.MissingSecrets)
	}
	m, ok := data.News.(map[string]interface{})
	if !ok || m["status"] != "error" || m["code"] != "upstreamError" {
		t.Errorf("News = %v, want upstreamError error object", data.News)
	}
}

func TestPollNewsSecretURLBlockedWhenPrivate(t *testing.T) {
	privateHit := false
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		privateHit = true
	}))
	defer private.Close()

	safeHit := false
	safe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		safeHit = true
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "articles": []interface{}{}})
	}))
	defer safe.Close()

	// "NEWS_API_URL" points at a loopback endpoint via the secrets layer — an
	// operator could turn the poller into an SSRF probe that way; the guard
	// must reject the private upstream and fall back to the compiled default.
	h := New(
		func(ctx context.Context, name string) string {
			if name == "NEWS_API_URL" {
				return private.URL
			}
			return ""
		},
		WithNewsURL(safe.URL),
		WithHTTPClient(safe.Client()),
		WithNewsStore(newMemStore()),
	)
	h.pollNews(context.Background())

	if privateHit {
		t.Error("private (loopback) news upstream was contacted — SSRF guard failed")
	}
	if !safeHit {
		t.Error("fallback news upstream not used after the private URL was rejected")
	}
}

// TestServeHTTPNeverHitsUpstreams proves /dashboard is served purely from the
// Redis-backed stores: after one poll, repeated requests must not contact the
// weather/place/rates/news upstreams at all.
func TestServeHTTPNeverHitsUpstreams(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "ok",
			"articles": []interface{}{
				newsArticle("Fresh", "https://x.com/fresh", recentPublished(1)),
			},
		})
	}))
	defer upstream.Close()

	h := New(
		func(ctx context.Context, name string) string { return "" },
		WithHTTPClient(upstream.Client()),
		WithWeatherURL(upstream.URL),
		WithPlaceURL(upstream.URL),
		WithRatesURL(upstream.URL),
		WithNewsURL(upstream.URL),
		WithNewsStore(newMemStore()),
		WithNewsStoreRU(newMemStore()),
		WithKV(newMemKV()),
	)

	// One full poll cycle warms news + weather + place + rates in the stores.
	h.pollAll(context.Background())

	mu.Lock()
	before := hits
	mu.Unlock()
	if before == 0 {
		t.Fatal("first poll did not hit the upstreams")
	}

	for i := 0; i < 5; i++ {
		decodeDash(t, h)
	}
	mu.Lock()
	after := hits
	mu.Unlock()
	if after != before {
		t.Errorf("dashboard requests hit upstreams: hits %d -> %d, want unchanged", before, after)
	}
}

func TestServeHTTPRatesWithoutCurrency(t *testing.T) {
	// With no MAIN_CURRENCY configured the poller stores the snapshot under
	// the USD key; the dashboard must compute the same key, or the rates card
	// would stay empty on the default config.
	h := testDashboard(t)
	h.pollSnapshot(context.Background())
	data := decodeDash(t, h)

	m, ok := data.Rates.(map[string]interface{})
	if !ok {
		t.Fatalf("Rates = %#v, want map", data.Rates)
	}
	if m["base"] != "USD" {
		t.Errorf("base = %v, want USD", m["base"])
	}
	if _, ok := m["rates"].(map[string]interface{}); !ok {
		t.Errorf("rates = %v, want map", m["rates"])
	}
}

// TestPollNowRefreshesSnapshots checks that PollNow kicks off a full
// out-of-cycle refresh (weather + place + rates snapshots and the news poll)
// instead of waiting for the next scheduled cycle.
func TestPollNowRefreshesSnapshots(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer upstream.Close()

	h := New(
		func(context.Context, string) string { return "" },
		WithHTTPClient(upstream.Client()),
		WithWeatherURL(upstream.URL),
		WithPlaceURL(upstream.URL),
		WithRatesURL(upstream.URL),
		WithNewsURL(upstream.URL),
		WithNewsURLRU(upstream.URL),
		WithNewsStore(newMemStore()),
		WithNewsStoreRU(newMemStore()),
		WithKV(newMemKV()),
	)

	h.PollNow()

	// pollAll = news EN + place + weather + rates (RU store also configured;
	// RU poll fires its own request). Wait for the fetches to land.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := hits
		mu.Unlock()
		if n >= 5 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	n := hits
	mu.Unlock()
	t.Fatalf("PollNow did not trigger a full refresh within 5s: hits = %d, want >= 5", n)
}

func TestPollNewsRedirectToPrivateBlocked(t *testing.T) {
	privateHit := false
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		privateHit = true
	}))
	defer inner.Close()

	// A public-looking upstream that redirects into the private network: the
	// pre-fetch host check passes for the outer URL, so only the redirect
	// guard can stop the fetch from reaching the inner server.
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL, http.StatusFound)
	}))
	defer redirector.Close()

	h := New(
		func(ctx context.Context, name string) string { return "" },
		WithNewsURL(redirector.URL),
		WithNewsStore(newMemStore()),
	)
	h.pollNews(context.Background())

	if privateHit {
		t.Error("redirect reached a private address — SSRF redirect guard failed")
	}
	data := decodeDash(t, h)
	m, ok := data.News.(map[string]interface{})
	if !ok || m["status"] != "error" || m["code"] != "upstreamError" {
		t.Errorf("News = %v, want upstreamError object after the blocked redirect", data.News)
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

func recentPublished(hoursAgo int) string {
	return time.Now().Add(-time.Duration(hoursAgo) * time.Hour).Format(time.RFC3339)
}

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
			newsArticle("A", "https://x.com/a", recentPublished(40)),
			newsArticle("B", "https://x.com/b", recentPublished(20)),
		}
		if call > 1 {
			// Second poll overlaps with the first: B returns again and must
			// not duplicate or overwrite, while C is brand new.
			articles = []interface{}{
				newsArticle("B", "https://x.com/b", recentPublished(20)),
				newsArticle("C", "https://x.com/c", recentPublished(5)),
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "articles": articles})
	}))
	defer news.Close()

	store := newMemStore()
	h := testDashboard(t, WithNewsURL(news.URL), WithNewsStore(store))

	h.pollNews(context.Background())
	arts := newsArticles(t, decodeDash(t, h))
	if len(arts) != 2 {
		t.Fatalf("after first poll: %d articles, want 2", len(arts))
	}

	h.pollNews(context.Background())
	arts = newsArticles(t, decodeDash(t, h))
	if len(arts) != 3 {
		t.Fatalf("after overlapping poll: %d articles, want 3", len(arts))
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
		PublishedAt: recentPublished(12),
		Source:      &newsstore.Source{Name: "Src"},
	}})

	h := testDashboard(t, WithNewsURL(news.URL), WithNewsQuota(denyQuota{}), WithNewsStore(store))
	h.pollNews(context.Background())

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

func TestServeHTTPMissingKeyServesStored(t *testing.T) {
	news := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "error",
			"code":    "apiKeyMissing",
			"message": "Your API key is missing",
		})
	}))
	defer news.Close()

	store := newMemStore()
	store.Store(context.Background(), []newsstore.Article{{
		Title:       "Old",
		URL:         "https://x.com/old",
		PublishedAt: recentPublished(10),
		Source:      &newsstore.Source{Name: "Src"},
	}})

	h := testDashboard(t, WithNewsURL(news.URL), WithNewsStore(store))
	h.pollNews(context.Background())

	data := decodeDash(t, h)
	arts := newsArticles(t, data)
	if len(arts) != 1 || arts[0]["title"] != "Old" {
		t.Errorf("articles = %v, want stored history", arts)
	}
	if len(data.MissingSecrets) != 1 || data.MissingSecrets[0] != "NEWS_API_KEY" {
		t.Errorf("MissingSecrets = %v, want [NEWS_API_KEY]", data.MissingSecrets)
	}
}

func TestRunPollsImmediately(t *testing.T) {
	hit := make(chan struct{}, 10)
	news := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- struct{}{}
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "articles": []interface{}{}})
	}))
	defer news.Close()

	store := newMemStore()
	h := testDashboard(t,
		WithNewsURL(news.URL),
		WithNewsStore(store),
		WithNewsPollInterval(func(context.Context) time.Duration { return time.Hour }),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.Run(ctx)
		close(done)
	}()

	select {
	case <-hit:
	case <-time.After(5 * time.Second):
		t.Fatal("poller did not run its first poll on startup")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("poller did not stop on context cancel")
	}
}

func TestRunPollsBothLanguages(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	upstream := func(name string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[name]++
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "articles": []interface{}{}})
		}))
		t.Cleanup(s.Close)
		return s
	}
	en := upstream("en")
	ru := upstream("ru")

	h := testDashboard(t,
		WithNewsURL(en.URL),
		WithNewsStore(newMemStore()),
		WithNewsURLRU(ru.URL),
		WithNewsStoreRU(newMemStore()),
		WithNewsPollInterval(func(context.Context) time.Duration { return time.Hour }),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.Run(ctx)
		close(done)
	}()
	defer func() { cancel(); <-done }()

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		got := len(hits)
		mu.Unlock()
		if got == 2 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("first poll did not hit both upstreams, hits: %v", hits)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// withSecret returns an Option that wraps the handler's secret getter so the
// named secret reports value, letting a test override a single secret while
// keeping the wired getter.
func withSecret(name, value string) Option {
	return func(h *Handler) {
		prev := h.getSecret
		h.getSecret = func(ctx context.Context, n string) string {
			if n == name {
				return value
			}
			return prev(ctx, n)
		}
	}
}

// testDashboard builds a Handler backed by throwaway httptest servers for the
// weather/place/rates upstreams and an in-memory snapshot store, plus any
// extra options. The news upstream is provided by the caller via WithNewsURL.
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
			WithKV(newMemKV()),
		}, opts...)...,
	)
}

// memKV is an in-memory Store for the dashboard snapshots, keyed like the
// Redis-backed store without expiry semantics (snapshotTTL is not enforced).
type memKV struct {
	mu     sync.Mutex
	values map[string]string
}

func newMemKV() *memKV {
	return &memKV{values: map[string]string{}}
}

func (m *memKV) Get(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.values[key], nil
}

func (m *memKV) Set(_ context.Context, key, value string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[key] = value
	return nil
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
