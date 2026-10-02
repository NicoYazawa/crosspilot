// Package embedding 提供文本向量化的 HTTP 客户端与 Redis 缓存。
//
// 缓存以文本哈希为键（见下方各方法），不缓存原始向量请求体——因为缓存键
// 要能被任意调用方复现，而请求体里含模型名等会让人误以为「换模型仍命中」。
package embedding

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

// GetEmbedding returns a cached embedding vector for the given cache key prefix + text.
func (c *Cache) GetEmbedding(ctx context.Context, text string) ([]float32, bool, error) {
	key := "semantic:" + fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	val, err := c.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
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
	if errors.Is(err, redis.Nil) {
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
