package aggregation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var httpClient = &http.Client{Timeout: 8 * time.Second}

type DashboardData struct {
	Weather        interface{} `json:"weather,omitempty"`
	WeatherPlace   string      `json:"weatherPlace,omitempty"`
	News           interface{} `json:"news,omitempty"`
	Rates          interface{} `json:"rates,omitempty"`
	MissingSecrets []string    `json:"missingSecrets,omitempty"`
	Error          string      `json:"error,omitempty"`
}

// Handler serves the aggregated dashboard. getSecret resolves a setting by
// name (e.g. "NEWS_API_KEY", "WEATHER_LOCATION", "MAIN_CURRENCY") at request
// time; returning "" falls back to built-in defaults.
func Handler(getSecret func(context.Context, string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		var mu sync.Mutex
		result := &DashboardData{}
		var wg sync.WaitGroup

		lat, lon := parseLocation(getSecret(ctx, "WEATHER_LOCATION"))
		weatherURL := buildWeatherURL(lat, lon)
		placeURL := fmt.Sprintf("https://api.bigdatacloud.net/data/reverse-geocode-client?latitude=%f&longitude=%f&localityLanguage=en", lat, lon)
		newsURL := "https://newsapi.org/v2/top-headlines?country=us"
		if k := getSecret(ctx, "NEWS_API_KEY"); k != "" {
			newsURL += "&apiKey=" + url.QueryEscape(k)
		}
		currencyURL := buildCurrencyURL(getSecret(ctx, "MAIN_CURRENCY"))

		wg.Add(4)
		go fetchJSON(ctx, &mu, &wg, result, "weather", weatherURL)
		go fetchJSON(ctx, &mu, &wg, result, "place", placeURL)
		go fetchJSON(ctx, &mu, &wg, result, "news", newsURL)
		go fetchJSON(ctx, &mu, &wg, result, "rates", currencyURL)
		wg.Wait()

		if isKeyError(result.News) {
			result.MissingSecrets = append(result.MissingSecrets, "NEWS_API_KEY")
		}

		if result.Weather == nil && result.News == nil && result.Rates == nil {
			result.Error = "all upstream services failed"
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
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

func buildWeatherURL(lat, lon float64) string {
	return fmt.Sprintf("https://api.open-meteo.com/v1/forecast?latitude=%.4f&longitude=%.4f&current_weather=true", lat, lon)
}

// buildCurrencyURL accepts a 3-letter base currency code; empty falls back to USD.
func buildCurrencyURL(base string) string {
	if base = strings.ToUpper(strings.TrimSpace(base)); base == "" {
		base = "USD"
	}
	return "https://api.exchangerate-api.com/v4/latest/" + url.QueryEscape(base)
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

func fetchJSON(ctx context.Context, mu *sync.Mutex, wg *sync.WaitGroup, dst *DashboardData, field, url string) {
	defer wg.Done()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Printf("dashboard: invalid URL for %s: %v", field, err)
		return
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("dashboard: fetch %s failed: %v", field, err)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("dashboard: read %s body: %v", field, err)
		return
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("dashboard: %s returned status %d", field, resp.StatusCode)
		if field != "news" {
			// Only news bodies are parsed on failure: newsapi reports key
			// problems as a 200/4xx error object we need for MissingSecrets.
			return
		}
	}

	var data interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		log.Printf("dashboard: unmarshal %s (status %d): %v", field, resp.StatusCode, err)
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
