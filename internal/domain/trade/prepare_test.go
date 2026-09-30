package trade

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/order"
)

// validCancelInput 返回一份「除被测字段外全部合法」的取消请求。
func validCancelInput() PrepareInput {
	in := validCreateInput()
	in.Action = ActionCancel
	in.Items = nil
	in.OrderID = "order-1"
	in.Reason = "买错了"
	return in
}

// validCancelDeps 返回与 validCancelInput 配套的依赖。
// 取消路径必须拿到订单快照：取消载荷描述的是「这张订单现在长什么样」。
func validCancelDeps() PrepareDeps {
	view := orderView()
	return PrepareDeps{Now: fixtureNow, Order: &view}
}

// TestPrepareBuildsNormalisedConfirmation 覆盖新建路径的全部规范化结果。
//
// 规范化不是美化：排序与合并决定了摘要是否稳定，标题覆盖决定了买家看到的名字
// 是否与库存同源，总额是否等于合并后的逐行之和决定了确认单与扣款是否一致。
func TestPrepareBuildsNormalisedConfirmation(t *testing.T) {
	input := validCreateInput()
	input.OperationID = "  op-1001  "
	input.BuyerID = " buyer-1 "
	input.SessionID = "session-1"
	input.ShippingAddress = validAddress()
	input.ShippingAddress.Recipient = "  李四  "
	// 故意乱序，并且把 sku-b 拆成两行：Prepare 必须排序并按 SKU 合并数量。
	input.Items = []Item{
		sku("prod-b", "sku-b", "模型标题 B", 500, catalog.USD, 2),
		sku("prod-a", "sku-a", "模型标题 A", 2000, catalog.USD, 1),
		sku("prod-b", "sku-b", "模型标题 B", 500, catalog.USD, 3),
	}
	deps := PrepareDeps{
		Now: fixtureNow,
		Inventory: inventoryOf(
			record("sku-a", "prod-a", "权威标题 A", 2000, 5, catalog.USD),
			record("sku-b", "prod-b", "权威标题 B", 500, 10, catalog.USD),
		),
	}

	out, err := Prepare(input, deps)
	if err != nil {
		t.Fatalf("Prepare 报错: %v", err)
	}
	if out.Replayed {
		t.Error("全新请求不应被标记为重放")
	}
	c := out.Confirmation

	if c.OperationID != "op-1001" || c.BuyerID != "buyer-1" || c.SessionID != "session-1" {
		t.Errorf("标识未去两端空白：%q/%q/%q", c.OperationID, c.BuyerID, c.SessionID)
	}
	if c.Action != ActionCreate {
		t.Errorf("动作 = %q，期望 %q", c.Action, ActionCreate)
	}
	if c.Status != StatusPending {
		t.Errorf("状态 = %q，期望 %q（新建的确认单必须等待决议）", c.Status, StatusPending)
	}
	if !isLowerHex(c.ConfirmationID, 32) {
		t.Errorf("确认单标识 = %q，期望 32 位小写十六进制", c.ConfirmationID)
	}
	if !c.CreatedAt.Equal(fixtureNow) {
		t.Errorf("创建时间 = %v，期望 %v", c.CreatedAt, fixtureNow)
	}
	if !c.ExpiresAt.Equal(input.ExpiresAt) {
		t.Errorf("有效期 = %v，期望 %v", c.ExpiresAt, input.ExpiresAt)
	}
	if c.ResolvedAt != nil {
		t.Errorf("尚未决议，ResolvedAt 应为 nil，得到 %v", c.ResolvedAt)
	}
	if c.Result != nil {
		t.Errorf("尚未执行，Result 应为 nil，得到 %+v", c.Result)
	}

	if c.Payload.Create == nil || c.Payload.Cancel != nil {
		t.Fatalf("载荷分支 = %+v，期望只有下单分支", c.Payload)
	}
	create := c.Payload.Create

	// 明细按 SKU 升序；同一 SKU 的两行合并成数量 5 的一行，而不是三行或任选一行。
	if len(create.Items) != 2 {
		t.Fatalf("明细行数 = %d，期望 2（同一 SKU 必须合并）：%+v", len(create.Items), create.Items)
	}
	wantA := sku("prod-a", "sku-a", "权威标题 A", 2000, catalog.USD, 1)
	wantB := sku("prod-b", "sku-b", "权威标题 B", 500, catalog.USD, 5)
	if create.Items[0] != wantA {
		t.Errorf("第 1 行 = %+v，期望 %+v", create.Items[0], wantA)
	}
	if create.Items[1] != wantB {
		t.Errorf("第 2 行 = %+v，期望 %+v", create.Items[1], wantB)
	}

	// 总额必须是合并、排序之后的逐行之和：2000×1 + 500×5 = 4500。
	if create.TotalAmountMinor != 4500 {
		t.Errorf("总额 = %d，期望 4500", create.TotalAmountMinor)
	}
	if create.Currency != catalog.USD {
		t.Errorf("币种 = %q，期望 %q", create.Currency, catalog.USD)
	}
	if create.ShippingAddress != validAddress() {
		t.Errorf("地址 = %+v，期望 %+v（所有字段都去掉两端空白）", create.ShippingAddress, validAddress())
	}

	if !isLowerHex(c.RequestHash, 64) {
		t.Errorf("请求摘要 = %q，期望 64 位小写十六进制", c.RequestHash)
	}
	if !isLowerHex(c.SnapshotHash, 64) {
		t.Errorf("快照摘要 = %q，期望 64 位小写十六进制", c.SnapshotHash)
	}
	// 两个摘要覆盖面不同（快照含身份与有效期，请求含动作与载荷），
	// 一旦相等就说明其中一侧的字段被漏掉了。
	if c.RequestHash == c.SnapshotHash {
		t.Error("请求摘要与快照摘要相同，说明其中一侧的覆盖面被改错了")
	}
}

func TestPrepareRejectsInvalidRequest(t *testing.T) {
	cases := []struct {
		name  string
		build func() (PrepareInput, PrepareDeps)
		want  string
	}{
		{"operation_id 为空", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.OperationID = "   "
			return in, validDeps()
		}, CodeInvalidArgument},
		{"operation_id 超长", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.OperationID = strings.Repeat("x", maxOperationIDLen+1)
			return in, validDeps()
		}, CodeInvalidArgument},
		{"buyer_id 为空", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.BuyerID = ""
			return in, validDeps()
		}, CodeInvalidArgument},
		{"buyer_id 超长", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.BuyerID = strings.Repeat("x", maxBuyerIDLen+1)
			return in, validDeps()
		}, CodeInvalidArgument},
		{"session_id 为空", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.SessionID = ""
			return in, validDeps()
		}, CodeInvalidArgument},
		{"session_id 超长", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.SessionID = strings.Repeat("x", maxSessionIDLen+1)
			return in, validDeps()
		}, CodeInvalidArgument},
		{"未知动作", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.Action = "refund"
			return in, validDeps()
		}, CodeInvalidArgument},
		{"有效期为时刻零", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.ExpiresAt = time.Time{}
			return in, validDeps()
		}, CodeInvalidArgument},

		{"没有任何明细", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.Items = nil
			return in, validDeps()
		}, CodeInvalidArgument},
		{"收货地址不完整", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.ShippingAddress.Line1 = "   "
			return in, validDeps()
		}, CodeInvalidArgument},
		{"sku_id 为空", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.Items = []Item{sku("prod-a", "  ", "登山包", 2599, catalog.USD, 1)}
			return in, validDeps()
		}, CodeInvalidArgument},
		{"sku_id 超长", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.Items = []Item{sku("prod-a", strings.Repeat("x", maxSKUIDLen+1), "登山包", 2599, catalog.USD, 1)}
			return in, validDeps()
		}, CodeInvalidArgument},
		{"product_id 为空", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.Items = []Item{sku("", "sku-a", "登山包", 2599, catalog.USD, 1)}
			return in, validDeps()
		}, CodeInvalidArgument},
		{"数量为零", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.Items = []Item{sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, 0)}
			return in, validDeps()
		}, CodeInvalidArgument},
		{"数量为负", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.Items = []Item{sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, -2)}
			return in, validDeps()
		}, CodeInvalidArgument},
		{"单价为负", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.Items = []Item{sku("prod-a", "sku-a", "登山包", -1, catalog.USD, 1)}
			return in, validDeps()
		}, CodeInvalidArgument},
		{"币种非法", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.Items = []Item{sku("prod-a", "sku-a", "登山包", 2599, "usd", 1)}
			return in, validDeps()
		}, CodeInvalidArgument},
		{"同一 SKU 的价格不一致", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.Items = []Item{
				sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, 1),
				sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, 1),
				sku("prod-a", "sku-a", "登山包", 2600, catalog.USD, 1),
			}
			return in, validDeps()
		}, CodeInvalidArgument},
		{"同一 SKU 的标题不一致", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.Items = []Item{
				sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, 1),
				sku("prod-a", "sku-a", "水壶", 2599, catalog.USD, 1),
			}
			return in, validDeps()
		}, CodeInvalidArgument},
		{"跨行币种不同", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.Items = []Item{
				sku("prod-a", "sku-a", "登山包", 1000, catalog.USD, 1),
				sku("prod-b", "sku-b", "水壶", 2000, catalog.JPY, 1),
			}
			deps := validDeps()
			deps.Inventory["sku-b"] = record("sku-b", "prod-b", "水壶", 2000, 5, catalog.JPY)
			return in, deps
		}, CodeInvalidArgument},
		{"数量相加溢出", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.Items = []Item{
				sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, math.MaxInt64),
				sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, 1),
			}
			return in, validDeps()
		}, CodeInvalidArgument},

		{"有效期已过", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.ExpiresAt = fixtureNow.Add(-time.Second)
			return in, validDeps()
		}, CodeConfirmationExpired},
		{"有效期恰好等于当前时刻", func() (PrepareInput, PrepareDeps) {
			in := validCreateInput()
			in.ExpiresAt = fixtureNow
			return in, validDeps()
		}, CodeConfirmationExpired},

		{"仓库没有这个 SKU", func() (PrepareInput, PrepareDeps) {
			return validCreateInput(), PrepareDeps{Now: fixtureNow}
		}, CodeNotFound},
		{"库存行的商品标识与明细不符", func() (PrepareInput, PrepareDeps) {
			deps := validDeps()
			deps.Inventory["sku-a"] = record("sku-a", "别的商品", "权威标题 A", 2599, 10, catalog.USD)
			return validCreateInput(), deps
		}, CodeNotFound},
		{"单价已变化", func() (PrepareInput, PrepareDeps) {
			deps := validDeps()
			deps.Inventory["sku-a"] = record("sku-a", "prod-a", "权威标题 A", 3199, 10, catalog.USD)
			return validCreateInput(), deps
		}, CodePriceChanged},
		{"币种已变化", func() (PrepareInput, PrepareDeps) {
			deps := validDeps()
			deps.Inventory["sku-a"] = record("sku-a", "prod-a", "权威标题 A", 2599, 10, catalog.JPY)
			return validCreateInput(), deps
		}, CodePriceChanged},
		{"库存不足", func() (PrepareInput, PrepareDeps) {
			deps := validDeps()
			deps.Inventory["sku-a"] = record("sku-a", "prod-a", "权威标题 A", 2599, 1, catalog.USD)
			return validCreateInput(), deps
		}, CodeInsufficientStock},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, deps := tc.build()
			out, err := Prepare(in, deps)
			wantCode(t, err, tc.want)
			// 失败时不得留下半成品：调用方只会看到错误，不会拿到一张可落库的确认单。
			if out.Replayed || out.Confirmation.ConfirmationID != "" {
				t.Errorf("失败时不应产出确认单：%+v", out)
			}
		})
	}
}

// TestPrepareAcceptsBoundaryLengths 断言上限本身是允许的。
// 「最长 N 字符」写成 >= N 就会把合法请求拒之门外，而这种偏差只有边界值能发现。
func TestPrepareAcceptsBoundaryLengths(t *testing.T) {
	in := validCreateInput()
	in.OperationID = strings.Repeat("x", maxOperationIDLen)
	in.BuyerID = strings.Repeat("b", maxBuyerIDLen)
	in.SessionID = strings.Repeat("s", maxSessionIDLen)
	in.Items = []Item{sku("prod-a", strings.Repeat("k", maxSKUIDLen), "登山包", 2599, catalog.USD, 1)}

	out, err := Prepare(in, PrepareDeps{Now: fixtureNow, Inventory: inventoryOf(
		record(strings.Repeat("k", maxSKUIDLen), "prod-a", "权威标题", 2599, 10, catalog.USD),
	)})
	if err != nil {
		t.Fatalf("恰好达到上限的请求不应被拒绝: %v", err)
	}
	if out.Replayed {
		t.Error("全新请求不应被标记为重放")
	}
}

// TestPrepareReplayPrecedesExpiryAndQuotes 是本包最重要的性质。
//
// 调用方重试一笔刚刚成功扣减过库存的请求时，价格可能已经变了、库存可能已经不够、
// 有效期可能已经过了——而这些都是这笔交易自己造成的。重放判定必须排在这些
// 依赖外部状态的核对之前，否则「同一次重试」会被判成「非法请求」。
func TestPrepareReplayPrecedesExpiryAndQuotes(t *testing.T) {
	input := validCreateInput()
	first, err := Prepare(input, validDeps())
	if err != nil {
		t.Fatalf("首次 Prepare 报错: %v", err)
	}
	existing := first.Confirmation

	// 新传入的库存把标题和价格都改了，库存也清零：重放绝不能采纳它们。
	retitled := inventoryOf(record("sku-a", "prod-a", "换了个标题", 9999, 0, catalog.USD))

	cases := []struct {
		name string
		deps PrepareDeps
	}{
		{"库存为空", PrepareDeps{Now: fixtureNow}.WithExisting(existing)},
		{"报价已变", PrepareDeps{Now: fixtureNow, Inventory: retitled}.WithExisting(existing)},
		{"有效期已过", PrepareDeps{Now: fixtureNow.Add(48 * time.Hour), Inventory: validDeps().Inventory}.WithExisting(existing)},
		{"三者同时发生", PrepareDeps{Now: fixtureNow.Add(48 * time.Hour), Inventory: retitled}.WithExisting(existing)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Prepare(input, tc.deps)
			if err != nil {
				t.Fatalf("重放不应失败，得到 %v", err)
			}
			if !out.Replayed {
				t.Error("同一 operation_id 的重复请求应被标记为重放")
			}

			got := out.Confirmation
			if got.ConfirmationID != existing.ConfirmationID {
				t.Errorf("确认单标识 = %q，期望复用 %q", got.ConfirmationID, existing.ConfirmationID)
			}
			if !got.ExpiresAt.Equal(existing.ExpiresAt) {
				t.Errorf("有效期 = %v，期望沿用 %v（重试时重算的有效期必须被忽略）", got.ExpiresAt, existing.ExpiresAt)
			}
			if got.SnapshotHash != existing.SnapshotHash || got.RequestHash != existing.RequestHash {
				t.Errorf("摘要发生了变化：%q/%q，期望沿用 %q/%q",
					got.RequestHash, got.SnapshotHash, existing.RequestHash, existing.SnapshotHash)
			}
			if got.Status != StatusPending {
				t.Errorf("状态 = %q，期望 %q", got.Status, StatusPending)
			}
			if !got.CreatedAt.Equal(existing.CreatedAt) {
				t.Errorf("创建时间 = %v，期望沿用 %v", got.CreatedAt, existing.CreatedAt)
			}
			// 标题覆盖只作用于新建路径：重放返回的是既有确认单里存着的标题，
			// 也就是买家当初看到的那一份，而不是这次传入的库存标题。
			if title := got.Payload.Create.Items[0].Title; title != "权威标题 A" {
				t.Errorf("标题 = %q，期望 %q（重放不得按新库存改写已展示的内容）", title, "权威标题 A")
			}
			if price := got.Payload.Create.Items[0].UnitPriceMinor; price != 2599 {
				t.Errorf("单价 = %d，期望 2599（重放不得采纳新报价）", price)
			}
		})
	}
}

func TestPrepareReplayOwnerMismatchPrecedesContentConflict(t *testing.T) {
	first, err := Prepare(validCreateInput(), validDeps())
	if err != nil {
		t.Fatalf("首次 Prepare 报错: %v", err)
	}
	existing := first.Confirmation

	// 同时换了买方又改了内容：必须先报归属错误。
	// 归属是权限问题，内容冲突是幂等问题；把权限问题报成幂等冲突，
	// 会让无权者据此推断出别人存了什么内容。
	other := validCreateInput()
	other.BuyerID = "buyer-2"
	other.ShippingAddress.City = "北京"
	_, err = Prepare(other, validDeps().WithExisting(existing))
	wantCode(t, err, CodeOwnerMismatch)

	otherSession := validCreateInput()
	otherSession.SessionID = "session-2"
	_, err = Prepare(otherSession, validDeps().WithExisting(existing))
	wantCode(t, err, CodeOwnerMismatch)

	// 身份相同但内容变了：同一操作编号不能对应两份内容。
	changedAddress := validCreateInput()
	changedAddress.ShippingAddress.City = "北京"
	_, err = Prepare(changedAddress, validDeps().WithExisting(existing))
	wantCode(t, err, CodeOperationConflict)

	changedItems := validCreateInput()
	changedItems.Items = []Item{sku("prod-a", "sku-a", "模型写的标题", 2599, catalog.USD, 3)}
	_, err = Prepare(changedItems, validDeps().WithExisting(existing))
	wantCode(t, err, CodeOperationConflict)

	// 换动作同样是「改了内容」：一份下单确认与一份取消确认不能共用一个操作编号。
	// 取消路径需要订单快照才能重建载荷，因此这里给的是取消用的依赖。
	changedAction := validCreateInput()
	changedAction.Action = ActionCancel
	changedAction.Items = nil
	changedAction.OrderID = "order-1"
	changedAction.Reason = "买错了"
	_, err = Prepare(changedAction, validCancelDeps().WithExisting(existing))
	wantCode(t, err, CodeOperationConflict)
}

// TestPrepareReplayOnCancelPath 覆盖取消路径的重放。
//
// 取消载荷由订单快照重建，因此重放时也必须能重建出与存储一致的内容；
// 订单在等待期间被改动，重建结果就会与存储不同，重放随之变成内容冲突。
func TestPrepareReplayOnCancelPath(t *testing.T) {
	input := validCancelInput()
	deps := validCancelDeps()
	first, err := Prepare(input, deps)
	if err != nil {
		t.Fatalf("首次 Prepare 报错: %v", err)
	}
	if first.Confirmation.Payload.Cancel == nil {
		t.Fatalf("期望取消载荷，得到 %+v", first.Confirmation.Payload)
	}

	out, err := Prepare(input, validCancelDeps().WithExisting(first.Confirmation))
	if err != nil {
		t.Fatalf("取消重放不应失败: %v", err)
	}
	if !out.Replayed {
		t.Error("同一 operation_id 的取消请求应被标记为重放")
	}
	if out.Confirmation.ConfirmationID != first.Confirmation.ConfirmationID {
		t.Errorf("确认单标识 = %q，期望复用 %q", out.Confirmation.ConfirmationID, first.Confirmation.ConfirmationID)
	}

	// 原因变了：重建得到的载荷与存储不一致 → 内容冲突。
	changedReason := validCancelInput()
	changedReason.Reason = "不想要了"
	_, err = Prepare(changedReason, validCancelDeps().WithExisting(first.Confirmation))
	wantCode(t, err, CodeOperationConflict)

	// 订单在等待期间被改了明细（总额仍然自洽）→ 重建结果不同 → 内容冲突。
	changedView := orderView()
	changedView.Items[0].Title = "换了名字的水壶"
	changedDeps := PrepareDeps{Now: fixtureNow, Order: &changedView}
	_, err = Prepare(input, changedDeps.WithExisting(first.Confirmation))
	wantCode(t, err, CodeOperationConflict)

	// 订单快照缺失时连载荷都重建不出来。这是领域层该有的行为：缺依赖就报缺依赖，
	// 而不是猜一个内容出来比对。**存储层负责把订单快照喂进来**——
	// 曾经有一版存储层只在「新建」分支读订单，于是取消的重放永远走到这里、
	// 拿到 NOT_FOUND 而不是既有确认单（见 P1 改动记录「回归」第 7 条）。
	// 领域层这条断言因此不是缺陷，而是分工的边界：谁来提供依赖是存储层的责任。
	_, err = Prepare(input, PrepareDeps{Now: fixtureNow}.WithExisting(first.Confirmation))
	wantCode(t, err, CodeNotFound)
}

func TestPrepareCancelActionBuildsPayloadFromOrder(t *testing.T) {
	out, err := Prepare(validCancelInput(), validCancelDeps())
	if err != nil {
		t.Fatalf("Prepare 报错: %v", err)
	}
	if out.Replayed {
		t.Error("全新请求不应被标记为重放")
	}
	c := out.Confirmation
	if c.Action != ActionCancel {
		t.Errorf("动作 = %q，期望 %q", c.Action, ActionCancel)
	}
	if c.Status != StatusPending {
		t.Errorf("状态 = %q，期望 %q", c.Status, StatusPending)
	}
	if c.Payload.Cancel == nil || c.Payload.Create != nil {
		t.Fatalf("载荷分支 = %+v，期望只有取消分支", c.Payload)
	}

	cancel := c.Payload.Cancel
	if cancel.OrderID != "order-1" {
		t.Errorf("订单号 = %q，期望 order-1", cancel.OrderID)
	}
	if cancel.Reason != "买错了" {
		t.Errorf("取消原因 = %q，期望 买错了", cancel.Reason)
	}
	if cancel.OrderStatus != order.StatusConfirmed {
		t.Errorf("订单状态 = %q，期望 %q", cancel.OrderStatus, order.StatusConfirmed)
	}
	if cancel.Currency != catalog.USD {
		t.Errorf("币种 = %q，期望 %q", cancel.Currency, catalog.USD)
	}
	// 总额取自明细之和（6497），而不是订单快照里的那一列：
	// 两者不一致时 CancelPayloadOf 会直接报错，走到这里说明它们本来就相等。
	if cancel.TotalAmountMinor != 6497 {
		t.Errorf("总额 = %d，期望 6497", cancel.TotalAmountMinor)
	}
	// 订单快照里的明细是乱序的，重建后必须按 SKU 升序。
	if len(cancel.Items) != 2 {
		t.Fatalf("明细行数 = %d，期望 2", len(cancel.Items))
	}
	if cancel.Items[0].SKUID != "sku-a" || cancel.Items[1].SKUID != "sku-b" {
		t.Errorf("明细顺序 = %q/%q，期望 sku-a/sku-b", cancel.Items[0].SKUID, cancel.Items[1].SKUID)
	}
	if cancel.ShippingAddress != validAddress() {
		t.Errorf("地址 = %+v，期望 %+v", cancel.ShippingAddress, validAddress())
	}
	if c.Payload.String() != "取消确认：订单 order-1" {
		t.Errorf("载荷描述 = %q", c.Payload.String())
	}
}

func TestPrepareCancelRejectsBadOrder(t *testing.T) {
	otherBuyerView := orderView()
	otherBuyerView.BuyerID = "buyer-2"
	draftView := orderView()
	draftView.Status = order.StatusDraft
	cancelledView := orderView()
	cancelledView.Status = order.StatusCancelled

	cases := []struct {
		name  string
		build func() (PrepareInput, PrepareDeps)
		want  string
	}{
		{"缺少订单号", func() (PrepareInput, PrepareDeps) {
			in := validCancelInput()
			in.OrderID = "  "
			return in, validCancelDeps()
		}, CodeInvalidArgument},
		{"订单号超长", func() (PrepareInput, PrepareDeps) {
			in := validCancelInput()
			in.OrderID = strings.Repeat("o", maxOrderIDLen+1)
			return in, validCancelDeps()
		}, CodeInvalidArgument},
		{"缺少取消原因", func() (PrepareInput, PrepareDeps) {
			in := validCancelInput()
			in.Reason = "   "
			return in, validCancelDeps()
		}, CodeInvalidArgument},
		{"取消原因超长", func() (PrepareInput, PrepareDeps) {
			in := validCancelInput()
			in.Reason = strings.Repeat("r", maxReasonLen+1)
			return in, validCancelDeps()
		}, CodeInvalidArgument},
		{"订单不存在", func() (PrepareInput, PrepareDeps) {
			return validCancelInput(), PrepareDeps{Now: fixtureNow}
		}, CodeNotFound},
		{"订单号不匹配", func() (PrepareInput, PrepareDeps) {
			view := orderView()
			view.OrderID = "order-2"
			return validCancelInput(), PrepareDeps{Now: fixtureNow, Order: &view}
		}, CodeNotFound},
		{"订单归属他人", func() (PrepareInput, PrepareDeps) {
			return validCancelInput(), PrepareDeps{Now: fixtureNow, Order: &otherBuyerView}
		}, CodeOwnerMismatch},
		{"订单总额与明细不符", func() (PrepareInput, PrepareDeps) {
			// 重建取消载荷失败时，错误必须原样上抛：把一份对不上的订单
			// 悄悄替换成「按明细算出来的金额」，会让取消回补的库存与退款金额对不上。
			view := orderView()
			view.TotalAmountMinor = 1
			return validCancelInput(), PrepareDeps{Now: fixtureNow, Order: &view}
		}, CodeOrderChanged},
		{"订单尚未确认", func() (PrepareInput, PrepareDeps) {
			return validCancelInput(), PrepareDeps{Now: fixtureNow, Order: &draftView}
		}, CodeOrderChanged},
		{"订单已取消", func() (PrepareInput, PrepareDeps) {
			return validCancelInput(), PrepareDeps{Now: fixtureNow, Order: &cancelledView}
		}, CodeOrderChanged},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, deps := tc.build()
			out, err := Prepare(in, deps)
			wantCode(t, err, tc.want)
			if out.Confirmation.ConfirmationID != "" {
				t.Errorf("失败时不应产出确认单：%+v", out.Confirmation)
			}
		})
	}
}

func TestCancelPayloadOfRebuildsFromView(t *testing.T) {
	got, err := CancelPayloadOf(orderView(), "买错了")
	if err != nil {
		t.Fatalf("CancelPayloadOf 报错: %v", err)
	}
	if got.OrderID != "order-1" || got.Reason != "买错了" {
		t.Errorf("订单号/原因 = %q/%q，期望 order-1/买错了", got.OrderID, got.Reason)
	}
	if got.OrderStatus != order.StatusConfirmed {
		t.Errorf("订单状态 = %q，期望 %q", got.OrderStatus, order.StatusConfirmed)
	}
	if got.Currency != catalog.USD || got.TotalAmountMinor != 6497 {
		t.Errorf("币种/总额 = %q/%d，期望 USD/6497", got.Currency, got.TotalAmountMinor)
	}
	if got.ShippingAddress != validAddress() {
		t.Errorf("地址 = %+v，期望 %+v", got.ShippingAddress, validAddress())
	}
	if len(got.Items) != 2 || got.Items[0].SKUID != "sku-a" || got.Items[1].SKUID != "sku-b" {
		t.Errorf("明细 = %+v，期望按 SKU 升序排列的两行", got.Items)
	}
}

func TestCancelPayloadOfRejectsBadView(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*OrderView)
		want   string
	}{
		{"明细为空", func(v *OrderView) { v.Items = nil }, CodeInventoryMigrationRequired},
		{"明细数量为零", func(v *OrderView) { v.Items[0].Quantity = 0 }, CodeInventoryMigrationRequired},
		{"明细数量为负", func(v *OrderView) { v.Items[0].Quantity = -1 }, CodeInventoryMigrationRequired},
		{"明细缺少规格标识", func(v *OrderView) { v.Items[0].SKUID = "" }, CodeInventoryMigrationRequired},

		{"总额与逐行金额不符", func(v *OrderView) { v.TotalAmountMinor = 1 }, CodeOrderChanged},
		{"币种与明细不符", func(v *OrderView) { v.Currency = "JPY" }, CodeOrderChanged},

		{"明细币种非法", func(v *OrderView) {
			v.Items = []Item{sku("prod-a", "sku-a", "登山包", 100, "usd", 1)}
		}, CodeInvalidArgument},
		{"明细之间币种不同", func(v *OrderView) {
			v.Items = []Item{
				sku("prod-a", "sku-a", "登山包", 100, catalog.USD, 1),
				sku("prod-b", "sku-b", "水壶", 200, catalog.JPY, 1),
			}
		}, CodeInvalidArgument},
		{"单行金额溢出", func(v *OrderView) {
			v.Items = []Item{sku("prod-a", "sku-a", "登山包", 2, catalog.USD, math.MaxInt64)}
		}, CodeInvalidArgument},
		{"逐行金额之和溢出", func(v *OrderView) {
			v.Items = []Item{
				sku("prod-a", "sku-a", "登山包", math.MaxInt64-100, catalog.USD, 1),
				sku("prod-b", "sku-b", "水壶", 200, catalog.USD, 1),
			}
		}, CodeInvalidArgument},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view := orderView()
			tc.mutate(&view)
			got, err := CancelPayloadOf(view, "买错了")
			wantCode(t, err, tc.want)
			// 失败时必须是零值载荷：半成品一旦被落库，取消就会按一份残缺快照执行。
			if got.OrderID != "" || got.Reason != "" || got.Items != nil ||
				got.Currency != "" || got.TotalAmountMinor != 0 || got.OrderStatus != "" {
				t.Errorf("失败时应返回零值载荷，得到 %+v", got)
			}
		})
	}
}

// TestCancelPayloadOfAcceptsUnrecordedTotalAndCurrency 记录迁移期的宽容规则：
// 旧订单可能没有记录总额与币种，此时用空值表示「未记录」而不是「等于零」，
// 因此这两项不参与比对，但重建结果仍然是明细算出来的真实值。
func TestCancelPayloadOfAcceptsUnrecordedTotalAndCurrency(t *testing.T) {
	view := orderView()
	view.TotalAmountMinor = 0
	view.Currency = ""

	got, err := CancelPayloadOf(view, "买错了")
	if err != nil {
		t.Fatalf("未记录总额与币种时应能重建: %v", err)
	}
	if got.TotalAmountMinor != 6497 {
		t.Errorf("总额 = %d，期望 6497（由明细算出）", got.TotalAmountMinor)
	}
	if got.Currency != catalog.USD {
		t.Errorf("币种 = %q，期望 %q", got.Currency, catalog.USD)
	}
}

func TestCancelPayloadMatchesDetectsEveryDifference(t *testing.T) {
	rebuilt, err := CancelPayloadOf(orderView(), "买错了")
	if err != nil {
		t.Fatalf("CancelPayloadOf 报错: %v", err)
	}
	if !CancelPayloadMatches(rebuilt, rebuilt) {
		t.Fatal("同一份载荷与自身比对应为 true")
	}

	cases := []struct {
		name   string
		mutate func(*CancelPayload)
	}{
		{"订单号不同", func(p *CancelPayload) { p.OrderID = "order-2" }},
		{"取消原因不同", func(p *CancelPayload) { p.Reason = "不想要了" }},
		{"订单状态不同", func(p *CancelPayload) { p.OrderStatus = order.StatusCancelled }},
		{"币种不同", func(p *CancelPayload) { p.Currency = catalog.JPY }},
		{"总额不同", func(p *CancelPayload) { p.TotalAmountMinor = 1 }},
		{"地址不同", func(p *CancelPayload) { p.ShippingAddress.City = "北京" }},
		{"明细行数不同", func(p *CancelPayload) { p.Items = p.Items[:1] }},
		{"明细内容不同", func(p *CancelPayload) { p.Items[0].Quantity = 7 }},
		{"明细顺序不同", func(p *CancelPayload) { p.Items[0], p.Items[1] = p.Items[1], p.Items[0] }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := rebuilt
			stored.Items = append([]Item(nil), rebuilt.Items...)
			tc.mutate(&stored)
			if CancelPayloadMatches(stored, rebuilt) {
				t.Errorf("%s 之后仍被判为一致，取消会按一份过期的快照执行", tc.name)
			}
			// 比对必须与参数顺序无关，否则调用方换个顺序就得到相反结论。
			if CancelPayloadMatches(rebuilt, stored) {
				t.Errorf("%s 之后反向比对仍被判为一致", tc.name)
			}
		})
	}
}

// TestSortAndTotalEmptyItemsIsRejected 直接调用防御性分支。
// 两个调用方都会先行判空，因此这条分支只有直接调用才能覆盖；
// 保留它是为了让「空明细」在任何调用路径上都不会算出总额 0 而被放行。
func TestSortAndTotalEmptyItemsIsRejected(t *testing.T) {
	ordered, total, currency, err := sortAndTotal(nil)
	wantCode(t, err, CodeInvalidArgument)
	if ordered != nil || total != 0 || currency != "" {
		t.Errorf("失败时返回值应为零值，得到 %+v/%d/%q", ordered, total, currency)
	}
}

// TestAddQuantityRejectsNonPositiveDelta 覆盖防御性分支：
// dedupeItems 已经保证 delta > 0，这里直接调用确保这层保护不会在重构中被删掉。
func TestAddQuantityRejectsNonPositiveDelta(t *testing.T) {
	item := sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, 3)

	if err := addQuantity(&item, 0); codeOf(t, err) != CodeInvalidArgument {
		t.Errorf("delta=0 应报 %s", CodeInvalidArgument)
	}
	if err := addQuantity(&item, -1); codeOf(t, err) != CodeInvalidArgument {
		t.Errorf("delta=-1 应报 %s", CodeInvalidArgument)
	}
	if item.Quantity != 3 {
		t.Errorf("失败时不得改动数量，得到 %d", item.Quantity)
	}

	if err := addQuantity(&item, 4); err != nil {
		t.Fatalf("正常相加不应报错: %v", err)
	}
	if item.Quantity != 7 {
		t.Errorf("数量 = %d，期望 7", item.Quantity)
	}

	overflow := sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, math.MaxInt64)
	if err := addQuantity(&overflow, 1); codeOf(t, err) != CodeInvalidArgument {
		t.Errorf("相加溢出应报 %s，而不是回绕", CodeInvalidArgument)
	}
	if overflow.Quantity != math.MaxInt64 {
		t.Errorf("溢出时数量被改动成 %d", overflow.Quantity)
	}
}

// TestBuildPayloadRejectsUnknownActionDirectly 覆盖 buildPayload 的兜底分支。
// Prepare 已经先行拦下未知动作，因此只有直接调用才能覆盖；
// 留着它是为了让「动作取值」这件事在两层都有防线。
func TestBuildPayloadRejectsUnknownActionDirectly(t *testing.T) {
	in := validCreateInput()
	in.Action = "refund"

	payload, err := buildPayload(in, validDeps())
	wantCode(t, err, CodeInvalidArgument)
	if payload.Create != nil || payload.Cancel != nil {
		t.Errorf("失败时应返回空载荷，得到 %+v", payload)
	}
}

// TestVerifyQuotesSkipsCancelPayloads 断言报价核对只针对下单载荷。
// 取消的金额一致性由「用当前订单重建载荷再比对」来保证，两者不是一回事。
func TestVerifyQuotesSkipsCancelPayloads(t *testing.T) {
	if err := verifyQuotes(cancelPayload(), nil); err != nil {
		t.Errorf("取消载荷不应在 Prepare 阶段核对库存，得到 %v", err)
	}
	wantCode(t, verifyQuotes(createPayload(), nil), CodeNotFound)
}

// TestApplyAuthoritativeTitles 覆盖标题覆盖的两条边角：
// 库存里没有这一行时保留请求里的标题（而不是清空），以及取消载荷没有可覆盖的标题。
func TestApplyAuthoritativeTitles(t *testing.T) {
	payload := createPayload()
	payload.Create.Items[0].Title = "模型写的标题"
	payload.Create.Items[1].Title = "模型写的另一标题"

	applyAuthoritativeTitles(payload, inventoryOf(record("sku-a", "prod-a", "权威标题 A", 2599, 10, catalog.USD)))
	if got := payload.Create.Items[0].Title; got != "权威标题 A" {
		t.Errorf("第 1 行标题 = %q，期望 权威标题 A（展示名必须与库存同源）", got)
	}
	// Prepare 的正规路径会先通过 verifyQuotes，因此这一行必定存在于库存；
	// 这里覆盖的是防御性行为：保留请求里的标题，而不是把它清成空白。
	if got := payload.Create.Items[1].Title; got != "模型写的另一标题" {
		t.Errorf("第 2 行标题 = %q，期望保留原值", got)
	}

	// 取消载荷没有可覆盖的标题，调用它不应 panic，也不应改动任何字段。
	cancel := cancelPayload()
	applyAuthoritativeTitles(cancel, nil)
	if cancel.Cancel.Items[0].Title != "登山包" {
		t.Errorf("取消载荷的标题被改成了 %q", cancel.Cancel.Items[0].Title)
	}
}

// TestPrepareDepsBuilders 覆盖依赖注入的三个构造器。
//
// 它们让调用方在事务里按顺序组装依赖；任何一个漏改字段，存储层就会拿到零值依赖
// ——例如忘记注入订单，取消请求会以 NOT_FOUND 收场，而问题其实出在装配上。
func TestPrepareDepsBuilders(t *testing.T) {
	view := orderView()
	inventory := validDeps().Inventory
	existing := storedConfirmation(t, createPayload(), ActionCreate, fixtureNow.Add(time.Minute))

	deps := PrepareDeps{Now: fixtureNow}.
		WithInventory(inventory).
		WithOrder(&view).
		WithExisting(existing)

	if len(deps.Inventory) != 1 || deps.Inventory["sku-a"].Title != "权威标题 A" {
		t.Errorf("库存未注入：%+v", deps.Inventory)
	}
	if deps.Order != &view {
		t.Errorf("订单未注入：%+v", deps.Order)
	}
	if !deps.hasExisting() {
		t.Error("既有确认单未注入")
	}
	if deps.existing.ConfirmationID != existing.ConfirmationID {
		t.Errorf("既有确认单标识 = %q，期望 %q", deps.existing.ConfirmationID, existing.ConfirmationID)
	}

	// 值语义：构造器必须返回副本。若就地改动接收者，调用方手上那份依赖会被
	// 后续步骤看到，事务里就会出现难以复现的顺序依赖。
	base := PrepareDeps{Now: fixtureNow}
	_ = base.WithInventory(inventory)
	_ = base.WithOrder(&view)
	_ = base.WithExisting(existing)
	if base.Inventory != nil || base.Order != nil || base.hasExisting() {
		t.Errorf("构造器改动了接收者：%+v", base)
	}

	// 用构造器组装出来的依赖必须能直接跑通两条路径。
	created, err := Prepare(validCreateInput(), PrepareDeps{Now: fixtureNow}.WithInventory(inventory))
	if err != nil {
		t.Fatalf("下单路径依赖组装失败: %v", err)
	}
	if created.Replayed || created.Confirmation.Payload.Create == nil {
		t.Errorf("下单路径未产出确认单：%+v", created)
	}

	cancelOut, err := Prepare(validCancelInput(), PrepareDeps{Now: fixtureNow}.WithOrder(&view))
	if err != nil {
		t.Fatalf("取消路径依赖组装失败: %v", err)
	}
	if cancelOut.Confirmation.Payload.Cancel == nil {
		t.Errorf("取消路径未产出取消载荷：%+v", cancelOut.Confirmation.Payload)
	}
}
