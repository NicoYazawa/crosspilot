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
type Order struct {
	ID        string
	BuyerID   string
	SessionID string
	Status    Status
	Lines     []Line
	Address   Address
	CreatedAt time.Time
	UpdatedAt time.Time
}

// New 构造一笔待确认订单。
func New(id, buyerID, sessionID string, lines []Line, address Address, now time.Time) (Order, error) {
	o := Order{
		ID:        id,
		BuyerID:   buyerID,
		SessionID: sessionID,
		Status:    StatusPending,
		Lines:     lines,
		Address:   address,
		CreatedAt: now,
		UpdatedAt: now,
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
func (o Order) Transition(from, to Status, now time.Time) (Order, error) {
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
	next.UpdatedAt = now
	return next, nil
}

// IsTerminal 报告订单是否已进入终态。
func (o Order) IsTerminal() bool { return o.Status.IsTerminal() }
