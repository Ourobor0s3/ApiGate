package cache

import (
	"net/http"
	"reflect"
	"testing"
)

func TestCacheKey(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "http://example.com/weather?lat=1&lon=2", nil)
	if got, want := cacheKey(r), "cache:GET:/weather?lat=1&lon=2"; got != want {
		t.Errorf("cacheKey() = %q, want %q", got, want)
	}

	r, _ = http.NewRequest(http.MethodGet, "http://example.com/news", nil)
	if got, want := cacheKey(r), "cache:GET:/news"; got != want {
		t.Errorf("cacheKey() = %q, want %q", got, want)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	resp := &cachedResponse{
		Status: 200,
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Custom":     []string{"a", "b"},
		},
		Body: []byte(`{"hello":"world"}`),
	}

	got, err := decodeResponse(encodeResponse(resp))
	if err != nil {
		t.Fatalf("decodeResponse() error: %v", err)
	}
	if got.Status != resp.Status {
		t.Errorf("Status = %d, want %d", got.Status, resp.Status)
	}
	if !reflect.DeepEqual(got.Headers, resp.Headers) {
		t.Errorf("Headers = %v, want %v", got.Headers, resp.Headers)
	}
	if string(got.Body) != string(resp.Body) {
		t.Errorf("Body = %q, want %q", got.Body, resp.Body)
	}
}

func TestShouldCache(t *testing.T) {
	c := New(nil, Config{NoCachePaths: []string{"/dashboard", "/api/secrets", "/"}})

	for _, path := range []string{"/dashboard", "/api/secrets", "/"} {
		if c.shouldCache(path) {
			t.Errorf("shouldCache(%q) = true, want false", path)
		}
	}
	for _, path := range []string{"/weather", "/news", "/dashboard/v2", "/api/secrets/v2"} {
		if !c.shouldCache(path) {
			t.Errorf("shouldCache(%q) = false, want true", path)
		}
	}
}
