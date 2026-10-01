// Package shopping 提供会话期间共享的购物信息。
//
// 源项目用 contextvars.ContextVar 在 12 个模块间隐式传递购物上下文。
// Go 没有 ambient per-goroutine 存储，所以本项目把上下文显式化：
// 整条调用链把 ShoppingContext 当作参数与返回值来搬运。
//
// 这一层没有任何业务规则——它只解决"如何把一份上下文安全地同时传给多个并行子任务"。
package shopping

import (
	"context"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// Context 是单次购物会话的购物上下文。
//
// 它包含买家、当前查询、品类偏好、目的地与预算等。该类型是不可变的：
// 所有修改必须返回新值，避免下游拿到中间状态。
type Context struct {
	BuyerID     string
	SessionID   string
	Query       string
	Category    string
	ShipTo      string
	Currency    catalog.Currency
	BudgetMajor *float64
	Dislikes    []string // dislike 硬约束：不在召回 top-k 里截断，直接屏蔽

	// RecallSnapshot 是上一次召回的命中 ID 集合，供 follow-up 工具查回。
	RecallSnapshot map[string]catalog.Product
	// RequestHash 是 buyer+session+query 的稳定哈希字符串，用于幂等判定。
	RequestHash string
}

// WithBuyer 构造一个新的上下文，返回新值（不可变性）。
func WithBuyer(c Context, buyerID string) Context {
	c.BuyerID = buyerID
	return c
}

// WithSession 构造一个新的上下文，返回新值（不可变性）。
func WithSession(c Context, sessionID string) Context {
	c.SessionID = sessionID
	return c
}

// WithQuery 替换查询文本。
func WithQuery(c Context, query string) Context {
	c.Query = query
	return c
}

// WithCategory 设置指定的品类。
func WithCategory(c Context, category string) Context {
	c.Category = category
	return c
}

// WithShipTo 设置目的地国家代码。
func WithShipTo(c Context, shipTo string) Context {
	c.ShipTo = shipTo
	return c
}

// WithCurrency 设置报价币种。
func WithCurrency(c Context, currency catalog.Currency) Context {
	c.Currency = currency
	return c
}

// WithBudget 设置预算上限（主单位金额）。
func WithBudget(c Context, budgetMajor float64) Context {
	v := budgetMajor
	c.BudgetMajor = &v
	return c
}

// WithDislikes 替换 dislike 列表。
func WithDislikes(c Context, dislikes []string) Context {
	out := make([]string, len(dislikes))
	copy(out, dislikes)
	c.Dislikes = out
	return c
}

// WithRecallSnapshot 记录召回快照。
func WithRecallSnapshot(c Context, snapshot map[string]catalog.Product) Context {
	out := make(map[string]catalog.Product, len(snapshot))
	for k, v := range snapshot {
		out[k] = v
	}
	c.RecallSnapshot = out
	return c
}

// WithRequestHash 设置请求摘要（幂等/审计用）。
func WithRequestHash(c Context, hash string) Context {
	c.RequestHash = hash
	return c
}

// ctxKey 是 ShoppingContext 在 context.Context 里的私有键。
//
// 即便 P3 把上下文显式化为签名传递，工具调用链中某些第三方代码
// （例如 tRPC-Agent-Go 的内置中间件）仍要求从 ctx 拉值；
// 把它放到 context 包以避免传值时的指针对指针对指针。
type ctxKey struct{}

// FromContext 从 Go context.Context 取出 ShoppingContext。
//
// 未设置时返回零值与 false。调用方必须自己判断是否存在——
// 这是显式约定的代价：调用方拿不到任何"安全的默认值"。
func FromContext(ctx context.Context) (Context, bool) {
	v := ctx.Value(ctxKey{})
	if v == nil {
		return Context{}, false
	}
	sc, ok := v.(Context)
	return sc, ok
}

// IntoContext 把 ShoppingContext 塞进 Go context.Context。
//
// 返回新的 context，原 ctx 不被修改。
func IntoContext(ctx context.Context, sc Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, sc)
}

// MustFromContext 取值或返回零值——适合「已经确认设置过」的代码路径。
func MustFromContext(ctx context.Context) Context {
	sc, _ := FromContext(ctx)
	return sc
}
