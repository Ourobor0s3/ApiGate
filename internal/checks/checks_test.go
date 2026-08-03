package checks

import (
	"errors"
	"testing"
	"time"
)

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
