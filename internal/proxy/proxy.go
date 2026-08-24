package proxy

import (
	"bufio"
	"compress/flate"
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
	// the `apiKey` query param on /news when one isn't already set.
	NewsAPIKey func(context.Context) string
	Breaker    BreakerConfig
}

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
// route has no target URL.
func (p *Proxy) Weather() http.Handler {
	return handlerOrNotFound(p.weather)
}

// News returns the /news upstream handler, or http.NotFound when the route has
// no target URL.
func (p *Proxy) News() http.Handler {
	return handlerOrNotFound(p.news)
}

func handlerOrNotFound(rp *httputil.ReverseProxy) http.Handler {
	if rp == nil {
		return http.HandlerFunc(http.NotFound)
	}
	return rp
}

// An empty target silently disables the route.
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

			// The gateway applies compression itself (Gzip middleware), so ask
			// the upstream for identity; otherwise compressed bytes would be
			// cached and re-compressed as if they were the raw body.
			req.Header.Set("Accept-Encoding", "identity")

			// Merge target URL params (e.g. ?country=us) without overriding
			// the request's own, then fill in param resolvers (the apiKey).
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
		// An upstream that compresses despite the identity request would
		// hand back encoded bytes the cache would store as raw; undo any
		// declared compression here.
		ModifyResponse: func(resp *http.Response) error {
			return decodeBody(resp)
		},
		Transport: transport,
	}, nil
}

// decodeBody unwraps a declared upstream compression back to the raw
// representation; unknown encodings pass through untouched rather than risk
// mislabeling them.
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
		// Some upstreams declare "deflate" but send raw DEFLATE without the
		// zlib wrapper. Peek at the stream header: a valid zlib block has
		// CM=8 and (CMF<<8|FLG) divisible by 31; anything else is decoded as
		// raw deflate instead of failing the whole response.
		br := bufio.NewReader(resp.Body)
		var r io.Reader
		if head, _ := br.Peek(2); len(head) == 2 && head[0]&0x0f == 8 && (uint16(head[0])<<8|uint16(head[1]))%31 == 0 {
			zr, err := zlib.NewReader(br)
			if err != nil {
				return err
			}
			r = zr
		} else {
			r = flate.NewReader(br)
		}
		resp.Body = &readCloser{Reader: r, closer: resp.Body}
	default:
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

// readCloser pairs a decompressing reader with the upstream body so the
// underlying connection is still closed.
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
		// ResponseHeaderTimeout only starts after TLS completes; without this
		// bound a stalled handshake pins the request until the client leaves.
		TLSHandshakeTimeout: 5 * time.Second,
	}
}
