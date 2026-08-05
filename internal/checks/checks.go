// Package checks probes user-configured URLs on a schedule and stores the
// latest result, so the dashboard can show each site's reachability status.
// Targets live in Redis, so they survive restarts; the polling interval comes
// from a per-request resolver (typically a secret), defaulting to 5 minutes.
// Probe history lives in one ZSET per target, score-pruned on every write and
// read, so records older than two days are removed automatically.
package checks

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultInterval is used when no CHECK_INTERVAL secret is set.
const DefaultInterval = 5 * time.Minute

// ErrExists and ErrInvalidURL classify Add errors for the REST handler.
var (
	ErrExists         = errors.New("check already exists")
	ErrInvalidURL     = errors.New("invalid URL")
	ErrPrivateAddress = errors.New("private address not allowed")
)

// historyLimit caps how many probe results are kept per target for uptime
// reporting. Older entries are trimmed away on each probe. The latest probe
// result is always the last entry of the history ZSET, so no separate status
// key is kept.
const historyLimit = 100

// historyWindow is how long a probe record stays in the history. Entries
// older than this are pruned by score on every write and read, so records of
// two days ago are deleted automatically without a background sweeper.
const historyWindow = 48 * time.Hour

// targetsKey is a Redis SET of the URLs to probe.
const targetsKey = "check:targets"

type Config struct {
	HTTPClient *http.Client
	Logger     *slog.Logger
	// Interval resolves the polling interval per cycle (e.g. from a
	// CHECK_INTERVAL secret); nil means DefaultInterval.
	Interval func(context.Context) time.Duration
	// OnStatusChange, if set, fires whenever a target flips between healthy
	// and failing (or vice versa), e.g. to post a webhook.
	OnStatusChange func(ctx context.Context, url string, from, to Status)
	// AllowPrivate, when true, permits targets that resolve to loopback,
	// private or link-local addresses. Off by default to prevent SSRF.
	AllowPrivate bool
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
	// Uptime is the percentage of the last probes that succeeded (0-100).
	Uptime float64 `json:"uptime,omitempty"`
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

// newCheckClient builds the probing client. Unless private addresses are
// explicitly allowed, every connection — including any redirect the probe
// follows — is validated against loopback/private/link-local ranges at dial
// time, so an Add-time DNS check alone can't be bypassed by redirecting a
// public URL to an internal address.
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

// guardedDialContext returns a DialContext that refuses connections to
// loopback/private/link-local addresses. Names that fail to resolve are passed
// through — the probe itself surfaces the failure.
func guardedDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if ips, err := net.LookupIP(host); err == nil {
			for _, ip := range ips {
				if blockedIP(ip) {
					return nil, ErrPrivateAddress
				}
			}
		}
		return base.DialContext(ctx, network, addr)
	}
}

// historyKeyPrefix namespaces the per-target history ZSETs, which otherwise
// share the "check:" prefix with the targets SET.
const historyKeyPrefix = "check:history:"

// historyTTL bounds how long a history key survives after its last probe, so a
// target that stops being polled cleans itself up instead of keeping its final
// record forever. Refreshed on every write, it comfortably exceeds
// historyWindow + the longest polling interval.
const historyTTL = 4 * 24 * time.Hour

// historyKey is the per-target ZSET of probe records: each member is a Status
// JSON blob scored by probe time (ascending, so rank order is oldest→newest).
// The key embeds a digest of the target URL, not the URL itself, so the
// keyspace stays uniform (check:history:<hex>) no matter how the URL is
// spelled — http:// vs https://, trailing slashes, query strings.
func historyKey(rawurl string) string { return historyKeyPrefix + sha1hex(rawurl) }

// legacyHistoryKey is the pre-digest key ("check:history:<url>") written by
// earlier versions. It is dropped on sight so it can't accumulate; the target
// URL is always available from the check:targets SET.
func legacyHistoryKey(rawurl string) string { return historyKeyPrefix + rawurl }

// sha1hex digests a string into the 40-hex-char form used in history keys.
func sha1hex(s string) string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(s)))
}

// Add registers a target and returns ErrExists for duplicates. Unless
// AllowPrivate is set, targets resolving to loopback/private/link-local
// addresses are rejected to prevent SSRF.
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

// guardPublicAddress resolves the target host and rejects it when any address
// is loopback, private or link-local. Names that fail to resolve are allowed
// through — the probe itself will surface the failure. This is a fast-fail
// check at registration time; the dial-time guard in newCheckClient is the
// real enforcement (it also covers redirect targets).
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
		if blockedIP(ip) {
			return ErrPrivateAddress
		}
	}
	return nil
}

// blockedIP reports whether an IP is loopback, private, link-local or
// unspecified — the ranges the SSRF guard rejects.
func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// List returns all targets (sorted by display name) and the current interval.
// Each target's history is read in one call and pruned of records older than
// historyWindow as a side effect; the latest probe result doubles as the
// current status.
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

// historyEntries returns a target's probe records (Status JSON blobs) in
// chronological order, pruning records older than historyWindow as a side
// effect so the two-day window holds even when probes are sparse. The returned
// list reflects the pruned state (the uptime/status below is computed from the
// records that are actually kept). A legacy list key from before the ZSET
// format is dropped on sight, and so is the legacy URL-keyed history key from
// before the digest format — both migrate on first read.
func (c *Checks) historyEntries(ctx context.Context, rawurl string) []string {
	c.rdb.Del(ctx, legacyHistoryKey(rawurl))
	members, err := c.rdb.ZRange(ctx, historyKey(rawurl), 0, -1).Result()
	if err != nil {
		if isWrongType(err) {
			c.rdb.Del(ctx, historyKey(rawurl))
		}
		return nil
	}
	if len(members) == 0 {
		return nil
	}
	cutoff := time.Now().Add(-historyWindow).UnixMilli()
	if err := c.rdb.ZRemRangeByScore(ctx, historyKey(rawurl), "0", strconv.FormatInt(cutoff, 10)).Err(); err != nil {
		c.logger.Warn("checks: cannot prune history", "url", rawurl, "err", err)
		return members
	}
	// Re-read after pruning so the returned entries are exactly what is kept.
	pruned, err := c.rdb.ZRange(ctx, historyKey(rawurl), 0, -1).Result()
	if err != nil {
		return members
	}
	return pruned
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

// CheckAll probes every target immediately and returns the fresh results, used
// by the "check now" button so the UI can show an up-to-date status right away.
func (c *Checks) CheckAll(ctx context.Context) ([]Item, time.Duration, error) {
	c.checkAll(ctx)
	return c.List(ctx)
}

// probeWorkers caps how many target probes run at once, so a large target list
// can't open an unbounded number of connections against both the targets and
// Redis on every cycle.
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
	if !c.storeStatus(ctx, rawurl, b, time.Now()) {
		return
	}

	// Fire the status-change hook only when there was a prior recorded result
	// and the healthy/failing state actually flipped, so it doesn't spam on
	// every probe of an unchanged target.
	if c.onChange != nil {
		prev := c.previousStatus(ctx, rawurl)
		if prev.CheckedAt != "" && prev.OK != s.OK {
			c.onChange(ctx, rawurl, prev, s)
		}
	}
}

// storeStatus appends one probe record to the target's history ZSET, pruning
// records older than historyWindow, capping the history to historyLimit and
// refreshing the key TTL. A legacy list key from the pre-ZSET format and the
// legacy URL-keyed history key are both dropped and the write retried, so
// existing Redis data migrates on the first probe after an upgrade.
// Returns false when the record could not be stored.
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

// previousStatus returns the status recorded before the just-stored one (the
// second-to-last entry of the history ZSET), or an empty Status when there
// was no prior result.
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

// uptime computes the percentage of OK probes across the stored history
// entries (each a Status JSON blob), 0 when there's no parseable history.
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
	// The request context dies with the client; probing should run to
	// completion so the store stays fresh, so use a bounded background context.
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
	// Probe it right away so the section shows a status without waiting for
	// the next interval tick. The request's context dies with the handler, so
	// use a fresh bounded context.
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
