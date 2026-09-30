package trade

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/order"
	trade "github.com/NicoYazawa/crosspilot/internal/domain/trade"
)

// fixedNow 是全部用例共用的固定时刻。
//
// 有效期会进入确认单快照摘要，只有注入固定时钟而不是调用 time.Now()，
// 「有效期恰好是多少」这类断言才不会被机器的运行时刻左右。
var fixedNow = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// wantExpiry 是默认有效期下的期望过期时刻。
//
// 故意用与服务完全相同的表达式而不是写死一个时间字面量：这里要钉住的是
// 「有效期 = 现在 + TTL」，而不是某一个具体的日历时刻。
var wantExpiry = fixedNow.Add(defaultConfirmationTTL)

// addressFixture 返回一份各字段均合法的收货地址。
// 需要构造非法地址的用例先取这个基准再只改坏一个字段，
// 这样失败一定归因于被改坏的那一条，而不是被别的缺失字段掩盖。
func addressFixture() order.Address {
	return order.Address{
		Recipient:  "王五",
		Phone:      "+86 139 0000 0000",
		Country:    "CN",
		Province:   "浙江省",
		City:       "杭州市",
		Line1:      "西湖区文三路 100 号",
		PostalCode: "310012",
	}
}

// validPlaceInput 返回一份「除被测字段外全部合法」的下单输入。
//
// 明细故意乱序且含重复规格：服务必须先按 SKU 排序、把同一规格的数量相加，
// 再交给账本，这些行为只有在输入本身是乱的时候才可能被断言到。
func validPlaceInput() PlaceOrderInput {
	return PlaceOrderInput{
		OperationID: "op-1",
		BuyerID:     "buyer-1",
		SessionID:   "session-1",
		Items: []OrderIntent{
			{ProductID: "prod-b", SKUID: "sku-b1", Quantity: 2},
			{ProductID: "prod-a", SKUID: "sku-a2", Quantity: 1},
			{ProductID: "prod-a", SKUID: "sku-a2", Quantity: 3},
		},
		ShippingAddress: addressFixture(),
	}
}

// catalogFixture 返回一份内容固定的商品目录读模型。
// 规格刻意不按标识升序排列，用来确认排序是服务做的，而不是碰巧与目录顺序一致。
func catalogFixture() *fakeCatalog {
	return &fakeCatalog{products: map[string]CatalogProduct{
		"prod-a": {
			ProductID: "prod-a",
			Title:     "登山包",
			SKUs: []CatalogSKU{
				{SKUID: "sku-a1", Spec: "30L", Price: catalog.MustMoney("19.90", catalog.USD), Stock: 3},
				{SKUID: "sku-a2", Spec: "45L", Price: catalog.MustMoney("25.99", catalog.USD), Stock: 7},
			},
		},
		"prod-b": {
			ProductID: "prod-b",
			Title:     "保温水壶",
			SKUs: []CatalogSKU{
				{SKUID: "sku-b1", Spec: "500ml", Price: catalog.MustMoney("15.00", catalog.USD), Stock: 9},
			},
		},
	}}
}

// sampleConfirmation 返回一张内容固定的确认单，用于断言「服务原样把账本的返回值
// 交给调用方」，而不是自己拼一张看起来差不多的。
func sampleConfirmation() trade.Confirmation {
	return trade.Confirmation{
		ConfirmationID: "c-0001",
		OperationID:    "op-1",
		BuyerID:        "buyer-1",
		SessionID:      "session-1",
		Action:         trade.ActionCreate,
		RequestHash:    "req-hash",
		SnapshotHash:   strings.Repeat("ab", 32),
		ExpiresAt:      wantExpiry,
		Status:         trade.StatusPending,
		CreatedAt:      fixedNow,
	}
}

// --- 测试替身 ---

// fakeStore 记录每一次调用，并可按需返回预设错误。
//
// 记录的粒度是「入参 + 次数」：服务层的职责就是「把正确的参数交给账本」，
// 只断言「没有报错」等于什么都没测——账本究竟被调用成什么样，才是被测对象。
type fakeStore struct {
	// calls 是本替身收到的调用总数，用来断言「校验失败时根本没有碰账本」。
	calls int

	prepareCalls []trade.PrepareRequest
	prepareErr   error
	prepareOut   trade.Confirmation

	confirmationCalls []confirmationArgs
	confirmationErr   error
	confirmationOut   trade.Confirmation

	confirmationsCalls []confirmationsArgs
	confirmationsErr   error
	confirmationsOut   []trade.Confirmation

	resolveCalls []resolveArgs
	resolveErr   error
	resolveOut   trade.Confirmation

	orderCalls []orderArgs
	orderErr   error
	orderOut   trade.OrderSnapshot

	ordersCalls []trade.OrderFilter
	ordersErr   error
	ordersOut   trade.OrderPage

	inventoryCalls [][]trade.SeedSKU
	inventoryErr   error
}

// confirmationArgs 是 Confirmation 一次调用的入参。
type confirmationArgs struct{ confirmationID, buyerID, sessionID string }

// confirmationsArgs 是 Confirmations 一次调用的入参。
type confirmationsArgs struct {
	buyerID, sessionID string
	limit              int
}

// resolveArgs 是 Resolve 一次调用的入参。
type resolveArgs struct {
	confirmationID, buyerID, sessionID, snapshotHash string
	decision                                         trade.Decision
}

// orderArgs 是 Order 一次调用的入参。
type orderArgs struct{ orderID, buyerID string }

func (s *fakeStore) InitializeInventory(_ context.Context, skus []trade.SeedSKU) error {
	s.calls++
	s.inventoryCalls = append(s.inventoryCalls, skus)
	return s.inventoryErr
}

func (s *fakeStore) Prepare(_ context.Context, req trade.PrepareRequest) (trade.Confirmation, error) {
	s.calls++
	s.prepareCalls = append(s.prepareCalls, req)
	return s.prepareOut, s.prepareErr
}

func (s *fakeStore) Confirmation(
	_ context.Context,
	confirmationID, buyerID, sessionID string,
) (trade.Confirmation, error) {
	s.calls++
	s.confirmationCalls = append(s.confirmationCalls,
		confirmationArgs{confirmationID: confirmationID, buyerID: buyerID, sessionID: sessionID})
	return s.confirmationOut, s.confirmationErr
}

func (s *fakeStore) Confirmations(
	_ context.Context,
	buyerID, sessionID string,
	limit int,
) ([]trade.Confirmation, error) {
	s.calls++
	s.confirmationsCalls = append(s.confirmationsCalls,
		confirmationsArgs{buyerID: buyerID, sessionID: sessionID, limit: limit})
	return s.confirmationsOut, s.confirmationsErr
}

func (s *fakeStore) Resolve(
	_ context.Context,
	confirmationID, buyerID, sessionID, snapshotHash string,
	decision trade.Decision,
) (trade.Confirmation, error) {
	s.calls++
	s.resolveCalls = append(s.resolveCalls, resolveArgs{
		confirmationID: confirmationID,
		buyerID:        buyerID,
		sessionID:      sessionID,
		snapshotHash:   snapshotHash,
		decision:       decision,
	})
	return s.resolveOut, s.resolveErr
}

func (s *fakeStore) Order(_ context.Context, orderID, buyerID string) (trade.OrderSnapshot, error) {
	s.calls++
	s.orderCalls = append(s.orderCalls, orderArgs{orderID: orderID, buyerID: buyerID})
	return s.orderOut, s.orderErr
}

func (s *fakeStore) Orders(_ context.Context, filter trade.OrderFilter) (trade.OrderPage, error) {
	s.calls++
	s.ordersCalls = append(s.ordersCalls, filter)
	return s.ordersOut, s.ordersErr
}

// fakeCatalog 是按标识索引的假商品目录，同时记录被查询过的商品标识。
type fakeCatalog struct {
	products map[string]CatalogProduct
	err      error
	calls    []string
}

func (c *fakeCatalog) Find(_ context.Context, productID string) (CatalogProduct, bool, error) {
	c.calls = append(c.calls, productID)
	if c.err != nil {
		return CatalogProduct{}, false, c.err
	}
	product, ok := c.products[productID]
	return product, ok, nil
}

// fakeClock 是固定时钟，避免任何断言依赖真实时间。
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// --- 断言辅助 ---

// newService 构造服务，失败即终止：构造失败属于测试装置的问题，不是被测行为。
func newService(t *testing.T, cfg Config) *Service {
	t.Helper()
	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("构造服务失败: %v", err)
	}
	return svc
}

// onlyPrepare 取出唯一一次 Prepare 调用的入参。
func onlyPrepare(t *testing.T, store *fakeStore) trade.PrepareRequest {
	t.Helper()
	if len(store.prepareCalls) != 1 {
		t.Fatalf("Prepare 调用次数 = %d，期望 1", len(store.prepareCalls))
	}
	return store.prepareCalls[0]
}

// codeOf 取出稳定的错误码。
//
// 断言错误码而不是错误消息：消息是给人看的、会改；错误码是接口层映射状态与
// 前端分支的依据，不能改，因此测试必须钉住它。
func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("期望返回错误，实际为 nil")
	}
	var storeErr *trade.StoreError
	if !errors.As(err, &storeErr) {
		t.Fatalf("错误 %v 不是 *trade.StoreError，取不出错误码", err)
	}
	return storeErr.Code
}

// wantCode 断言错误码。
func wantCode(t *testing.T, err error, want string) {
	t.Helper()
	if got := codeOf(t, err); got != want {
		t.Fatalf("错误码 = %s，期望 %s（原始错误：%v）", got, want, err)
	}
}

// isLowerHex 报告字符串是否为指定长度的小写十六进制。
// 生成的标识形状是线上契约，必须逐字符检查，而不是只看「非空」。
func isLowerHex(s string, length int) bool {
	if len(s) != length {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// --- 构造 ---

func TestNewRejectsMissingDependencies(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"缺少账本", Config{Clock: &fakeClock{now: fixedNow}}, "Store"},
		{"缺少时钟", Config{Store: &fakeStore{}}, "Clock"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, err := New(tc.cfg)
			if err == nil {
				t.Fatal("期望构造失败，实际成功")
			}
			if svc != nil {
				t.Errorf("构造失败时服务应为 nil，实际 %v", svc)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 %q 未点名缺失的依赖 %q", err, tc.want)
			}
		})
	}
}

func TestNewResolvesConfirmationTTL(t *testing.T) {
	cases := []struct {
		name  string
		given time.Duration
		want  time.Duration
	}{
		{"零值取默认", 0, defaultConfirmationTTL},
		{"负值取默认", -time.Minute, defaultConfirmationTTL},
		{"显式值保留", 90 * time.Second, 90 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newService(t, Config{
				Store:           &fakeStore{},
				Clock:           &fakeClock{now: fixedNow},
				ConfirmationTTL: tc.given,
			})
			if svc.ttl != tc.want {
				t.Errorf("有效期 = %v，期望 %v", svc.ttl, tc.want)
			}
			if svc.ttl != defaultConfirmationTTL && tc.given <= 0 {
				t.Errorf("有效期未回落到默认值：%v", svc.ttl)
			}
		})
	}
}

// TestNewAllowsMissingCatalog 钉住「目录可以为空」这个构造决策。
//
// 空的目录只应影响下单这一条路径：账本的读路径与决议路径不该因为商品目录还没接好
// 而整体不可用，因此 New 不拒绝，PlaceOrder 自己给出明确错误。
func TestNewAllowsMissingCatalog(t *testing.T) {
	store := &fakeStore{confirmationOut: sampleConfirmation()}
	svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

	_, err := svc.PlaceOrder(context.Background(), validPlaceInput())
	wantCode(t, err, trade.CodeInvalidArgument)
	if !strings.Contains(err.Error(), "商品目录") {
		t.Errorf("错误 %q 未说明商品目录缺失", err)
	}
	if store.calls != 0 {
		t.Errorf("没有目录时不应触碰账本，实际调用 %d 次", store.calls)
	}

	got, err := svc.Get(context.Background(), "c-0001", "buyer-1", "session-1")
	if err != nil {
		t.Fatalf("没有目录时查单应照常工作，实际失败: %v", err)
	}
	if !reflect.DeepEqual(got, sampleConfirmation()) {
		t.Errorf("查单结果 = %+v，期望 %+v", got, sampleConfirmation())
	}
}

// --- PlaceOrder ---

func TestPlaceOrderHappyPath(t *testing.T) {
	store := &fakeStore{prepareOut: sampleConfirmation()}
	productCatalog := catalogFixture()
	svc := newService(t, Config{Store: store, Catalog: productCatalog, Clock: &fakeClock{now: fixedNow}})

	got, err := svc.PlaceOrder(context.Background(), validPlaceInput())
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	if !reflect.DeepEqual(got, sampleConfirmation()) {
		t.Errorf("返回值 = %+v，期望原样返回账本的确认单 %+v", got, sampleConfirmation())
	}

	// 逐字段钉住交给账本的请求：排序、合并、单价换算与标题格式都在这里体现。
	want := trade.PrepareRequest{
		OperationID: "op-1",
		BuyerID:     "buyer-1",
		SessionID:   "session-1",
		Action:      trade.ActionCreate,
		Items: []trade.Item{
			{
				ProductID:      "prod-a",
				SKUID:          "sku-a2",
				Title:          "登山包（45L）",
				UnitPriceMinor: 2599,
				Currency:       catalog.USD,
				Quantity:       4,
			},
			{
				ProductID:      "prod-b",
				SKUID:          "sku-b1",
				Title:          "保温水壶（500ml）",
				UnitPriceMinor: 1500,
				Currency:       catalog.USD,
				Quantity:       2,
			},
		},
		ShippingAddress: addressFixture(),
		ExpiresAt:       fixedNow.Add(defaultConfirmationTTL),
	}
	if received := onlyPrepare(t, store); !reflect.DeepEqual(received, want) {
		t.Errorf("Prepare 入参 =\n%+v\n期望\n%+v", received, want)
	}

	// 目录查询顺序也必须是被排序之后的顺序：先解析哪一行决定了账本看到的明细顺序。
	if wantCalls := []string{"prod-a", "prod-b"}; !reflect.DeepEqual(productCatalog.calls, wantCalls) {
		t.Errorf("目录查询顺序 = %v，期望 %v", productCatalog.calls, wantCalls)
	}

	t.Run("标识两端空白被去掉后交给账本", func(t *testing.T) {
		store := &fakeStore{}
		svc := newService(t, Config{
			Store: store, Catalog: catalogFixture(), Clock: &fakeClock{now: fixedNow},
		})
		input := validPlaceInput()
		input.OperationID = "  op-1  "
		input.BuyerID = " buyer-1 "
		input.SessionID = "\tsession-1\n"

		if _, err := svc.PlaceOrder(context.Background(), input); err != nil {
			t.Fatalf("下单失败: %v", err)
		}
		received := onlyPrepare(t, store)
		if received.OperationID != "op-1" || received.BuyerID != "buyer-1" || received.SessionID != "session-1" {
			t.Errorf("标识未被规范化：%q / %q / %q",
				received.OperationID, received.BuyerID, received.SessionID)
		}
	})
}

func TestPlaceOrderOperationID(t *testing.T) {
	cases := []struct {
		name      string
		provided  string
		wantValue string // 非空表示必须是这个值
		generated bool
	}{
		{"调用方留空则生成", "", "", true},
		{"只有空白也生成", "   ", "", true},
		{"调用方给了就沿用", "op-keep", "op-keep", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			svc := newService(t, Config{
				Store: store, Catalog: catalogFixture(), Clock: &fakeClock{now: fixedNow},
			})
			input := validPlaceInput()
			input.OperationID = tc.provided

			if _, err := svc.PlaceOrder(context.Background(), input); err != nil {
				t.Fatalf("下单失败: %v", err)
			}
			got := onlyPrepare(t, store).OperationID
			if tc.generated {
				if !isLowerHex(got, 32) {
					t.Fatalf("生成的幂等键 = %q，期望 32 位小写十六进制", got)
				}
				return
			}
			if got != tc.wantValue {
				t.Errorf("幂等键 = %q，期望 %q", got, tc.wantValue)
			}
		})
	}
}

func TestPlaceOrderRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PlaceOrderInput)
	}{
		{"买家为空", func(in *PlaceOrderInput) { in.BuyerID = "" }},
		{"买家只有空白", func(in *PlaceOrderInput) { in.BuyerID = "   " }},
		{"会话为空", func(in *PlaceOrderInput) { in.SessionID = "" }},
		{"明细为 nil", func(in *PlaceOrderInput) { in.Items = nil }},
		{"明细为空切片", func(in *PlaceOrderInput) { in.Items = []OrderIntent{} }},
		{"商品标识为空", func(in *PlaceOrderInput) { in.Items[0].ProductID = "" }},
		{"规格标识为空", func(in *PlaceOrderInput) { in.Items[0].SKUID = "" }},
		{"数量为零", func(in *PlaceOrderInput) { in.Items[0].Quantity = 0 }},
		{"数量为负", func(in *PlaceOrderInput) { in.Items[0].Quantity = -1 }},
		{"第二行数量为负", func(in *PlaceOrderInput) { in.Items[1].Quantity = -3 }},
		{"末尾行规格为空", func(in *PlaceOrderInput) { in.Items[2].SKUID = " " }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			productCatalog := catalogFixture()
			svc := newService(t, Config{Store: store, Catalog: productCatalog, Clock: &fakeClock{now: fixedNow}})

			input := validPlaceInput()
			tc.mutate(&input)

			_, err := svc.PlaceOrder(context.Background(), input)
			wantCode(t, err, trade.CodeInvalidArgument)
			if store.calls != 0 {
				t.Errorf("参数非法时不应触碰账本，实际调用 %d 次", store.calls)
			}
			if len(productCatalog.calls) != 0 {
				t.Errorf("参数非法时不应查询目录，实际查询 %v", productCatalog.calls)
			}
		})
	}
}

func TestPlaceOrderResolvesThroughCatalog(t *testing.T) {
	cases := []struct {
		name    string
		catalog ProductCatalog
	}{
		{
			name:    "商品不存在",
			catalog: &fakeCatalog{products: map[string]CatalogProduct{}},
		},
		{
			name: "规格不存在",
			catalog: &fakeCatalog{products: map[string]CatalogProduct{
				"prod-a": {ProductID: "prod-a", Title: "登山包", SKUs: []CatalogSKU{
					{SKUID: "sku-a1", Spec: "30L", Price: catalog.MustMoney("19.90", catalog.USD), Stock: 3},
				}},
			}},
		},
		{
			name: "目录返回了另一件商品",
			catalog: &fakeCatalog{products: map[string]CatalogProduct{
				"prod-a": {ProductID: "prod-other", Title: "登山包", SKUs: []CatalogSKU{
					{SKUID: "sku-a2", Spec: "45L", Price: catalog.MustMoney("25.99", catalog.USD), Stock: 7},
				}},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			svc := newService(t, Config{Store: store, Catalog: tc.catalog, Clock: &fakeClock{now: fixedNow}})

			_, err := svc.PlaceOrder(context.Background(), validPlaceInput())
			wantCode(t, err, trade.CodeNotFound)
			if store.calls != 0 {
				t.Errorf("解析失败时不应触碰账本，实际调用 %d 次", store.calls)
			}
		})
	}
}

// TestPlaceOrderPropagatesCatalogError 确认目录故障原样上抛。
//
// 服务层不知道目录是暂时不可用还是数据损坏，换成自己的错误会让调用方
// 既丢了错误类型也丢了重试依据。
func TestPlaceOrderPropagatesCatalogError(t *testing.T) {
	sentinel := errors.New("目录暂时不可用")
	store := &fakeStore{}
	svc := newService(t, Config{
		Store:   store,
		Catalog: &fakeCatalog{err: sentinel},
		Clock:   &fakeClock{now: fixedNow},
	})

	_, err := svc.PlaceOrder(context.Background(), validPlaceInput())
	if !errors.Is(err, sentinel) {
		t.Fatalf("错误 = %v，期望包含目录返回的 %v", err, sentinel)
	}
	if store.calls != 0 {
		t.Errorf("目录故障时不应触碰账本，实际调用 %d 次", store.calls)
	}
}

// TestPlaceOrderRejectsUnmergeableQuantity 钉住合并后的数量溢出。
//
// 每一行单独看都是合法正整数，只有相加之后才越界；不在这里挡住，
// 越界会以回绕后的负数流进账本。
func TestPlaceOrderRejectsUnmergeableQuantity(t *testing.T) {
	store := &fakeStore{}
	svc := newService(t, Config{
		Store: store, Catalog: catalogFixture(), Clock: &fakeClock{now: fixedNow},
	})
	input := validPlaceInput()
	input.Items = []OrderIntent{
		{ProductID: "prod-a", SKUID: "sku-a2", Quantity: math.MaxInt64},
		{ProductID: "prod-a", SKUID: "sku-a2", Quantity: 1},
	}

	_, err := svc.PlaceOrder(context.Background(), input)
	wantCode(t, err, trade.CodeInvalidArgument)
	if !strings.Contains(err.Error(), "超出可表示范围") {
		t.Errorf("错误 %q 未说明数量越界", err)
	}
	if store.calls != 0 {
		t.Errorf("数量越界时不应触碰账本，实际调用 %d 次", store.calls)
	}
}

// TestPlaceOrderRejectsUnrepresentablePrice 确认目录报价超出最小单位可表示范围时
// 被拒绝，而不是悄悄截断成一个更便宜的价。
func TestPlaceOrderRejectsUnrepresentablePrice(t *testing.T) {
	store := &fakeStore{}
	catalogWithHugePrice := &fakeCatalog{products: map[string]CatalogProduct{
		"prod-huge": {
			ProductID: "prod-huge",
			Title:     "天价商品",
			SKUs: []CatalogSKU{{
				SKUID: "sku-huge",
				Spec:  "仅此一件",
				Price: catalog.MustMoney("99999999999999999.99", catalog.USD),
				Stock: 1,
			}},
		},
	}}
	svc := newService(t, Config{
		Store: store, Catalog: catalogWithHugePrice, Clock: &fakeClock{now: fixedNow},
	})

	_, err := svc.PlaceOrder(context.Background(), PlaceOrderInput{
		OperationID:     "op-1",
		BuyerID:         "buyer-1",
		SessionID:       "session-1",
		Items:           []OrderIntent{{ProductID: "prod-huge", SKUID: "sku-huge", Quantity: 1}},
		ShippingAddress: addressFixture(),
	})
	wantCode(t, err, trade.CodeInvalidArgument)
	if !strings.Contains(err.Error(), "最小货币单位") {
		t.Errorf("错误 %q 未说明价格无法换算", err)
	}
	if store.calls != 0 {
		t.Errorf("价格非法时不应触碰账本，实际调用 %d 次", store.calls)
	}
}

// --- Resolve ---

func TestResolveDelegatesDecision(t *testing.T) {
	cases := []struct {
		name     string
		approved bool
		want     trade.Decision
	}{
		{"批准", true, trade.DecisionApprove},
		{"拒绝", false, trade.DecisionReject},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{resolveOut: sampleConfirmation()}
			svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})
			hash := strings.Repeat("ab", 32)

			got, err := svc.Resolve(context.Background(), ResolveInput{
				ConfirmationID: "c-0001",
				BuyerID:        "buyer-1",
				SessionID:      "session-1",
				SnapshotHash:   hash,
				Approved:       tc.approved,
			})
			if err != nil {
				t.Fatalf("决议失败: %v", err)
			}
			if !reflect.DeepEqual(got, sampleConfirmation()) {
				t.Errorf("返回值 = %+v，期望原样返回账本的确认单", got)
			}

			want := []resolveArgs{{
				confirmationID: "c-0001",
				buyerID:        "buyer-1",
				sessionID:      "session-1",
				snapshotHash:   hash,
				decision:       tc.want,
			}}
			if !reflect.DeepEqual(store.resolveCalls, want) {
				t.Errorf("Resolve 入参 = %+v，期望 %+v", store.resolveCalls, want)
			}
		})
	}
}

func TestResolveRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ResolveInput)
	}{
		{"确认单标识为空", func(in *ResolveInput) { in.ConfirmationID = "" }},
		{"买家为空", func(in *ResolveInput) { in.BuyerID = "" }},
		{"会话为空", func(in *ResolveInput) { in.SessionID = "" }},
		{"快照摘要为空", func(in *ResolveInput) { in.SnapshotHash = "" }},
		{"快照摘要只有空白", func(in *ResolveInput) { in.SnapshotHash = "  " }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

			input := ResolveInput{
				ConfirmationID: "c-0001",
				BuyerID:        "buyer-1",
				SessionID:      "session-1",
				SnapshotHash:   strings.Repeat("ab", 32),
				Approved:       true,
			}
			tc.mutate(&input)

			_, err := svc.Resolve(context.Background(), input)
			wantCode(t, err, trade.CodeInvalidArgument)
			if store.calls != 0 {
				t.Errorf("参数非法时不应触碰账本，实际调用 %d 次", store.calls)
			}
		})
	}
}

// TestResolvePropagatesStoreError 确认账本给出的错误码（例如摘要不符）原样到达调用方。
func TestResolvePropagatesStoreError(t *testing.T) {
	store := &fakeStore{resolveErr: trade.Errorf(trade.CodeSnapshotMismatch, "确认内容已经变化")}
	svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

	_, err := svc.Resolve(context.Background(), ResolveInput{
		ConfirmationID: "c-0001",
		BuyerID:        "buyer-1",
		SessionID:      "session-1",
		SnapshotHash:   strings.Repeat("ab", 32),
		Approved:       true,
	})
	wantCode(t, err, trade.CodeSnapshotMismatch)
}

// --- CancelOrder ---

func TestCancelOrderHappyPath(t *testing.T) {
	store := &fakeStore{prepareOut: sampleConfirmation()}
	svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

	got, err := svc.CancelOrder(context.Background(), CancelOrderInput{
		OperationID: "op-cancel",
		BuyerID:     "buyer-1",
		SessionID:   "session-1",
		OrderID:     "o-0001",
		Reason:      "买错了",
	})
	if err != nil {
		t.Fatalf("发起取消失败: %v", err)
	}
	if !reflect.DeepEqual(got, sampleConfirmation()) {
		t.Errorf("返回值 = %+v，期望原样返回账本的确认单", got)
	}

	want := trade.PrepareRequest{
		OperationID: "op-cancel",
		BuyerID:     "buyer-1",
		SessionID:   "session-1",
		Action:      trade.ActionCancel,
		OrderID:     "o-0001",
		Reason:      "买错了",
		ExpiresAt:   fixedNow.Add(defaultConfirmationTTL),
	}
	if received := onlyPrepare(t, store); !reflect.DeepEqual(received, want) {
		t.Errorf("Prepare 入参 =\n%+v\n期望\n%+v", received, want)
	}
}

func TestCancelOrderGeneratesOperationID(t *testing.T) {
	store := &fakeStore{}
	svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

	if _, err := svc.CancelOrder(context.Background(), CancelOrderInput{
		BuyerID:   "buyer-1",
		SessionID: "session-1",
		OrderID:   "o-0001",
		Reason:    "买错了",
	}); err != nil {
		t.Fatalf("发起取消失败: %v", err)
	}

	if got := onlyPrepare(t, store).OperationID; !isLowerHex(got, 32) {
		t.Errorf("生成的幂等键 = %q，期望 32 位小写十六进制", got)
	}
}

// TestCancelOrderPropagatesStoreError 确认账本对取消请求的拒绝（订单状态已变等）
// 原样到达调用方，服务层不把它改写成自己的判断。
func TestCancelOrderPropagatesStoreError(t *testing.T) {
	store := &fakeStore{prepareErr: trade.Errorf(trade.CodeOrderChanged, "订单内容或状态已变化")}
	svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

	_, err := svc.CancelOrder(context.Background(), CancelOrderInput{
		BuyerID:   "buyer-1",
		SessionID: "session-1",
		OrderID:   "o-0001",
		Reason:    "买错了",
	})
	wantCode(t, err, trade.CodeOrderChanged)
}

func TestCancelOrderRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CancelOrderInput)
	}{
		{"买家为空", func(in *CancelOrderInput) { in.BuyerID = "" }},
		{"会话为空", func(in *CancelOrderInput) { in.SessionID = " " }},
		{"订单号为空", func(in *CancelOrderInput) { in.OrderID = "" }},
		{"原因为空", func(in *CancelOrderInput) { in.Reason = "" }},
		{"原因只有空白", func(in *CancelOrderInput) { in.Reason = "  " }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

			input := CancelOrderInput{
				OperationID: "op-cancel",
				BuyerID:     "buyer-1",
				SessionID:   "session-1",
				OrderID:     "o-0001",
				Reason:      "买错了",
			}
			tc.mutate(&input)

			_, err := svc.CancelOrder(context.Background(), input)
			wantCode(t, err, trade.CodeInvalidArgument)
			if store.calls != 0 {
				t.Errorf("参数非法时不应触碰账本，实际调用 %d 次", store.calls)
			}
		})
	}
}

// --- 读路径 ---

func TestGetDelegates(t *testing.T) {
	store := &fakeStore{confirmationOut: sampleConfirmation()}
	svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

	got, err := svc.Get(context.Background(), "c-0001", "buyer-1", "session-1")
	if err != nil {
		t.Fatalf("查单失败: %v", err)
	}
	if !reflect.DeepEqual(got, sampleConfirmation()) {
		t.Errorf("返回值 = %+v，期望原样返回账本的确认单", got)
	}

	want := []confirmationArgs{{confirmationID: "c-0001", buyerID: "buyer-1", sessionID: "session-1"}}
	if !reflect.DeepEqual(store.confirmationCalls, want) {
		t.Errorf("Confirmation 入参 = %+v，期望 %+v", store.confirmationCalls, want)
	}
}

func TestGetRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name                       string
		confirmationID, buyer, ses string
	}{
		{"确认单标识为空", "", "buyer-1", "session-1"},
		{"买家为空", "c-0001", " ", "session-1"},
		{"会话为空", "c-0001", "buyer-1", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

			_, err := svc.Get(context.Background(), tc.confirmationID, tc.buyer, tc.ses)
			wantCode(t, err, trade.CodeInvalidArgument)
			if store.calls != 0 {
				t.Errorf("参数非法时不应触碰账本，实际调用 %d 次", store.calls)
			}
		})
	}
}

// TestListClampsLimit 确认条数被收敛到 1..20，并且交给账本的是收敛后的值。
//
// 断言账本收到的数值而不是返回值：收敛发生在服务层，账本没有机会纠正它，
// 只看返回值无法区分「账本被要求查 20 条」与「账本自己截断成 20 条」。
func TestListClampsLimit(t *testing.T) {
	cases := []struct {
		name  string
		limit int
		want  int
	}{
		{"零收敛到下界", 0, 1},
		{"负数收敛到下界", -3, 1},
		{"下界保留", 1, 1},
		{"区间内保留", 7, 7},
		{"上界保留", 20, 20},
		{"超出上界收敛", 21, 20},
		{"远超出上界收敛", 1000, 20},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantOut := []trade.Confirmation{sampleConfirmation()}
			store := &fakeStore{confirmationsOut: wantOut}
			svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

			got, err := svc.List(context.Background(), "buyer-1", "session-1", tc.limit)
			if err != nil {
				t.Fatalf("列确认单失败: %v", err)
			}
			if !reflect.DeepEqual(got, wantOut) {
				t.Errorf("返回值 = %+v，期望原样返回账本的一页 %+v", got, wantOut)
			}

			want := []confirmationsArgs{{buyerID: "buyer-1", sessionID: "session-1", limit: tc.want}}
			if !reflect.DeepEqual(store.confirmationsCalls, want) {
				t.Errorf("Confirmations 入参 = %+v，期望 %+v", store.confirmationsCalls, want)
			}
		})
	}
}

func TestListRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name       string
		buyer, ses string
	}{
		{"买家为空", "", "session-1"},
		{"会话为空", "buyer-1", "   "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

			got, err := svc.List(context.Background(), tc.buyer, tc.ses, 5)
			wantCode(t, err, trade.CodeInvalidArgument)
			if got != nil {
				t.Errorf("出错时应返回 nil 切片，实际 %+v", got)
			}
			if store.calls != 0 {
				t.Errorf("参数非法时不应触碰账本，实际调用 %d 次", store.calls)
			}
		})
	}
}

func TestGetOrderDelegates(t *testing.T) {
	wantOut := trade.OrderSnapshot{
		OrderID:          "o-0001",
		BuyerID:          "buyer-1",
		Status:           order.StatusConfirmed,
		TotalAmountMinor: 8197,
		Currency:         catalog.USD,
	}
	store := &fakeStore{orderOut: wantOut}
	svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

	got, err := svc.GetOrder(context.Background(), "o-0001", "buyer-1")
	if err != nil {
		t.Fatalf("查订单失败: %v", err)
	}
	if !reflect.DeepEqual(got, wantOut) {
		t.Errorf("返回值 = %+v，期望原样返回账本的订单快照 %+v", got, wantOut)
	}

	want := []orderArgs{{orderID: "o-0001", buyerID: "buyer-1"}}
	if !reflect.DeepEqual(store.orderCalls, want) {
		t.Errorf("Order 入参 = %+v，期望 %+v", store.orderCalls, want)
	}
}

func TestGetOrderRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name           string
		orderID, buyer string
	}{
		{"订单号为空", "", "buyer-1"},
		{"买家为空", "o-0001", "  "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

			_, err := svc.GetOrder(context.Background(), tc.orderID, tc.buyer)
			wantCode(t, err, trade.CodeInvalidArgument)
			if store.calls != 0 {
				t.Errorf("参数非法时不应触碰账本，实际调用 %d 次", store.calls)
			}
		})
	}
}

// TestListOrdersDelegatesFilterVerbatim 钉住「过滤条件原样转发」。
//
// 服务层在这里不做任何裁剪：一旦它自己把 Limit 也收敛一遍，调用方就会同时面对
// 两份分页契约，而其中一份随实现漂移。
func TestListOrdersDelegatesFilterVerbatim(t *testing.T) {
	wantOut := trade.OrderPage{
		Orders: []trade.OrderSnapshot{{OrderID: "o-0001", BuyerID: "buyer-1"}},
		Total:  3,
		Offset: 2,
		Limit:  5,
	}
	store := &fakeStore{ordersOut: wantOut}
	svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

	filter := trade.OrderFilter{
		BuyerID: "buyer-1",
		Status:  order.StatusConfirmed,
		Offset:  2,
		Limit:   5,
	}
	got, err := svc.ListOrders(context.Background(), filter)
	if err != nil {
		t.Fatalf("列订单失败: %v", err)
	}
	if !reflect.DeepEqual(got, wantOut) {
		t.Errorf("返回值 = %+v，期望原样返回账本的一页 %+v", got, wantOut)
	}
	if !reflect.DeepEqual(store.ordersCalls, []trade.OrderFilter{filter}) {
		t.Errorf("Orders 入参 = %+v，期望 %+v", store.ordersCalls, filter)
	}
}

func TestListOrdersPropagatesStoreError(t *testing.T) {
	store := &fakeStore{ordersErr: trade.Errorf(trade.CodeInvalidQuery, "分页参数非法")}
	svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

	if _, err := svc.ListOrders(context.Background(), trade.OrderFilter{BuyerID: "buyer-1"}); err == nil {
		t.Fatal("期望返回错误，实际为 nil")
	}
	wantCode(t, store.ordersErr, trade.CodeInvalidQuery)
}

// --- 库存初始化 ---

func TestInitializeInventoryDelegates(t *testing.T) {
	skus := []trade.SeedSKU{{
		SKUID:          "sku-a2",
		ProductID:      "prod-a",
		Title:          "登山包（45L）",
		Stock:          7,
		UnitPriceMinor: 2599,
		Currency:       catalog.USD,
	}}
	store := &fakeStore{}
	svc := newService(t, Config{Store: store, Clock: &fakeClock{now: fixedNow}})

	if err := svc.InitializeInventory(context.Background(), skus); err != nil {
		t.Fatalf("初始化库存失败: %v", err)
	}
	if !reflect.DeepEqual(store.inventoryCalls, [][]trade.SeedSKU{skus}) {
		t.Errorf("InitializeInventory 入参 = %+v，期望 %+v", store.inventoryCalls, skus)
	}

	sentinel := trade.Errorf(trade.CodeInventoryMigrationRequired, "历史订单占用超出库存种子")
	store.inventoryErr = sentinel
	err := svc.InitializeInventory(context.Background(), skus)
	if !errors.Is(err, sentinel) {
		t.Errorf("错误 = %v，期望原样返回账本的 %v", err, sentinel)
	}
}

// --- 纯函数 ---

func TestMergeIntents(t *testing.T) {
	t.Run("合并同规格并排序", func(t *testing.T) {
		got, err := mergeIntents([]OrderIntent{
			{ProductID: "prod-b", SKUID: "sku-b1", Quantity: 2},
			{ProductID: "prod-a", SKUID: "sku-a2", Quantity: 1},
			{ProductID: "prod-a", SKUID: "sku-a2", Quantity: 3},
		})
		if err != nil {
			t.Fatalf("合并失败: %v", err)
		}
		want := []OrderIntent{
			{ProductID: "prod-a", SKUID: "sku-a2", Quantity: 4},
			{ProductID: "prod-b", SKUID: "sku-b1", Quantity: 2},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("合并结果 = %+v，期望 %+v", got, want)
		}
	})

	t.Run("同规格不同商品按商品标识兜底排序", func(t *testing.T) {
		got, err := mergeIntents([]OrderIntent{
			{ProductID: "prod-z", SKUID: "sku-1", Quantity: 1},
			{ProductID: "prod-a", SKUID: "sku-1", Quantity: 2},
		})
		if err != nil {
			t.Fatalf("合并失败: %v", err)
		}
		want := []OrderIntent{
			{ProductID: "prod-a", SKUID: "sku-1", Quantity: 2},
			{ProductID: "prod-z", SKUID: "sku-1", Quantity: 1},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("合并结果 = %+v，期望 %+v", got, want)
		}
	})

	t.Run("空输入得到空结果", func(t *testing.T) {
		got, err := mergeIntents(nil)
		if err != nil {
			t.Fatalf("合并失败: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("合并结果 = %+v，期望空", got)
		}
	})

	t.Run("数量相加越界被拒绝", func(t *testing.T) {
		_, err := mergeIntents([]OrderIntent{
			{ProductID: "prod-a", SKUID: "sku-a2", Quantity: math.MaxInt64},
			{ProductID: "prod-a", SKUID: "sku-a2", Quantity: 1},
		})
		wantCode(t, err, trade.CodeInvalidArgument)
	})
}

func TestClampLimit(t *testing.T) {
	cases := []struct {
		limit int
		want  int
	}{
		{-100, 1},
		{0, 1},
		{1, 1},
		{19, 19},
		{20, 20},
		{21, 20},
		{100, 20},
	}

	for _, tc := range cases {
		if got := clampLimit(tc.limit); got != tc.want {
			t.Errorf("clampLimit(%d) = %d，期望 %d", tc.limit, got, tc.want)
		}
	}
}
