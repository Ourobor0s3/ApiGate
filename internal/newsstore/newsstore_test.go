package newsstore

import (
	"strings"
	"testing"
	"time"
)

func TestLangNamespaceKeys(t *testing.T) {
	en := New(nil)
	ru := NewLang(nil, "ru")
	if got := en.indexKey(); got != "news:index:en" {
		t.Errorf("en indexKey = %q, want %q", got, "news:index:en")
	}
	if got := en.articlePrefix(); got != "news:article:en:" {
		t.Errorf("en articlePrefix = %q, want %q", got, "news:article:en:")
	}
	if got := ru.indexKey(); got != "news:index:ru" {
		t.Errorf("ru indexKey = %q, want %q", got, "news:index:ru")
	}
	if got := ru.articlePrefix(); got != "news:article:ru:" {
		t.Errorf("ru articlePrefix = %q, want %q", got, "news:article:ru:")
	}
	if got := NewLang(nil, "").indexKey(); got != "news:index:en" {
		t.Errorf("empty lang indexKey = %q, want %q", got, "news:index:en")
	}
	if got := NewLang(nil, "").articlePrefix(); got != "news:article:en:" {
		t.Errorf("empty lang articlePrefix = %q, want %q", got, "news:article:en:")
	}
}

func TestArticleID(t *testing.T) {
	u := "https://example.com/news/2026/08/05/story?id=1"
	if got := articleID(u); got != articleID(u) {
		t.Errorf("articleID not deterministic: %q vs %q", got, articleID(u))
	}
	if got := articleID(u); len(got) != 40 {
		t.Errorf("articleID length = %d, want 40 hex chars", len(got))
	}
	if a, b := articleID("http://example.com/a"), articleID("https://example.com/a"); a == b {
		t.Errorf("scheme variants must not collide: %q == %q", a, b)
	}
	if a, b := articleID("https://example.com/a"), articleID("https://example.com/b"); a == b {
		t.Errorf("distinct URLs must not collide: %q == %q", a, b)
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

func TestStorableArticlesSkipsOversized(t *testing.T) {
	normal := Article{URL: "https://x.com/a", Title: "A"}
	huge := Article{URL: "https://x.com/b", Title: "B", Content: strings.Repeat("x", maxArticleBytes)}
	noURL := Article{Title: "no url"}
	badURL := Article{URL: "", Title: "also no url"}

	got := storableArticles([]Article{normal, huge, noURL, badURL})
	if len(got) != 1 {
		t.Fatalf("storableArticles() kept %d articles, want 1 (the normal one)", len(got))
	}
	if got[0].id != articleID(normal.URL) {
		t.Errorf("kept article id = %q, want %q", got[0].id, articleID(normal.URL))
	}
}
