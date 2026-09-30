package order

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/govalues/decimal"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// fixedNow 是全部用例共用的固定时钟值。
//
// 订单的 CreatedAt / ConfirmedAt / CancelledAt 是迁移的见证字段，必须逐值断言；
// 只有注入固定时间而不是调用 time.Now()，这些断言才不会被机器快慢左右。
var fixedNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// testAddress 返回一个各字段均合法的收货地址。
// 需要构造非法地址的用例先取这个基准，再只改坏一个字段，
// 这样失败一定归因于被改坏的那一条约束。
func testAddress() Address {
	return Address{
		Recipient:  "张伟",
		Phone:      "+86 138 0000 0000",
		Country:    "CN",
		Province:   "广东省",
		City:       "深圳市",
		Line1:      "南山区科技园 1 号",
		Line2:      "A 座 1801",
		PostalCode: "518000",
	}
}

// testLine 返回一行 1299.00 CNY × 2 的合法订单行，小计 2598.00 CNY。
func testLine(t *testing.T) Line {
	t.Helper()
	line, err := NewLine("p-001", "sku-001", "无线降噪耳机", catalog.MustMoney("1299.00", catalog.CNY), 2)
	if err != nil {
		t.Fatalf("构造订单行失败: %v", err)
	}
	return line
}

// testDraft 返回一笔合法的 DRAFT 订单。
func testDraft(t *testing.T) Order {
	t.Helper()
	o, err := NewDraft("o-001", "b-001", []Line{testLine(t)}, testAddress(), fixedNow)
	if err != nil {
		t.Fatalf("构造草稿订单失败: %v", err)
	}
	return o
}

// testConfirmed 返回一笔合法的 CONFIRMED 订单，确认时间等于 fixedNow。
func testConfirmed(t *testing.T) Order {
	t.Helper()
	o, err := NewConfirmed("o-001", "b-001", []Line{testLine(t)}, testAddress(), fixedNow)
	if err != nil {
		t.Fatalf("构造已确认订单失败: %v", err)
	}
	return o
}

// TestAllStatusesReturnsCopy 保护 AllStatuses 的「返回副本」契约。
//
// 调用方拿到的是可自由修改的切片；一旦返回内部切片，某个调用方的就地修改
// 就会污染其他调用方看到的状态全集。
func TestAllStatusesReturnsCopy(t *testing.T) {
	want := []Status{StatusDraft, StatusConfirmed, StatusCancelled}

	got := AllStatuses()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AllStatuses() = %v，期望 %v", got, want)
	}

	// 就地改坏副本，内部状态全集不应受影响。
	got[0] = Status("BOGUS")
	got[1] = Status("BOGUS")

	again := AllStatuses()
	if !reflect.DeepEqual(again, want) {
		t.Errorf("修改返回值后 AllStatuses() = %v，期望仍为 %v", again, want)
	}
	if Status("BOGUS").Valid() {
		t.Error("内部状态全集被调用方污染：BOGUS 变成了合法状态")
	}
}

// TestStatusValid 保护「只有三个大写取值合法」这一契约。
func TestStatusValid(t *testing.T) {
	cases := []struct {
		name string
		s    Status
		want bool
	}{
		{"草稿合法", StatusDraft, true},
		{"已确认合法", StatusConfirmed, true},
		{"已取消合法", StatusCancelled, true},
		{"空字符串非法", Status(""), false},
		{"小写非法", Status("confirmed"), false},
		{"未知大写非法", Status("SHIPPED"), false},
		{"中文非法", Status("已确认"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.Valid(); got != tc.want {
				t.Errorf("Status(%q).Valid() = %v，期望 %v", string(tc.s), got, tc.want)
			}
		})
	}
}

// TestStatusIsTerminal 保护终态判定：只有 CANCELLED 没有出边。
// 未知状态没有出边，但不是终态——把未知当终态会让脏数据被当成正常结束。
func TestStatusIsTerminal(t *testing.T) {
	cases := []struct {
		name string
		s    Status
		want bool
	}{
		{"草稿非终态", StatusDraft, false},
		{"已确认非终态", StatusConfirmed, false},
		{"已取消是终态", StatusCancelled, true},
		{"未知状态不是终态", Status("SHIPPED"), false},
		{"空状态不是终态", Status(""), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.IsTerminal(); got != tc.want {
				t.Errorf("Status(%q).IsTerminal() = %v，期望 %v", string(tc.s), got, tc.want)
			}
		})
	}
}

// TestStatusCanTransitionToAllPairs 穷举状态机全表：3 个合法来源 × 3 个目标。
//
// 状态机是交易账本的核心不变量，必须逐对固定下来：
// DRAFT→{CONFIRMED, CANCELLED}、CONFIRMED→{CANCELLED}、CANCELLED 无出边。
// 顺带记录未知来源没有出边，避免脏状态被当成可迁移状态。
func TestStatusCanTransitionToAllPairs(t *testing.T) {
	cases := []struct {
		name string
		from Status
		to   Status
		want bool
	}{
		{"草稿到草稿禁止", StatusDraft, StatusDraft, false},
		{"草稿到已确认允许", StatusDraft, StatusConfirmed, true},
		{"草稿到已取消允许", StatusDraft, StatusCancelled, true},

		{"已确认到草稿禁止", StatusConfirmed, StatusDraft, false},
		{"已确认到已确认禁止", StatusConfirmed, StatusConfirmed, false},
		{"已确认到已取消允许", StatusConfirmed, StatusCancelled, true},

		{"已取消到草稿禁止", StatusCancelled, StatusDraft, false},
		{"已取消到已确认禁止", StatusCancelled, StatusConfirmed, false},
		{"已取消到已取消禁止", StatusCancelled, StatusCancelled, false},

		{"未知来源无出边", Status("SHIPPED"), StatusConfirmed, false},
		{"空来源无出边", Status(""), StatusCancelled, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.from.CanTransitionTo(tc.to); got != tc.want {
				t.Errorf("%s.CanTransitionTo(%s) = %v，期望 %v",
					tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// TestStatusJSONRoundTrip 固定线上契约：状态就是这三个裸的大写字符串。
// 改成小写不会让别的测试失败，却会静默改变对外契约，因此这里断言精确字节。
func TestStatusJSONRoundTrip(t *testing.T) {
	cases := []struct {
		status Status
		want   string
	}{
		{StatusDraft, `"DRAFT"`},
		{StatusConfirmed, `"CONFIRMED"`},
		{StatusCancelled, `"CANCELLED"`},
	}

	for _, tc := range cases {
		t.Run(tc.status.String(), func(t *testing.T) {
			raw, err := json.Marshal(tc.status)
			if err != nil {
				t.Fatalf("序列化 %s 失败: %v", tc.status, err)
			}
			if string(raw) != tc.want {
				t.Errorf("序列化 %s = %s，期望 %s", tc.status, raw, tc.want)
			}

			var decoded Status
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("反序列化 %s 失败: %v", raw, err)
			}
			if decoded != tc.status {
				t.Errorf("往返后 = %s，期望 %s", decoded, tc.status)
			}
		})
	}
}

// TestStatusMarshalJSONRejectsUnknown 保护写出侧：未知状态必须报错，
// 不能悄悄把一个脏状态写进线上报文。
func TestStatusMarshalJSONRejectsUnknown(t *testing.T) {
	for _, bad := range []Status{"", "SHIPPED", "confirmed", "已确认"} {
		raw, err := json.Marshal(bad)
		if !errors.Is(err, ErrInvalidStatus) {
			t.Errorf("序列化 %q 应返回 ErrInvalidStatus，得到 %v", string(bad), err)
		}
		if raw != nil {
			t.Errorf("序列化 %q 失败时不应产出内容，得到 %s", string(bad), raw)
		}
	}
}

// TestStatusUnmarshalJSONAcceptsValidValues 覆盖读入侧的三条正常路径。
func TestStatusUnmarshalJSONAcceptsValidValues(t *testing.T) {
	cases := []struct {
		raw  string
		want Status
	}{
		{`"DRAFT"`, StatusDraft},
		{`"CONFIRMED"`, StatusConfirmed},
		{`"CANCELLED"`, StatusCancelled},
	}

	for _, tc := range cases {
		t.Run(string(tc.want), func(t *testing.T) {
			var got Status
			if err := json.Unmarshal([]byte(tc.raw), &got); err != nil {
				t.Fatalf("反序列化 %s 失败: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("反序列化 %s = %s，期望 %s", tc.raw, got, tc.want)
			}
		})
	}
}

// TestStatusUnmarshalJSONRejectsBadInput 覆盖读入侧的全部拒绝路径。
//
// 小写 "confirmed" 必须被拒绝：把大小写不敏感当宽容，会让契约悄悄放宽，
// 而放宽之后再收紧就是破坏性变更。非字符串、未知取值、null 同理。
func TestStatusUnmarshalJSONRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"小写已确认", `"confirmed"`},
		{"首字母大写", `"Confirmed"`},
		{"带尾部空白", `"DRAFT "`},
		{"未知取值", `"SHIPPED"`},
		{"空字符串", `""`},
		{"JSON null", `null`},
		{"数字", `123`},
		{"布尔", `true`},
		{"对象", `{}`},
		{"数组", `["DRAFT"]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StatusDraft // 预置合法值，确认失败时不会被悄悄改写
			err := json.Unmarshal([]byte(tc.raw), &got)
			if !errors.Is(err, ErrInvalidStatus) {
				t.Errorf("反序列化 %s 应返回 ErrInvalidStatus，得到 %v", tc.raw, err)
			}
			if got != StatusDraft {
				t.Errorf("反序列化 %s 失败后目标被改写为 %q", tc.raw, string(got))
			}
		})
	}
}

// TestStatusString 保护 Stringer：状态对外呈现就是常量本身。
func TestStatusString(t *testing.T) {
	cases := []struct {
		s    Status
		want string
	}{
		{StatusDraft, "DRAFT"},
		{StatusConfirmed, "CONFIRMED"},
		{StatusCancelled, "CANCELLED"},
		{Status("SHIPPED"), "SHIPPED"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.s.String(); got != tc.want {
				t.Errorf("Status(%q).String() = %q，期望 %q", string(tc.s), got, tc.want)
			}
		})
	}
}

// TestAddressValidate 逐个覆盖 Address.Validate 的失败分支，外加合法基准。
// 每个用例只改坏一个字段，失败必归因于该字段。
func TestAddressValidate(t *testing.T) {
	t.Run("合法地址", func(t *testing.T) {
		if err := testAddress().Validate(); err != nil {
			t.Errorf("合法地址不应报错，得到 %v", err)
		}
	})

	cases := []struct {
		name   string
		mutate func(*Address)
	}{
		{"收货人为空", func(a *Address) { a.Recipient = "" }},
		{"收货人只有空白", func(a *Address) { a.Recipient = "   " }},
		{"联系电话为空", func(a *Address) { a.Phone = "" }},
		{"联系电话只有空白", func(a *Address) { a.Phone = " \t " }},
		{"国家代码过短", func(a *Address) { a.Country = "C" }},
		{"国家代码过长", func(a *Address) { a.Country = "CHN" }},
		{"国家代码为空", func(a *Address) { a.Country = "" }},
		{"国家代码小写", func(a *Address) { a.Country = "cn" }},
		{"国家代码含数字", func(a *Address) { a.Country = "C1" }},
		{"城市为空", func(a *Address) { a.City = "" }},
		{"城市只有空白", func(a *Address) { a.City = "  " }},
		{"详细地址为空", func(a *Address) { a.Line1 = "" }},
		{"详细地址只有空白", func(a *Address) { a.Line1 = "\n" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := testAddress()
			tc.mutate(&addr)
			if err := addr.Validate(); !errors.Is(err, ErrInvalidOrder) {
				t.Errorf("%s 应返回 ErrInvalidOrder，得到 %v", tc.name, err)
			}
		})
	}
}

// TestLineValidate 逐个覆盖 Line.Validate 的失败分支，外加合法基准。
func TestLineValidate(t *testing.T) {
	t.Run("合法订单行", func(t *testing.T) {
		if err := testLine(t).Validate(); err != nil {
			t.Errorf("合法订单行不应报错，得到 %v", err)
		}
	})

	cases := []struct {
		name   string
		mutate func(*Line)
	}{
		{"商品标识为空", func(l *Line) { l.ProductID = "" }},
		{"规格标识为空", func(l *Line) { l.SKUID = "" }},
		{"规格标识只有空白", func(l *Line) { l.SKUID = "  " }},
		{"数量为零", func(l *Line) { l.Quantity = 0 }},
		{"数量为负", func(l *Line) { l.Quantity = -1 }},
		{"币种非法", func(l *Line) { l.UnitPrice = catalog.Money{Currency: "cny"} }},
		{"单价为负", func(l *Line) { l.UnitPrice = catalog.MustMoney("-0.01", catalog.CNY) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := testLine(t)
			tc.mutate(&line)
			if err := line.Validate(); !errors.Is(err, ErrInvalidOrder) {
				t.Errorf("%s 应返回 ErrInvalidOrder，得到 %v", tc.name, err)
			}
		})
	}
}

// TestNewLine 固定构造器的两端：合法输入逐字段落地，非法输入原样透出校验错误。
func TestNewLine(t *testing.T) {
	price := catalog.MustMoney("29.99", catalog.USD)

	line, err := NewLine("p-001", "sku-001", "无线降噪耳机", price, 3)
	if err != nil {
		t.Fatalf("构造订单行失败: %v", err)
	}
	if line.ProductID != "p-001" || line.SKUID != "sku-001" || line.Title != "无线降噪耳机" {
		t.Errorf("构造结果字段错位: %+v", line)
	}
	if !line.UnitPrice.Equal(price) {
		t.Errorf("单价 = %s，期望 %s", line.UnitPrice, price)
	}
	if line.Quantity != 3 {
		t.Errorf("数量 = %d，期望 3", line.Quantity)
	}

	failed, err := NewLine("", "sku-001", "无线降噪耳机", price, 3)
	if !errors.Is(err, ErrInvalidOrder) {
		t.Errorf("空商品标识应返回 ErrInvalidOrder，得到 %v", err)
	}
	if failed != (Line{}) {
		t.Errorf("构造失败时应返回零值订单行，得到 %+v", failed)
	}
}

// TestLineSubtotal 固定「单价 × 数量」的精确结果。
// 若这里退化成 Mul（静默舍入），订单总额就会与逐行相加对不上。
func TestLineSubtotal(t *testing.T) {
	cases := []struct {
		name     string
		price    catalog.Money
		quantity int
		want     string
	}{
		{"两位币种整除", catalog.MustMoney("29.99", catalog.USD), 3, "89.97 USD"},
		{"两位币种乘 1", catalog.MustMoney("1299.00", catalog.CNY), 2, "2598.00 CNY"},
		{"两位币种带尾零", catalog.MustMoney("49.90", catalog.CNY), 3, "149.70 CNY"},
		{"零位币种", catalog.MustMoney("1000", catalog.JPY), 3, "3000 JPY"},
		{"零位币种乘 1", catalog.MustMoney("12800", catalog.KRW), 1, "12800 KRW"},
		{"三位币种", catalog.MustMoney("1.234", catalog.KWD), 2, "2.468 KWD"},
		{"零金额", catalog.MustMoney("0.00", catalog.USD), 5, "0.00 USD"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, err := NewLine("p-001", "sku-001", "测试商品", tc.price, tc.quantity)
			if err != nil {
				t.Fatalf("构造订单行失败: %v", err)
			}

			subtotal, err := line.Subtotal()
			if err != nil {
				t.Fatalf("计算小计失败: %v", err)
			}
			if got := subtotal.String(); got != tc.want {
				t.Errorf("%s × %d = %s，期望 %s", tc.price, tc.quantity, got, tc.want)
			}
			if subtotal.Currency != tc.price.Currency {
				t.Errorf("小计币种 = %s，期望 %s", subtotal.Currency, tc.price.Currency)
			}
		})
	}
}

// TestLineSubtotalRejectsInexactOrOverflow 保护 MulStrict 的两条错误路径。
//
// 单价 1.005 USD 本身违反 Money 的不变式（USD 只有两位小数），
// Validate 看不到这一点，只在小计相乘时才暴露——这正是必须用严格乘法的原因：
// 静默舍入会让订单总额与「单价 × 数量」对不上，属于数据事故。
func TestLineSubtotalRejectsInexactOrOverflow(t *testing.T) {
	t.Run("精度不足必须报错", func(t *testing.T) {
		line := Line{
			ProductID: "p-001",
			SKUID:     "sku-001",
			Title:     "精度不一致的单价",
			UnitPrice: catalog.Money{Amount: decimal.MustParse("1.005"), Currency: catalog.USD},
			Quantity:  3,
		}
		// 1.005 × 3 = 3.015，在两位小数下只能写成 3.02，必须拒绝而不是收敛。
		if err := line.Validate(); err != nil {
			t.Fatalf("该单价本身通过了字段校验，Validate 不应报错: %v", err)
		}

		subtotal, err := line.Subtotal()
		if !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("无法精确表示时应返回 ErrInvalidOrder，得到 %v", err)
		}
		if !errors.Is(err, catalog.ErrInexactResult) {
			t.Errorf("底层应为 catalog.ErrInexactResult，得到 %v", err)
		}
		if subtotal != (catalog.Money{}) {
			t.Errorf("出错时应返回零金额，得到 %s", subtotal)
		}
	})

	t.Run("相乘溢出必须报错", func(t *testing.T) {
		line, err := NewLine("p-huge", "sku-huge", "天价商品",
			catalog.MustMoney("99999999999999999.99", catalog.CNY), 1000)
		if err != nil {
			t.Fatalf("构造订单行失败: %v", err)
		}

		if _, err := line.Subtotal(); err == nil {
			t.Error("小计溢出时应返回错误，而不是回绕")
		} else if !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("溢出应包装 ErrInvalidOrder，得到 %v", err)
		}
	})
}

// TestNewDraft 固定草稿构造：状态恒为 DRAFT，创建时间等于注入时钟，
// 确认/取消的见证字段必须为空，否则「未确认却带确认时间」的订单会溜进账本。
func TestNewDraft(t *testing.T) {
	lines := []Line{testLine(t)}

	o, err := NewDraft("o-001", "b-001", lines, testAddress(), fixedNow)
	if err != nil {
		t.Fatalf("构造草稿订单失败: %v", err)
	}
	if o.ID != "o-001" || o.BuyerID != "b-001" {
		t.Errorf("标识字段错位: ID=%q BuyerID=%q", o.ID, o.BuyerID)
	}
	if o.Status != StatusDraft {
		t.Errorf("状态 = %s，期望 %s", o.Status, StatusDraft)
	}
	if !o.CreatedAt.Equal(fixedNow) {
		t.Errorf("创建时间 = %v，期望 %v", o.CreatedAt, fixedNow)
	}
	if o.ConfirmedAt != nil {
		t.Errorf("草稿不应带确认时间，得到 %v", o.ConfirmedAt)
	}
	if o.CancelledAt != nil {
		t.Errorf("草稿不应带取消时间，得到 %v", o.CancelledAt)
	}
	if o.CancelReason != "" {
		t.Errorf("草稿不应带取消原因，得到 %q", o.CancelReason)
	}
	if len(o.Lines) != 1 || !o.Lines[0].UnitPrice.Equal(lines[0].UnitPrice) {
		t.Errorf("订单行未原样落地: %+v", o.Lines)
	}
	if o.IsTerminal() {
		t.Error("草稿不应处于终态")
	}
}

// TestNewConfirmed 固定已确认构造：状态为 CONFIRMED 且 ConfirmedAt 就是注入时钟。
// 确认时间必须由构造器填，不能留给调用方随手写。
func TestNewConfirmed(t *testing.T) {
	o, err := NewConfirmed("o-001", "b-001", []Line{testLine(t)}, testAddress(), fixedNow)
	if err != nil {
		t.Fatalf("构造已确认订单失败: %v", err)
	}
	if o.Status != StatusConfirmed {
		t.Errorf("状态 = %s，期望 %s", o.Status, StatusConfirmed)
	}
	if !o.CreatedAt.Equal(fixedNow) {
		t.Errorf("创建时间 = %v，期望 %v", o.CreatedAt, fixedNow)
	}
	if o.ConfirmedAt == nil {
		t.Fatal("已确认订单必须带确认时间")
	}
	if !o.ConfirmedAt.Equal(fixedNow) {
		t.Errorf("确认时间 = %v，期望 %v", *o.ConfirmedAt, fixedNow)
	}
	if o.CancelledAt != nil {
		t.Errorf("已确认订单不应带取消时间，得到 %v", o.CancelledAt)
	}
	if o.CancelReason != "" {
		t.Errorf("已确认订单不应带取消原因，得到 %q", o.CancelReason)
	}
	if o.IsTerminal() {
		t.Error("已确认订单不应处于终态（还可以取消）")
	}
	if !o.IsCancelable() {
		t.Error("已确认订单应可取消")
	}
}

// TestNewDraftAndNewConfirmedPropagateValidateErrors 确认两个构造器都会先校验，
// 并把底层 sentinel 原样透出，而不是返回一笔半成品订单。
func TestNewDraftAndNewConfirmedPropagateValidateErrors(t *testing.T) {
	validLines := []Line{testLine(t)}

	cases := []struct {
		name    string
		id      string
		buyerID string
		lines   []Line
		address Address
		wantErr error
	}{
		{"订单标识为空", "", "b-001", validLines, testAddress(), ErrInvalidOrder},
		{"买家标识为空", "o-001", "", validLines, testAddress(), ErrInvalidOrder},
		{"无订单行", "o-001", "b-001", nil, testAddress(), ErrEmptyLines},
		{"地址非法", "o-001", "b-001", validLines, Address{Country: "CN"}, ErrInvalidOrder},
	}

	constructors := []struct {
		name string
		call func(id, buyerID string, lines []Line, addr Address, now time.Time) (Order, error)
	}{
		{"NewDraft", NewDraft},
		{"NewConfirmed", NewConfirmed},
	}

	for _, ctor := range constructors {
		for _, tc := range cases {
			t.Run(ctor.name+"/"+tc.name, func(t *testing.T) {
				o, err := ctor.call(tc.id, tc.buyerID, tc.lines, tc.address, fixedNow)
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("应返回 %v，得到 %v", tc.wantErr, err)
				}
				if o.ID != "" || o.Status != "" || len(o.Lines) != 0 {
					t.Errorf("构造失败时应返回零值订单，得到 %+v", o)
				}
				if !o.CreatedAt.IsZero() {
					t.Errorf("构造失败时不应留下创建时间，得到 %v", o.CreatedAt)
				}
			})
		}
	}
}

// TestOrderValidate 覆盖 Order.Validate 的每一个约束分支。
// 每个用例都从一笔合法草稿出发，只破坏一处，失败必归因于该处。
func TestOrderValidate(t *testing.T) {
	usdLine, err := NewLine("p-002", "sku-002", "海外仓商品", catalog.MustMoney("99.00", catalog.USD), 1)
	if err != nil {
		t.Fatalf("构造订单行失败: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*Order)
		wantErr error
	}{
		{"合法草稿", func(*Order) {}, nil},
		{"订单标识为空", func(o *Order) { o.ID = "" }, ErrInvalidOrder},
		{"买家标识为空", func(o *Order) { o.BuyerID = "" }, ErrInvalidOrder},
		{"状态未知", func(o *Order) { o.Status = Status("SHIPPED") }, ErrInvalidStatus},
		{"状态为空", func(o *Order) { o.Status = Status("") }, ErrInvalidStatus},
		{"无订单行", func(o *Order) { o.Lines = nil }, ErrEmptyLines},
		{"订单行为空切片", func(o *Order) { o.Lines = []Line{} }, ErrEmptyLines},
		{"订单行缺少规格标识", func(o *Order) { o.Lines[0].SKUID = " " }, ErrInvalidOrder},
		{"订单行数量为零", func(o *Order) { o.Lines[0].Quantity = 0 }, ErrInvalidOrder},
		{"币种不一致", func(o *Order) { o.Lines = []Line{o.Lines[0], usdLine} }, ErrCurrencyMismatch},
		{"城市为空", func(o *Order) { o.Address.City = "" }, ErrInvalidOrder},
		{"已取消缺取消时间", func(o *Order) {
			o.Status = StatusCancelled
			o.CancelledAt = nil
			o.CancelReason = "买家反悔"
		}, ErrInvalidOrder},
		{"已取消缺取消原因", func(o *Order) {
			o.Status = StatusCancelled
			o.CancelledAt = &fixedNow
			o.CancelReason = ""
		}, ErrInvalidOrder},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := testDraft(t)
			tc.mutate(&o)
			err := o.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("合法订单不应报错，得到 %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("应返回 %v，得到 %v", tc.wantErr, err)
			}
		})
	}
}

// TestOrderValidateAcceptsCancelledWithWitnessFields 确认已取消订单在
// 「取消时间 + 取消原因」齐备时通过校验——这是上面两个拒绝用例的正向对照。
func TestOrderValidateAcceptsCancelledWithWitnessFields(t *testing.T) {
	o := testDraft(t)
	o.Status = StatusCancelled
	o.CancelledAt = &fixedNow
	o.CancelReason = "库存不足"

	if err := o.Validate(); err != nil {
		t.Errorf("带齐见证字段的已取消订单不应报错，得到 %v", err)
	}
}

// TestOrderCurrency 保护币种取值：空订单返回空币种，非空订单取首行币种。
// 后者不做校验，Validate 才负责发现币种不一致。
func TestOrderCurrency(t *testing.T) {
	t.Run("空订单无币种", func(t *testing.T) {
		o := testDraft(t)
		o.Lines = nil
		if got := o.Currency(); got != catalog.Currency("") {
			t.Errorf("空订单币种 = %q，期望空币种", got)
		}
	})

	t.Run("取首行币种", func(t *testing.T) {
		o := testDraft(t)
		if got := o.Currency(); got != catalog.CNY {
			t.Errorf("币种 = %s，期望 %s", got, catalog.CNY)
		}
	})
}

// TestOrderTotal 固定总额算法：各行小计之和，币种取自订单行。
func TestOrderTotal(t *testing.T) {
	second, err := NewLine("p-002", "sku-002", "耳机线", catalog.MustMoney("49.90", catalog.CNY), 3)
	if err != nil {
		t.Fatalf("构造订单行失败: %v", err)
	}
	jpy, err := NewLine("p-jpy", "sku-jpy", "日本仓商品", catalog.MustMoney("1000", catalog.JPY), 3)
	if err != nil {
		t.Fatalf("构造订单行失败: %v", err)
	}

	cases := []struct {
		name  string
		lines []Line
		want  string
	}{
		// 1299.00 × 2 + 49.90 × 3 = 2598.00 + 149.70 = 2747.70
		{"两行两位币种", []Line{testLine(t), second}, "2747.70 CNY"},
		{"单行两位币种", []Line{testLine(t)}, "2598.00 CNY"},
		{"零位币种", []Line{jpy}, "3000 JPY"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := testDraft(t)
			o.Lines = tc.lines

			total, err := o.Total()
			if err != nil {
				t.Fatalf("求和失败: %v", err)
			}
			if got := total.String(); got != tc.want {
				t.Errorf("总额 = %s，期望 %s", got, tc.want)
			}
			if total.Currency != o.Currency() {
				t.Errorf("总额币种 = %s，期望 %s", total.Currency, o.Currency())
			}
		})
	}
}

// TestOrderTotalWithoutLines 空订单没有可取的币种，求和必须报错而不是返回 0。
func TestOrderTotalWithoutLines(t *testing.T) {
	o := testDraft(t)
	o.Lines = nil

	total, err := o.Total()
	if !errors.Is(err, ErrEmptyLines) {
		t.Errorf("空订单求和应返回 ErrEmptyLines，得到 %v", err)
	}
	if total != (catalog.Money{}) {
		t.Errorf("出错时应返回零金额，得到 %s", total)
	}
}

// TestOrderTotalPropagatesLineErrors 覆盖 Total 内两条错误传播路径：
// 单行小计失败（精度不足）与累计溢出。
// 两者都不能被吞掉，否则总额会静默偏离账面。
func TestOrderTotalPropagatesLineErrors(t *testing.T) {
	t.Run("单行小计失败向上传播", func(t *testing.T) {
		o := testDraft(t)
		o.Lines = []Line{{
			ProductID: "p-bad",
			SKUID:     "sku-bad",
			Title:     "精度不一致的单价",
			UnitPrice: catalog.Money{Amount: decimal.MustParse("1.005"), Currency: catalog.USD},
			Quantity:  3,
		}}

		if _, err := o.Total(); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("小计失败应包装 ErrInvalidOrder，得到 %v", err)
		}
	})

	t.Run("累计溢出向上传播", func(t *testing.T) {
		huge := Line{
			ProductID: "p-huge",
			SKUID:     "sku-huge",
			Title:     "天价商品",
			UnitPrice: catalog.MustMoney("99999999999999999.99", catalog.CNY),
			Quantity:  1,
		}
		o := testDraft(t)
		o.Lines = []Line{huge, huge} // 单行可表示，两行相加溢出

		if _, err := o.Total(); !errors.Is(err, catalog.ErrAmountOverflow) {
			t.Errorf("累计溢出应包装 catalog.ErrAmountOverflow，得到 %v", err)
		}
	})
}

// TestOrderTransitionSucceeds 覆盖三条合法迁移。
//
// 关键断言有三处：
//  1. 目标状态正确；
//  2. 见证字段（ConfirmedAt / CancelledAt / CancelReason）由迁移器填上；
//  3. 接收者本身不被改动——Transition 返回值副本，值语义一旦破坏，
//     后续用同一变量继续迁移就会基于错误的状态。
func TestOrderTransitionSucceeds(t *testing.T) {
	later := fixedNow.Add(time.Hour)

	cases := []struct {
		name            string
		build           func(*testing.T) Order
		from            Status
		to              Status
		reason          string
		wantConfirmedAt *time.Time
		wantCancelledAt *time.Time
		wantReason      string
	}{
		{
			name: "草稿确认", build: testDraft,
			from: StatusDraft, to: StatusConfirmed,
			wantConfirmedAt: &later,
		},
		{
			name: "草稿直接取消", build: testDraft,
			from: StatusDraft, to: StatusCancelled, reason: "买家反悔",
			wantCancelledAt: &later, wantReason: "买家反悔",
		},
		{
			// 取消不抹掉既有的确认时间：确认过再取消的历史必须留在订单上。
			name: "已确认取消", build: testConfirmed,
			from: StatusConfirmed, to: StatusCancelled, reason: "库存不足",
			wantConfirmedAt: &fixedNow, wantCancelledAt: &later, wantReason: "库存不足",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.build(t)
			before := o // 值拷贝，用于证明迁移没有就地改写接收者

			next, err := o.Transition(tc.from, tc.to, later, tc.reason)
			if err != nil {
				t.Fatalf("迁移 %s→%s 失败: %v", tc.from, tc.to, err)
			}

			if next.Status != tc.to {
				t.Errorf("迁移后状态 = %s，期望 %s", next.Status, tc.to)
			}
			if tc.wantConfirmedAt == nil {
				if next.ConfirmedAt != nil {
					t.Errorf("非确认迁移不应写确认时间，得到 %v", *next.ConfirmedAt)
				}
			} else if next.ConfirmedAt == nil {
				t.Error("确认迁移必须写上确认时间")
			} else if !next.ConfirmedAt.Equal(*tc.wantConfirmedAt) {
				t.Errorf("确认时间 = %v，期望 %v", *next.ConfirmedAt, *tc.wantConfirmedAt)
			}

			if tc.wantCancelledAt == nil {
				if next.CancelledAt != nil {
					t.Errorf("非取消迁移不应写取消时间，得到 %v", *next.CancelledAt)
				}
			} else if next.CancelledAt == nil {
				t.Error("取消迁移必须写上取消时间")
			} else if !next.CancelledAt.Equal(*tc.wantCancelledAt) {
				t.Errorf("取消时间 = %v，期望 %v", *next.CancelledAt, *tc.wantCancelledAt)
			}

			if next.CancelReason != tc.wantReason {
				t.Errorf("取消原因 = %q，期望 %q", next.CancelReason, tc.wantReason)
			}

			// 承接字段应保持原样：ConfirmedAt 只在确认时写，CreatedAt 永不变。
			if !next.CreatedAt.Equal(before.CreatedAt) {
				t.Errorf("迁移改写了创建时间: %v → %v", before.CreatedAt, next.CreatedAt)
			}

			if !reflect.DeepEqual(before, o) {
				t.Errorf("迁移就地改动了接收者：迁移前 %+v，迁移后 %+v", before, o)
			}

			// 迁移结果自身必须是通过校验的订单。
			if err := next.Validate(); err != nil {
				t.Errorf("迁移后的订单应仍然合法，得到 %v", err)
			}
		})
	}
}

// TestOrderTransitionSucceedsKeepsConfirmedAtOnCancel 单独固定一条容易写错的语义：
// 已确认订单被取消时，早先的确认时间不能被抹掉——账本要留下「确认过再取消」的痕迹。
func TestOrderTransitionSucceedsKeepsConfirmedAtOnCancel(t *testing.T) {
	confirmed := testConfirmed(t)
	later := fixedNow.Add(2 * time.Hour)

	cancelled, err := confirmed.Transition(StatusConfirmed, StatusCancelled, later, "买家申请退款")
	if err != nil {
		t.Fatalf("取消已确认订单失败: %v", err)
	}
	if cancelled.ConfirmedAt == nil || !cancelled.ConfirmedAt.Equal(fixedNow) {
		t.Errorf("取消后确认时间被抹掉或改写: %v", cancelled.ConfirmedAt)
	}
	if cancelled.CancelledAt == nil || !cancelled.CancelledAt.Equal(later) {
		t.Errorf("取消时间 = %v，期望 %v", cancelled.CancelledAt, later)
	}
	if cancelled.Status != StatusCancelled {
		t.Errorf("状态 = %s，期望 %s", cancelled.Status, StatusCancelled)
	}
}

// TestOrderTransitionRejectsBadMoves 覆盖全部拒绝路径，并确认失败时返回零值订单。
//
// 三条错误语义互不替代：
//   - 当前状态与 from 不符 → ErrStaleStatus（并发下的状态已被改动的信号）；
//   - 迁移本身不在状态机里 → ErrIllegalTransition；
//   - 取消但没写原因 → ErrCancelReasonRequired（不允许留下没有理由的取消单）。
func TestOrderTransitionRejectsBadMoves(t *testing.T) {
	cases := []struct {
		name    string
		build   func(*testing.T) Order
		from    Status
		to      Status
		reason  string
		wantErr error
	}{
		{
			name: "草稿却声称从已确认迁出", build: testDraft,
			from: StatusConfirmed, to: StatusCancelled, reason: "库存不足",
			wantErr: ErrStaleStatus,
		},
		{
			name: "已确认却声称从草稿迁出", build: testConfirmed,
			from: StatusDraft, to: StatusConfirmed,
			wantErr: ErrStaleStatus,
		},
		{
			name: "已确认回退到草稿", build: testConfirmed,
			from: StatusConfirmed, to: StatusDraft,
			wantErr: ErrIllegalTransition,
		},
		{
			name: "草稿迁移到自身", build: testDraft,
			from: StatusDraft, to: StatusDraft,
			wantErr: ErrIllegalTransition,
		},
		{
			name: "草稿迁移到未知状态", build: testDraft,
			from: StatusDraft, to: Status("SHIPPED"),
			wantErr: ErrIllegalTransition,
		},
		{
			name: "已取消再确认", build: func(t *testing.T) Order {
				return cancelledOrder(t, fixedNow)
			},
			from: StatusCancelled, to: StatusConfirmed,
			wantErr: ErrIllegalTransition,
		},
		{
			name: "已取消再取消", build: func(t *testing.T) Order {
				return cancelledOrder(t, fixedNow)
			},
			from: StatusCancelled, to: StatusCancelled, reason: "重复取消",
			wantErr: ErrIllegalTransition,
		},
		{
			name: "已取消回到草稿", build: func(t *testing.T) Order {
				return cancelledOrder(t, fixedNow)
			},
			from: StatusCancelled, to: StatusDraft,
			wantErr: ErrIllegalTransition,
		},
		{
			name: "取消未给原因", build: testDraft,
			from: StatusDraft, to: StatusCancelled,
			wantErr: ErrCancelReasonRequired,
		},
		{
			name: "已确认取消未给原因", build: testConfirmed,
			from: StatusConfirmed, to: StatusCancelled,
			wantErr: ErrCancelReasonRequired,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.build(t)
			before := o
			later := fixedNow.Add(time.Hour)

			next, err := o.Transition(tc.from, tc.to, later, tc.reason)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("应返回 %v，得到 %v", tc.wantErr, err)
			}
			if next.ID != "" || next.Status != "" || len(next.Lines) != 0 {
				t.Errorf("迁移失败时应返回零值订单，得到 %+v", next)
			}
			if !reflect.DeepEqual(before, o) {
				t.Errorf("迁移失败不应改动接收者：迁移前 %+v，迁移后 %+v", before, o)
			}
		})
	}
}

// cancelledOrder 构造一笔状态与见证字段都自洽的已取消订单。
func cancelledOrder(t *testing.T, at time.Time) Order {
	t.Helper()
	o := testDraft(t)
	o.Status = StatusCancelled
	o.CancelledAt = &at
	o.CancelReason = "买家反悔"
	return o
}

// TestOrderIsTerminalAndIsCancelable 固定两个派生谓词与状态的一一对应。
// IsCancelable 只认 CONFIRMED：草稿取消走的是另一条语义，已取消不可再取消。
func TestOrderIsTerminalAndIsCancelable(t *testing.T) {
	cases := []struct {
		name           string
		status         Status
		wantTerminal   bool
		wantCancelable bool
	}{
		{"草稿", StatusDraft, false, false},
		{"已确认", StatusConfirmed, false, true},
		{"已取消", StatusCancelled, true, false},
		{"未知状态", Status("SHIPPED"), false, false},
		{"空状态", Status(""), false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := testDraft(t)
			o.Status = tc.status

			if got := o.IsTerminal(); got != tc.wantTerminal {
				t.Errorf("Status=%s IsTerminal() = %v，期望 %v", tc.status, got, tc.wantTerminal)
			}
			if got := o.IsCancelable(); got != tc.wantCancelable {
				t.Errorf("Status=%s IsCancelable() = %v，期望 %v", tc.status, got, tc.wantCancelable)
			}
		})
	}
}

// TestOrderJSONExposesStatusAsBareUppercaseString 端到端确认订单经 JSON 往返后
// 状态不被改写：Status 的编解码契约必须与订单结构体一起工作。
func TestOrderJSONExposesStatusAsBareUppercaseString(t *testing.T) {
	original := testDraft(t)

	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("序列化订单失败: %v", err)
	}

	var decoded Order
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("反序列化订单失败: %v", err)
	}
	if decoded.Status != StatusDraft {
		t.Errorf("往返后状态 = %s，期望 %s", decoded.Status, StatusDraft)
	}
	if err := decoded.Validate(); err != nil {
		t.Errorf("往返后的订单应仍然合法，得到 %v", err)
	}

	// 小写状态在订单层也必须被拒绝，不能因为包了结构体就放宽。
	var rejected Order
	if err := json.Unmarshal([]byte(`{"Status":"confirmed"}`), &rejected); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("订单内小写状态应返回 ErrInvalidStatus，得到 %v", err)
	}
}
