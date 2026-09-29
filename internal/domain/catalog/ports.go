package catalog

import "context"

// ProductRepository 是商品目录的持久化端口。
//
// 接口定义在领域层、由基础设施层实现，依赖方向因此指向内侧。
// 实现不得返回裸的驱动错误：查不到应返回包装了 ErrNotFound 的错误。
type ProductRepository interface {
	// Get 按标识取一件商品。不存在时返回 ErrNotFound。
	Get(ctx context.Context, id string) (Product, error)

	// List 按条件检索商品。spec 需先经 Validate，实现内部自行 Normalize。
	List(ctx context.Context, spec ProductSearchSpec) ([]Product, error)
}

// ProductIndex 是商品向量索引端口，服务于语义检索与召回。
type ProductIndex interface {
	// Upsert 写入或覆盖一批商品的向量。
	Upsert(ctx context.Context, products []Product, vectors [][]float32) error

	// Query 按查询向量召回最相近的商品。
	Query(ctx context.Context, vector []float32, spec ProductSearchSpec) ([]Product, error)
}

// ExchangeRateProvider 是汇率来源端口。
type ExchangeRateProvider interface {
	// Rate 返回 base 兑 quote 的汇率。无可用汇率时返回错误。
	Rate(ctx context.Context, base, quote Currency) (ExchangeRate, error)
}
