// Package order 是订单的领域模型：订单、订单行、收货地址与状态机。
//
// 状态流转由 CanTransitionTo 与 Transition 统一把关，终态不可再变；
// 非法流转返回错误而不是静默忽略。本包不做 I/O，持久化由外层实现。
package order

import "errors"

var (
	// ErrInvalidOrder 表示订单不满足领域约束。
	ErrInvalidOrder = errors.New("order: 非法订单")

	// ErrInvalidStatus 表示订单状态不是已知取值。
	ErrInvalidStatus = errors.New("order: 非法订单状态")

	// ErrIllegalTransition 表示订单状态不允许做该迁移。
	ErrIllegalTransition = errors.New("order: 非法状态迁移")

	// ErrEmptyLines 表示订单没有任何订单行。
	ErrEmptyLines = errors.New("order: 订单行不能为空")

	// ErrCurrencyMismatch 表示订单内的币种不一致。
	ErrCurrencyMismatch = errors.New("order: 币种不一致")

	// ErrNotFound 表示按标识未查到订单。
	ErrNotFound = errors.New("order: 订单不存在")

	// ErrIdempotencyConflict 表示同一幂等键下存在内容不同的订单。
	ErrIdempotencyConflict = errors.New("order: 幂等键冲突")

	// ErrStaleStatus 表示订单当前状态与预期不符，迁移被拒绝。
	ErrStaleStatus = errors.New("order: 状态已变更")

	// ErrCancelReasonRequired 表示取消订单时没有给出原因。
	ErrCancelReasonRequired = errors.New("order: 取消必须给出原因")
)
