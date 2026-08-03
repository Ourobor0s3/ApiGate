package newsstore

import (
	"testing"
	"time"
)

func TestArticleKey(t *testing.T) {
	if got := articleKey("https://x.com/a"); got != "news:article:https://x.com/a" {
		t.Errorf("articleKey() = %q, want news:article:https://x.com/a", got)
	}
}

func TestPublishedScore(t *testing.T) {
	ts := "2026-08-03T10:00:00Z"
	want, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatal(err)
	}
	if got := publishedScore(Article{PublishedAt: ts}); got != want.Unix() {
		t.Errorf("publishedScore(parseable) = %d, want %d", got, want.Unix())
	}

	got := publishedScore(Article{PublishedAt: "garbage"})
	if d := time.Since(time.Unix(got, 0)); d < -5*time.Second || d > 5*time.Second {
		t.Errorf("publishedScore(unparseable) = %d, want ~now", got)
	}
}
