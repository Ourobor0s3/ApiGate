// Package checks probes user-configured URLs on a schedule and stores the
// latest result for the dashboard. Targets live in Redis; probe history is
// one ZSET per target, pruned on every write/read.
package checks

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ourobor0s3/ApiGate/internal/netguard"
	"github.com/redis/go-redis/v9"
)

// DefaultInterval is used when no CHECK_INTERVAL secret is set.
const DefaultInterval = 5 * time.Minute

var (
	ErrExists         = errors.New("check already exists")
	ErrInvalidURL     = errors.New("invalid URL")
	ErrPrivateAddress = errors.New("private address not allowed")
)

// historyLimit caps probe records per target; the latest result is always the
// last ZSET entry, so no separate status key is kept.
const historyLimit = 100

const historyWindow = 48 * time.Hour

// checkDrainBytes bounds how much of a probe body is read so keep-alive
// connections stay reusable without transferring huge bodies.
const checkDrainBytes = 64 << 10

// targetsKey is a Redis SET of the URLs to probe.
const targetsKey = "check:targets"

type Config struct {
	HTTPClient *http.Client
	Logger     *slog.Logger
	// Interval resolves the polling interval each cycle (e.g. from a
	// CHECK_INTERVAL secret).
	Interval func(context.Context) time.Duration
	// OnStatusChange fires when a target flips between healthy and failing,
	// e.g. to post a webhook.
	OnStatusChange func(ctx context.Context, url string, from, to Status)
	// AllowPrivate permits loopback/private/link-local targets; off by
	// default to prevent SSRF.
	AllowPrivate bool
}

type Status struct {
	OK        bool   `json:"ok"`
	Code      int    `json:"code,omitempty"`
	LatencyMs int64  `json:"latencyMs,omitempty"`
	CheckedAt string `json:"checkedAt,omitempty"`
}

type Item struct {
	Name   string  `json:"name"`
	URL    string  `json:"url"`
	Status *Status `json:"status"`
	Uptime float64 `json:"uptime,omitempty"` // percent of OK probes, 0-100
}

type Checks struct {
	rdb          *redis.Client
	httpClient   *http.Client
	logger       *slog.Logger
	interval     func(context.Context) time.Duration
	onChange     func(context.Context, string, Status, Status)
	allowPrivate bool
}

func New(rdb *redis.Client, cfg Config) *Checks {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = newCheckClient(cfg.AllowPrivate)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval == nil {
		cfg.Interval = func(context.Context) time.Duration { return DefaultInterval }
	}
	return &Checks{rdb: rdb, httpClient: cfg.HTTPClient, logger: cfg.Logger, interval: cfg.Interval, onChange: cfg.OnStatusChange, allowPrivate: cfg.AllowPrivate}
}

// newCheckClient validates every connection — including redirect hops —
// against private ranges at dial time, so an Add-time DNS check alone can't be
// bypassed by redirecting a public URL inward.
func newCheckClient(allowPrivate bool) *http.Client {
	base := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DialContext:           base.DialContext,
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   5,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
	}
	if !allowPrivate {
		transport.DialContext = guardedDialContext(base)
	}
	return &http.Client{Timeout: 8 * time.Second, Transport: transport}
}

// guardedDialContext maps netguard.ErrBlocked to the API-facing
// ErrPrivateAddress so the REST handler can classify it.
func guardedDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dial := netguard.RestrictedDialContext(base)
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if errors.Is(err, netguard.ErrBlocked) {
			return nil, ErrPrivateAddress
		}
		return conn, err
	}
}

const historyKeyPrefix = "check:history:"

const historyTTL = 4 * 24 * time.Hour

// historyKey embeds a URL digest so the keyspace stays uniform regardless of
// URL spelling; each member is a Status JSON blob scored by probe time.
func historyKey(rawurl string) string { return historyKeyPrefix + sha1hex(rawurl) }

// legacyHistoryKey is the pre-digest key, dropped on sight.
func legacyHistoryKey(rawurl string) string { return historyKeyPrefix + rawurl }

func sha1hex(s string) string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(s)))
}

func (c *Checks) Add(ctx context.Context, rawurl string) error {
	if err := validateURL(rawurl); err != nil {
		return err
	}
	if !c.allowPrivate {
		if err := guardPublicAddress(ctx, rawurl); err != nil {
			return err
		}
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

// guardPublicAddress is the Add-time fast-fail; the dial-time guard in
// newCheckClient (which also covers redirects) is the real enforcement.
func guardPublicAddress(ctx context.Context, rawurl string) error {
	u, err := url.Parse(rawurl)
	if err != nil || u.Hostname() == "" {
		return ErrInvalidURL
	}
	// Bound the lookup so a slow DNS server can't stall a /api/checks POST.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := (&net.Resolver{}).LookupIP(ctx, "ip", u.Hostname())
	if err != nil {
		return nil
	}
	for _, ip := range ips {
		if netguard.BlockedIP(ip) {
			return ErrPrivateAddress
		}
	}
	return nil
}

// List returns all targets (sorted by display name) and the current interval.
func (c *Checks) List(ctx context.Context) ([]Item, time.Duration, error) {
	urls, err := c.rdb.SMembers(ctx, targetsKey).Result()
	if err != nil {
		return nil, 0, err
	}

	items := make([]Item, 0, len(urls))
	for _, u := range urls {
		item := Item{Name: shortName(u), URL: u}
		if entries := c.historyEntries(ctx, u); entries != nil {
			item.Status = lastStatus(entries)
			item.Uptime = uptime(entries)
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, c.interval(ctx), nil
}

// historyEntries returns a target's probe records in chronological order,
// pruning records older than historyWindow as a side effect so the two-day
// window holds even when probes are sparse. Legacy keys are dropped on sight
// — both formats migrate on first read.
func (c *Checks) historyEntries(ctx context.Context, rawurl string) []string {
	key := historyKey(rawurl)
	pipe := c.rdb.Pipeline()
	pipe.Del(ctx, legacyHistoryKey(rawurl))
	cutoff := strconv.FormatInt(time.Now().Add(-historyWindow).UnixMilli(), 10)
	pipe.ZRemRangeByScore(ctx, key, "0", cutoff)
	pruned := pipe.ZRange(ctx, key, 0, -1)
	if _, err := pipe.Exec(ctx); err != nil {
		c.logger.Warn("checks: cannot read history", "url", rawurl, "err", err)
		if isWrongType(err) {
			c.rdb.Del(ctx, key)
		}
		return nil
	}
	members, err := pruned.Result()
	if err != nil {
		return nil
	}
	return members
}

// lastStatus parses the latest probe result (the last history entry) into a
// Status, or nil when there is no usable entry yet.
func lastStatus(entries []string) *Status {
	if len(entries) == 0 {
		return nil
	}
	var s Status
	if json.Unmarshal([]byte(entries[len(entries)-1]), &s) != nil {
		return nil
	}
	return &s
}

// Delete removes a target and its probe history. The whole history is one
// key per target, so nothing else needs cleaning.
func (c *Checks) Delete(ctx context.Context, rawurl string) error {
	if _, err := c.rdb.SRem(ctx, targetsKey, rawurl).Result(); err != nil {
		return err
	}
	return c.rdb.Del(ctx, historyKey(rawurl)).Err()
}

// isWrongType reports whether a Redis error is WRONGTYPE, which happens when a
// key holds a value of another type (e.g. a legacy list written by earlier
// versions of this package).
func isWrongType(err error) bool {
	return err != nil && strings.Contains(err.Error(), "WRONGTYPE")
}

// Run probes all targets until ctx is cancelled; the interval is re-read every
// cycle so a changed CHECK_INTERVAL secret takes effect without a restart.
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

// CheckAll probes immediately and returns fresh results ("check now" button).
func (c *Checks) CheckAll(ctx context.Context) ([]Item, time.Duration, error) {
	c.checkAll(ctx)
	return c.List(ctx)
}

const probeWorkers = 10

func (c *Checks) checkAll(ctx context.Context) {
	urls, err := c.rdb.SMembers(ctx, targetsKey).Result()
	if err != nil {
		c.logger.Warn("checks: cannot load targets", "err", err)
		return
	}

	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < probeWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range jobs {
				c.checkOne(ctx, u)
			}
		}()
	}

sendLoop:
	for _, u := range urls {
		select {
		case jobs <- u:
		case <-ctx.Done():
			break sendLoop
		}
	}
	close(jobs)
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
	at := time.Now()

	s := Status{CheckedAt: at.UTC().Format(time.RFC3339)}
	if err != nil {
		c.logger.Warn("checks: request failed", "url", rawurl, "err", err)
	} else {
		// Drain a bounded slice so the keep-alive connection is reusable.
		_, _ = io.CopyN(io.Discard, resp.Body, checkDrainBytes)
		resp.Body.Close()
		s.OK = resp.StatusCode >= 200 && resp.StatusCode < 400
		s.Code = resp.StatusCode
		s.LatencyMs = latency
	}

	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	// One timestamp for the record and its history score: they must never
	// disagree (e.g. across a midnight boundary).
	if !c.storeStatus(ctx, rawurl, b, at) {
		return
	}

	// Fire the hook only when a prior result exists and the state flipped.
	if c.onChange != nil {
		prev := c.previousStatus(ctx, rawurl)
		if prev.CheckedAt != "" && prev.OK != s.OK {
			c.onChange(ctx, rawurl, prev, s)
		}
	}
}

// storeStatus appends one record, pruning past historyWindow, capping at
// historyLimit and refreshing the TTL. Legacy keys are dropped and the write
// retried so old data migrates on the first probe after an upgrade.
func (c *Checks) storeStatus(ctx context.Context, rawurl string, record []byte, at time.Time) bool {
	write := func() error {
		pipe := c.rdb.Pipeline()
		pipe.Del(ctx, legacyHistoryKey(rawurl))
		pipe.ZAdd(ctx, historyKey(rawurl), redis.Z{Score: float64(at.UnixMilli()), Member: record})
		pipe.ZRemRangeByScore(ctx, historyKey(rawurl), "0", strconv.FormatInt(at.Add(-historyWindow).UnixMilli(), 10))
		pipe.ZRemRangeByRank(ctx, historyKey(rawurl), 0, -(historyLimit + 1))
		pipe.Expire(ctx, historyKey(rawurl), historyTTL)
		_, err := pipe.Exec(ctx)
		return err
	}
	if err := write(); err != nil {
		if !isWrongType(err) || c.rdb.Del(ctx, historyKey(rawurl)).Err() != nil {
			c.logger.Warn("checks: cannot store status", "url", rawurl, "err", err)
			return false
		}
		if err := write(); err != nil {
			c.logger.Warn("checks: cannot store status", "url", rawurl, "err", err)
			return false
		}
	}
	return true
}

// previousStatus reads the second-to-last history entry (before the just-stored one).
func (c *Checks) previousStatus(ctx context.Context, rawurl string) Status {
	members, err := c.rdb.ZRange(ctx, historyKey(rawurl), -2, -2).Result()
	if err != nil || len(members) == 0 {
		return Status{}
	}
	var prev Status
	if json.Unmarshal([]byte(members[0]), &prev) == nil {
		return prev
	}
	return Status{}
}

func uptime(entries []string) float64 {
	var ok, total int
	for _, e := range entries {
		var s Status
		if json.Unmarshal([]byte(e), &s) != nil {
			continue
		}
		total++
		if s.OK {
			ok++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(ok) / float64(total) * 100
}

// shortName returns the display name: the URL's hostname.
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

// Handler exposes the checks store as a small REST API: GET lists targets +
// statuses, POST {url} adds one, DELETE ?url= removes it.
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

func (h *Handler) runAll(w http.ResponseWriter, r *http.Request) {
	// The request context dies with the client; probing needs its own bound.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	items, interval, err := h.ch.CheckAll(ctx)
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
		case errors.Is(err, ErrInvalidURL), errors.Is(err, ErrPrivateAddress):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	// Probe right away on a fresh bounded context so the section shows a
	// status without waiting for the next tick.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.ch.checkOne(ctx, rawurl)
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
