package proxy

import (
	"context"
	"fmt"
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
}

type route struct {
	proxy *httputil.ReverseProxy
}

type Proxy struct {
	routes map[string]*route
}

func New(cfg Config) (*Proxy, error) {
	p := &Proxy{routes: make(map[string]*route)}

	var newsParams map[string]func(context.Context) string
	if cfg.NewsAPIKey != nil {
		newsParams = map[string]func(context.Context) string{"apiKey": cfg.NewsAPIKey}
	}

	if err := addRoute(p.routes, "/weather", cfg.WeatherAPI, nil); err != nil {
		return nil, err
	}
	if err := addRoute(p.routes, "/news", cfg.NewsAPI, newsParams); err != nil {
		return nil, err
	}
	return p, nil
}

func addRoute(routes map[string]*route, prefix, target string, params map[string]func(context.Context) string) error {
	if target == "" {
		return nil
	}
	targetURL, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("invalid target URL for %s: %w", prefix, err)
	}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.URL.Path = targetURL.Path
			req.URL.RawPath = ""
			req.Host = targetURL.Host

			for name, fn := range params {
				if req.URL.Query().Get(name) != "" {
					continue
				}
				if v := fn(req.Context()); v != "" {
					q := req.URL.Query()
					q.Set(name, v)
					req.URL.RawQuery = q.Encode()
				}
			}
		},
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          20,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}
	routes[prefix] = &route{proxy: rp}
	return nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for prefix, rt := range p.routes {
		if strings.HasPrefix(r.URL.Path, prefix) {
			rt.proxy.ServeHTTP(w, r)
			return
		}
	}
	http.NotFound(w, r)
}
