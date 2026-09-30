package order

import (
	"fmt"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// Order 是一笔订单。
//
// 状态只能经 Transition 迁移，不能直接赋值：状态机是交易账本的核心不变量，
// 绕过它就会产生账实不符的订单。
//
// ConfirmedAt / CancelledAt 是状态迁移的见证字段，而不是可以由调用方随手写的
// 普通时间戳——它们只在对应迁移发生时被填上。
type Order struct {
	ID           string
	BuyerID      string
	Status       Status
	Lines        []Line
	Address      Address
	CreatedAt    time.Time
	ConfirmedAt  *time.Time
	CancelledAt  *time.Time
	CancelReason string
}

// NewDraft 构造一笔草稿订单。
func NewDraft(id, buyerID string, lines []Line, address Address, now time.Time) (Order, error) {
	o := Order{
		ID:        id,
		BuyerID:   buyerID,
		Status:    StatusDraft,
		Lines:     lines,
		Address:   address,
		CreatedAt: now,
	}
	if err := o.Validate(); err != nil {
		return Order{}, err
	}
	return o, nil
}

// NewConfirmed 构造一笔已确认订单。
//
// 交易账本在确认生效的同一个事务里落库，因此订单没有「先建后确认」的中间态；
// 直接构造出目标状态比先建草稿再迁移更能表达这一点。
func NewConfirmed(id, buyerID string, lines []Line, address Address, now time.Time) (Order, error) {
	o := Order{
		ID:          id,
		BuyerID:     buyerID,
		Status:      StatusConfirmed,
		Lines:       lines,
		Address:     address,
		CreatedAt:   now,
		ConfirmedAt: &now,
	}
	if err := o.Validate(); err != nil {
		return Order{}, err
	}
	return o, nil
}

// Validate 报告订单是否满足约束。
func (o Order) Validate() error {
	if o.ID == "" {
		return fmt.Errorf("%w: 订单标识为空", ErrInvalidOrder)
	}
	if o.BuyerID == "" {
		return fmt.Errorf("%w: 订单 %s 缺少买家标识", ErrInvalidOrder, o.ID)
	}
	if !o.Status.Valid() {
		return fmt.Errorf("%w: 订单 %s 状态 %q", ErrInvalidStatus, o.ID, string(o.Status))
	}
	if len(o.Lines) == 0 {
		return fmt.Errorf("%w: 订单 %s", ErrEmptyLines, o.ID)
	}
	for i, line := range o.Lines {
		if err := line.Validate(); err != nil {
			return fmt.Errorf("%w: 订单 %s 第 %d 行: %w", ErrInvalidOrder, o.ID, i, err)
		}
	}
	if err := o.validateUniformCurrency(); err != nil {
		return err
	}
	if err := o.Address.Validate(); err != nil {
		return err
	}
	if o.Status == StatusCancelled {
		if o.CancelledAt == nil {
			return fmt.Errorf("%w: 订单 %s 已取消但缺少取消时间", ErrInvalidOrder, o.ID)
		}
		if o.CancelReason == "" {
			return fmt.Errorf("%w: 订单 %s 已取消但缺少取消原因", ErrInvalidOrder, o.ID)
		}
	}
	return nil
}

// validateUniformCurrency 确认所有订单行使用同一币种。
// 调用方需保证 Lines 非空（Validate 已先行校验）。
func (o Order) validateUniformCurrency() error {
	base := o.Lines[0].UnitPrice.Currency
	for _, line := range o.Lines[1:] {
		if line.UnitPrice.Currency != base {
			return fmt.Errorf("%w: 订单 %s 同时含 %s 与 %s",
				ErrCurrencyMismatch, o.ID, base, line.UnitPrice.Currency)
		}
	}
	return nil
}

// Currency 返回订单币种，取自订单行。空订单返回空币种。
func (o Order) Currency() catalog.Currency {
	if len(o.Lines) == 0 {
		return ""
	}
	return o.Lines[0].UnitPrice.Currency
}

// Total 返回订单总额，即各行小计之和。币种取自订单行。
func (o Order) Total() (catalog.Money, error) {
	if len(o.Lines) == 0 {
		return catalog.Money{}, fmt.Errorf("%w: 订单 %s", ErrEmptyLines, o.ID)
	}

	total := catalog.Zero(o.Lines[0].UnitPrice.Currency)
	for _, line := range o.Lines {
		subtotal, err := line.Subtotal()
		if err != nil {
			return catalog.Money{}, err
		}
		sum, err := total.Add(subtotal)
		if err != nil {
			return catalog.Money{}, fmt.Errorf("order: 订单 %s 累计失败: %w", o.ID, err)
		}
		total = sum
	}
	return total, nil
}

// Transition 校验并执行状态迁移，返回迁移后的订单副本。
// 当前状态不是 from 时返回 ErrStaleStatus；迁移本身不合法时返回 ErrIllegalTransition。
//
// 取消迁移必须给出非空原因；缺少原因返回 ErrCancelReasonRequired，而不是写下一张
// 没有取消理由却已取消的订单。
func (o Order) Transition(from, to Status, now time.Time, cancelReason string) (Order, error) {
	if o.Status != from {
		return Order{}, fmt.Errorf("%w: 订单 %s 当前为 %s，预期 %s",
			ErrStaleStatus, o.ID, o.Status, from)
	}
	if !from.CanTransitionTo(to) {
		return Order{}, fmt.Errorf("%w: 订单 %s 不能从 %s 迁移到 %s",
			ErrIllegalTransition, o.ID, from, to)
	}

	next := o
	next.Status = to
	switch to {
	case StatusDraft:
		// 回到草稿不是合法迁移（CanTransitionTo 已经拦下），
		// 列在这里只是为了让编译器确认所有状态都被显式考虑过。
	case StatusConfirmed:
		next.ConfirmedAt = &now
	case StatusCancelled:
		if cancelReason == "" {
			return Order{}, fmt.Errorf("%w: 订单 %s", ErrCancelReasonRequired, o.ID)
		}
		next.CancelledAt = &now
		next.CancelReason = cancelReason
	}
	return next, nil
}

// IsTerminal 报告订单是否已进入终态。
func (o Order) IsTerminal() bool { return o.Status.IsTerminal() }

// IsCancelable 报告订单当前是否可以被取消，即处于 CONFIRMED。
func (o Order) IsCancelable() bool { return o.Status == StatusConfirmed }
