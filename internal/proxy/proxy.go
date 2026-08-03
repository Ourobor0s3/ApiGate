package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

type Config struct {
	WeatherAPI string
	NewsAPI    string
	// NewsAPIKey, if set, resolves an API key per request and appends it as
	// the `apiKey` query param on the /news route when one isn't already set.
	NewsAPIKey func(context.Context) string
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
	if p.weather, err = newReverseProxy("/weather", cfg.WeatherAPI, nil, transport); err != nil {
		return nil, err
	}
	if p.news, err = newReverseProxy("/news", cfg.NewsAPI, newsParams, transport); err != nil {
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
func newReverseProxy(prefix, target string, params map[string]func(context.Context) string, transport http.RoundTripper) (*httputil.ReverseProxy, error) {
	if target == "" {
		return nil, nil
	}
	targetURL, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("invalid target URL for %s: %w", prefix, err)
	}

	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.URL.Path = targetURL.Path
			req.URL.RawPath = ""
			req.Host = targetURL.Host

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
		Transport: transport,
	}, nil
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
