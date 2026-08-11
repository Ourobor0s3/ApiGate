package interval

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := map[string]time.Duration{
		"5m":      5 * time.Minute,
		"1m30s":   90 * time.Second,
		"6m 30s":  6*time.Minute + 30*time.Second,
		" 10m ":   10 * time.Minute,
		"1h 15m":  75 * time.Minute,
		"":        2 * time.Minute,
		"garbage": 2 * time.Minute,
		"0s":      2 * time.Minute,
		"-1m":     2 * time.Minute,
	}
	def := 2 * time.Minute
	for in, want := range cases {
		if got := Parse(in, def); got != want {
			t.Errorf("Parse(%q) = %v, want %v", in, got, want)
		}
	}
}
