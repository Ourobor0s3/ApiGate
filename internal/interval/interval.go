package interval

import (
	"strings"
	"time"
)

// Parse converts a Go duration string ("5m", "6m 30s") into a duration,
// falling back to def on empty or invalid input.
func Parse(v string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(strings.Join(strings.Fields(v), "")); err == nil && d > 0 {
		return d
	}
	return def
}
