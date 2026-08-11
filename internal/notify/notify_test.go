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
	c := New("")
	if c.Enabled() {
		t.Fatal("client built without a URL must report disabled")
	}
	c.Post(context.Background(), map[string]any{"event": "x"})
}

func TestPostNon2xxIsTolerated(t *testing.T) {
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.Post(context.Background(), map[string]any{"event": "x"})

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook request never arrived")
	}
}
