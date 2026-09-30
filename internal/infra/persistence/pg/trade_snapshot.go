package pg

import (
	"time"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/order"
	"github.com/NicoYazawa/crosspilot/internal/domain/trade"
)

// catalogCurrency 把数据库里的币种字符串还原成领域币种。
func catalogCurrency(raw string) catalog.Currency { return catalog.Currency(raw) }

// buildOrder 由执行信息构造一笔已确认的领域订单。
//
// 订单头与订单行都必须经领域构造器校验：库存扣减成功只说明「数量对得上」，
// 不说明「金额与币种能组成一笔合法订单」。少这一层校验，
// 数据库里就会出现一张总额与逐行金额不符的订单。
func buildOrder(orderID, buyerID string, execution *trade.CreateExecution, now time.Time) (order.Order, error) {
	lines, err := buildLines(execution.Lines, catalogCurrency(execution.Currency))
	if err != nil {
		return order.Order{}, err
	}
	return order.NewConfirmed(orderID, buyerID, lines, execution.Address, now)
}

func buildLines(executionLines []trade.ExecutionLine, currency catalog.Currency) ([]order.Line, error) {
	lines := make([]order.Line, 0, len(executionLines))
	for _, line := range executionLines {
		unitPrice, err := trade.Money(line.UnitPriceMinor, currency)
		if err != nil {
			return nil, err
		}
		built, err := order.NewLine(line.ProductID, line.SKUID, line.Title, unitPrice, int(line.Quantity))
		if err != nil {
			return nil, err
		}
		lines = append(lines, built)
	}
	return lines, nil
}

// orderSnapshotOf 把领域订单转成对外快照。
func orderSnapshotOf(o order.Order) (trade.OrderSnapshot, error) {
	total, err := o.Total()
	if err != nil {
		return trade.OrderSnapshot{}, err
	}
	minor, err := trade.MinorUnits(total)
	if err != nil {
		return trade.OrderSnapshot{}, err
	}
	lines, err := lineSnapshotsOf(o.Lines)
	if err != nil {
		return trade.OrderSnapshot{}, err
	}

	return trade.OrderSnapshot{
		OrderID:          o.ID,
		BuyerID:          o.BuyerID,
		Status:           o.Status,
		TotalAmountMajor: total.Amount.String(),
		Currency:         o.Currency(),
		ShippingAddress:  o.Address.OneLine(),
		Lines:            lines,
		CreatedAt:        o.CreatedAt.UTC(),
		CancelReason:     o.CancelReason,
		TotalAmountMinor: minor,
		AmountScope:      trade.AmountScope,
		OrderKind:        trade.OrderKind,
	}, nil
}

func lineSnapshotsOf(lines []order.Line) ([]trade.LineSnapshot, error) {
	out := make([]trade.LineSnapshot, 0, len(lines))
	for _, line := range lines {
		out = append(out, trade.LineSnapshot{
			ProductID:      line.ProductID,
			SKUID:          line.SKUID,
			Title:          line.Title,
			UnitPriceMajor: line.UnitPrice.Amount.String(),
			Quantity:       int64(line.Quantity),
			Currency:       line.UnitPrice.Currency,
		})
	}
	return out, nil
}

// snapshotOfView 由订单视图重建快照，供取消路径使用。
func snapshotOfView(view trade.OrderView, cancelReason string, cancelledAt time.Time) (trade.OrderSnapshot, error) {
	lines := make([]trade.LineSnapshot, 0, len(view.Items))
	for _, item := range view.Items {
		unitPrice, err := trade.Money(item.UnitPriceMinor, item.Currency)
		if err != nil {
			return trade.OrderSnapshot{}, err
		}
		lines = append(lines, trade.LineSnapshot{
			ProductID:      item.ProductID,
			SKUID:          item.SKUID,
			Title:          item.Title,
			UnitPriceMajor: unitPrice.Amount.String(),
			Quantity:       item.Quantity,
			Currency:       item.Currency,
		})
	}

	total, err := trade.Money(view.TotalAmountMinor, catalogCurrency(view.Currency))
	if err != nil {
		return trade.OrderSnapshot{}, err
	}

	status := view.Status
	// 调用方在取消成功后把视图状态改成 CANCELLED，这里按视图走
	cancelledReason := cancelReason
	if status != order.StatusCancelled {
		cancelledReason = ""
	}
	return trade.OrderSnapshot{
		OrderID:          view.OrderID,
		BuyerID:          view.BuyerID,
		Status:           status,
		TotalAmountMajor: total.Amount.String(),
		Currency:         catalogCurrency(view.Currency),
		ShippingAddress:  view.ShippingAddress.OneLine(),
		Lines:            lines,
		CreatedAt:        cancelledAt.UTC(),
		CancelReason:     cancelledReason,
		TotalAmountMinor: view.TotalAmountMinor,
		AmountScope:      trade.AmountScope,
		OrderKind:        trade.OrderKind,
	}, nil
}

// buildSnapshot 由订单视图与订单头字段构造快照，供读取路径使用。
func buildSnapshot(view trade.OrderView, createdAt *time.Time, cancelReason *string) (trade.OrderSnapshot, error) {
	at := time.Time{}
	if createdAt != nil {
		at = *createdAt
	}
	reason := ""
	if cancelReason != nil {
		reason = *cancelReason
	}
	return snapshotOfView(view, reason, at)
}
