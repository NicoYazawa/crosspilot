package order

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

var fixedNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func testAddress() Address {
	return Address{
		Recipient:  "张伟",
		Phone:      "+86 138 0000 0000",
		Country:    "CN",
		Province:   "广东省",
		City:       "深圳市",
		Line1:      "南山区科技园 1 号",
		PostalCode: "518000",
	}
}

func testLine(t *testing.T) Line {
	t.Helper()
	line, err := NewLine("p-001", "无线降噪耳机", catalog.MustMoney("1299.00", catalog.CNY), 2)
	if err != nil {
		t.Fatalf("构造订单行失败: %v", err)
	}
	return line
}

func testOrder(t *testing.T) Order {
	t.Helper()
	o, err := New("o-001", "b-001", "s-001", []Line{testLine(t)}, testAddress(), fixedNow)
	if err != nil {
		t.Fatalf("构造订单失败: %v", err)
	}
	return o
}

func TestStatusValid(t *testing.T) {
	for _, s := range AllStatuses() {
		if !s.Valid() {
			t.Errorf("%s 应为合法状态", s)
		}
	}
	for _, bad := range []Status{"", "PENDING", "shipped", "已确认"} {
		if bad.Valid() {
			t.Errorf("%q 不应为合法状态", bad)
		}
	}
}

func TestStatusIsTerminal(t *testing.T) {
	cases := map[Status]bool{
		StatusPending:   false,
		StatusConfirmed: true,
		StatusCancelled: true,
		StatusExpired:   true,
	}
	for s, want := range cases {
		if got := s.IsTerminal(); got != want {
			t.Errorf("%s.IsTerminal() = %v，期望 %v", s, got, want)
		}
	}
}

func TestStatusCanTransitionTo(t *testing.T) {
	if !StatusPending.CanTransitionTo(StatusConfirmed) {
		t.Error("pending 应可迁移到 confirmed")
	}
	if !StatusPending.CanTransitionTo(StatusCancelled) {
		t.Error("pending 应可迁移到 cancelled")
	}
	if !StatusPending.CanTransitionTo(StatusExpired) {
		t.Error("pending 应可迁移到 expired")
	}
	if StatusPending.CanTransitionTo(StatusPending) {
		t.Error("pending 不应迁移到自身")
	}
	if StatusConfirmed.CanTransitionTo(StatusCancelled) {
		t.Error("终态 confirmed 不应再有出边")
	}
	// 未知状态没有出边
	if Status("bogus").CanTransitionTo(StatusConfirmed) {
		t.Error("未知状态不应有出边")
	}
}

func TestStatusJSONRejectsUnknown(t *testing.T) {
	if _, err := json.Marshal(StatusPending); err != nil {
		t.Fatalf("序列化合法状态失败: %v", err)
	}
	if _, err := json.Marshal(Status("bogus")); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("序列化未知状态应返回 ErrInvalidStatus，得到 %v", err)
	}

	var s Status
	if err := json.Unmarshal([]byte(`"confirmed"`), &s); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if s != StatusConfirmed {
		t.Errorf("反序列化结果 = %s，期望 confirmed", s)
	}

	for _, bad := range []string{`"shipped"`, `123`, `{}`} {
		var target Status
		if err := json.Unmarshal([]byte(bad), &target); !errors.Is(err, ErrInvalidStatus) {
			t.Errorf("反序列化 %s 应返回 ErrInvalidStatus，得到 %v", bad, err)
		}
	}
}

func TestStatusString(t *testing.T) {
	if got := StatusPending.String(); got != "pending" {
		t.Errorf("String() = %q，期望 \"pending\"", got)
	}
}

func TestLineSubtotal(t *testing.T) {
	line := testLine(t) // 1299.00 × 2
	subtotal, err := line.Subtotal()
	if err != nil {
		t.Fatalf("计算小计失败: %v", err)
	}
	if got := subtotal.String(); got != "2598.00 CNY" {
		t.Errorf("小计 = %s，期望 2598.00 CNY", got)
	}
}

func TestNewLineRejectsBadInput(t *testing.T) {
	price := catalog.MustMoney("10.00", catalog.CNY)

	if _, err := NewLine("", "标题", price, 1); !errors.Is(err, ErrInvalidOrder) {
		t.Errorf("空商品标识应被拒绝，得到 %v", err)
	}
	if _, err := NewLine("p", "标题", price, 0); !errors.Is(err, ErrInvalidOrder) {
		t.Errorf("零数量应被拒绝，得到 %v", err)
	}
	if _, err := NewLine("p", "标题", price, -1); !errors.Is(err, ErrInvalidOrder) {
		t.Errorf("负数量应被拒绝，得到 %v", err)
	}
	if _, err := NewLine("p", "标题", catalog.Money{Currency: "cny"}, 1); !errors.Is(err, ErrInvalidOrder) {
		t.Errorf("非法币种应被拒绝，得到 %v", err)
	}
	if _, err := NewLine("p", "标题", catalog.MustMoney("-1.00", catalog.CNY), 1); !errors.Is(err, ErrInvalidOrder) {
		t.Errorf("负单价应被拒绝，得到 %v", err)
	}
}

func TestAddressValidate(t *testing.T) {
	if err := testAddress().Validate(); err != nil {
		t.Errorf("合法地址不应报错，得到 %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Address)
	}{
		{"收货人为空", func(a *Address) { a.Recipient = "  " }},
		{"电话为空", func(a *Address) { a.Phone = "" }},
		{"国家码小写", func(a *Address) { a.Country = "cn" }},
		{"国家码过长", func(a *Address) { a.Country = "CHN" }},
		{"城市为空", func(a *Address) { a.City = "" }},
		{"详细地址为空", func(a *Address) { a.Line1 = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := testAddress()
			tc.mutate(&addr)
			if err := addr.Validate(); !errors.Is(err, ErrInvalidOrder) {
				t.Errorf("应返回 ErrInvalidOrder，得到 %v", err)
			}
		})
	}
}

func TestNewOrderStartsPending(t *testing.T) {
	o := testOrder(t)
	if o.Status != StatusPending {
		t.Errorf("新订单状态 = %s，期望 pending", o.Status)
	}
	if !o.CreatedAt.Equal(fixedNow) {
		t.Errorf("创建时间 = %v，期望 %v", o.CreatedAt, fixedNow)
	}
	if o.IsTerminal() {
		t.Error("新订单不应处于终态")
	}
}

func TestOrderValidateRejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Order)
		want   error
	}{
		{"订单标识为空", func(o *Order) { o.ID = "" }, ErrInvalidOrder},
		{"买家标识为空", func(o *Order) { o.BuyerID = "" }, ErrInvalidOrder},
		{"状态非法", func(o *Order) { o.Status = "bogus" }, ErrInvalidStatus},
		{"无订单行", func(o *Order) { o.Lines = nil }, ErrEmptyLines},
		{"地址非法", func(o *Order) { o.Address.City = "" }, ErrInvalidOrder},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := testOrder(t)
			tc.mutate(&o)
			if err := o.Validate(); !errors.Is(err, tc.want) {
				t.Errorf("应返回 %v，得到 %v", tc.want, err)
			}
		})
	}
}

func TestOrderValidateRejectsMixedCurrency(t *testing.T) {
	cnLine := testLine(t)
	usLine, err := NewLine("p-002", "耳机", catalog.MustMoney("99.00", catalog.USD), 1)
	if err != nil {
		t.Fatal(err)
	}

	o := testOrder(t)
	o.Lines = []Line{cnLine, usLine}
	if err := o.Validate(); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("混合币种应返回 ErrCurrencyMismatch，得到 %v", err)
	}
	if _, err := o.Total(); err == nil {
		t.Error("混合币种求和应报错")
	}
}

func TestOrderTotal(t *testing.T) {
	first := testLine(t) // 1299.00 × 2 = 2598.00
	second, err := NewLine("p-002", "耳机线", catalog.MustMoney("49.90", catalog.CNY), 3)
	if err != nil {
		t.Fatal(err)
	}

	o := testOrder(t)
	o.Lines = []Line{first, second}

	total, err := o.Total()
	if err != nil {
		t.Fatalf("求和失败: %v", err)
	}
	// 2598.00 + 149.70 = 2747.70
	if got := total.String(); got != "2747.70 CNY" {
		t.Errorf("总额 = %s，期望 2747.70 CNY", got)
	}
}

func TestOrderTotalWithoutLines(t *testing.T) {
	o := testOrder(t)
	o.Lines = nil
	if _, err := o.Total(); !errors.Is(err, ErrEmptyLines) {
		t.Errorf("空订单求和应返回 ErrEmptyLines，得到 %v", err)
	}
}

func TestOrderTransition(t *testing.T) {
	o := testOrder(t)
	later := fixedNow.Add(time.Hour)

	confirmed, err := o.Transition(StatusPending, StatusConfirmed, later)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if confirmed.Status != StatusConfirmed {
		t.Errorf("迁移后状态 = %s，期望 confirmed", confirmed.Status)
	}
	if !confirmed.UpdatedAt.Equal(later) {
		t.Errorf("迁移后更新时间 = %v，期望 %v", confirmed.UpdatedAt, later)
	}
	if !confirmed.IsTerminal() {
		t.Error("confirmed 应为终态")
	}
	// 迁移返回副本，原订单不受影响
	if o.Status != StatusPending {
		t.Error("迁移不应修改原订单")
	}
}

func TestOrderTransitionRejectsStaleStatus(t *testing.T) {
	o := testOrder(t)
	// 实际是 pending，却声称从 confirmed 迁出
	if _, err := o.Transition(StatusConfirmed, StatusCancelled, fixedNow); !errors.Is(err, ErrStaleStatus) {
		t.Errorf("状态不符应返回 ErrStaleStatus，得到 %v", err)
	}
}

func TestNewOrderRejectsBadInput(t *testing.T) {
	if _, err := New("", "b-001", "s-001", []Line{testLine(t)}, testAddress(), fixedNow); !errors.Is(err, ErrInvalidOrder) {
		t.Errorf("空订单标识应被拒绝，得到 %v", err)
	}
}

func TestOrderValidateRejectsBadLineInside(t *testing.T) {
	o := testOrder(t)
	o.Lines = []Line{{
		ProductID: "p-001",
		Title:     "耳机",
		UnitPrice: catalog.MustMoney("10.00", catalog.CNY),
		Quantity:  0, // 非法
	}}
	if err := o.Validate(); !errors.Is(err, ErrInvalidOrder) {
		t.Errorf("含非法订单行应返回 ErrInvalidOrder，得到 %v", err)
	}
}

func TestLineSubtotalOverflowReturnsError(t *testing.T) {
	line, err := NewLine("p-huge", "天价商品", catalog.MustMoney("99999999999999999.99", catalog.CNY), 2)
	if err != nil {
		t.Fatalf("构造订单行失败: %v", err)
	}
	if _, err := line.Subtotal(); err == nil {
		t.Error("小计溢出时应返回错误")
	}
}

func TestOrderTotalOverflowReturnsError(t *testing.T) {
	big, err := NewLine("p-big", "天价商品", catalog.MustMoney("99999999999999999.99", catalog.CNY), 1)
	if err != nil {
		t.Fatal(err)
	}

	o := testOrder(t)
	o.Lines = []Line{big, big} // 各行小计均可表示，但累计溢出
	if _, err := o.Total(); err == nil {
		t.Error("累计溢出时应返回错误")
	}

	// 单行小计本身溢出
	overflowing, err := NewLine("p-big", "天价商品", catalog.MustMoney("99999999999999999.99", catalog.CNY), 2)
	if err != nil {
		t.Fatal(err)
	}
	single := testOrder(t)
	single.Lines = []Line{overflowing}
	if _, err := single.Total(); err == nil {
		t.Error("单行小计溢出时应返回错误")
	}
}

func TestOrderTransitionRejectsIllegalMove(t *testing.T) {
	confirmed := testOrder(t)
	confirmed.Status = StatusConfirmed

	if _, err := confirmed.Transition(StatusConfirmed, StatusPending, fixedNow); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("终态迁出应返回 ErrIllegalTransition，得到 %v", err)
	}
}
