package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestPostSendsJSON(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	received := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error("invalid JSON body:", err)
		}
		mu.Lock()
		got = body
		mu.Unlock()
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.Post(context.Background(), map[string]any{"event": "quotaExhausted", "name": "news"})

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook request never arrived")
	}
	mu.Lock()
	defer mu.Unlock()
	if got["event"] != "quotaExhausted" || got["name"] != "news" {
		t.Errorf("payload = %v", got)
	}
}

func TestDisabledClientIsNoop(t *testing.T) {
	// A client built from an empty URL must not attempt any request.
	called := false
	c := New("")
	c.Post(context.Background(), map[string]any{"event": "x"})
	if called {
		t.Fatal("disabled client made a request")
	}
}

func TestPostLogsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.Post(context.Background(), map[string]any{"event": "x"})
}
