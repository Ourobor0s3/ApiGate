package quota

import (
	"testing"
	"time"
)

func TestDayKey(t *testing.T) {
	got := dayKey("news", time.Date(2026, 8, 3, 15, 30, 0, 0, time.UTC))
	if want := "quota:news:2026-08-03"; got != want {
		t.Errorf("dayKey() = %q, want %q", got, want)
	}
}
