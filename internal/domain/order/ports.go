package order

import (
	"context"
	"time"
)

// Repository 是订单的持久化端口。
//
// 实现必须满足两条语义，否则交易账本会出现重复下单或重复确认：
//
//  1. Create 在同一幂等键上只成功一次。重复调用返回首次创建的那笔订单，
//     而不是新建一笔；若幂等键相同但内容不同，返回 ErrIdempotencyConflict。
//  2. Transition 是带前置状态条件的原子更新（compare-and-set）。
//     当前状态与 from 不符时不得写入，返回 ErrStaleStatus。
//
// 实现不得返回裸的驱动错误：查不到应包装 ErrNotFound，状态竞争应包装 ErrStaleStatus。
type Repository interface {
	// Create 以幂等键创建订单。
	Create(ctx context.Context, o Order, idempotencyKey string) (Order, error)

	// Get 按标识取订单。不存在时返回 ErrNotFound。
	Get(ctx context.Context, id string) (Order, error)

	// GetByIdempotencyKey 按幂等键取订单，供重试路径复用首次结果。
	GetByIdempotencyKey(ctx context.Context, key string) (Order, error)

	// Transition 原子地把订单从 from 迁移到 to，返回迁移后的订单。
	Transition(ctx context.Context, id string, from, to Status) (Order, error)
}

// Clock 提供当前时间，便于在测试中固定时间推进。
type Clock interface {
	Now() time.Time
}
