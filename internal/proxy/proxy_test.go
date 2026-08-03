package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewSkipsEmptyRoutes(t *testing.T) {
	p, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	rec := httptest.NewRecorder()
	p.Weather().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/weather", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("Weather() code = %d, want %d", rec.Code, http.StatusNotFound)
	}

	rec = httptest.NewRecorder()
	p.News().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/news", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("News() code = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestNewRejectsInvalidURL(t *testing.T) {
	if _, err := New(Config{WeatherAPI: "://bad"}); err == nil {
		t.Error("New() with invalid URL: expected error, got nil")
	}
}

func TestNewsAppendsAPIKey(t *testing.T) {
	var gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p, err := New(Config{
		NewsAPI:    upstream.URL,
		NewsAPIKey: func(context.Context) string { return "topsecret" },
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	rec := httptest.NewRecorder()
	p.News().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/news?country=us", nil))

	if !strings.Contains(gotQuery, "apiKey=topsecret") {
		t.Errorf("upstream query %q missing apiKey=topsecret", gotQuery)
	}
	if !strings.Contains(gotQuery, "country=us") {
		t.Errorf("upstream query %q dropped original params", gotQuery)
	}
}

func TestMergesTargetQueryParams(t *testing.T) {
	var gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p, err := New(Config{
		WeatherAPI: upstream.URL + "?current_weather=true",
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// Target param fills the gap when the request does not provide it.
	rec := httptest.NewRecorder()
	p.Weather().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/weather?latitude=55", nil))
	if gotQuery != "current_weather=true&latitude=55" {
		t.Errorf("merged query = %q, want current_weather=true&latitude=55", gotQuery)
	}

	// Request params win over target defaults.
	rec = httptest.NewRecorder()
	p.Weather().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/weather?latitude=55&current_weather=false", nil))
	if gotQuery != "current_weather=false&latitude=55" {
		t.Errorf("overridden query = %q, want current_weather=false&latitude=55", gotQuery)
	}
}
