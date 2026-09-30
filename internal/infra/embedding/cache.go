package embedding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Cache wraps a Redis client to provide embedding and semantic caches.
type Cache struct {
	client *redis.Client
	ttl    time.Duration
}

// NewCache creates a new embedding cache backed by Redis.
func NewCache(client *redis.Client, ttl time.Duration) *Cache {
	return &Cache{client: client, ttl: ttl}
}

// embeddingCacheKey returns the cache key for an embedding request.
func embeddingCacheKey(embedding []float32) string {
	h := sha256.Sum256(asBytes(embedding))
	return "emb:" + hex.EncodeToString(h[:8])
}

func asBytes(v []float32) []byte {
	b, _ := json.Marshal(v)
	return b
}

// GetEmbedding returns a cached embedding vector for the given cache key prefix + text.
func (c *Cache) GetEmbedding(ctx context.Context, text string) ([]float32, bool, error) {
	key := "semantic:" + fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	val, err := c.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var vec []float32
	if err := json.Unmarshal(val, &vec); err != nil {
		return nil, false, err
	}
	return vec, true, nil
}

// SetEmbedding stores an embedding vector in cache.
func (c *Cache) SetEmbedding(ctx context.Context, text string, vec []float32) error {
	key := "semantic:" + fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	data, err := json.Marshal(vec)
	if err != nil {
		return err
	}
	return c.client.Set(ctx, key, data, c.ttl).Err()
}

// GetEmbeddingByHash returns a cached embedding by its hash key.
func (c *Cache) GetEmbeddingByHash(ctx context.Context, hash string) ([]float32, bool, error) {
	key := "emb:" + hash
	val, err := c.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var vec []float32
	if err := json.Unmarshal(val, &vec); err != nil {
		return nil, false, err
	}
	return vec, true, nil
}

// SetEmbeddingByHash stores an embedding vector by its hash key.
func (c *Cache) SetEmbeddingByHash(ctx context.Context, hash string, vec []float32) error {
	key := "emb:" + hash
	data, err := json.Marshal(vec)
	if err != nil {
		return err
	}
	return c.client.Set(ctx, key, data, c.ttl).Err()
}
