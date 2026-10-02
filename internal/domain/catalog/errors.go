// Package catalog 是商品目录的领域模型：金额、币种、汇率、商品与检索条件。
//
// 本包不依赖任何基础设施，也不做 I/O。金额一律用 Money 表示，其小数位由币种
// 决定；任何溢出或精度损失都返回错误，不会静默降级。
package catalog

import "errors"

// 领域错误一律以 sentinel 形式导出，由上层用 errors.Is 判定，
// 不在此层做日志、不 panic、不携带 HTTP 语义。
var (
	// ErrInvalidCurrency 表示币种代码不是合法的三位大写字母。
	ErrInvalidCurrency = errors.New("catalog: 非法币种")

	// ErrCurrencyMismatch 表示对不同币种的金额做了加减比较。
	ErrCurrencyMismatch = errors.New("catalog: 币种不一致")

	// ErrInvalidAmount 表示金额为 NaN 或 Inf。
	ErrInvalidAmount = errors.New("catalog: 非法金额")

	// ErrInvalidRate 表示汇率不是有限正数。
	ErrInvalidRate = errors.New("catalog: 非法汇率")

	// ErrSameCurrency 表示把同一币种当作两种币种建兑换关系。
	ErrSameCurrency = errors.New("catalog: 币种相同，无需兑换")

	// ErrNotFound 表示按标识未查到商品。
	ErrNotFound = errors.New("catalog: 商品不存在")

	// ErrInvalidProduct 表示商品不满足领域约束。
	ErrInvalidProduct = errors.New("catalog: 非法商品")

	// ErrAmountOverflow 表示金额超出该币种小数位下可表示的范围。
	ErrAmountOverflow = errors.New("catalog: 金额溢出")

	// ErrInexactResult 表示运算结果必须舍入才能在币种小数位内表示。
	ErrInexactResult = errors.New("catalog: 结果不精确")
)
