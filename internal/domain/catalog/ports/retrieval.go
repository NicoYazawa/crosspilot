package ports

import (
	"context"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// EmbeddingClient produces a dense vector for a text query.
type EmbeddingClient interface {
	// Embed returns a normalized embedding vector for the given text.
	Embed(ctx context.Context, text string) ([]float32, error)
}

// ProductVectorIndex searches for product IDs by embedding similarity.
type ProductVectorIndex interface {
	// Search returns up to topN product IDs with similarity scores (COSINE).
	Search(ctx context.Context, embedding []float32, topN int) ([]VectorHit, error)
}

// VectorHit is a single vector search result.
type VectorHit struct {
	ProductID string
	Score     float32
}

// Reranker re-ranks product candidates by query relevance.
type Reranker interface {
	// Rerank re-ranks the given product IDs, returning scores in the same order.
	Rerank(ctx context.Context, query string, products []catalog.Product) ([]float32, error)
}

// ProductRepository loads product aggregates from the authoritative catalog.
type ProductRepository interface {
	// FindByIDs returns products matching the given IDs, in the same order.
	// Missing IDs are omitted from the result (not errors).
	FindByIDs(ctx context.Context, ids []string) ([]catalog.Product, error)

	// ListAll returns all products in the catalog.
	ListAll(ctx context.Context) ([]catalog.Product, error)
}
