package ports

import (
	"context"
)

// KnowledgeChunk is a single retrieved knowledge fragment.
type KnowledgeChunk struct {
	Content string
	Score   float32
	Metadata map[string]string
}

// CategoryKnowledgeBase retrieves category domain knowledge.
type CategoryKnowledgeBase interface {
	// Search returns top-K knowledge chunks relevant to the query.
	Search(ctx context.Context, query string, topK int) ([]KnowledgeChunk, error)
}
