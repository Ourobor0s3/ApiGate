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
	names := []string{}
	for iter.Next(ctx) {
		names = append(names, strings.TrimPrefix(iter.Val(), "secret:"))
	}
	return names, iter.Err()
}

// Handler exposes the store as a small REST API. Values are never returned in
// responses — only secret names are listed.
func Handler(s *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			names, err := s.List(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"secrets": names})
		case http.MethodPost:
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
			if err := s.Set(r.Context(), in.Name, in.Value); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			name := r.URL.Query().Get("name")
			if !validName.MatchString(name) {
				http.Error(w, "name must match [A-Za-z0-9_-]+", http.StatusBadRequest)
				return
			}
			if err := s.Delete(r.Context(), name); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
