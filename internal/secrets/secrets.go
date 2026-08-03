package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/redis/go-redis/v9"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

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
	names := make([]string, 0)
	for iter.Next(ctx) {
		names = append(names, strings.TrimPrefix(iter.Val(), "secret:"))
	}
	return names, iter.Err()
}

// Handler exposes the store as a small REST API. Values are never returned in
// responses — only secret names are listed.
type Handler struct {
	store *Store
}

func NewHandler(s *Store) *Handler {
	return &Handler{store: s}
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
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"secrets": names})
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
	if err := h.store.Set(r.Context(), in.Name, in.Value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
	w.WriteHeader(http.StatusNoContent)
}
