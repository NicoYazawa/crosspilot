package trade

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

func TestActionValid(t *testing.T) {
	cases := []struct {
		name   string
		action Action
		want   bool
	}{
		{"下单", ActionCreate, true},
		{"取消", ActionCancel, true},
		{"空串", "", false},
		{"大写", "CREATE", false},
		{"未知动作", "refund", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.action.Valid(); got != tc.want {
				t.Errorf("Action(%q).Valid() = %v，期望 %v", tc.action, got, tc.want)
			}
		})
	}
}

func TestConfirmationStatusValid(t *testing.T) {
	cases := []struct {
		name   string
		status ConfirmationStatus
		want   bool
	}{
		{"待决议", StatusPending, true},
		{"已批准", StatusApproved, true},
		{"已拒绝", StatusRejected, true},
		{"空串", "", false},
		{"大写", "PENDING", false},
		{"未知状态", "expired", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.status.Valid(); got != tc.want {
				t.Errorf("ConfirmationStatus(%q).Valid() = %v，期望 %v", tc.status, got, tc.want)
			}
		})
	}
}

// TestWireLiteralValues 钉住对外契约的裸字符串。
//
// 这些取值会直接出现在 JSON 与前端分支里：改成小写或改个词不会让任何编译失败，
// 却会静默破坏既有消费方，所以必须由测试锁住逐字取值。
func TestWireLiteralValues(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"动作 create", string(ActionCreate), "create"},
		{"动作 cancel", string(ActionCancel), "cancel"},
		{"状态 pending", string(StatusPending), "pending"},
		{"状态 approved", string(StatusApproved), "approved"},
		{"状态 rejected", string(StatusRejected), "rejected"},
		{"决议 approved", string(DecisionApprove), "approved"},
		{"决议 rejected", string(DecisionReject), "rejected"},
		{"金额口径", AmountScope, "merchandise_only"},
		{"单据类型", OrderKind, "purchase_intent"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("取值 = %q，期望 %q", tc.got, tc.want)
			}
		})
	}
}

func TestErrorCodeLiteralValues(t *testing.T) {
	cases := []struct {
		code string
		want string
	}{
		{CodeInvalidArgument, "INVALID_ARGUMENT"},
		{CodeInvalidQuery, "INVALID_QUERY"},
		{CodeNotFound, "NOT_FOUND"},
		{CodeOwnerMismatch, "OWNER_MISMATCH"},
		{CodeOperationConflict, "OPERATION_CONFLICT"},
		{CodeConfirmationExpired, "CONFIRMATION_EXPIRED"},
		{CodeInsufficientStock, "INSUFFICIENT_STOCK"},
		{CodePriceChanged, "PRICE_CHANGED"},
		{CodeSnapshotMismatch, "SNAPSHOT_MISMATCH"},
		{CodeDecisionConflict, "DECISION_CONFLICT"},
		{CodeOrderChanged, "ORDER_CHANGED"},
		{CodeInventoryMigrationRequired, "INVENTORY_MIGRATION_REQUIRED"},
		{CodeDuplicateSKU, "DUPLICATE_SKU"},
		{CodeSKUMigrationNotAllowed, "SKU_MIGRATION_NOT_ALLOWED"},
		{CodeTransactionConflictExhaust, "TRANSACTION_CONFLICT_EXHAUSTED"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if tc.code != tc.want {
				t.Errorf("错误码常量 = %q，期望 %q", tc.code, tc.want)
			}
		})
	}
}

func TestDecisionOfAndStatus(t *testing.T) {
	if got := DecisionOf(true); got != DecisionApprove {
		t.Errorf("DecisionOf(true) = %q，期望 %q", got, DecisionApprove)
	}
	if got := DecisionOf(false); got != DecisionReject {
		t.Errorf("DecisionOf(false) = %q，期望 %q", got, DecisionReject)
	}

	// Decision.Status() 是直接的类型转换，因此决议与状态必须逐字一致；
	// 一旦两侧取值分叉，批准就会把确认单写成「决议值不是合法状态」的样子。
	if got := DecisionApprove.Status(); got != StatusApproved {
		t.Errorf("DecisionApprove.Status() = %q，期望 %q", got, StatusApproved)
	}
	if got := DecisionReject.Status(); got != StatusRejected {
		t.Errorf("DecisionReject.Status() = %q，期望 %q", got, StatusRejected)
	}
}

func TestConfirmationExpired(t *testing.T) {
	cases := []struct {
		name      string
		status    ConfirmationStatus
		expiresAt time.Time
		want      bool
	}{
		{"待决议且已过期", StatusPending, fixtureNow.Add(-time.Second), true},
		{"待决议且恰好到期", StatusPending, fixtureNow, true},
		{"待决议且未到期", StatusPending, fixtureNow.Add(time.Nanosecond), false},
		{"待决议且提前一小时", StatusPending, fixtureNow.Add(time.Hour), false},
		// 已决议的确认单无论过了多久都不能算过期：重试要看到「已生效」，
		// 而不是「已过期」，否则调用方会以为交易没发生而重新下单。
		{"已批准且早已过期", StatusApproved, fixtureNow.Add(-time.Hour), false},
		{"已拒绝且早已过期", StatusRejected, fixtureNow.Add(-time.Hour), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Confirmation{Status: tc.status, ExpiresAt: tc.expiresAt}
			if got := c.Expired(fixtureNow); got != tc.want {
				t.Errorf("Expired(now) = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestRequestHashStableForEquivalentContent 覆盖幂等键最重要的性质：
// 内容相同而写法不同（顺序不同、同一 SKU 被拆成多行）必须得到同一个摘要。
// 否则调用方的重试会被判成另一笔交易，账本里就会出现两张确认单、两次扣库存。
func TestRequestHashStableForEquivalentContent(t *testing.T) {
	inventory := inventoryOf(
		record("sku-a", "prod-a", "测试商品", 1000, 10, catalog.USD),
		record("sku-b", "prod-b", "防水登山包（大号）", 2500, 10, catalog.USD),
	)
	deps := PrepareDeps{Now: fixtureNow, Inventory: inventory}

	lineA := sku("prod-a", "sku-a", "模型写的标题", 1000, catalog.USD, 1)
	lineB := sku("prod-b", "sku-b", "模型写的标题", 2500, catalog.USD, 2)

	straight := validCreateInput()
	straight.Items = []Item{lineA, lineB}

	reordered := validCreateInput()
	reordered.Items = []Item{lineB, lineA}

	// 同一 SKU 被模型拆成两行：数量必须相加成一行，摘要与「直接给一行数量 2」相同。
	split := validCreateInput()
	split.Items = []Item{
		lineA,
		sku("prod-b", "sku-b", "模型写的标题", 2500, catalog.USD, 1),
		sku("prod-b", "sku-b", "模型写的标题", 2500, catalog.USD, 1),
	}

	base := requestHashOf(t, straight, deps)
	if !isLowerHex(base, 64) {
		t.Errorf("请求摘要 = %q，期望 64 位小写十六进制", base)
	}
	if got := requestHashOf(t, reordered, deps); got != base {
		t.Errorf("明细顺序不同却得到不同摘要：%s vs %s", got, base)
	}
	if got := requestHashOf(t, split, deps); got != base {
		t.Errorf("同一 SKU 拆成两行却得到不同摘要：%s vs %s", got, base)
	}
}

// TestRequestHashChangesWhenContentChanges 是上一条的对照：
// 内容真的变了，摘要就必须变，否则幂等键会把两次不同的交易当成同一次。
func TestRequestHashChangesWhenContentChanges(t *testing.T) {
	baseInput := func() PrepareInput {
		in := validCreateInput()
		in.Items = []Item{sku("prod-a", "sku-a", "标题", 1000, catalog.USD, 1)}
		return in
	}
	baseDeps := func() PrepareDeps {
		return PrepareDeps{
			Now:       fixtureNow,
			Inventory: inventoryOf(record("sku-a", "prod-a", "权威标题", 1000, 100, catalog.USD)),
		}
	}
	base := requestHashOf(t, baseInput(), baseDeps())

	cases := []struct {
		name  string
		build func() (PrepareInput, PrepareDeps)
	}{
		{"数量变化", func() (PrepareInput, PrepareDeps) {
			in := baseInput()
			in.Items[0].Quantity = 2
			return in, baseDeps()
		}},
		{"单价变化（库存同步改价）", func() (PrepareInput, PrepareDeps) {
			in := baseInput()
			in.Items[0].UnitPriceMinor = 1200
			deps := baseDeps()
			deps.Inventory["sku-a"] = record("sku-a", "prod-a", "权威标题", 1200, 100, catalog.USD)
			return in, deps
		}},
		{"币种变化（库存同步改币种）", func() (PrepareInput, PrepareDeps) {
			in := baseInput()
			in.Items[0].UnitPriceMinor = 1200
			in.Items[0].Currency = catalog.JPY
			deps := baseDeps()
			deps.Inventory["sku-a"] = record("sku-a", "prod-a", "权威标题", 1200, 100, catalog.JPY)
			return in, deps
		}},
		{"收货地址变化", func() (PrepareInput, PrepareDeps) {
			in := baseInput()
			in.ShippingAddress.City = "北京"
			return in, baseDeps()
		}},
		{"增加一行明细", func() (PrepareInput, PrepareDeps) {
			in := baseInput()
			in.Items = append(in.Items, sku("prod-b", "sku-b", "另一件", 500, catalog.USD, 1))
			deps := baseDeps()
			deps.Inventory["sku-b"] = record("sku-b", "prod-b", "另一件", 500, 100, catalog.USD)
			return in, deps
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, deps := tc.build()
			if got := requestHashOf(t, in, deps); got == base {
				t.Errorf("%s 之后摘要仍是 %s，幂等键漏掉了这次变更", tc.name, got)
			}
		})
	}
}

// TestRequestHashIgnoresJSONEscapingOfNonASCII 断言规范编码与「谁序列化的」无关。
//
// 同一份内容，Go 侧用字面量写非 ASCII 标题、线上用 \uXXXX 转义写，必须得到同一个
// 摘要：encoding/json 的转义策略、字段顺序都是实现细节，一旦它们进入哈希，
// 同一笔重试在不同实现或不同语言之间就会产生两个幂等键。
func TestRequestHashIgnoresJSONEscapingOfNonASCII(t *testing.T) {
	ascii := asciiAddress()
	literal := Payload{Create: &CreatePayload{
		Items: []Item{
			sku("prod-a", "sku-a", "测试商品", 1000, catalog.USD, 1),
			sku("prod-b", "sku-b", "防水登山包（大号）", 2500, catalog.USD, 1),
		},
		ShippingAddress:  ascii,
		Currency:         catalog.USD,
		TotalAmountMinor: 3500,
	}}

	// 手工写的线上表示：非 ASCII 全部转义，成员顺序与字面量版不同。
	const escapedWire = `{
		"amount_scope": "merchandise_only",
		"order_kind": "purchase_intent",
		"items": [
			{"product_id": "prod-a", "sku_id": "sku-a", "title": "\u6d4b\u8bd5\u5546\u54c1", "unit_price_minor": 1000, "currency": "USD", "quantity": 1},
			{"product_id": "prod-b", "sku_id": "sku-b", "title": "\u9632\u6c34\u767b\u5c71\u5305\uff08\u5927\u53f7\uff09", "unit_price_minor": 2500, "currency": "USD", "quantity": 1}
		],
		"currency": "USD",
		"total_amount_minor": 3500,
		"shipping_address": {
			"recipient": "Li Si", "phone": "13800000000", "country": "CN", "province": "Shanghai",
			"city": "Shanghai", "line1": "No.1 Century Avenue", "line2": "", "postal_code": "200120"
		}
	}`

	var escaped Payload
	if err := json.Unmarshal([]byte(escapedWire), &escaped); err != nil {
		t.Fatalf("手工线上表示解析失败: %v", err)
	}
	// 自检：先确认手写的转义序列解出来正是预期文字，否则后面的摘要比较失败
	// 会被误读成「规范编码有问题」。
	if got := escaped.Create.Items[0].Title; got != "测试商品" {
		t.Fatalf("转义序列解出 %q，期望 测试商品（测试常量写错了）", got)
	}
	if got := escaped.Create.Items[1].Title; got != "防水登山包（大号）" {
		t.Fatalf("转义序列解出 %q，期望 防水登山包（大号）（测试常量写错了）", got)
	}
	if escaped.Create.ShippingAddress != ascii {
		t.Fatalf("地址解析结果 = %+v，期望 %+v", escaped.Create.ShippingAddress, ascii)
	}

	literalHash, err := RequestHash(ActionCreate, literal)
	if err != nil {
		t.Fatalf("RequestHash 报错: %v", err)
	}
	escapedHash, err := RequestHash(ActionCreate, escaped)
	if err != nil {
		t.Fatalf("RequestHash 报错: %v", err)
	}
	if literalHash != escapedHash {
		t.Errorf("同一份内容因转义写法不同而得到不同摘要：%s vs %s", literalHash, escapedHash)
	}

	// 对照：真的换了标题，摘要必须变——否则上面的相等只是「标题压根没进哈希」。
	changed := Payload{Create: &CreatePayload{
		Items: []Item{
			sku("prod-a", "sku-a", "测试商品X", 1000, catalog.USD, 1),
			sku("prod-b", "sku-b", "防水登山包（大号）", 2500, catalog.USD, 1),
		},
		ShippingAddress:  ascii,
		Currency:         catalog.USD,
		TotalAmountMinor: 3500,
	}}
	changedHash, err := RequestHash(ActionCreate, changed)
	if err != nil {
		t.Fatalf("RequestHash 报错: %v", err)
	}
	if changedHash == literalHash {
		t.Error("标题变化后摘要未变，说明标题没有参与摘要")
	}
}

// TestComputeSnapshotHashBindsIdentityAndPayload 断言快照摘要覆盖的每一项。
//
// 少覆盖一项就意味着那一项可以在「用户看到确认单」与「用户按下批准」之间被改掉
// 而摘要不变，这正是两阶段提交最贵的缺陷。
func TestComputeSnapshotHashBindsIdentityAndPayload(t *testing.T) {
	base := storedConfirmation(t, createPayload(), ActionCreate, fixtureNow.Add(time.Minute))
	baseHash := mustHash(t, base)

	if !isLowerHex(baseHash, 64) {
		t.Errorf("快照摘要 = %q，期望 64 位小写十六进制", baseHash)
	}
	if again := mustHash(t, base); again != baseHash {
		t.Errorf("同一份内容两次计算得到不同摘要：%s vs %s", again, baseHash)
	}

	cases := []struct {
		name   string
		mutate func(*Confirmation)
	}{
		{"买方变化", func(c *Confirmation) { c.BuyerID = "buyer-2" }},
		{"会话变化", func(c *Confirmation) { c.SessionID = "session-2" }},
		{"动作变化", func(c *Confirmation) { c.Action = ActionCancel }},
		{"载荷变化", func(c *Confirmation) {
			// 载荷里有指针，必须换一份新的，避免改动共享到基准上。
			payload := createPayload()
			payload.Create.Items[0].Quantity = 3
			payload.Create.TotalAmountMinor = 9096
			c.Payload = payload
		}},
		{"有效期变化", func(c *Confirmation) { c.ExpiresAt = c.ExpiresAt.Add(time.Second) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			tc.mutate(&changed)
			if got := mustHash(t, changed); got == baseHash {
				t.Errorf("%s 之后摘要未变（%s），该字段可以绕过快照校验", tc.name, got)
			}
			// 存储值仍是旧摘要：重算之后必须能与它区分开。
			if SnapshotMatches(changed.SnapshotHash, mustHash(t, changed)) {
				t.Errorf("%s 之后存储摘要与新算摘要仍然相等，存储层无法发现变化", tc.name)
			}
		})
	}
}

// TestConfirmationBindingCoversExactlyFiveFields 防止摘要覆盖面被悄悄缩小：
// 少一个键不会让任何断言失败，只会让摘要安静地失去意义。
func TestConfirmationBindingCoversExactlyFiveFields(t *testing.T) {
	c := Confirmation{BuyerID: "b", SessionID: "s", Action: ActionCreate, ExpiresAt: fixtureNow}
	binding := c.Binding()

	want := []string{"buyer_id", "session_id", "action", "payload", "expires_at"}
	if len(binding) != len(want) {
		t.Errorf("Binding() 成员数 = %d，期望 %d：%v", len(binding), len(want), binding)
	}
	for _, key := range want {
		if _, ok := binding[key]; !ok {
			t.Errorf("Binding() 缺少成员 %q", key)
		}
	}
	if got := binding["action"]; got != string(ActionCreate) {
		t.Errorf("binding[action] = %#v，期望 %q", got, string(ActionCreate))
	}
}

// TestBindingExpiresAtUsesOffsetInsteadOfZulu 断言时间字段的线上形状。
//
// canonical.Time 把 UTC 写成 +00:00 而不是 Z：Z 只出现在 RFC3339 的一种写法里，
// 一旦这里改用 time.Format(time.RFC3339)，同一时刻在不同实现之间就会算出不同摘要。
func TestBindingExpiresAtUsesOffsetInsteadOfZulu(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{
			"UTC 写作 +00:00",
			time.Date(2026, 9, 9, 8, 5, 0, 0, time.UTC),
			"2026-09-09T08:05:00+00:00",
		},
		{
			"非零偏移写作 ±HH:MM",
			time.Date(2026, 9, 9, 8, 5, 0, 0, time.FixedZone("CST", 8*3600)),
			"2026-09-09T08:05:00+08:00",
		},
		{
			"微秒保留六位",
			time.Date(2026, 9, 9, 8, 5, 0, 120000000, time.UTC),
			"2026-09-09T08:05:00.120000+00:00",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Confirmation{BuyerID: "b", SessionID: "s", Action: ActionCreate, ExpiresAt: tc.at}
			raw, ok := c.Binding()["expires_at"].(string)
			if !ok {
				t.Fatalf("expires_at 类型 = %T，期望 string（摘要必须是可规范编码的字符串）", c.Binding()["expires_at"])
			}
			if raw != tc.want {
				t.Errorf("expires_at = %q，期望 %q", raw, tc.want)
			}
			if raw != "" && raw[len(raw)-1] == 'Z' {
				t.Errorf("expires_at = %q 用了 Z 后缀，期望 ±HH:MM 偏移形式", raw)
			}
		})
	}
}

// TestBindingTruncatesNanoseconds 覆盖微秒截断。
//
// 纳秒精度一旦进入哈希，同一时刻的两次采样就会算出不同摘要，
// 「内容没变」与「摘要没变」随之分叉；但微秒以上的差异必须仍然能区分。
func TestBindingTruncatesNanoseconds(t *testing.T) {
	base := Confirmation{
		BuyerID:   "buyer-1",
		SessionID: "session-1",
		Action:    ActionCreate,
		ExpiresAt: time.Date(2026, 9, 9, 8, 5, 0, 123456000, time.UTC),
	}
	baseHash := mustHash(t, base)

	withinMicro := base
	withinMicro.ExpiresAt = base.ExpiresAt.Add(999 * time.Nanosecond)
	if got := mustHash(t, withinMicro); got != baseHash {
		t.Errorf("同一微秒内的纳秒差异改变了摘要：%s vs %s", got, baseHash)
	}
	if got := withinMicro.Binding()["expires_at"]; got != "2026-09-09T08:05:00.123456+00:00" {
		t.Errorf("expires_at = %#v，期望 %q", got, "2026-09-09T08:05:00.123456+00:00")
	}

	nextMicro := base
	nextMicro.ExpiresAt = base.ExpiresAt.Add(time.Microsecond)
	if got := mustHash(t, nextMicro); got == baseHash {
		t.Errorf("跨微秒的差异未改变摘要（%s），过期时刻可以被悄悄推迟", got)
	}
}

func TestSnapshotMatchesAndSnapshotEqual(t *testing.T) {
	// original 是基准摘要：命名避开内置函数 real，含义也更贴切
	original := mustHash(t, storedConfirmation(t, createPayload(), ActionCreate, fixtureNow.Add(time.Minute)))

	cases := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"同一摘要", original, original, true},
		{"不同摘要", "abc", "abd", false},
		{"长度不同", "abc", "ab", false},
		{"空串相等", "", "", true},
		{"一空一非空", "", "a", false},
		{"同长度不同内容", "0123456789abcdef", "0123456789abcdee", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SnapshotMatches(tc.a, tc.b); got != tc.want {
				t.Errorf("SnapshotMatches(%q, %q) = %v，期望 %v", tc.a, tc.b, got, tc.want)
			}
			// SnapshotEqual 是同一实现的对外别名，两者必须给出同一结论。
			if got := SnapshotEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("SnapshotEqual(%q, %q) = %v，期望 %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestNewOrderIDIsRandomLowerHex(t *testing.T) {
	first, err := NewOrderID()
	if err != nil {
		t.Fatalf("NewOrderID 报错: %v", err)
	}
	if !isLowerHex(first, 32) {
		t.Errorf("订单号 = %q，期望 32 位小写十六进制", first)
	}

	second, err := NewOrderID()
	if err != nil {
		t.Fatalf("NewOrderID 报错: %v", err)
	}
	if !isLowerHex(second, 32) {
		t.Errorf("订单号 = %q，期望 32 位小写十六进制", second)
	}
	// 标识必须来自随机源：凡是可预测的顺序号，都能被外部拿来推测他人的订单。
	if first == second {
		t.Errorf("两次生成的订单号相同（%q），随机源或编码有问题", first)
	}
}

func TestTitleOrFallback(t *testing.T) {
	cases := []struct {
		name  string
		title string
		skuID string
		want  string
	}{
		{"正常标题", "登山包", "sku-1", "登山包"},
		{"空标题用规格号兜底", "", "sku-1", "sku-1"},
		{"纯空白标题用规格号兜底", "   ", "sku-1", "sku-1"},
		{"制表符换行也算空白", "\t\n", "sku-1", "sku-1"},
		{"标题两侧空白保留", " 登山包 ", "sku-1", " 登山包 "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TitleOrFallback(tc.title, tc.skuID); got != tc.want {
				t.Errorf("TitleOrFallback(%q, %q) = %q，期望 %q", tc.title, tc.skuID, got, tc.want)
			}
		})
	}
}

// unwrapProbe 是专供「解包同一性」断言使用的错误类型。
//
// 用自定义类型而不是 errors.New：只有具体指针才能直截了当地回答
// 「解包拿到的就是当初传进去的那个对象」，也才不会退化成 error 接口比较。
type unwrapProbe struct {
	msg string
}

func (e *unwrapProbe) Error() string { return e.msg }

// asProbe 把解包结果还原成具体错误指针，供同一性比较使用。
//
// 形参声明为 any 而非 error 是刻意的：errorlint 见到 error 上的类型断言就会
// 要求改用 errors.As，而 errors.As 会沿错误链继续往下找，恰好把断言从
// 「Unwrap 直接吐出 cause」削弱成「链上某处存在 cause」。
func asProbe(v any) (*unwrapProbe, bool) {
	probe, ok := v.(*unwrapProbe)
	return probe, ok
}

func TestStoreErrorFormatting(t *testing.T) {
	err := Errorf(CodeNotFound, "订单 %s 不存在", "order-1")
	if got, want := err.Error(), "NOT_FOUND: 订单 order-1 不存在"; got != want {
		t.Errorf("Error() = %q，期望 %q", got, want)
	}
	if err.Code != CodeNotFound {
		t.Errorf("Code = %q，期望 %q", err.Code, CodeNotFound)
	}
	if err.Unwrap() != nil {
		t.Errorf("没有底层原因时 Unwrap() 应为 nil，得到 %v", err.Unwrap())
	}

	cause := &unwrapProbe{msg: "底层原因"}
	wrapped := Wrapf(CodeOrderChanged, cause, "订单 %s 已变化", "order-1")
	if got, want := wrapped.Error(), "ORDER_CHANGED: 订单 order-1 已变化"; got != want {
		t.Errorf("Error() = %q，期望 %q", got, want)
	}
	if wrapped.Code != CodeOrderChanged {
		t.Errorf("Code = %q，期望 %q", wrapped.Code, CodeOrderChanged)
	}
	// 底层原因要能被 errors.Is/As 走到：日志排查靠它，但对外只暴露错误码与说明。
	if !errors.Is(wrapped, cause) {
		t.Errorf("errors.Is(wrapped, cause) 应为 true（wrapped=%v）", wrapped)
	}
	// 这里要守住的是「Unwrap 一次就吐出当初那个 cause 本身」，而不是「链上能找到 cause」：
	// 后者用 errors.Is(got, cause) 就能满足，那样 Unwrap 返回中间包装时也会通过，断言被削弱。
	// 故把解包结果还原成具体指针后比较同一性——比较的是指针，不是 error 接口。
	got, ok := asProbe(errors.Unwrap(wrapped))
	if !ok || got != cause {
		t.Errorf("Unwrap() = %v，期望 %v（必须是同一个对象，而非能走到它的包装）", errors.Unwrap(wrapped), cause)
	}
}

// TestStoreErrorIsInvalidArgument 断言只有参数类错误才被判定为 ErrInvalidArgument。
// 若把状态冲突也塞进这个基类，接口层就会把「确认单已过期」当成「请求格式错误」返回。
func TestStoreErrorIsInvalidArgument(t *testing.T) {
	cases := []struct {
		code string
		want bool
	}{
		{CodeInvalidArgument, true},
		{CodeNotFound, false},
		{CodeOwnerMismatch, false},
		{CodeOperationConflict, false},
		{CodeConfirmationExpired, false},
		{CodeSnapshotMismatch, false},
		{CodeDecisionConflict, false},
		{CodeOrderChanged, false},
	}

	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			err := Errorf(tc.code, "说明")
			if got := errors.Is(err, ErrInvalidArgument); got != tc.want {
				t.Errorf("errors.Is(%s, ErrInvalidArgument) = %v，期望 %v", tc.code, got, tc.want)
			}
		})
	}

	if errors.Is(errors.New("别的错误"), ErrInvalidArgument) {
		t.Error("无关错误不应被判定为 ErrInvalidArgument")
	}
}
