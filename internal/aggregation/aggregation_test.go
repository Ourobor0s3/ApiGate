package aggregation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
	weather := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"weather": "ok"})
	}))
	defer weather.Close()

	place := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"city": "Rome"})
	}))
	defer place.Close()

	newsHit := false
	news := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		newsHit = true
	}))
	defer news.Close()

	rates := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"base":  "USD",
			"rates": map[string]interface{}{"EUR": 0.9},
		})
	}))
	defer rates.Close()

	h := New(
		func(context.Context, string) string { return "" },
		WithHTTPClient(weather.Client()),
		WithWeatherURL(weather.URL),
		WithPlaceURL(place.URL),
		WithNewsURL(news.URL),
		WithRatesURL(rates.URL),
		WithNewsQuota(denyQuota{}),
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))

	var data DashboardData
	if err := json.NewDecoder(rec.Body).Decode(&data); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if newsHit {
		t.Error("news upstream hit despite exhausted quota")
	}
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
	weather := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"weather": "ok"})
	}))
	defer weather.Close()

	place := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"city": "Rome"})
	}))
	defer place.Close()

	newsHit := false
	news := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		newsHit = true
		json.NewEncoder(w).Encode(map[string]interface{}{"articles": []interface{}{}})
	}))
	defer news.Close()

	rates := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"base":  "USD",
			"rates": map[string]interface{}{"EUR": 0.9},
		})
	}))
	defer rates.Close()

	h := New(
		func(context.Context, string) string { return "" },
		WithHTTPClient(weather.Client()),
		WithWeatherURL(weather.URL),
		WithPlaceURL(place.URL),
		WithNewsURL(news.URL),
		WithRatesURL(rates.URL),
		WithNewsQuota(allowQuota{}),
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))

	var data DashboardData
	if err := json.NewDecoder(rec.Body).Decode(&data); err != nil {
		t.Fatalf("decode response: %v", err)
	}
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

	h := New(
		func(context.Context, string) string { return "" },
		WithHTTPClient(fail.Client()),
		WithWeatherURL(fail.URL),
		WithPlaceURL(fail.URL),
		WithNewsURL(fail.URL),
		WithRatesURL(fail.URL),
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))

	var data DashboardData
	if err := json.NewDecoder(rec.Body).Decode(&data); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if data.Error != "all upstream services failed" {
		t.Errorf("Error = %q, want %q", data.Error, "all upstream services failed")
	}
	if len(data.MissingSecrets) != 0 {
		t.Errorf("MissingSecrets = %v, want none", data.MissingSecrets)
	}
}
