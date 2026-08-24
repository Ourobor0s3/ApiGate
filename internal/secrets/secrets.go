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

// maxSecretValue bounds one stored secret so a bad write can't plant a huge
// value every request would re-read.
const maxSecretValue = 8 << 10

// redactAPIKeyParam strips an apiKey query param from URL-shaped values before
// listing, so a key embedded in e.g. NEWS_API_URL is never echoed back.
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

// storer is the handler's store surface; tests fake it.
type storer interface {
	Get(ctx context.Context, name string) (string, error)
	Set(ctx context.Context, name, value string) error
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) ([]string, error)
}

// A stored Redis secret overrides both Default and Env; masked names never
// have their value returned.
type Setting struct {
	Name    string
	Default string
	Env     string
	Masked  bool
}

// Option configures a Handler.
type Option func(*Handler)

// WithOnChange fires after a secret is created or deleted — main uses it to
// refresh the dashboard when a data-affecting secret changes.
func WithOnChange(f func(string)) Option {
	return func(h *Handler) { h.onChange = f }
}

// Handler exposes the store as a small REST API. Values are never returned —
// only names, plus the settings catalog so the UI can offer every variable.
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
