// Package checks probes user-configured URLs on a schedule and stores the
// latest result, so the dashboard can show each site's reachability status.
// Targets live in Redis, so they survive restarts; the polling interval comes
// from a per-request resolver (typically a secret), defaulting to 5 minutes.
package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultInterval is used when no CHECK_INTERVAL secret is set.
const DefaultInterval = 5 * time.Minute

// ErrExists and ErrInvalidURL classify Add errors for the REST handler.
var (
	ErrExists     = errors.New("check already exists")
	ErrInvalidURL = errors.New("invalid URL")
)

// statusTTL bounds how long a stored result lives if a target is removed
// without its status key being cleaned up.
const statusTTL = 24 * time.Hour

// targetsKey is a Redis SET of the URLs to probe.
const targetsKey = "check:targets"

type Config struct {
	HTTPClient *http.Client
	Logger     *slog.Logger
	// Interval resolves the polling interval per cycle (e.g. from a
	// CHECK_INTERVAL secret); nil means DefaultInterval.
	Interval func(context.Context) time.Duration
}

// Status is the outcome of one probe.
type Status struct {
	OK        bool   `json:"ok"`
	Code      int    `json:"code,omitempty"`
	LatencyMs int64  `json:"latencyMs,omitempty"`
	CheckedAt string `json:"checkedAt,omitempty"`
}

// Item is one monitored target plus its latest status (nil until probed).
type Item struct {
	Name   string  `json:"name"`
	URL    string  `json:"url"`
	Status *Status `json:"status"`
}

type Checks struct {
	rdb        *redis.Client
	httpClient *http.Client
	logger     *slog.Logger
	interval   func(context.Context) time.Duration
}

func New(rdb *redis.Client, cfg Config) *Checks {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 8 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval == nil {
		cfg.Interval = func(context.Context) time.Duration { return DefaultInterval }
	}
	return &Checks{rdb: rdb, httpClient: cfg.HTTPClient, logger: cfg.Logger, interval: cfg.Interval}
}

func statusKey(url string) string { return "check:status:" + url }

// Add registers a target and returns ErrExists for duplicates.
func (c *Checks) Add(ctx context.Context, rawurl string) error {
	if err := validateURL(rawurl); err != nil {
		return err
	}
	added, err := c.rdb.SAdd(ctx, targetsKey, rawurl).Result()
	if err != nil {
		return err
	}
	if added == 0 {
		return ErrExists
	}
	return nil
}

// List returns all targets (sorted by display name) and the current interval.
// Statuses are fetched in one pipelined round trip rather than one per target.
func (c *Checks) List(ctx context.Context) ([]Item, time.Duration, error) {
	urls, err := c.rdb.SMembers(ctx, targetsKey).Result()
	if err != nil {
		return nil, 0, err
	}

	items := make([]Item, 0, len(urls))
	if len(urls) > 0 {
		pipe := c.rdb.Pipeline()
		statuses := make(map[string]*redis.StringCmd, len(urls))
		for _, u := range urls {
			statuses[u] = pipe.Get(ctx, statusKey(u))
		}
		pipe.Exec(ctx)
		for _, u := range urls {
			item := Item{Name: shortName(u), URL: u}
			if b, err := statuses[u].Bytes(); err == nil {
				var s Status
				if json.Unmarshal(b, &s) == nil {
					item.Status = &s
				}
			}
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, c.interval(ctx), nil
}

// Delete removes a target and its stored status.
func (c *Checks) Delete(ctx context.Context, rawurl string) error {
	if _, err := c.rdb.SRem(ctx, targetsKey, rawurl).Result(); err != nil {
		return err
	}
	return c.rdb.Del(ctx, statusKey(rawurl)).Err()
}

// Run probes all targets on the current interval until ctx is cancelled. The
// interval is re-read every cycle, so changing the CHECK_INTERVAL secret takes
// effect on the next round without a restart.
func (c *Checks) Run(ctx context.Context) {
	interval := c.interval(ctx)
	c.logger.Info("checks: started", "interval", interval)
	c.checkAll(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("checks: stopped")
			return
		case <-ticker.C:
			interval = c.interval(ctx)
			ticker.Reset(interval)
			c.checkAll(ctx)
		}
	}
}

// CheckNow probes a single target immediately and stores the result, used to
// give instant feedback when a check is added.
func (c *Checks) CheckNow(ctx context.Context, rawurl string) {
	c.checkOne(ctx, rawurl)
}

// CheckAll probes every target immediately and returns the fresh results, used
// by the "check now" button so the UI can show an up-to-date status right away.
func (c *Checks) CheckAll(ctx context.Context) ([]Item, time.Duration, error) {
	c.checkAll(ctx)
	return c.List(ctx)
}

func (c *Checks) checkAll(ctx context.Context) {
	urls, err := c.rdb.SMembers(ctx, targetsKey).Result()
	if err != nil {
		c.logger.Warn("checks: cannot load targets", "err", err)
		return
	}
	var wg sync.WaitGroup
	for _, u := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			c.checkOne(ctx, u)
		}(u)
	}
	wg.Wait()
}

func (c *Checks) checkOne(ctx context.Context, rawurl string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return
	}
	start := time.Now()
	resp, err := c.httpClient.Do(req)
	latency := time.Since(start).Milliseconds()

	s := Status{CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	if err != nil {
		c.logger.Warn("checks: request failed", "url", rawurl, "err", err)
	} else {
		resp.Body.Close()
		s.OK = resp.StatusCode >= 200 && resp.StatusCode < 400
		s.Code = resp.StatusCode
		s.LatencyMs = latency
	}

	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	if err := c.rdb.Set(ctx, statusKey(rawurl), b, statusTTL).Err(); err != nil {
		c.logger.Warn("checks: cannot store status", "url", rawurl, "err", err)
	}
}

// shortName returns the most identifying short part of a URL — its hostname.
func shortName(rawurl string) string {
	u, err := url.Parse(rawurl)
	if err != nil || u.Hostname() == "" {
		return rawurl
	}
	return u.Hostname()
}

func validateURL(rawurl string) error {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ErrInvalidURL
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: only http/https URLs are supported", ErrInvalidURL)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%w: URL must include a host", ErrInvalidURL)
	}
	return nil
}

// ParseInterval converts a Go duration string (e.g. "5m", "6m 30s") into an
// interval, falling back to DefaultInterval on empty or invalid input. Internal
// whitespace is ignored so "6m 30s" parses the same as "6m30s".
func ParseInterval(v string) time.Duration {
	if d, err := time.ParseDuration(strings.Join(strings.Fields(v), "")); err == nil && d > 0 {
		return d
	}
	return DefaultInterval
}

// Handler exposes the checks store as a small REST API, mirroring the secrets
// handler: GET lists targets + statuses, POST {url} adds one, DELETE ?url=
// removes it.
type Handler struct {
	ch *Checks
}

func NewHandler(ch *Checks) *Handler {
	return &Handler{ch: ch}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/checks", h.list)
	mux.HandleFunc("POST /api/checks", h.add)
	mux.HandleFunc("POST /api/checks/run", h.runAll)
	mux.HandleFunc("DELETE /api/checks", h.delete)
}

// writeList renders the checks payload shared by GET /api/checks and
// POST /api/checks/run.
func (h *Handler) writeList(w http.ResponseWriter, items []Item, interval time.Duration) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"checks":   items,
		"interval": interval.String(),
	})
}

// runAll re-probes every target right now and returns the fresh list, backing
// the "Check now" button. Checks run concurrently, so latency is bounded by the
// slowest single probe rather than the sum of all of them.
func (h *Handler) runAll(w http.ResponseWriter, r *http.Request) {
	items, interval, err := h.ch.CheckAll(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.writeList(w, items, interval)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	items, interval, err := h.ch.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.writeList(w, items, interval)
}

func (h *Handler) add(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var in struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	rawurl := strings.TrimSpace(in.URL)
	if rawurl == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	if err := h.ch.Add(r.Context(), rawurl); err != nil {
		switch {
		case errors.Is(err, ErrExists):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, ErrInvalidURL):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	// Probe it right away so the section shows a status without waiting for
	// the next interval tick. The request's context dies with the handler, so
	// use a fresh bounded context.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.ch.CheckNow(ctx, rawurl)
	}()
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	rawurl := r.URL.Query().Get("url")
	if rawurl == "" {
		http.Error(w, "url query param is required", http.StatusBadRequest)
		return
	}
	if err := h.ch.Delete(r.Context(), rawurl); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
