package checks

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestHistoryKeyDigestFormat(t *testing.T) {
	for _, u := range []string{
		"https://example.com",
		"http://example.com/path?q=1",
		"https://go.dev/play/",
	} {
		key := historyKey(u)
		if !strings.HasPrefix(key, "check:history:") {
			t.Errorf("historyKey(%q) = %q, missing prefix", u, key)
		}
		id := strings.TrimPrefix(key, "check:history:")
		if len(id) != 40 {
			t.Errorf("historyKey(%q) digest length = %d, want 40 hex", u, len(id))
		}
		for _, c := range id {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Errorf("historyKey(%q) = %q, want hex digest", u, key)
				break
			}
		}
	}
	if historyKey("https://a.example/x") == historyKey("http://a.example/x") {
		t.Error("scheme variants must not share a history key")
	}
	if historyKey("https://a.example/x") != historyKey("https://a.example/x") {
		t.Error("historyKey not deterministic")
	}
}

func TestShortName(t *testing.T) {
	cases := map[string]string{
		"https://api.open-meteo.com/v1/forecast":    "api.open-meteo.com",
		"http://example.com:8080/path?q=1":          "example.com",
		"https://newsapi.org/v2/everything?lang=en": "newsapi.org",
		"not a url": "not a url",
		"https://":  "https://",
	}
	for in, want := range cases {
		if got := shortName(in); got != want {
			t.Errorf("shortName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateURL(t *testing.T) {
	for _, ok := range []string{
		"https://example.com",
		"http://example.com/path",
		"https://example.com:8080",
		"https://sub.example.org?q=1",
	} {
		if err := validateURL(ok); err != nil {
			t.Errorf("validateURL(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"ftp://example.com",
		"//no-scheme.com",
		"example.com",
		"https://",
		"https:///no-host",
	} {
		if err := validateURL(bad); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("validateURL(%q) = %v, want ErrInvalidURL", bad, err)
		}
	}
}

func TestParseInterval(t *testing.T) {
	cases := map[string]time.Duration{
		"5m":      5 * time.Minute,
		"1m30s":   90 * time.Second,
		"6m 30s":  6*time.Minute + 30*time.Second,
		" 10m ":   10 * time.Minute,
		"1h 15m":  75 * time.Minute,
		"":        5 * time.Minute,
		"garbage": 5 * time.Minute,
		"0s":      5 * time.Minute,
		"-1m":     5 * time.Minute,
	}
	for in, want := range cases {
		if got := ParseInterval(in); got != want {
			t.Errorf("ParseInterval(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestUptime(t *testing.T) {
	ok := func() string {
		b, _ := json.Marshal(Status{OK: true})
		return string(b)
	}
	down := func() string {
		b, _ := json.Marshal(Status{OK: false})
		return string(b)
	}
	cases := []struct {
		name    string
		entries []string
		want    float64
	}{
		{"empty", nil, 0},
		{"all up", []string{ok(), ok(), ok(), ok()}, 100},
		{"half", []string{ok(), down(), ok(), down()}, 50},
		{"one of three", []string{down(), down(), ok()}, 100.0 / 3},
		{"garbage entries ignored", []string{"not json", ok(), down(), "x"}, 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := uptime(tc.entries); abs(got-tc.want) > 1e-9 {
				t.Errorf("uptime() = %v, want %v", got, tc.want)
			}
		})
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// TestIsWrongType guards the legacy-key migration path: only WRONGTYPE errors
// trigger the drop-and-retry, everything else is logged and skipped.
func TestIsWrongType(t *testing.T) {
	if !isWrongType(errors.New("WRONGTYPE Operation against a key holding the wrong kind of value")) {
		t.Error("isWrongType() = false for a WRONGTYPE error")
	}
	for _, err := range []error{nil, errors.New("dial tcp: connection refused")} {
		if isWrongType(err) {
			t.Errorf("isWrongType(%v) = true, want false", err)
		}
	}
}

func TestGuardedDialRefusesLoopback(t *testing.T) {
	base := &net.Dialer{Timeout: time.Second}
	dial := guardedDialContext(base)

	if conn, err := dial(context.Background(), "tcp", "127.0.0.1:80"); !errors.Is(err, ErrPrivateAddress) {
		if conn != nil {
			conn.Close()
		}
		t.Fatalf("dial 127.0.0.1 err = %v, want ErrPrivateAddress", err)
	}
	if conn, err := dial(context.Background(), "tcp", "169.254.169.254:80"); !errors.Is(err, ErrPrivateAddress) {
		if conn != nil {
			conn.Close()
		}
		t.Fatalf("dial 169.254.169.254 err = %v, want ErrPrivateAddress", err)
	}
}

func TestLastStatus(t *testing.T) {
	up := `{"ok":true,"code":200}`
	down := `{"ok":false,"code":503}`
	if got := lastStatus(nil); got != nil {
		t.Fatalf("lastStatus(nil) = %v, want nil", got)
	}
	got := lastStatus([]string{up, down})
	if got == nil || got.OK || got.Code != 503 {
		t.Fatalf("lastStatus = %+v, want the last (down) entry", got)
	}
	if got := lastStatus([]string{up, "not json"}); got != nil {
		t.Fatalf("lastStatus with garbage last entry = %+v, want nil", got)
	}
}
