package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/NicoYazawa/crosspilot/internal/domain/order"
	"github.com/NicoYazawa/crosspilot/internal/domain/trade"
)

// 编译期确认本类型实现了领域端口。
var _ trade.Store = (*Store)(nil)

// InitializeInventory 用商品目录初始化或刷新库存。
//
// 两条语义必须同时成立：
//
//   - 新 SKU 落库时扣掉「历史已确认订单占用的数量」，否则重建库存会让已卖出的货
//     重新变成可卖——这是超卖的另一种形式，且比并发超卖更隐蔽；
//   - 已存在的 SKU 只刷新报价与展示字段，绝不重置库存数量。
//
// 全程只读两次（一次全量库存、一次历史占用聚合），不按 SKU 逐个查询：
// 目录扩张到上千 SKU 时，逐条查询会把启动拖成分钟级。
func (s *Store) InitializeInventory(ctx context.Context, skus []trade.SeedSKU) error {
	if len(skus) == 0 {
		return nil
	}

	// 在事务之前校验：参数错误不该占用一把数据库锁
	seen := make(map[string]struct{}, len(skus))
	for _, sku := range skus {
		if sku.SKUID == "" {
			return trade.Errorf(trade.CodeInvalidArgument, "sku_id 必须为非空字符串")
		}
		if _, dup := seen[sku.SKUID]; dup {
			return trade.Errorf(trade.CodeInvalidArgument, "目录中存在重复 SKU：%s", sku.SKUID)
		}
		seen[sku.SKUID] = struct{}{}
		if sku.Stock < 0 {
			return trade.Errorf(trade.CodeInvalidArgument, "stock 必须为不小于 0 的整数")
		}
		if sku.UnitPriceMinor < 0 {
			return trade.Errorf(trade.CodeInvalidArgument, "unit_price_minor 必须为不小于 0 的整数")
		}
		if !sku.Currency.Valid() {
			return trade.Errorf(trade.CodeInvalidArgument, "currency 必须为受支持的三位代码，实际 %q", string(sku.Currency))
		}
	}

	return s.retry(ctx, "initialize_inventory", func(ctx context.Context) error {
		return s.inTx(ctx, "initialize_inventory", func(tx pgx.Tx) error {
			if err := lockInventoryInitialization(ctx, tx); err != nil {
				return err
			}

			existing, err := loadAllInventory(ctx, tx)
			if err != nil {
				return err
			}
			occupied, err := loadOccupiedStock(ctx, tx)
			if err != nil {
				return err
			}

			for _, sku := range skus {
				row, ok := existing[sku.SKUID]
				if !ok {
					taken := occupied[sku.SKUID]
					if taken < 0 || taken > sku.Stock {
						return trade.Errorf(trade.CodeInventoryMigrationRequired,
							"SKU %s 的历史订单占用超出库存种子，请核对迁移", sku.SKUID)
					}
					if err := insertInventory(ctx, tx, sku, sku.Stock-taken); err != nil {
						return err
					}
					continue
				}
				if row.ProductID != sku.ProductID {
					return trade.Errorf(trade.CodeInvalidArgument, "已有 SKU 不可迁移到另一个商品：%s", sku.SKUID)
				}
				if err := refreshInventoryQuote(ctx, tx, sku); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// inventoryRow 是一条已存在的库存行。
type inventoryRow struct {
	ProductID string
	Stock     int64
}

func loadAllInventory(ctx context.Context, tx pgx.Tx) (map[string]inventoryRow, error) {
	rows, err := tx.Query(ctx, `SELECT sku_id, product_id, stock FROM trade_sku_inventory`)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取库存快照失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string]inventoryRow)
	for rows.Next() {
		var skuID string
		var row inventoryRow
		if err := rows.Scan(&skuID, &row.ProductID, &row.Stock); err != nil {
			return nil, fmt.Errorf("pg: 扫描库存行失败: %w", err)
		}
		out[skuID] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 遍历库存行失败: %w", err)
	}
	return out, nil
}

// loadOccupiedStock 汇总已确认订单已经占用的数量。
//
// 只统计 CONFIRMED：取消的订单已经把库存还回去了，再算一次会重复扣减。
func loadOccupiedStock(ctx context.Context, tx pgx.Tx) (map[string]int64, error) {
	const query = `
SELECT line.sku_id, COALESCE(SUM(line.quantity), 0)
FROM order_lines AS line
JOIN orders AS o ON o.order_id = line.order_id
WHERE o.status = 'CONFIRMED'
GROUP BY line.sku_id`

	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("pg: 汇总历史占用失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var skuID string
		var quantity int64
		if err := rows.Scan(&skuID, &quantity); err != nil {
			return nil, fmt.Errorf("pg: 扫描历史占用失败: %w", err)
		}
		out[skuID] = quantity
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 遍历历史占用失败: %w", err)
	}
	return out, nil
}

func insertInventory(ctx context.Context, tx pgx.Tx, sku trade.SeedSKU, stock int64) error {
	const query = `
INSERT INTO trade_sku_inventory (sku_id, product_id, title, stock, unit_price_minor, currency)
VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := tx.Exec(ctx, query, sku.SKUID, sku.ProductID, sku.Title, stock, sku.UnitPriceMinor, string(sku.Currency))
	if err != nil {
		return fmt.Errorf("pg: 写入库存行 %s 失败: %w", sku.SKUID, err)
	}
	return nil
}

// refreshInventoryQuote 只更新报价与展示字段。
//
// 刻意不更新 stock：重启、目录刷新、多实例并发初始化都不应该改变可售数量。
func refreshInventoryQuote(ctx context.Context, tx pgx.Tx, sku trade.SeedSKU) error {
	const query = `
UPDATE trade_sku_inventory
SET title = $2, unit_price_minor = $3, currency = $4
WHERE sku_id = $1`
	if _, err := tx.Exec(ctx, query, sku.SKUID, sku.Title, sku.UnitPriceMinor, string(sku.Currency)); err != nil {
		return fmt.Errorf("pg: 刷新库存报价 %s 失败: %w", sku.SKUID, err)
	}
	return nil
}

// Inventory 返回指定 SKU 的库存；skus 为空表示全部。
func (s *Store) Inventory(ctx context.Context, skus []string) (map[string]int64, error) {
	const base = `SELECT sku_id, stock FROM trade_sku_inventory`
	var (
		rows pgx.Rows
		err  error
	)
	if len(skus) == 0 {
		rows, err = s.pool.Query(ctx, base)
	} else {
		rows, err = s.pool.Query(ctx, base+` WHERE sku_id = ANY($1)`, skus)
	}
	if err != nil {
		return nil, fmt.Errorf("pg: 读取库存失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var skuID string
		var stock int64
		if err := rows.Scan(&skuID, &stock); err != nil {
			return nil, fmt.Errorf("pg: 扫描库存失败: %w", err)
		}
		out[skuID] = stock
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 遍历库存失败: %w", err)
	}
	return out, nil
}

// Prepare 建立或复取一张确认单。
//
// 整个流程在一个事务、一个按操作编号命名的咨询锁之内完成：
//
//  1. 取锁——同一操作编号的并发请求在这里排队；
//  2. 读既有确认单——有则走幂等重放；
//  3. 读本次涉及的全部库存行与（取消时的）订单；
//  4. 交给领域层做校验、规范化与算摘要；
//  5. 写入确认单。
//
// 唯一约束冲突被显式捕获并重试整轮：这表示有另一个请求刚刚先写进去了一行，
// 重试会走到第 2 步读到它并重放。把冲突当成失败会让并发重试的调用方
// 收到一个本不该有的错误。
func (s *Store) Prepare(ctx context.Context, req trade.PrepareRequest) (trade.Confirmation, error) {
	var confirmation trade.Confirmation
	err := s.retry(ctx, "prepare_confirmation", func(ctx context.Context) error {
		return s.inTx(ctx, "prepare_confirmation", func(tx pgx.Tx) error {
			if err := lockOperation(ctx, tx, req.OperationID); err != nil {
				return err
			}

			// PrepareRequest 与 PrepareInput 字段逐个对应且类型一致，
			// 直接转换即可；逐字段写字面量只会多一处需要跟着改的抄写点。
			input := trade.PrepareInput(req)

			existing, found, err := loadConfirmationByOperation(ctx, tx, req.OperationID)
			if err != nil {
				return err
			}

			deps := trade.PrepareDeps{Now: s.clock.Now()}
			if found {
				// 幂等重放：既有确认单本身就带着当时的载荷与摘要，
				// 不需要（也不应该）再读一次库存与价格。
				deps = deps.WithExisting(existing)
			}

			// 取消载荷必须由订单快照重建，因此无论是否重放都要读到订单。
			// 只在「新建」分支里读会让取消的重放直接失败：领域层算不出请求摘要，
			// 于是同一个操作编号的第二次调用拿到 NOT_FOUND 而不是既有的确认单
			// ——端口契约里「重复调用返回首次建立的那张」在取消动作上就不成立了。
			// 代价是取消重放多一次订单查询；取消请求本就远少于下单，这个代价换的是契约成立。
			if !found || input.Action == trade.ActionCancel {
				inventory, orderView, err := loadPrepareContext(ctx, tx, input)
				if err != nil {
					return err
				}
				deps = deps.WithInventory(inventory).WithOrder(orderView)
			}

			output, err := trade.Prepare(input, deps)
			if err != nil {
				return err
			}

			if output.Replayed {
				confirmation = output.Confirmation
				return nil
			}
			if err := insertConfirmation(ctx, tx, output.Confirmation); err != nil {
				if isUniqueViolation(err) {
					return &retryableUniqueError{err: err}
				}
				return err
			}
			confirmation = output.Confirmation
			return nil
		})
	})
	if err != nil {
		return trade.Confirmation{}, err
	}
	return confirmation, nil
}

// loadPrepareContext 读齐 prepare 需要的外部信息：库存行与（取消时的）订单。
func loadPrepareContext(ctx context.Context, tx pgx.Tx, input trade.PrepareInput) (map[string]trade.InventoryRecord, *trade.OrderView, error) {
	if input.Action == trade.ActionCancel {
		orderView, err := loadOrderView(ctx, tx, input.OrderID)
		if err != nil {
			return nil, nil, err
		}
		return nil, orderView, nil
	}

	skuIDs := make([]string, 0, len(input.Items))
	for _, item := range input.Items {
		skuIDs = append(skuIDs, item.SKUID)
	}
	inventory, err := loadInventoryRecords(ctx, tx, skuIDs)
	if err != nil {
		return nil, nil, err
	}
	return inventory, nil, nil
}

func loadInventoryRecords(ctx context.Context, tx pgx.Tx, skuIDs []string) (map[string]trade.InventoryRecord, error) {
	if len(skuIDs) == 0 {
		return map[string]trade.InventoryRecord{}, nil
	}

	const query = `
SELECT sku_id, product_id, title, stock, unit_price_minor, currency
FROM trade_sku_inventory
WHERE sku_id = ANY($1)`

	rows, err := tx.Query(ctx, query, skuIDs)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取库存报价失败: %w", err)
	}
	defer rows.Close()

	out := make(map[string]trade.InventoryRecord, len(skuIDs))
	for rows.Next() {
		var record trade.InventoryRecord
		var currency string
		if err := rows.Scan(&record.SKUID, &record.ProductID, &record.Title,
			&record.Stock, &record.UnitPriceMinor, &currency); err != nil {
			return nil, fmt.Errorf("pg: 扫描库存报价失败: %w", err)
		}
		record.Currency = catalogCurrency(currency)
		out[record.SKUID] = record
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 遍历库存报价失败: %w", err)
	}
	return out, nil
}

// loadOrderView 读取取消确认需要比对的订单快照。
//
// 它必须带上收货地址：地址是取消载荷的一部分，少了它，
// 「订单地址被改过」这件事在决议时就彻底看不见了。
func loadOrderView(ctx context.Context, tx pgx.Tx, orderID string) (*trade.OrderView, error) {
	if orderID == "" {
		return nil, nil
	}

	const orderQuery = `
SELECT order_id, buyer_id, status, currency, total_amount_minor, shipping_address_json
FROM orders
WHERE order_id = $1`

	var (
		view        trade.OrderView
		currency    string
		addressJSON []byte
	)
	err := tx.QueryRow(ctx, orderQuery, orderID).Scan(
		&view.OrderID, &view.BuyerID, &view.Status, &currency, &view.TotalAmountMinor, &addressJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pg: 读取订单失败: %w", err)
	}
	view.Currency = currency

	if len(addressJSON) > 0 {
		if err := json.Unmarshal(addressJSON, &view.ShippingAddress); err != nil {
			return nil, fmt.Errorf("pg: 订单 %s 的收货地址无法解析: %w", orderID, err)
		}
	}

	const linesQuery = `
SELECT product_id, sku_id, title, unit_price_minor, currency, quantity
FROM order_lines
WHERE order_id = $1
ORDER BY sku_id`

	rows, err := tx.Query(ctx, linesQuery, orderID)
	if err != nil {
		return nil, fmt.Errorf("pg: 读取订单行失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			item     trade.Item
			lineCur  string
			quantity int64
		)
		if err := rows.Scan(&item.ProductID, &item.SKUID, &item.Title,
			&item.UnitPriceMinor, &lineCur, &quantity); err != nil {
			return nil, fmt.Errorf("pg: 扫描订单行失败: %w", err)
		}
		item.Currency = catalogCurrency(lineCur)
		item.Quantity = quantity
		view.Items = append(view.Items, item)
		view.SKUs = append(view.SKUs, item.SKUID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 遍历订单行失败: %w", err)
	}
	return &view, nil
}

func insertConfirmation(ctx context.Context, tx pgx.Tx, c trade.Confirmation) error {
	payload, err := json.Marshal(c.Payload)
	if err != nil {
		return fmt.Errorf("pg: 序列化确认单载荷失败: %w", err)
	}

	const query = `
INSERT INTO trade_confirmations (
    confirmation_id, operation_id, buyer_id, session_id, action,
    request_hash, payload, snapshot_hash, expires_at, status, result, created_at, resolved_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`

	if _, err := tx.Exec(ctx, query,
		c.ConfirmationID, c.OperationID, c.BuyerID, c.SessionID, string(c.Action),
		c.RequestHash, payload, c.SnapshotHash, c.ExpiresAt.UTC(), string(c.Status), nil, c.CreatedAt.UTC(), nil,
	); err != nil {
		return fmt.Errorf("pg: 写入确认单失败: %w", err)
	}
	return nil
}

// confirmationColumns 是读取确认单的列清单，供多处复用。
const confirmationColumns = `
confirmation_id, operation_id, buyer_id, session_id, action,
request_hash, payload, snapshot_hash, expires_at, status, result, created_at, resolved_at`

// scanConfirmation 把一行确认单读成领域对象。
func scanConfirmation(row pgx.Row) (trade.Confirmation, error) {
	var (
		c          trade.Confirmation
		action     string
		status     string
		payloadRaw []byte
		resultRaw  []byte
	)
	err := row.Scan(
		&c.ConfirmationID, &c.OperationID, &c.BuyerID, &c.SessionID, &action,
		&c.RequestHash, &payloadRaw, &c.SnapshotHash, &c.ExpiresAt, &status, &resultRaw, &c.CreatedAt, &c.ResolvedAt,
	)
	if err != nil {
		return trade.Confirmation{}, err
	}
	c.Action = trade.Action(action)
	c.Status = trade.ConfirmationStatus(status)
	// 时间统一按 UTC 读回：摘要覆盖有效期，而 pgx 读回的 timestamptz
	// 携带数据库会话的时区。不归一就会让「写入前的摘要」与
	// 「读回后重算的摘要」不相等，决议将永远无法通过快照校验。
	c.ExpiresAt = c.ExpiresAt.UTC()
	c.CreatedAt = c.CreatedAt.UTC()
	if c.ResolvedAt != nil {
		at := c.ResolvedAt.UTC()
		c.ResolvedAt = &at
	}

	if err := json.Unmarshal(payloadRaw, &c.Payload); err != nil {
		return trade.Confirmation{}, fmt.Errorf("pg: 确认单 %s 的载荷无法解析: %w", c.ConfirmationID, err)
	}
	if len(resultRaw) > 0 {
		var snapshot trade.OrderSnapshot
		if err := json.Unmarshal(resultRaw, &snapshot); err != nil {
			return trade.Confirmation{}, fmt.Errorf("pg: 确认单 %s 的结果无法解析: %w", c.ConfirmationID, err)
		}
		c.Result = &snapshot
	}
	return c, nil
}

func loadConfirmationByOperation(ctx context.Context, tx pgx.Tx, operationID string) (trade.Confirmation, bool, error) {
	query := `SELECT ` + confirmationColumns + ` FROM trade_confirmations WHERE operation_id = $1`
	c, err := scanConfirmation(tx.QueryRow(ctx, query, operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return trade.Confirmation{}, false, nil
	}
	if err != nil {
		return trade.Confirmation{}, false, fmt.Errorf("pg: 读取确认单失败: %w", err)
	}
	return c, true, nil
}

// Confirmation 按标识取确认单。不存在返回 CodeNotFound。
func (s *Store) Confirmation(ctx context.Context, confirmationID, buyerID, sessionID string) (trade.Confirmation, error) {
	query := `SELECT ` + confirmationColumns + ` FROM trade_confirmations WHERE confirmation_id = $1`
	c, err := scanConfirmation(s.pool.QueryRow(ctx, query, confirmationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return trade.Confirmation{}, trade.Errorf(trade.CodeNotFound, "交易确认不存在")
	}
	if err != nil {
		return trade.Confirmation{}, fmt.Errorf("pg: 读取确认单失败: %w", err)
	}
	if c.BuyerID != buyerID || c.SessionID != sessionID {
		return trade.Confirmation{}, trade.Errorf(trade.CodeOwnerMismatch, "无权访问此买家或会话的交易确认")
	}
	return c, nil
}

// Confirmations 列出当前买家与会话最近的确认单。
func (s *Store) Confirmations(ctx context.Context, buyerID, sessionID string, limit int) ([]trade.Confirmation, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 20 {
		limit = 20
	}

	query := `SELECT ` + confirmationColumns + `
FROM trade_confirmations
WHERE buyer_id = $1 AND session_id = $2
ORDER BY created_at DESC, confirmation_id DESC
LIMIT $3`

	rows, err := s.pool.Query(ctx, query, buyerID, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("pg: 列出确认单失败: %w", err)
	}
	defer rows.Close()

	var out []trade.Confirmation
	for rows.Next() {
		c, err := scanConfirmation(rows)
		if err != nil {
			return nil, fmt.Errorf("pg: 扫描确认单失败: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 遍历确认单失败: %w", err)
	}
	return out, nil
}

// Resolve 执行用户对确认单的决议。
//
// 事务内的顺序与领域层 Resolve 的顺序一致，且每一步都不可调换：
// 读确认单 → 身份校验 → 快照校验 → 幂等返回 → 过期检查 → 执行 → 记录。
//
// 「幂等返回」必须排在「过期检查」之前：网络重试时确认单可能早已过期，
// 而交易其实已经完成；此时报「已过期」会让调用方以为交易没做，
// 进而重新下单，产生第二笔交易。
//
// 确认单行用 FOR UPDATE 锁住：决议是「读状态、判断、再写状态」，
// 不加行锁的话两个并发决议会同时读到 pending。
func (s *Store) Resolve(
	ctx context.Context,
	confirmationID, buyerID, sessionID, snapshotHash string,
	decision trade.Decision,
) (trade.Confirmation, error) {
	var resolved trade.Confirmation

	err := s.retry(ctx, "resolve_confirmation", func(ctx context.Context) error {
		return s.inTx(ctx, "resolve_confirmation", func(tx pgx.Tx) error {
			query := `SELECT ` + confirmationColumns + ` FROM trade_confirmations WHERE confirmation_id = $1 FOR UPDATE`
			stored, err := scanConfirmation(tx.QueryRow(ctx, query, confirmationID))
			if errors.Is(err, pgx.ErrNoRows) {
				return trade.Errorf(trade.CodeNotFound, "交易确认不存在")
			}
			if err != nil {
				return fmt.Errorf("pg: 读取确认单失败: %w", err)
			}

			output, err := trade.Resolve(stored, trade.ResolveInput{
				ConfirmationID: confirmationID,
				BuyerID:        buyerID,
				SessionID:      sessionID,
				SnapshotHash:   snapshotHash,
				Decision:       decision,
			}, s.clock.Now())
			if err != nil {
				return err
			}

			switch {
			case output.Decided:
				// 已决议：原样返回首次结果，一行都不写
				resolved = output.Confirmation
				return nil

			case output.Create == nil && output.Cancel == nil:
				// 拒绝：只改状态并记录决议，不碰库存与订单
				finalized, err := finalizeRejection(ctx, tx, output.Confirmation, s.clockNow())
				if err != nil {
					return err
				}
				resolved = finalized
				return nil

			case output.Create != nil:
				result, err := s.executeCreate(ctx, tx, output.Confirmation, output.Create)
				if err != nil {
					return err
				}
				finalized, err := finalizeApproval(ctx, tx, output.Confirmation, result, s.clockNow())
				if err != nil {
					return err
				}
				resolved = finalized
				return nil

			default:
				result, err := s.executeCancel(ctx, tx, output.Confirmation, output.Cancel)
				if err != nil {
					return err
				}
				finalized, err := finalizeApproval(ctx, tx, output.Confirmation, result, s.clockNow())
				if err != nil {
					return err
				}
				resolved = finalized
				return nil
			}
		})
	})
	if err != nil {
		return trade.Confirmation{}, err
	}
	return resolved, nil
}

// executeCreate 扣减库存并写入订单，返回订单快照。
func (s *Store) executeCreate(
	ctx context.Context,
	tx pgx.Tx,
	confirmation trade.Confirmation,
	execution *trade.CreateExecution,
) (trade.OrderSnapshot, error) {
	for _, line := range execution.Lines {
		// 比较并更新：条件全部写在 WHERE 里，由数据库在持锁状态下判定。
		// 先读一次再判断会留下窗口——两个事务都能读到「够」，然后都扣。
		outcome, err := tx.Exec(ctx, `
UPDATE trade_sku_inventory
SET stock = stock - $2
WHERE sku_id = $1
  AND stock >= $2
  AND unit_price_minor = $3
  AND currency = $4`,
			line.SKUID, line.Quantity, line.UnitPriceMinor, execution.Currency)
		if err != nil {
			return trade.OrderSnapshot{}, fmt.Errorf("pg: 扣减库存失败: %w", err)
		}
		if outcome.RowsAffected() == 1 {
			continue
		}

		// 条件更新失败有两种原因，必须分开报：把「价格变了」说成「库存不足」
		// 会让调用方去改数量，而真正该做的是重新生成确认单。
		if err := checkDrift(ctx, tx, line, execution.Currency); err != nil {
			return trade.OrderSnapshot{}, err
		}
		return trade.OrderSnapshot{}, trade.Errorf(trade.CodeInsufficientStock,
			"SKU %s 库存不足，请重新选择数量", line.SKUID)
	}

	if err := s.fault("resolve_confirmation", FaultAfterInventoryDebit); err != nil {
		return trade.OrderSnapshot{}, err
	}

	orderID, err := trade.NewOrderID()
	if err != nil {
		return trade.OrderSnapshot{}, err
	}
	now := s.clockNow()

	domainOrder, err := buildOrder(orderID, confirmation.BuyerID, execution, now)
	if err != nil {
		return trade.OrderSnapshot{}, err
	}
	snapshot, err := orderSnapshotOf(domainOrder)
	if err != nil {
		return trade.OrderSnapshot{}, err
	}

	addressJSON, err := json.Marshal(execution.Address)
	if err != nil {
		return trade.OrderSnapshot{}, fmt.Errorf("pg: 序列化收货地址失败: %w", err)
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO orders (
    order_id, buyer_id, status, currency, total_amount_minor,
    shipping_address_json, created_at, confirmed_at, cancelled_at, cancel_reason
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULL, NULL)`,
		orderID, confirmation.BuyerID, string(order.StatusConfirmed), execution.Currency,
		execution.TotalMinor, addressJSON, now, now,
	); err != nil {
		return trade.OrderSnapshot{}, fmt.Errorf("pg: 写入订单失败: %w", err)
	}

	for _, line := range execution.Lines {
		if _, err := tx.Exec(ctx, `
INSERT INTO order_lines (order_id, sku_id, product_id, title, unit_price_minor, currency, quantity)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			orderID, line.SKUID, line.ProductID, line.Title, line.UnitPriceMinor, execution.Currency, line.Quantity,
		); err != nil {
			return trade.OrderSnapshot{}, fmt.Errorf("pg: 写入订单行失败: %w", err)
		}
	}

	if err := s.fault("resolve_confirmation", FaultAfterOrderInsert); err != nil {
		return trade.OrderSnapshot{}, err
	}
	return snapshot, nil
}

// checkDrift 在扣减失败后判定是否因为报价变化。
//
// 重新读一次不是多余的：条件更新只说「条件不成立」，不说「哪一条不成立」。
// 在同一个事务里读到的仍然是这份数据的一致视图，因此这一次判定是可靠的。
func checkDrift(ctx context.Context, tx pgx.Tx, line trade.ExecutionLine, currency string) error {
	var (
		price       int64
		rowCurrency string
	)
	err := tx.QueryRow(ctx,
		`SELECT unit_price_minor, currency FROM trade_sku_inventory WHERE sku_id = $1`,
		line.SKUID).Scan(&price, &rowCurrency)
	if errors.Is(err, pgx.ErrNoRows) {
		return trade.Errorf(trade.CodeNotFound, "商品 SKU 不存在或尚未初始化持久库存")
	}
	if err != nil {
		return fmt.Errorf("pg: 复核库存报价失败: %w", err)
	}
	if price != line.UnitPriceMinor || rowCurrency != currency {
		return trade.Errorf(trade.CodePriceChanged, "商品价格或币种已变化，请重新生成确认")
	}
	return nil
}

// executeCancel 取消订单并回补库存。
func (s *Store) executeCancel(
	ctx context.Context,
	tx pgx.Tx,
	confirmation trade.Confirmation,
	execution *trade.CancelExecution,
) (trade.OrderSnapshot, error) {
	view, err := loadOrderView(ctx, tx, execution.OrderID)
	if err != nil {
		return trade.OrderSnapshot{}, err
	}
	if view == nil {
		return trade.OrderSnapshot{}, trade.Errorf(trade.CodeNotFound, "订单不存在")
	}
	if view.BuyerID != confirmation.BuyerID {
		return trade.OrderSnapshot{}, trade.Errorf(trade.CodeOwnerMismatch, "无权访问其他买家的订单")
	}
	if len(view.Items) == 0 {
		return trade.OrderSnapshot{}, trade.Errorf(trade.CodeInventoryMigrationRequired, "旧订单明细不合法，请核对迁移")
	}

	payload := confirmation.Payload.Cancel
	rebuilt, err := trade.CancelPayloadOf(*view, payload.Reason)
	if err != nil {
		return trade.OrderSnapshot{}, err
	}
	if !trade.CancelPayloadMatches(*payload, rebuilt) {
		return trade.OrderSnapshot{}, trade.Errorf(trade.CodeOrderChanged, "订单内容或状态已变化，请重新生成取消确认")
	}

	for _, line := range execution.Lines {
		outcome, err := tx.Exec(ctx, `
UPDATE trade_sku_inventory
SET stock = stock + $2
WHERE sku_id = $1`,
			line.SKUID, line.Quantity)
		if err != nil {
			return trade.OrderSnapshot{}, fmt.Errorf("pg: 回补库存失败: %w", err)
		}
		if outcome.RowsAffected() != 1 {
			return trade.OrderSnapshot{}, trade.Errorf(trade.CodeInventoryMigrationRequired,
				"旧订单 SKU 缺少持久库存记录，请完成迁移后再取消")
		}
	}

	if err := s.fault("resolve_confirmation", FaultAfterInventoryRestore); err != nil {
		return trade.OrderSnapshot{}, err
	}

	cancelledAt := s.clockNow()
	outcome, err := tx.Exec(ctx, `
UPDATE orders
SET status = $2, cancelled_at = $3, cancel_reason = $4
WHERE order_id = $1 AND buyer_id = $5 AND status = $6`,
		execution.OrderID, string(order.StatusCancelled), cancelledAt, execution.Reason,
		confirmation.BuyerID, string(order.StatusConfirmed))
	if err != nil {
		return trade.OrderSnapshot{}, fmt.Errorf("pg: 取消订单失败: %w", err)
	}
	if outcome.RowsAffected() != 1 {
		return trade.OrderSnapshot{}, trade.Errorf(trade.CodeOrderChanged, "订单状态已变化，无法重复取消")
	}

	cancelled := *view
	cancelled.Status = order.StatusCancelled
	snapshot, err := snapshotOfView(cancelled, execution.Reason, cancelledAt)
	if err != nil {
		return trade.OrderSnapshot{}, err
	}
	return snapshot, nil
}

// finalizeApproval 把确认单置为已批准、写入决议记录，并返回带结果的确认单。
//
// now 由调用方从注入的时钟取得并传进来：决议时间是账本的一部分，
// 函数内部自己取系统时间会让它在测试里再也无法固定。
func finalizeApproval(
	ctx context.Context,
	tx pgx.Tx,
	confirmation trade.Confirmation,
	result trade.OrderSnapshot,
	now time.Time,
) (trade.Confirmation, error) {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return trade.Confirmation{}, fmt.Errorf("pg: 序列化决议结果失败: %w", err)
	}

	if err := recordOperation(ctx, tx, confirmation, trade.DecisionApprove, resultJSON, now); err != nil {
		return trade.Confirmation{}, err
	}
	return markResolved(ctx, tx, confirmation, trade.StatusApproved, resultJSON, now)
}

// finalizeRejection 记录一次拒绝。
//
// 拒绝也要落一行决议记录：调用方需要能区分「用户明确拒绝了」与「用户还没看」，
// 只改确认单状态会让这段时间线变得不可审计。
func finalizeRejection(
	ctx context.Context,
	tx pgx.Tx,
	confirmation trade.Confirmation,
	now time.Time,
) (trade.Confirmation, error) {
	if err := recordOperation(ctx, tx, confirmation, trade.DecisionReject, nil, now); err != nil {
		return trade.Confirmation{}, err
	}
	return markResolved(ctx, tx, confirmation, trade.StatusRejected, nil, now)
}

func recordOperation(
	ctx context.Context,
	tx pgx.Tx,
	confirmation trade.Confirmation,
	decision trade.Decision,
	resultJSON []byte,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
INSERT INTO trade_operations (operation_id, confirmation_id, approved, decision, result, resolved_at)
VALUES ($1, $2, $3, $4, $5, $6)`,
		confirmation.OperationID, confirmation.ConfirmationID,
		decision == trade.DecisionApprove, string(decision), resultJSON, now,
	); err != nil {
		return fmt.Errorf("pg: 写入决议记录失败: %w", err)
	}
	return nil
}

func markResolved(
	ctx context.Context,
	tx pgx.Tx,
	confirmation trade.Confirmation,
	status trade.ConfirmationStatus,
	resultJSON []byte,
	now time.Time,
) (trade.Confirmation, error) {
	outcome, err := tx.Exec(ctx, `
UPDATE trade_confirmations
SET status = $2, result = $3, resolved_at = $4
WHERE confirmation_id = $1 AND status = $5`,
		confirmation.ConfirmationID, string(status), resultJSON, now, string(trade.StatusPending))
	if err != nil {
		return trade.Confirmation{}, fmt.Errorf("pg: 更新确认单状态失败: %w", err)
	}
	if outcome.RowsAffected() != 1 {
		// 走到这里说明另一处刚刚改过这张确认单。确认单行在本事务开头已被
		// FOR UPDATE 锁住，因此这一分支只可能在锁被绕过（例如有人直连数据库
		// 改状态）时出现。宁可报冲突也不覆盖。
		return trade.Confirmation{}, trade.Errorf(trade.CodeDecisionConflict, "此确认已经作出另一项决议，不能修改")
	}

	updated := confirmation
	updated.Status = status
	at := now
	updated.ResolvedAt = &at
	if len(resultJSON) > 0 {
		var snapshot trade.OrderSnapshot
		if err := json.Unmarshal(resultJSON, &snapshot); err != nil {
			return trade.Confirmation{}, fmt.Errorf("pg: 解析决议结果失败: %w", err)
		}
		updated.Result = &snapshot
	}
	return updated, nil
}

// clockNow 返回按 UTC 归一的当前时间。
func (s *Store) clockNow() time.Time { return s.clock.Now().UTC() }

// Order 按标识取订单快照。
func (s *Store) Order(ctx context.Context, orderID, buyerID string) (trade.OrderSnapshot, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return trade.OrderSnapshot{}, fmt.Errorf("pg: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	view, err := loadOrderView(ctx, tx, orderID)
	if err != nil {
		return trade.OrderSnapshot{}, err
	}
	if view == nil {
		return trade.OrderSnapshot{}, trade.Errorf(trade.CodeNotFound, "订单不存在")
	}
	if view.BuyerID != buyerID {
		return trade.OrderSnapshot{}, trade.Errorf(trade.CodeOwnerMismatch, "无权访问其他买家的订单")
	}
	if len(view.Items) == 0 {
		return trade.OrderSnapshot{}, trade.Errorf(trade.CodeInventoryMigrationRequired, "旧订单明细不合法，请核对迁移")
	}

	var (
		createdAt    *time.Time
		cancelReason *string
	)
	if err := tx.QueryRow(ctx,
		`SELECT created_at, cancel_reason FROM orders WHERE order_id = $1`, orderID).Scan(
		&createdAt, &cancelReason); err != nil {
		return trade.OrderSnapshot{}, fmt.Errorf("pg: 读取订单头失败: %w", err)
	}

	return buildSnapshot(*view, createdAt, cancelReason)
}

// Orders 列出当前买家的订单。
func (s *Store) Orders(ctx context.Context, filter trade.OrderFilter) (trade.OrderPage, error) {
	if filter.Offset < 0 || filter.Limit < 1 || filter.Limit > 100 {
		return trade.OrderPage{}, trade.Errorf(trade.CodeInvalidQuery, "订单筛选或分页参数无效")
	}
	if filter.Status != "" && !filter.Status.Valid() {
		return trade.OrderPage{}, trade.Errorf(trade.CodeInvalidQuery, "订单筛选或分页参数无效")
	}

	// 计数与分页用同一组过滤条件：total 是过滤后的全量数，与 limit 无关，
	// 否则分页控件算不出总页数。
	countQuery := `SELECT COUNT(*) FROM orders WHERE buyer_id = $1`
	countArgs := []any{filter.BuyerID}
	if filter.Status != "" {
		countQuery += ` AND status = $2`
		countArgs = append(countArgs, string(filter.Status))
	}

	var total int
	if err := s.pool.QueryRow(ctx, countQuery, countArgs...).Scan(&total); err != nil {
		return trade.OrderPage{}, fmt.Errorf("pg: 统计订单失败: %w", err)
	}

	listQuery := `SELECT order_id FROM orders WHERE buyer_id = $1`
	listArgs := []any{filter.BuyerID}
	if filter.Status != "" {
		listQuery += ` AND status = $2`
		listArgs = append(listArgs, string(filter.Status))
	}
	listQuery += fmt.Sprintf(` ORDER BY created_at DESC, order_id DESC OFFSET $%d LIMIT $%d`,
		len(listArgs)+1, len(listArgs)+2)
	listArgs = append(listArgs, filter.Offset, filter.Limit)

	rows, err := s.pool.Query(ctx, listQuery, listArgs...)
	if err != nil {
		return trade.OrderPage{}, fmt.Errorf("pg: 列出订单失败: %w", err)
	}
	ids := make([]string, 0, filter.Limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return trade.OrderPage{}, fmt.Errorf("pg: 扫描订单标识失败: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return trade.OrderPage{}, fmt.Errorf("pg: 遍历订单失败: %w", err)
	}

	page := trade.OrderPage{Orders: []trade.OrderSnapshot{}, Total: total, Offset: filter.Offset, Limit: filter.Limit}
	for _, id := range ids {
		snapshot, err := s.Order(ctx, id, filter.BuyerID)
		if err != nil {
			return trade.OrderPage{}, err
		}
		page.Orders = append(page.Orders, snapshot)
	}
	return page, nil
}
