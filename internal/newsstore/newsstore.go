// Package newsstore keeps newsapi articles in Redis so the dashboard can show
// history instead of only the latest page. Articles are keyed by a SHA-1
// digest of their URL, stored once with a 4-day TTL; the index ZSET (member =
// URL digest, score = publishedAt) provides dedup and newest-first ordering.
// Keys are namespaced per language: "news:index:<lang>" and
// "news:article:<lang>:<hex>".
package newsstore

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Article is a newsapi article persisted in Redis. URL is the dedup identity.
type Article struct {
	Source      *Source `json:"source,omitempty"`
	Author      string  `json:"author,omitempty"`
	Title       string  `json:"title,omitempty"`
	Description string  `json:"description,omitempty"`
	URL         string  `json:"url,omitempty"`
	URLToImage  string  `json:"urlToImage,omitempty"`
	PublishedAt string  `json:"publishedAt,omitempty"`
	Content     string  `json:"content,omitempty"`
}

// Source is the news source embedded in each article.
type Source struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// ArticleTTL is how long a stored article stays alive, measured from when it
// was first stored, so accumulated history expires gradually. The index
// outlives every article it points at by indexTTL, so a reachable article is
// never orphaned by its index.
const (
	ArticleTTL = 4 * 24 * time.Hour
	indexTTL   = ArticleTTL + 24*time.Hour
)

const (
	articlePrefix = "news:article:"
	indexPrefix   = "news:index:"
	// legacyIndex is the bare, language-less index key written by earlier
	// versions; it is dropped on sight so it can't accumulate.
	legacyIndex = "news:index"
)

type Store struct {
	rdb  *redis.Client
	lang string
}

// New returns the store for the default (English) language namespace,
// news:index:en / news:article:en:<digest>.
func New(rdb *redis.Client) *Store {
	return NewLang(rdb, "en")
}

// NewLang builds a store scoped to a language namespace, news:index:<lang> /
// news:article:<lang>:<digest>, so English and Russian headlines never share
// an index. An empty lang falls back to the English namespace.
func NewLang(rdb *redis.Client, lang string) *Store {
	if lang == "" {
		lang = "en"
	}
	return &Store{rdb: rdb, lang: lang}
}

func (s *Store) indexKey() string {
	return indexPrefix + s.lang
}

func (s *Store) articlePrefix() string {
	return articlePrefix + s.lang + ":"
}

// articleID is the Redis key fragment identifying an article: a SHA-1 digest
// of its URL, producing short, uniform keys (news:article:en:<40 hex>) instead
// of fragmenting the keyspace by scheme/slashes. Dedup keeps working because
// the same URL always yields the same digest.
func articleID(rawurl string) string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(rawurl)))
}

// dropLegacy removes the bare "news:index" key written by earlier versions
// once, on first sight; the language-suffixed index belongs to the English
// store only. Deleting a missing key is a no-op, so this runs on every access.
func (s *Store) dropLegacy(ctx context.Context) {
	if s.lang != "en" {
		return
	}
	s.rdb.Del(ctx, legacyIndex)
}

// publishedScore orders the index by publication time; articles without a
// parseable date count as just-now so they surface at the top.
func publishedScore(a Article) int64 {
	t, err := time.Parse(time.RFC3339, a.PublishedAt)
	if err != nil {
		return time.Now().Unix()
	}
	return t.Unix()
}

// storeScript atomically writes a batch of articles in one round trip. A key
// is only set when new (NX keeps the value and original TTL of an existing
// key), and the index is touched only for newly stored articles, so overlapping
// fetches never duplicate or rewrite history. A store that added something
// also refreshes the index TTL by ttl + one-day margin.
var storeScript = redis.NewScript(`
local index = KEYS[1]
local prefix = ARGV[1]
local ttl = tonumber(ARGV[#ARGV-1])
local margin = tonumber(ARGV[#ARGV])
local added = 0
local i = 2
while i < #ARGV-1 do
	if redis.call('SET', prefix .. ARGV[i], ARGV[i+1], 'NX', 'EX', ttl) then
		redis.call('ZADD', index, tonumber(ARGV[i+2]), ARGV[i])
		added = added + 1
	end
	i = i + 3
end
if added > 0 then
	redis.call('EXPIRE', index, ttl + margin)
end
return added
`)

// maxArticleBytes caps a single stored article's encoded size; real newsapi
// entries are a few KB at most. Skipping an outlier keeps one oversized
// response from bloating Redis for the article's whole TTL.
const maxArticleBytes = 64 << 10

// storedArticle couples an article ready for persistence with its dedup
// identity and JSON blob.
type storedArticle struct {
	id      string
	blob    []byte
	article Article
}

// storableArticles selects the articles worth persisting: it drops entries
// without a URL (no dedup identity) and any that encode beyond maxArticleBytes.
func storableArticles(articles []Article) []storedArticle {
	out := make([]storedArticle, 0, len(articles))
	for _, a := range articles {
		if a.URL == "" {
			continue
		}
		b, err := json.Marshal(a)
		if err != nil || len(b) > maxArticleBytes {
			continue
		}
		out = append(out, storedArticle{id: articleID(a.URL), blob: b, article: a})
	}
	return out
}

// Store adds articles without overwriting existing ones. A key already present
// keeps its value and original TTL (SET NX); the index dedups on the URL
// digest member, so the same article never appears twice.
func (s *Store) Store(ctx context.Context, articles []Article) error {
	s.dropLegacy(ctx)
	stored := storableArticles(articles)
	if len(stored) == 0 {
		return nil
	}
	args := []interface{}{s.articlePrefix()}
	for _, a := range stored {
		args = append(args, a.id, a.blob, publishedScore(a.article))
	}
	args = append(args, int64(ArticleTTL/time.Second), int64((indexTTL-ArticleTTL)/time.Second))
	_, err := storeScript.Run(ctx, s.rdb, []string{s.indexKey()}, args...).Result()
	return err
}

// All returns every stored article, newest first, without duplicates. Index
// members whose article key has expired are dropped lazily, so the index does
// not grow without bound. The bare legacy index key is removed on sight.
func (s *Store) All(ctx context.Context) ([]Article, error) {
	s.dropLegacy(ctx)
	members, err := s.rdb.ZRevRange(ctx, s.indexKey(), 0, -1).Result()
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, nil
	}

	keys := make([]string, len(members))
	for i, m := range members {
		keys[i] = s.articlePrefix() + m
	}
	vals, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}

	var out []Article
	var stale []string
	for i, m := range members {
		v, ok := vals[i].(string)
		if !ok || v == "" {
			stale = append(stale, m)
			continue
		}
		var a Article
		if err := json.Unmarshal([]byte(v), &a); err != nil {
			stale = append(stale, m)
			continue
		}
		out = append(out, a)
	}
	if len(stale) > 0 {
		staleAny := make([]interface{}, len(stale))
		for i, m := range stale {
			staleAny[i] = m
		}
		s.rdb.ZRem(ctx, s.indexKey(), staleAny...)
	}
	return out, nil
}
