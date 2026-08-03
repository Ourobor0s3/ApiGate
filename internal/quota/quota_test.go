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

func TestSecondsUntilNextDay(t *testing.T) {
	now := time.Date(2026, 8, 3, 23, 59, 30, 0, time.UTC)
	if got := secondsUntilNextDay(now); got != 31 {
		t.Errorf("secondsUntilNextDay() = %d, want 31", got)
	}
	midnight := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	if got := secondsUntilNextDay(midnight); got != 24*60*60+1 {
		t.Errorf("secondsUntilNextDay() = %d, want %d", got, 24*60*60+1)
	}
}
