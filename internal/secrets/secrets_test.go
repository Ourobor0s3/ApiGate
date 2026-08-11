package secrets

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	cases := map[string]bool{
		"NEWS_API_KEY":     true,
		"WEATHER_LOCATION": true,
		"MAIN_CURRENCY":    true,
		"a-b_c":            true,
		"x1":               true,
		"":                 false,
		"has space":        false,
		"a/b":              false,
		"ключ":             false,
	}
	for name, want := range cases {
		if got := validName.MatchString(name); got != want {
			t.Errorf("validName(%q) = %v, want %v", name, got, want)
		}
	}
}

// fakeStore is an in-memory storer for handler tests (Redis-free).
type fakeStore struct {
	values map[string]string
}

func (f *fakeStore) Get(_ context.Context, name string) (string, error) {
	return f.values[name], nil
}

func (f *fakeStore) Set(_ context.Context, name, value string) error {
	f.values[name] = value
	return nil
}

func (f *fakeStore) Delete(_ context.Context, name string) error {
	delete(f.values, name)
	return nil
}

func (f *fakeStore) List(_ context.Context) ([]string, error) {
	out := make([]string, 0, len(f.values))
	for name := range f.values {
		out = append(out, name)
	}
	return out, nil
}

func newFakeStore() *fakeStore {
	return &fakeStore{values: map[string]string{}}
}

// do performs one request (target includes the query string) against the
// full registered mux.
func do(h *Handler, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	mux := http.NewServeMux()
	h.Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestOnChangeFiresOnWriteAndDelete(t *testing.T) {
	var changed []string
	h := NewHandler(newFakeStore(),
		[]Setting{{Name: "WEATHER_LOCATION", Default: "55.7558,37.6173"}},
		WithOnChange(func(name string) { changed = append(changed, name) }),
	)

	if w := do(h, "POST", "/api/secrets", `{"name":"WEATHER_LOCATION","value":"10,20"}`); w.Code != http.StatusNoContent {
		t.Fatalf("create = %d, want 204", w.Code)
	}
	if len(changed) != 1 || changed[0] != "WEATHER_LOCATION" {
		t.Fatalf("after create: changed = %v, want [WEATHER_LOCATION]", changed)
	}

	// A read must never trigger a refresh.
	do(h, "GET", "/api/secrets", "")
	if len(changed) != 1 {
		t.Fatalf("GET fired onChange, changed = %v", changed)
	}

	if w := do(h, "DELETE", "/api/secrets?name=WEATHER_LOCATION", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", w.Code)
	}
	if len(changed) != 2 {
		t.Fatalf("after delete: changed = %v, want 2 entries", changed)
	}
}

func TestOnChangeNotFiredOnRejectedWrite(t *testing.T) {
	var changed []string
	h := NewHandler(newFakeStore(), nil,
		WithOnChange(func(name string) { changed = append(changed, name) }),
	)

	// Invalid name, empty value and malformed JSON must not fire the callback:
	// a failed admin action must not trigger a data refresh.
	if w := do(h, "POST", "/api/secrets", `{"name":"a/b","value":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("create with invalid name = %d, want 400", w.Code)
	}
	if w := do(h, "POST", "/api/secrets", `{"name":"X","value":""}`); w.Code != http.StatusBadRequest {
		t.Fatalf("create with empty value = %d, want 400", w.Code)
	}
	if w := do(h, "POST", "/api/secrets", `{not json`); w.Code != http.StatusBadRequest {
		t.Fatalf("create with bad JSON = %d, want 400", w.Code)
	}
	if len(changed) != 0 {
		t.Errorf("onChange fired %d times on rejected writes, want 0", len(changed))
	}
}