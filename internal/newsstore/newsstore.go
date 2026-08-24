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

// Article is a newsapi article; URL is the dedup identity.
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

// ArticleTTL runs from first storage so history expires gradually; the index
// outlives every article it points at, so a reachable article is never orphaned.
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

// New returns the English-namespace store.
func New(rdb *redis.Client) *Store {
	return NewLang(rdb, "en")
}

// NewLang scopes the store to a language namespace so EN and RU headlines
// never share an index; empty lang falls back to "en".
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

// articleID is a SHA-1 digest of the URL: short uniform keys, stable dedup.
func articleID(rawurl string) string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(rawurl)))
}

func (s *Store) dropLegacy(ctx context.Context) {
	if s.lang != "en" {
		return
	}
	s.rdb.Del(ctx, legacyIndex)
}

// publishedScore orders the index by publication time; undated articles count
// as just-now so they surface at the top.
func publishedScore(a Article) int64 {
	t, err := time.Parse(time.RFC3339, a.PublishedAt)
	if err != nil {
		return time.Now().Unix()
	}
	return t.Unix()
}

// storeScript writes a batch atomically: SET NX keeps the original value and
// TTL of existing keys, only new articles touch the index, and any addition
// refreshes the index TTL.
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

// maxArticleBytes keeps one oversized response from bloating Redis.
const maxArticleBytes = 64 << 10

type storedArticle struct {
	id      string
	blob    []byte
	article Article
}

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

// Store adds articles without overwriting existing ones: a key already
// present keeps its value and original TTL (SET NX), the index dedups on the
// URL digest, so the same article never appears twice. Returns how many
// articles were newly stored.
func (s *Store) Store(ctx context.Context, articles []Article) (int64, error) {
	s.dropLegacy(ctx)
	stored := storableArticles(articles)
	if len(stored) == 0 {
		return 0, nil
	}
	args := []interface{}{s.articlePrefix()}
	for _, a := range stored {
		args = append(args, a.id, a.blob, publishedScore(a.article))
	}
	args = append(args, int64(ArticleTTL/time.Second), int64((indexTTL-ArticleTTL)/time.Second))
	return storeScript.Run(ctx, s.rdb, []string{s.indexKey()}, args...).Int64()
}

// All returns every stored article, newest first. Index members whose article
// key expired are dropped lazily so the index stays bounded.
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
