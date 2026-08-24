package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodeBody must hand the raw representation downstream for every encoding a
// misbehaving upstream might declare, including raw-DEFLATE-masquerading-as-
// "deflate", and leave unknown encodings untouched.
func TestDecodeBody(t *testing.T) {
	plain := []byte("hello gateway")

	var gzBuf, zlBuf, rawBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	gw.Write(plain)
	gw.Close()
	zw := zlib.NewWriter(&zlBuf)
	zw.Write(plain)
	zw.Close()
	fw, _ := flate.NewWriter(&rawBuf, flate.DefaultCompression)
	fw.Write(plain)
	fw.Close()

	cases := []struct {
		name string
		enc  string
		body []byte
	}{
		{"gzip", "gzip", gzBuf.Bytes()},
		{"x-gzip", "x-gzip", gzBuf.Bytes()},
		{"zlib deflate", "deflate", zlBuf.Bytes()},
		{"raw deflate", "deflate", rawBuf.Bytes()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				Header:        http.Header{"Content-Encoding": []string{tc.enc}},
				Body:          io.NopCloser(bytes.NewReader(tc.body)),
				ContentLength: int64(len(tc.body)),
			}
			if err := decodeBody(resp); err != nil {
				t.Fatalf("decodeBody: %v", err)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("decoded %q err=%v, want %q", got, err, plain)
			}
			if resp.Header.Get("Content-Encoding") != "" {
				t.Error("Content-Encoding not removed")
			}
		})
	}

	// identity is stripped as a no-op marker; unknown encodings (br) pass
	// through with their header intact rather than being mislabeled.
	for enc, wantKept := range map[string]bool{"identity": false, "br": true} {
		resp := &http.Response{
			Header:        http.Header{"Content-Encoding": []string{enc}},
			Body:          io.NopCloser(bytes.NewReader(plain)),
			ContentLength: int64(len(plain)),
		}
		if err := decodeBody(resp); err != nil {
			t.Fatalf("%s: decodeBody: %v", enc, err)
		}
		got, _ := io.ReadAll(resp.Body)
		if !bytes.Equal(got, plain) {
			t.Errorf("%s: body changed to %q", enc, got)
		}
		if gotEnc := resp.Header.Get("Content-Encoding"); wantKept && gotEnc != enc {
			t.Errorf("%s: Content-Encoding = %q, want kept", enc, gotEnc)
		}
	}
}

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

// TestUpstreamGzipIsDecoded guards the raw-body contract: an upstream that
// ignores the Accept-Encoding: identity request and compresses anyway must not
// leak compressed bytes (labeled as nothing) into the cache or to clients.
func TestUpstreamGzipIsDecoded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		gz.Write([]byte(`{"ok":true}`))
		gz.Close()
	}))
	defer upstream.Close()

	p, err := New(Config{WeatherAPI: upstream.URL})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	rec := httptest.NewRecorder()
	p.Weather().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/weather", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != `{"ok":true}` {
		t.Errorf("body = %q, want decompressed %q", got, `{"ok":true}`)
	}
	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want empty (decoded upstream)", enc)
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
