// Package vector 是 Qdrant 向量库的 HTTP 适配器：检索用 Search，灌数据用 Upsert。
//
// 只走 HTTP 而不引入官方 SDK：跨语言调用面窄，HTTP 契约足够稳定，
// 少一个依赖就少一处版本漂移的入口。
package vector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// QdrantClient is a client for the Qdrant vector database.
type QdrantClient struct {
	baseURL    string
	collection string
	httpClient *http.Client
}

// NewQdrantClient creates a new Qdrant client.
func NewQdrantClient(baseURL, collection string) *QdrantClient {
	return &QdrantClient{
		baseURL:    baseURL,
		collection: collection,
		httpClient: &http.Client{Timeout: 30},
	}
}

type searchResult struct {
	ID      string  `json:"id"`
	Score   float32 `json:"score"`
	Payload any     `json:"payload,omitempty"`
}

// Search queries Qdrant for similar product IDs.
func (c *QdrantClient) Search(ctx context.Context, embedding []float32, topN int) ([]Hit, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("qdrant client not configured: baseURL is empty")
	}

	reqBody := map[string]any{
		"vector":       embedding,
		"limit":        topN,
		"with_payload": false,
	}
	reqJSON, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/collections/%s/points/search", c.baseURL, c.collection),
		bytes.NewReader(reqJSON))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	// 响应体是只读流，关闭失败不影响已读内容，显式丢弃即可
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("qdrant search error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Result []searchResult `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	hits := make([]Hit, len(result.Result))
	for i, r := range result.Result {
		hits[i] = Hit{ProductID: r.ID, Score: r.Score}
	}
	return hits, nil
}

// Upsert inserts or updates product vectors in Qdrant.
func (c *QdrantClient) Upsert(ctx context.Context, points []Point) error {
	if c.baseURL == "" {
		return fmt.Errorf("qdrant client not configured")
	}

	reqBody := map[string]any{
		"points": points,
	}
	reqJSON, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/collections/%s/points", c.baseURL, c.collection),
		bytes.NewReader(reqJSON))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	// 同上：只读响应流，关闭错误无需上报
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("qdrant upsert error %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// Point describes a product vector to upsert into Qdrant.
type Point struct {
	ID        string
	Vector    []float32
	ProductID string
}

// Hit is a single vector search result.
type Hit struct {
	ProductID string
	Score     float32
}

// compile-time interface check
var _ interface {
	Search(ctx context.Context, embedding []float32, topN int) ([]Hit, error)
} = (*QdrantClient)(nil)
