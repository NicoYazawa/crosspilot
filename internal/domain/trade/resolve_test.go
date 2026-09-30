package trade

import (
	"strings"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/order"
)

func TestResolveRequiresIdentityAndValidDecision(t *testing.T) {
	stored := storedConfirmation(t, createPayload(), ActionCreate, fixtureNow.Add(time.Minute))

	cases := []struct {
		name   string
		mutate func(*ResolveInput)
	}{
		{"买方为空", func(in *ResolveInput) { in.BuyerID = "   " }},
		{"买方超长", func(in *ResolveInput) { in.BuyerID = strings.Repeat("b", maxBuyerIDLen+1) }},
		{"会话为空", func(in *ResolveInput) { in.SessionID = "" }},
		{"会话超长", func(in *ResolveInput) { in.SessionID = strings.Repeat("s", maxSessionIDLen+1) }},
		{"决议为空", func(in *ResolveInput) { in.Decision = "" }},
		{"决议近似值不等于合法值", func(in *ResolveInput) { in.Decision = "approve" }},
		{"决议大小写不符", func(in *ResolveInput) { in.Decision = "Approved" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := resolveInputOf(stored, DecisionApprove)
			tc.mutate(&input)
			out, err := Resolve(stored, input, fixtureNow)
			wantCode(t, err, CodeInvalidArgument)
			// 参数不合法时连确认单都不该回传：调用方只能看到错误。
			if out.Decided || out.Create != nil || out.Cancel != nil || out.Confirmation.ConfirmationID != "" {
				t.Errorf("失败时不应产出任何结果：%+v", out)
			}
		})
	}
}

// TestResolveValidatesSnapshotShape 覆盖摘要形状的四类坏输入。
//
// 形状不对与内容不对返回同一个错误码：调用方不需要区分「你编的」与「你改的」，
// 但必须有测试确保四种情况都不会漏过去。
func TestResolveValidatesSnapshotShape(t *testing.T) {
	stored := storedConfirmation(t, createPayload(), ActionCreate, fixtureNow.Add(time.Minute))

	cases := []struct {
		name string
		hash string
	}{
		{"空串", ""},
		{"长度不足", strings.Repeat("a", 63)},
		{"长度超出", stored.SnapshotHash + "a"},
		{"大写十六进制", strings.ToUpper(stored.SnapshotHash)},
		{"非十六进制字符", strings.Repeat("g", 64)},
		{"形状合法的错误值", strings.Repeat("0", 64)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := resolveInputOf(stored, DecisionApprove)
			input.SnapshotHash = tc.hash
			out, err := Resolve(stored, input, fixtureNow)
			wantCode(t, err, CodeSnapshotMismatch)
			if out.Create != nil || out.Cancel != nil {
				t.Errorf("失败时不应产出执行动作：%+v", out)
			}
		})
	}

	// 形状检查排在归属检查之前：形状不对的摘要根本不可能是存储层产出的，
	// 先据此拒绝比先查归属更省事，也不会泄露「这个确认单属于谁」。
	shapeFirst := resolveInputOf(stored, DecisionApprove)
	shapeFirst.BuyerID = "buyer-2"
	shapeFirst.SnapshotHash = strings.Repeat("0", 63)
	if _, err := Resolve(stored, shapeFirst, fixtureNow); codeOf(t, err) != CodeSnapshotMismatch {
		t.Errorf("形状不合法的摘要应报 %s，而不是先报归属错误", CodeSnapshotMismatch)
	}
}

// TestResolveOwnerMismatchPrecedesSnapshotContent 断言归属检查排在摘要内容比对之前。
// 否则无权者只要拿到一个随机摘要，就能通过错误码区分「确认单不属于我」与「内容变了」。
func TestResolveOwnerMismatchPrecedesSnapshotContent(t *testing.T) {
	stored := storedConfirmation(t, createPayload(), ActionCreate, fixtureNow.Add(time.Minute))

	cases := []struct {
		name   string
		mutate func(*ResolveInput)
	}{
		{"买方不符且摘要值错误", func(in *ResolveInput) {
			in.BuyerID = "buyer-2"
			in.SnapshotHash = strings.Repeat("0", 64)
		}},
		{"会话不符且摘要值错误", func(in *ResolveInput) {
			in.SessionID = "session-2"
			in.SnapshotHash = strings.Repeat("0", 64)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := resolveInputOf(stored, DecisionApprove)
			tc.mutate(&input)
			_, err := Resolve(stored, input, fixtureNow)
			wantCode(t, err, CodeOwnerMismatch)
		})
	}
}

// TestResolveRecomputesSnapshotHashInsteadOfTrustingStoredValue 是快照校验的核心。
//
// 只比对「传入值 == 存储值」只能发现传错了值；重算一遍才能发现存储的载荷
// 在批准与执行之间被改过。下面两种情形都必须被拒绝。
func TestResolveRecomputesSnapshotHashInsteadOfTrustingStoredValue(t *testing.T) {
	t.Run("载荷被改而摘要未改", func(t *testing.T) {
		stored := storedConfirmation(t, createPayload(), ActionCreate, fixtureNow.Add(time.Minute))
		tampered := stored
		tampered.Payload.Create.Items[0].Quantity = 99

		// 传入的正是存储里那个摘要：如果不重算，这次决议会被放行并按 99 件下单。
		input := resolveInputOf(stored, DecisionApprove)
		_, err := Resolve(tampered, input, fixtureNow)
		wantCode(t, err, CodeSnapshotMismatch)
	})

	t.Run("存储摘要被改而载荷未改", func(t *testing.T) {
		stored := storedConfirmation(t, createPayload(), ActionCreate, fixtureNow.Add(time.Minute))
		tampered := stored
		tampered.SnapshotHash = strings.Repeat("0", 64)

		// 调用方拿着被改过的摘要来决议：两边一致，但与重算结果不一致。
		input := resolveInputOf(tampered, DecisionApprove)
		_, err := Resolve(tampered, input, fixtureNow)
		wantCode(t, err, CodeSnapshotMismatch)
	})
}

// TestResolveIdempotentReplayPrecedesExpiry 是 Resolve 最重要的性质。
//
// 已决议的确认单必须原样返回，先于过期检查：否则网络重试会看到「已过期」，
// 而交易其实早已完成，调用方会以为需要重新下单。
func TestResolveIdempotentReplayPrecedesExpiry(t *testing.T) {
	expired := fixtureNow.Add(-24 * time.Hour)

	cases := []struct {
		name     string
		status   ConfirmationStatus
		decision Decision
		decided  bool
	}{
		{"已批准且再次批准", StatusApproved, DecisionApprove, true},
		{"已拒绝且再次拒绝", StatusRejected, DecisionReject, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := storedConfirmation(t, createPayload(), ActionCreate, expired)
			stored.Status = tc.status

			out, err := Resolve(stored, resolveInputOf(stored, tc.decision), fixtureNow)
			if err != nil {
				t.Fatalf("重放已决议的确认单不应失败: %v", err)
			}
			if !out.Decided {
				t.Error("已决议的确认单应以 Decided=true 返回")
			}
			if out.Create != nil || out.Cancel != nil {
				t.Errorf("重放不得产生第二笔交易：%+v", out)
			}
			if out.Confirmation.ConfirmationID != stored.ConfirmationID || out.Confirmation.Status != tc.status {
				t.Errorf("返回的确认单 = %+v，期望原样返回 %q/%q",
					out.Confirmation, stored.ConfirmationID, tc.status)
			}
		})
	}

	conflicts := []struct {
		name     string
		status   ConfirmationStatus
		decision Decision
	}{
		{"已批准却要求拒绝", StatusApproved, DecisionReject},
		{"已拒绝却要求批准", StatusRejected, DecisionApprove},
	}

	for _, tc := range conflicts {
		t.Run(tc.name, func(t *testing.T) {
			stored := storedConfirmation(t, createPayload(), ActionCreate, expired)
			stored.Status = tc.status
			_, err := Resolve(stored, resolveInputOf(stored, tc.decision), fixtureNow)
			wantCode(t, err, CodeDecisionConflict)
		})
	}
}

// TestResolveExpiredPendingForBothDecisions 断言过期检查排在拒绝分支之前：
// 一张过期的确认单，无论批准还是拒绝都必须报告过期，而不是「已拒绝」。
// 否则用户会以为自己的拒绝被记录下来了，而实际上存储层什么都没变。
func TestResolveExpiredPendingForBothDecisions(t *testing.T) {
	cases := []struct {
		name      string
		expiresAt time.Time
	}{
		{"恰好到期", fixtureNow},
		{"已经过去一秒", fixtureNow.Add(-time.Second)},
		{"已经过去一天", fixtureNow.Add(-24 * time.Hour)},
	}

	for _, tc := range cases {
		for _, decision := range []Decision{DecisionApprove, DecisionReject} {
			t.Run(tc.name+"/"+string(decision), func(t *testing.T) {
				stored := storedConfirmation(t, createPayload(), ActionCreate, tc.expiresAt)
				out, err := Resolve(stored, resolveInputOf(stored, decision), fixtureNow)
				wantCode(t, err, CodeConfirmationExpired)
				if out.Decided || out.Confirmation.Status != "" {
					t.Errorf("过期失败时不应产出结果：%+v", out)
				}
			})
		}
	}
}

func TestResolveApproveCreateProducesExecution(t *testing.T) {
	stored := storedConfirmation(t, createPayload(), ActionCreate, fixtureNow.Add(time.Minute))

	out, err := Resolve(stored, resolveInputOf(stored, DecisionApprove), fixtureNow)
	if err != nil {
		t.Fatalf("Resolve 报错: %v", err)
	}
	if out.Decided {
		t.Error("首次批准不应被标记为已决议重放")
	}
	if out.Cancel != nil {
		t.Errorf("下单确认不应产出取消动作：%+v", out.Cancel)
	}
	if out.Create == nil {
		t.Fatal("批准下单确认必须产出执行动作")
	}

	execution := out.Create
	if execution.BuyerID != "buyer-1" {
		t.Errorf("买方 = %q，期望 buyer-1", execution.BuyerID)
	}
	if execution.Currency != "USD" {
		t.Errorf("币种 = %q，期望 USD", execution.Currency)
	}
	if execution.TotalMinor != 6497 {
		t.Errorf("总额 = %d，期望 6497", execution.TotalMinor)
	}
	if execution.Address != validAddress() {
		t.Errorf("地址 = %+v，期望 %+v", execution.Address, validAddress())
	}

	wantLines := []ExecutionLine{
		{ProductID: "prod-a", SKUID: "sku-a", Title: "登山包", UnitPriceMinor: 2599, Quantity: 2},
		{ProductID: "prod-b", SKUID: "sku-b", Title: "水壶", UnitPriceMinor: 1299, Quantity: 1},
	}
	if len(execution.Lines) != len(wantLines) {
		t.Fatalf("订单行数 = %d，期望 %d", len(execution.Lines), len(wantLines))
	}
	for i, want := range wantLines {
		if execution.Lines[i] != want {
			t.Errorf("第 %d 行 = %+v，期望 %+v", i, execution.Lines[i], want)
		}
	}

	// 返回的确认单仍是待决议状态：状态翻转由存储层在同一个事务里完成，
	// 本函数只回答「该执行什么」，不假装交易已经落库。
	if out.Confirmation.Status != StatusPending {
		t.Errorf("确认单状态 = %q，期望 %q", out.Confirmation.Status, StatusPending)
	}
	if out.Confirmation.SnapshotHash != stored.SnapshotHash {
		t.Errorf("返回的摘要 = %q，期望原样返回 %q", out.Confirmation.SnapshotHash, stored.SnapshotHash)
	}
}

func TestResolveApproveCancelProducesExecution(t *testing.T) {
	stored := storedConfirmation(t, cancelPayload(), ActionCancel, fixtureNow.Add(time.Minute))

	out, err := Resolve(stored, resolveInputOf(stored, DecisionApprove), fixtureNow)
	if err != nil {
		t.Fatalf("Resolve 报错: %v", err)
	}
	if out.Create != nil {
		t.Errorf("取消确认不应产出下单动作：%+v", out.Create)
	}
	if out.Cancel == nil {
		t.Fatal("批准取消确认必须产出执行动作")
	}

	execution := out.Cancel
	if execution.BuyerID != "buyer-1" || execution.OrderID != "order-1" || execution.Reason != "买错了" {
		t.Errorf("执行内容 = %+v，期望 buyer-1/order-1/买错了", execution)
	}
	if execution.TotalMinor != 5198 {
		t.Errorf("总额 = %d，期望 5198", execution.TotalMinor)
	}
	wantLines := []ExecutionLine{
		{ProductID: "prod-a", SKUID: "sku-a", Title: "登山包", UnitPriceMinor: 2599, Quantity: 2},
	}
	if len(execution.Lines) != len(wantLines) || execution.Lines[0] != wantLines[0] {
		t.Errorf("订单行 = %+v，期望 %+v", execution.Lines, wantLines)
	}
	if out.Confirmation.Status != StatusPending {
		t.Errorf("确认单状态 = %q，期望 %q", out.Confirmation.Status, StatusPending)
	}
}

func TestResolveRejectMarksConfirmationRejected(t *testing.T) {
	stored := storedConfirmation(t, createPayload(), ActionCreate, fixtureNow.Add(time.Minute))

	// 传入带时区的当前时刻：记录下来的决议时间必须归一到 UTC，
	// 否则同一笔决议在两台不同时区的机器上会得到不同表示。
	now := fixtureNow.In(time.FixedZone("CST", 8*3600))
	out, err := Resolve(stored, resolveInputOf(stored, DecisionReject), now)
	if err != nil {
		t.Fatalf("Resolve 报错: %v", err)
	}
	if out.Decided {
		t.Error("首次拒绝不应被标记为已决议重放")
	}
	if out.Create != nil || out.Cancel != nil {
		t.Errorf("拒绝不得产出任何执行动作：%+v/%+v", out.Create, out.Cancel)
	}
	if out.Confirmation.Status != StatusRejected {
		t.Errorf("状态 = %q，期望 %q", out.Confirmation.Status, StatusRejected)
	}
	if out.Confirmation.ResolvedAt == nil {
		t.Fatal("拒绝必须记录决议时刻")
	}
	if !out.Confirmation.ResolvedAt.Equal(fixtureNow) {
		t.Errorf("决议时刻 = %v，期望 %v", out.Confirmation.ResolvedAt, fixtureNow)
	}
	if out.Confirmation.ResolvedAt.Location() != time.UTC {
		t.Errorf("决议时刻时区 = %v，期望 UTC", out.Confirmation.ResolvedAt.Location())
	}
	if out.Confirmation.ConfirmationID != stored.ConfirmationID {
		t.Errorf("确认单标识 = %q，期望 %q", out.Confirmation.ConfirmationID, stored.ConfirmationID)
	}

	// 确认单是值类型：Resolve 必须返回副本，而不是顺手改动调用方手里那一份。
	if stored.Status != StatusPending || stored.ResolvedAt != nil {
		t.Errorf("原确认单被就地改动：%q/%v", stored.Status, stored.ResolvedAt)
	}
}

func TestResolveRejectsEmptyStoredPayload(t *testing.T) {
	stored := storedConfirmation(t, Payload{}, ActionCreate, fixtureNow.Add(time.Minute))

	// 摘要自洽，因此能走到执行分支；但空载荷必须在这里被拦下，
	// 否则会生成一张没有内容的订单。
	_, err := Resolve(stored, resolveInputOf(stored, DecisionApprove), fixtureNow)
	wantCode(t, err, CodeInvalidArgument)

	// 拒绝决议不需要载荷内容，因此空载荷被拒绝也是允许的（不产生交易）。
	out, err := Resolve(stored, resolveInputOf(stored, DecisionReject), fixtureNow)
	if err != nil {
		t.Fatalf("空载荷的拒绝决议不应报错: %v", err)
	}
	if out.Confirmation.Status != StatusRejected {
		t.Errorf("状态 = %q，期望 %q", out.Confirmation.Status, StatusRejected)
	}
}

// TestResolveRevalidatesStoredPayloadBeforeExecuting 断言执行前会再校验一次载荷。
//
// 摘要只证明「内容没被改过」，不证明「内容自洽」：一张总额与明细不符的下单载荷
// 完全可以是当初写进库里的，执行时再校验是最后一道闸门。
func TestResolveRevalidatesStoredPayloadBeforeExecuting(t *testing.T) {
	t.Run("下单载荷总额与明细不符", func(t *testing.T) {
		payload := createPayload()
		payload.Create.TotalAmountMinor = 1
		stored := storedConfirmation(t, payload, ActionCreate, fixtureNow.Add(time.Minute))

		_, err := Resolve(stored, resolveInputOf(stored, DecisionApprove), fixtureNow)
		wantCode(t, err, CodeInvalidArgument)
	})

	t.Run("下单载荷地址不完整", func(t *testing.T) {
		payload := createPayload()
		payload.Create.ShippingAddress = order.Address{Recipient: "李四"}
		stored := storedConfirmation(t, payload, ActionCreate, fixtureNow.Add(time.Minute))

		_, err := Resolve(stored, resolveInputOf(stored, DecisionApprove), fixtureNow)
		wantCode(t, err, CodeInvalidArgument)
	})

	t.Run("取消载荷缺少原因", func(t *testing.T) {
		payload := cancelPayload()
		payload.Cancel.Reason = "   "
		stored := storedConfirmation(t, payload, ActionCancel, fixtureNow.Add(time.Minute))

		_, err := Resolve(stored, resolveInputOf(stored, DecisionApprove), fixtureNow)
		wantCode(t, err, CodeInvalidArgument)
	})

	t.Run("取消载荷订单状态非法", func(t *testing.T) {
		payload := cancelPayload()
		payload.Cancel.OrderStatus = "BOGUS"
		stored := storedConfirmation(t, payload, ActionCancel, fixtureNow.Add(time.Minute))

		_, err := Resolve(stored, resolveInputOf(stored, DecisionApprove), fixtureNow)
		wantCode(t, err, CodeInvalidArgument)
	})
}

func TestResolveAcceptsExpiryOneNanosecondInTheFuture(t *testing.T) {
	// 有效期比当前时刻晚一纳秒就仍然有效：边界判断写成 <= 会把刚生成的确认单判成过期。
	stored := storedConfirmation(t, createPayload(), ActionCreate, fixtureNow.Add(time.Nanosecond))

	out, err := Resolve(stored, resolveInputOf(stored, DecisionApprove), fixtureNow)
	if err != nil {
		t.Fatalf("未到期的确认单不应被拒绝: %v", err)
	}
	if out.Create == nil {
		t.Fatalf("期望产出下单动作，得到 %+v", out)
	}
	if out.Create.Currency != string(catalog.USD) {
		t.Errorf("币种 = %q，期望 %q", out.Create.Currency, catalog.USD)
	}
}
