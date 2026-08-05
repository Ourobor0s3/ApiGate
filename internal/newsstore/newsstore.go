// Package newsstore keeps newsapi articles in Redis so the dashboard can show
// a history of articles instead of only the latest page. Articles are keyed by
// a SHA-1 digest of their URL and stored once with a 4-day TTL — re-fetched
// duplicates are skipped, never overwritten. An index ZSET (member = URL digest,
// score = publishedAt) provides dedup and newest-first ordering. Keys are
// namespaced per language: "news:index:<lang>" and "news:article:<lang>:<hex>",
// so the digest keeps the keyspace uniform instead of fragmenting it into
// "news:article:https:..." / "news:article:http:..." pseudo-directories.
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

// ArticleTTL is how long a stored article stays alive. Each article keeps its
// own TTL, measured from when it was first stored, so accumulated history
// expires gradually rather than all at once. The index outlives every article
// it points at by one indexTTLMargin day, so a reachable article is never
// orphaned by its index.
const (
	ArticleTTL = 4 * 24 * time.Hour
	// indexTTL extends the index ZSET one day past the newest article it
	// references, so an article is never orphaned by an index that died first.
	indexTTL = ArticleTTL + 24*time.Hour
)

const (
	articlePrefix = "news:article:"
	indexPrefix   = "news:index:"
	// legacyIndex is the bare, language-less index key written by versions
	// before the language namespaces; it is dropped on sight so it can't
	// accumulate. Its URL-keyed articles expire on their own TTL.
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
// news:article:<lang>:<digest>, so English and Russian headlines never share an
// index or dedup against each other (the dashboard serves news in each UI
// language via /dashboard.newsRu). An empty lang falls back to the English
// namespace.
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

// articleID is the Redis key fragment identifying an article: a SHA-1 digest of
// its URL. Digesting produces short, uniform keys (news:article:en:<40 hex>)
// instead of embedding the raw URL, whose scheme and slashes fragment the
// keyspace into "news:article:https:..." / "news:article:http:..." entries.
// The article's real URL still travels inside its JSON, and dedup keeps working
// because the same URL always yields the same digest.
func articleID(rawurl string) string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(rawurl)))
}

// dropLegacy removes the bare "news:index" key written by earlier versions
// once, on first sight. The language-suffixed index belongs to the English
// store only; a RU store never writes the legacy key. Deleting a missing key is
// a no-op, so this is cheap enough to run on every access.
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

// storeScript atomically writes a batch of articles in one round trip. For each
// article the key is only set when new (NX keeps the value and original TTL of
// an existing key), and the index is touched only for newly stored articles, so
// overlapping fetches never duplicate or rewrite history. A store that added
// something also refreshes the index TTL (second-to-last ARGV) plus the margin
// (last ARGV), so the index self-clears soon after the last addition while
// outliving its newest article by indexTTL.
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

// Store adds articles without overwriting existing ones. A key already present
// keeps its value and original TTL (SET NX); the index dedups on the URL
// digest member, so the same article never appears twice regardless of how many
// fetches return it.
func (s *Store) Store(ctx context.Context, articles []Article) error {
	s.dropLegacy(ctx)
	args := []interface{}{s.articlePrefix()}
	for _, a := range articles {
		if a.URL == "" {
			continue
		}
		b, err := json.Marshal(a)
		if err != nil {
			continue
		}
		args = append(args, articleID(a.URL), b, publishedScore(a))
	}
	if len(args) == 1 {
		return nil
	}
	args = append(args, int64(ArticleTTL/time.Second), int64(indexTTL/time.Second))
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
