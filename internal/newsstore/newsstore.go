// Package newsstore keeps newsapi articles in Redis so the dashboard can show
// a history of articles instead of only the latest page. Articles are keyed by
// their URL and stored once with a 4-day TTL — re-fetched duplicates are
// skipped, never overwritten. An index ZSET (member = URL, score = publishedAt)
// provides dedup and newest-first ordering.
package newsstore

import (
	"context"
	"encoding/json"
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
// expires gradually rather than all at once.
const ArticleTTL = 4 * 24 * time.Hour

const (
	articlePrefix = "news:article:"
	indexKey      = "news:index"
)

type Store struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Store {
	return &Store{rdb: rdb}
}

func articleKey(url string) string {
	return articlePrefix + url
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
// overlapping fetches never duplicate or rewrite history.
var storeScript = redis.NewScript(`
local index = KEYS[1]
local prefix = ARGV[1]
local ttl = tonumber(ARGV[#ARGV])
local added = 0
local i = 2
while i < #ARGV do
	if redis.call('SET', prefix .. ARGV[i], ARGV[i+1], 'NX', 'EX', ttl) then
		redis.call('ZADD', index, tonumber(ARGV[i+2]), ARGV[i])
		added = added + 1
	end
	i = i + 3
end
return added
`)

// Store adds articles without overwriting existing ones. A key already present
// keeps its value and original TTL (SET NX); the index dedups on the URL
// member, so the same article never appears twice regardless of how many
// fetches return it.
func (s *Store) Store(ctx context.Context, articles []Article) error {
	args := []interface{}{articlePrefix}
	for _, a := range articles {
		if a.URL == "" {
			continue
		}
		b, err := json.Marshal(a)
		if err != nil {
			continue
		}
		args = append(args, a.URL, b, publishedScore(a))
	}
	if len(args) == 1 {
		return nil
	}
	args = append(args, int64(ArticleTTL/time.Second))
	_, err := storeScript.Run(ctx, s.rdb, []string{indexKey}, args...).Result()
	return err
}

// All returns every stored article, newest first, without duplicates. Index
// members whose article key has expired are dropped lazily, so the index does
// not grow without bound.
func (s *Store) All(ctx context.Context) ([]Article, error) {
	members, err := s.rdb.ZRevRange(ctx, indexKey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, nil
	}

	keys := make([]string, len(members))
	for i, m := range members {
		keys[i] = articleKey(m)
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
		s.rdb.ZRem(ctx, indexKey, staleAny...)
	}
	return out, nil
}
