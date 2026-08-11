package proxy

import (
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

type Config struct {
	WeatherAPI string
	NewsAPI    string
	// NewsAPIKey, if set, resolves an API key per request and appends it as
	// the `apiKey` query param on the /news route when one isn't already set.
	NewsAPIKey func(context.Context) string
	// Breaker, if set, wraps each upstream transport in a circuit breaker.
	Breaker BreakerConfig
}

// Proxy holds the upstream reverse proxies. Route registration is exact on the
// mux (GET /weather, GET /news), so no prefix matching is needed here.
type Proxy struct {
	weather *httputil.ReverseProxy
	news    *httputil.ReverseProxy
}

func New(cfg Config) (*Proxy, error) {
	p := &Proxy{}
	transport := newTransport()

	var newsParams map[string]func(context.Context) string
	if cfg.NewsAPIKey != nil {
		newsParams = map[string]func(context.Context) string{"apiKey": cfg.NewsAPIKey}
	}

	var err error
	if p.weather, err = newReverseProxy("/weather", cfg.WeatherAPI, nil, transport, cfg.Breaker); err != nil {
		return nil, err
	}
	if p.news, err = newReverseProxy("/news", cfg.NewsAPI, newsParams, transport, cfg.Breaker); err != nil {
		return nil, err
	}
	return p, nil
}

// Weather returns the /weather upstream handler, or http.NotFound when the
// route has no target URL (empty WEATHER_API_URL).
func (p *Proxy) Weather() http.Handler {
	return handlerOrNotFound(p.weather)
}

// News returns the /news upstream handler, or http.NotFound when the route has
// no target URL (empty NEWS_API_URL).
func (p *Proxy) News() http.Handler {
	return handlerOrNotFound(p.news)
}

func handlerOrNotFound(rp *httputil.ReverseProxy) http.Handler {
	if rp == nil {
		return http.HandlerFunc(http.NotFound)
	}
	return rp
}

// newReverseProxy builds a proxy for prefix; an empty target silently disables
// the route (returns nil, nil) so the mux falls back to http.NotFound.
func newReverseProxy(prefix, target string, params map[string]func(context.Context) string, transport http.RoundTripper, breaker BreakerConfig) (*httputil.ReverseProxy, error) {
	if target == "" {
		return nil, nil
	}
	targetURL, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("invalid target URL for %s: %w", prefix, err)
	}

	if breaker.FailureThreshold > 0 {
		transport = &breakerRoundTripper{next: transport, breaker: NewBreaker(breaker)}
	}

	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.URL.Path = targetURL.Path
			req.URL.RawPath = ""
			req.Host = targetURL.Host

			// The gateway applies compression itself (the Gzip middleware), so ask
			// the upstream for identity encoding. Otherwise the client's
			// Accept-Encoding: gzip would be forwarded upstream and the
			// compressed bytes it returns would be cached (and later
			// re-compressed) as if they were the raw body.
			req.Header.Set("Accept-Encoding", "identity")

			// Merge query params baked into the target URL (e.g. ?country=us),
			// letting the request's own params take precedence; then fill in any
			// param resolvers (the news apiKey). Encode once at the end.
			rq := req.URL.Query()
			for k, vs := range targetURL.Query() {
				if _, ok := rq[k]; !ok {
					rq[k] = vs
				}
			}
			for name, fn := range params {
				if rq.Get(name) != "" {
					continue
				}
				if v := fn(req.Context()); v != "" {
					rq.Set(name, v)
				}
			}
			req.URL.RawQuery = rq.Encode()
		},
		// An upstream that ignores the identity request would otherwise leak
		// a stale Content-Encoding into the body; every downstream layer
		// (cache, gzip) assumes the body is the raw representation, so any
		// compression is undone here.
		ModifyResponse: func(resp *http.Response) error {
			return decodeBody(resp)
		},
		Transport: transport,
	}, nil
}

// decodeBody unwraps an upstream response back to its raw representation. The
// gateway asks for `Accept-Encoding: identity` and caches the returned bytes
// as-is, so a misbehaving upstream that compresses anyway would have its
// compressed bytes stored and served to clients as if they were the raw body.
// Only the declared encoding is removed; unknown or already-decompressed
// bodies pass through untouched.
func decodeBody(resp *http.Response) error {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	switch enc {
	case "":
		return nil
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return err
		}
		resp.Body = &readCloser{Reader: zr, closer: resp.Body}
	case "deflate":
		zr, err := zlib.NewReader(resp.Body)
		if err != nil {
			return err
		}
		resp.Body = &readCloser{Reader: zr, closer: resp.Body}
	default:
		// "identity", "br", "compress" and anything else: keep the body wired
		// through unchanged rather than risk mislabeling it.
		if enc == "identity" {
			resp.Header.Del("Content-Encoding")
		}
		return nil
	}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return nil
}

// readCloser pairs a decompressing reader with the upstream body it is reading
// from, so the underlying connection is still closed when the response is
// consumed.
type readCloser struct {
	io.Reader
	closer io.Closer
}

func (rc *readCloser) Close() error {
	return rc.closer.Close()
}

func newTransport() http.RoundTripper {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
}
