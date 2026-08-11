package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/redis/go-redis/v9"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// maxSecretValue caps a stored secret (API keys, URLs, intervals are all a few
// hundred bytes at most), so one bad write can't plant a huge value that every
// request would then re-read into memory.
const maxSecretValue = 8 << 10

// redactAPIKeyParam strips an apiKey query parameter from a URL-shaped value
// before it is listed, so a key embedded in an upstream URL (e.g. a
// NEWS_API_URL with ?apiKey=...) is never echoed back by /api/secrets.
// Non-URL values pass through untouched.
func redactAPIKeyParam(v string) string {
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" {
		return v
	}
	q := u.Query()
	if q.Get("apiKey") == "" {
		return v
	}
	q.Del("apiKey")
	u.RawQuery = q.Encode()
	return u.String()
}

type Store struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Store {
	return &Store{rdb: rdb}
}

func key(name string) string { return "secret:" + name }

func (s *Store) Get(ctx context.Context, name string) (string, error) {
	v, err := s.rdb.Get(ctx, key(name)).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

func (s *Store) Set(ctx context.Context, name, value string) error {
	return s.rdb.Set(ctx, key(name), value, 0).Err()
}

func (s *Store) Delete(ctx context.Context, name string) error {
	return s.rdb.Del(ctx, key(name)).Err()
}

func (s *Store) List(ctx context.Context) ([]string, error) {
	iter := s.rdb.Scan(ctx, 0, "secret:*", 0).Iterator()
	var names []string
	for iter.Next(ctx) {
		name := strings.TrimPrefix(iter.Val(), "secret:")
		if validName.MatchString(name) {
			names = append(names, name)
		}
	}
	return names, iter.Err()
}

// storer is the narrow store surface the REST handler needs. *Store satisfies
// it; tests substitute an in-memory fake, keeping the handler Redis-free.
type storer interface {
	Get(ctx context.Context, name string) (string, error)
	Set(ctx context.Context, name, value string) error
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) ([]string, error)
}

// Setting describes one runtime-configurable variable shown on the API
// Secrets page: its built-in default and the env value (if set). A stored
// Redis secret of the same name overrides both; Masked names never have their
// value returned (api keys).
type Setting struct {
	Name    string
	Default string
	Env     string
	Masked  bool
}

// Option configures a Handler.
type Option func(*Handler)

// WithOnChange registers a callback fired after a secret is created or
// deleted — never on read or on a rejected write. main uses it to trigger an
// immediate data refresh when a data-affecting secret changes, so the
// dashboard reflects the new location, currency or news upstream without
// waiting for the next scheduled poll.
func WithOnChange(f func(string)) Option {
	return func(h *Handler) { h.onChange = f }
}

// Handler exposes the store as a small REST API. Values are never returned in
// responses — only secret names are listed, plus the known settings catalog
// (with defaults) so the UI can offer every changeable variable in one place.
type Handler struct {
	store    storer
	settings []Setting
	onChange func(string)
}

func NewHandler(s storer, settings []Setting, opts ...Option) *Handler {
	h := &Handler{store: s, settings: settings}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Register wires the GET/POST/DELETE routes on /api/secrets. Method and path
// are matched exactly by the Go 1.22+ ServeMux, so anything else gets 405/404.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/secrets", h.list)
	mux.HandleFunc("POST /api/secrets", h.create)
	mux.HandleFunc("DELETE /api/secrets", h.delete)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	names, err := h.store.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	stored := make(map[string]bool, len(names))
	for _, n := range names {
		stored[n] = true
	}

	// A stored secret only surfaces its value when it is not masked; masked
	// names (api keys) come back with Stored=true so the UI can show that a
	// value is set without revealing it.
	settings := make([]map[string]interface{}, 0, len(h.settings))
	for _, s := range h.settings {
		row := map[string]interface{}{
			"name":    s.Name,
			"default": s.Default,
			"stored":  stored[s.Name],
			"masked":  s.Masked,
		}
		if !s.Masked {
			if s.Env != "" {
				row["env"] = redactAPIKeyParam(s.Env)
			}
			if v, err := h.store.Get(r.Context(), s.Name); err == nil && v != "" {
				row["value"] = redactAPIKeyParam(v)
			}
		}
		settings = append(settings, row)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"secrets": names, "settings": settings})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var in struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if !validName.MatchString(in.Name) || in.Value == "" {
		http.Error(w, "name must match [A-Za-z0-9_-]+ and value must be non-empty", http.StatusBadRequest)
		return
	}
	if len(in.Value) > maxSecretValue {
		http.Error(w, "value too long (max 8 KiB)", http.StatusBadRequest)
		return
	}
	if err := h.store.Set(r.Context(), in.Name, in.Value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if h.onChange != nil {
		h.onChange(in.Name)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !validName.MatchString(name) {
		http.Error(w, "name must match [A-Za-z0-9_-]+", http.StatusBadRequest)
		return
	}
	if err := h.store.Delete(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if h.onChange != nil {
		h.onChange(name)
	}
	w.WriteHeader(http.StatusNoContent)
}
