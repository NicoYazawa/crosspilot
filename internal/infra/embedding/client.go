package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
)

// Client is an HTTP embedding client for OpenAI-compatible embedding APIs.
type Client struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewClient creates a new embedding client.
func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		httpClient: &http.Client{
			Timeout: 30,
		},
	}
}

type embedRequest struct {
	Input string `json:"input"`
	Model string `json:"model"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns a normalized embedding vector for the given text.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("embedding client not configured: baseURL is empty")
	}

	reqBody, _ := json.Marshal(embedRequest{Input: text, Model: c.model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	// 响应体是只读流，关闭失败不影响已读到的内容，按惯例显式丢弃错误
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedding API error %d: %s", resp.StatusCode, string(body))
	}

	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if len(result.Data) == 0 || len(result.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embedding API returned no data")
	}

	vec := result.Data[0].Embedding
	// Normalize L2
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm > 0 {
		invNorm := 1.0 / math.Sqrt(norm)
		for i := range vec {
			vec[i] = float32(float64(vec[i]) * invNorm)
		}
	}
	return vec, nil
}
