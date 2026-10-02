package commerce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tradesvc "github.com/NicoYazawa/crosspilot/internal/application/trade"
	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/order"
	"github.com/NicoYazawa/crosspilot/internal/domain/trade"
	presauth "github.com/NicoYazawa/crosspilot/internal/presentation/auth"
)

// 本文件是 package commerce 的内部测试：writeServiceError / decodeOptionalBody
// 这些决定「错误码怎么映射状态码」「空 body 算不算错」的逻辑都是未导出的，
// 站在包外只能通过一整条装配链路才能触发，反而让每条分支的意图被稀释。
// Handler 持有的是具体类型 *tradesvc.Service，但 Service 可由 New + 接口替身
// 构造，因此不需要为了可测性改动生产代码结构。

// fixedNow 是所有用例共享的「现在」，经 fakeClock 注入，测试里没有真实等待。
var fixedNow = time.Unix(1_700_000_000, 0).UTC()

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeClock 实现 tradesvc.Clock。
type fakeClock struct{ now time.Time }

func (c fakeClock) Now() time.Time { return c.now }

// resolveCall 记录一次经服务层抵达账本的决议，用于断言 handler 有没有
// 把「缺字段」和「显式 false」这两种语义正确地分开传递。
type resolveCall struct {
	confirmationID string
	buyerID        string
	sessionID      string
	snapshotHash   string
	decision       trade.Decision
}

// fakeStore 是 tradesvc.Store 的接口替身，只记录被调用的方法并回放预设结果。
// 它不实现任何业务语义——语义由应用层与领域层负责，这一层要测的是
// 「HTTP 形状 → 服务调用」是否被正确转发。
type fakeStore struct {
	resolveCalls []resolveCall
	resolveErr   error
	resolveResp  trade.Confirmation

	prepareCalls int
	prepareErr   error
	prepareResp  trade.Confirmation
	lastPrepare  trade.PrepareRequest

	confirmationCalls int
	confirmationErr   error
	confirmationResp  trade.Confirmation

	confirmationsCalls     int
	confirmationsErr       error
	confirmationsResp      []trade.Confirmation
	lastConfirmationsBuyer string
	lastConfirmationsSess  string
	lastConfirmationsLimit int

	orderCalls   int
	orderErr     error
	orderResp    trade.OrderSnapshot
	lastOrderID  string
	lastOrderBuy string

	ordersCalls int
	ordersErr   error
	ordersResp  trade.OrderPage
	lastFilter  trade.OrderFilter
}

func (f *fakeStore) InitializeInventory(context.Context, []trade.SeedSKU) error { return nil }

func (f *fakeStore) Prepare(_ context.Context, req trade.PrepareRequest) (trade.Confirmation, error) {
	f.prepareCalls++
	f.lastPrepare = req
	return f.prepareResp, f.prepareErr
}

func (f *fakeStore) Confirmation(_ context.Context, _, _, _ string) (trade.Confirmation, error) {
	f.confirmationCalls++
	return f.confirmationResp, f.confirmationErr
}

func (f *fakeStore) Confirmations(
	_ context.Context, buyerID, sessionID string, limit int,
) ([]trade.Confirmation, error) {
	f.confirmationsCalls++
	f.lastConfirmationsBuyer, f.lastConfirmationsSess, f.lastConfirmationsLimit = buyerID, sessionID, limit
	return f.confirmationsResp, f.confirmationsErr
}

func (f *fakeStore) Resolve(
	_ context.Context, confirmationID, buyerID, sessionID, snapshotHash string, decision trade.Decision,
) (trade.Confirmation, error) {
	f.resolveCalls = append(f.resolveCalls, resolveCall{
		confirmationID: confirmationID,
		buyerID:        buyerID,
		sessionID:      sessionID,
		snapshotHash:   snapshotHash,
		decision:       decision,
	})
	return f.resolveResp, f.resolveErr
}

func (f *fakeStore) Order(_ context.Context, orderID, buyerID string) (trade.OrderSnapshot, error) {
	f.orderCalls++
	f.lastOrderID, f.lastOrderBuy = orderID, buyerID
	return f.orderResp, f.orderErr
}

func (f *fakeStore) Orders(_ context.Context, filter trade.OrderFilter) (trade.OrderPage, error) {
	f.ordersCalls++
	f.lastFilter = filter
	return f.ordersResp, f.ordersErr
}

// newTestHandler 用替身账本拼出一个可工作的 Handler。
func newTestHandler(t *testing.T, st *fakeStore) *Handler {
	t.Helper()
	return newTestHandlerWithCatalog(t, st, nil)
}

// newTestHandlerWithCatalog 额外注入商品目录替身，供下单路径使用。
func newTestHandlerWithCatalog(t *testing.T, st *fakeStore, cat tradesvc.ProductCatalog) *Handler {
	t.Helper()
	svc, err := tradesvc.New(tradesvc.Config{Store: st, Catalog: cat, Clock: fakeClock{now: fixedNow}})
	if err != nil {
		t.Fatalf("构造交易服务失败：%v", err)
	}
	return NewHandler(svc, discardLogger())
}

// fakeCatalog 是 tradesvc.ProductCatalog 的替身，回放一件商品及其规格报价。
type fakeCatalog struct {
	product catalog.OrderableProduct
	found   bool
	err     error
	calls   int
}

func (f *fakeCatalog) Find(_ context.Context, _ string) (tradesvc.CatalogProduct, bool, error) {
	f.calls++
	return f.product, f.found, f.err
}

// withIdentity 把买家与会话放进请求上下文，模拟鉴权中间件已跑过。
func withIdentity(r *http.Request, buyer, session string) *http.Request {
	ctx := presauth.WithSession(presauth.WithBuyer(r.Context(), buyer), session)
	return r.WithContext(ctx)
}

// ---- writeServiceError --------------------------------------------------

// TestWriteServiceError_错误码到状态码映射 逐一钉死账本错误码的分类。
//
// 分类依据是「客户端重试同一个请求会怎样」，所以每个码的落点都必须精确：
// 把 403 写成 404 会让越权在日志里与「输错 ID」混淆；把 409 写成 500 会让
// 前端把「确认单过期」当成服务故障。同时断言错误码原样回传——前端就是按
// CodeConfirmationExpired / CodeInvalidArgument 这些字符串分支的。
func TestWriteServiceError_错误码到状态码映射(t *testing.T) {
	cases := []struct {
		code           string
		wantStatus     int
		wantRetryAfter bool
	}{
		{trade.CodeNotFound, http.StatusNotFound, false},
		{trade.CodeInvalidArgument, http.StatusBadRequest, false},
		{trade.CodeInvalidQuery, http.StatusBadRequest, false},
		{trade.CodeOwnerMismatch, http.StatusForbidden, false},
		{trade.CodeTransactionConflictExhaust, http.StatusServiceUnavailable, true},
		// 以下都落在 default 的 409：重试同一请求不会成功，但重新拉状态再发会。
		{trade.CodeOperationConflict, http.StatusConflict, false},
		{trade.CodeConfirmationExpired, http.StatusConflict, false},
		{trade.CodeInsufficientStock, http.StatusConflict, false},
		{trade.CodePriceChanged, http.StatusConflict, false},
		{trade.CodeSnapshotMismatch, http.StatusConflict, false},
		{trade.CodeDecisionConflict, http.StatusConflict, false},
		{trade.CodeOrderChanged, http.StatusConflict, false},
		{trade.CodeInventoryMigrationRequired, http.StatusConflict, false},
		{trade.CodeDuplicateSKU, http.StatusConflict, false},
		{trade.CodeSKUMigrationNotAllowed, http.StatusConflict, false},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeServiceError(rec, discardLogger(), trade.Errorf(tc.code, "被拒"))

			if rec.Code != tc.wantStatus {
				t.Fatalf("错误码 %s 应映射到 %d，实际 %d", tc.code, tc.wantStatus, rec.Code)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("响应不是合法 JSON：%v，body=%s", err, rec.Body.String())
			}
			if body["error"] != tc.code {
				t.Errorf("错误码应原样回传 %q，实际 %q", tc.code, body["error"])
			}
			if tc.wantRetryAfter {
				if got := rec.Header().Get("Retry-After"); got != "1" {
					t.Errorf("事务冲突耗尽应带 Retry-After: 1，实际 %q", got)
				}
			} else if got := rec.Header().Get("Retry-After"); got != "" {
				t.Errorf("非 503 不应带 Retry-After，实际 %q", got)
			}
		})
	}
}

// TestWriteServiceError_非账本错误 覆盖 errors.As 失败后的分类。
//
// 领域层的形状校验错误没有错误码，必须被归成 400；真故障归 500。
// 如果实现把「不是 StoreError」一律当成 500，那么「数量为负」这类
// 调用方错误会变成服务端故障告警，运维会被噪声淹没。
func TestWriteServiceError_非账本错误(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"trade.ErrInvalidArgument", trade.ErrInvalidArgument, http.StatusBadRequest, trade.CodeInvalidArgument},
		{"order.ErrInvalidOrder", order.ErrInvalidOrder, http.StatusBadRequest, trade.CodeInvalidArgument},
		{"order.ErrInvalidStatus", order.ErrInvalidStatus, http.StatusBadRequest, trade.CodeInvalidArgument},
		{"order.ErrEmptyLines", order.ErrEmptyLines, http.StatusBadRequest, trade.CodeInvalidArgument},
		{"order.ErrCurrencyMismatch", order.ErrCurrencyMismatch, http.StatusBadRequest, trade.CodeInvalidArgument},
		{"order.ErrCancelReasonRequired", order.ErrCancelReasonRequired, http.StatusBadRequest, trade.CodeInvalidArgument},
		{"未归类的故障", errors.New("数据库连接断了"), http.StatusInternalServerError, "INTERNAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeServiceError(rec, discardLogger(), tc.err)

			if rec.Code != tc.wantStatus {
				t.Fatalf("应映射到 %d，实际 %d", tc.wantStatus, rec.Code)
			}
			var body map[string]string
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body["error"] != tc.wantCode {
				t.Errorf("错误码 = %q，期望 %q", body["error"], tc.wantCode)
			}
		})
	}
}

// TestWriteServiceError_被包装的StoreError仍能识别 守住 errors.As 而非 ==。
//
// 服务层经常用 fmt.Errorf("...: %w") 再包一层排障信息；用裸类型断言
// （err.(*trade.StoreError)）会在包装后失效，把一次正常的下单失败
// 误报成 500。这条用例专门构造包装过的错误来钉住这个陷阱。
func TestWriteServiceError_被包装的StoreError仍能识别(t *testing.T) {
	wrapped := fmt.Errorf("resolve 阶段：%w", trade.Errorf(trade.CodeConfirmationExpired, "已过期"))
	rec := httptest.NewRecorder()
	writeServiceError(rec, discardLogger(), wrapped)

	if rec.Code != http.StatusConflict {
		t.Fatalf("被包装的 StoreError 仍应识别出码并映射到 409，实际 %d", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != trade.CodeConfirmationExpired {
		t.Errorf("错误码 = %q，期望 %q", body["error"], trade.CodeConfirmationExpired)
	}
}

// ---- decodeOptionalBody -------------------------------------------------

// TestDecodeOptionalBody_空body走默认值 守住「取消订单可以不带 body」。
//
// Content-Length: 0 是真实客户端取消请求的常态。把它当成解析失败，
// 会让最普通的取消动作必须带一个空 JSON 才能通过；但也不能把
// 「畸形 JSON」一并放过——那会让拼错的请求静默变成一次取消。
func TestDecodeOptionalBody_空body走默认值(t *testing.T) {
	rec := httptest.NewRecorder()
	// 空 body 且 Content-Length: 0，是服务端看到的真实形态。
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
	var dst struct {
		OperationID string `json:"operation_id"`
		Reason      string `json:"reason"`
	}
	dst.OperationID = "预置值" // 证明解码器在空 body 时根本不碰 dst
	ok := decodeOptionalBody(rec, discardLogger(), req, &dst)

	if !ok {
		t.Fatalf("空 body 应被接受（全部字段缺省），实际返回 false，body=%s", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("空 body 不应写出错误状态，实际 %d", rec.Code)
	}
	if dst.OperationID != "预置值" || dst.Reason != "" {
		t.Errorf("空 body 时 dst 不应被改动，实际 %+v", dst)
	}

	// 畸形 JSON 才是错误：不能把「空」与「坏」混为一谈。
	badReq := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("not json"))
	badRec := httptest.NewRecorder()
	if decodeOptionalBody(badRec, discardLogger(), badReq, &dst) {
		t.Fatal("非法 JSON 应返回 false")
	}
	if badRec.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON 应返回 400，实际 %d", badRec.Code)
	}
	if !strings.Contains(badRec.Body.String(), "invalid_body") {
		t.Errorf("响应应含 invalid_body，实际 %s", badRec.Body.String())
	}
}

// ---- resolveConfirmation：approved 缺失 vs 显式 false --------------------

// TestResolveConfirmation_approved缺失与false是两种语义 是本层最关键的断言。
//
// Approved 用 *bool 接收：缺字段会让一次漏传的请求被当成「买家拒绝」，
// 而拒绝会真的把订单状态改掉。因此两条必须分别断言：
//   - 缺 approved → 400，且绝不触达服务层；
//   - approved:false → 200，且服务层收到的决议确实是 DecisionReject。
//
// 只测其中一条都会漏掉「用值类型接收」这个实现的致命退化。
func TestResolveConfirmation_approved缺失与false是两种语义(t *testing.T) {
	st := &fakeStore{
		resolveResp: trade.Confirmation{
			ConfirmationID: "conf-1",
			BuyerID:        "buyer-1",
			SessionID:      "sess-1",
			Action:         trade.ActionCreate,
			Status:         trade.StatusRejected,
			SnapshotHash:   "hash-1",
			ExpiresAt:      fixedNow.Add(time.Minute),
			CreatedAt:      fixedNow,
		},
	}
	h := newTestHandler(t, st)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/confirmations/conf-1/resolve", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = withIdentity(req, "buyer-1", "sess-1")
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, req)
		return rec
	}

	// 缺 approved：400，且服务层一次都没被调用。
	rec := post(`{"snapshot_hash":"hash-1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺少 approved 应 400，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "missing_approved") {
		t.Errorf("响应应含 missing_approved，实际 %s", rec.Body.String())
	}
	if len(st.resolveCalls) != 0 {
		t.Errorf("缺少 approved 时绝不能触碰服务层，实际调用 %d 次", len(st.resolveCalls))
	}

	// 显式 false：200，且到服务层的决议是「拒绝」。
	rec = post(`{"snapshot_hash":"hash-1","approved":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approved:false 应被接受，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	if len(st.resolveCalls) != 1 {
		t.Fatalf("approved:false 应触达服务层一次，实际 %d 次", len(st.resolveCalls))
	}
	if got := st.resolveCalls[0]; got.decision != trade.DecisionReject {
		t.Errorf("approved:false 应翻成 %q，实际 %q", trade.DecisionReject, got.decision)
	}
	if got := st.resolveCalls[0]; got.snapshotHash != "hash-1" || got.confirmationID != "conf-1" ||
		got.buyerID != "buyer-1" || got.sessionID != "sess-1" {
		t.Errorf("决议请求参数未按契约透传：%+v", got)
	}

	// 显式 true 作为对照：必须翻成 DecisionApprove。
	rec = post(`{"snapshot_hash":"hash-1","approved":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approved:true 应被接受，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	if len(st.resolveCalls) != 2 || st.resolveCalls[1].decision != trade.DecisionApprove {
		t.Errorf("approved:true 应翻成 %q，实际调用记录 %+v", trade.DecisionApprove, st.resolveCalls)
	}
}

// ---- 身份缺失 ------------------------------------------------------------

// TestHandler_无身份_401且不触达服务层 守住「处理器只信 context 里的身份」。
//
// 一条漏挂鉴权中间件的路由，如果处理器不自己挡一道，就会以空买家身份
// 直接查询账本——查询本人订单变成查询「所有人的订单」。同时它也说明
// 身份不从请求体取：无论 body 里写什么都无关紧要。
func TestHandler_无身份_401且不触达服务层(t *testing.T) {
	st := &fakeStore{}
	h := newTestHandler(t, st)

	// 决议端点：body 里塞满身份字段也没用。
	req := httptest.NewRequest(http.MethodPost, "/confirmations/conf-1/resolve",
		strings.NewReader(`{"snapshot_hash":"h","approved":true,"buyer_id":"attacker"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无身份应 401，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unauthorized") {
		t.Errorf("响应应含 unauthorized，实际 %s", rec.Body.String())
	}
	if len(st.resolveCalls) != 0 {
		t.Errorf("无身份时绝不能调用服务层，实际调用 %d 次", len(st.resolveCalls))
	}

	// 订单列表端点同样如此。
	req2 := httptest.NewRequest(http.MethodGet, "/orders", nil)
	rec2 := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("无身份列订单应 401，实际 %d", rec2.Code)
	}
	if st.ordersCalls != 0 {
		t.Errorf("无身份时绝不能调用服务层，实际调用 %d 次", st.ordersCalls)
	}
}

// ---- 订单列表：查询参数校验与上下文身份透传 --------------------------------

// TestListOrders_身份与分页透传并映射DTO 覆盖 handler 的正常装配。
//
// 买家必须来自 context（而不是查询参数），分页参数必须原样进过滤器。
// 若实现把 buyer 写死成默认值或漏传 limit，按买家隔离与分页都会失效，
// 而只断言 200 的测试看不出来。
func TestListOrders_身份与分页透传并映射DTO(t *testing.T) {
	st := &fakeStore{
		ordersResp: trade.OrderPage{
			Orders: []trade.OrderSnapshot{{
				OrderID:          "order-1",
				BuyerID:          "buyer-1",
				Status:           order.StatusConfirmed,
				TotalAmountMajor: "12.34",
				TotalAmountMinor: 1234,
				CreatedAt:        fixedNow,
			}},
			Total: 1, Offset: 0, Limit: 5,
		},
	}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodGet, "/orders?offset=0&limit=5", nil)
	req = withIdentity(req, "buyer-1", "sess-1")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	if st.lastFilter.BuyerID != "buyer-1" {
		t.Errorf("过滤器买家应来自 context，实际 %q", st.lastFilter.BuyerID)
	}
	if st.lastFilter.Limit != 5 || st.lastFilter.Offset != 0 {
		t.Errorf("分页参数未透传：%+v", st.lastFilter)
	}
	var body struct {
		Orders []map[string]any `json:"orders"`
		Total  int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if body.Total != 1 || len(body.Orders) != 1 {
		t.Fatalf("响应分页字段不符：total=%d len=%d", body.Total, len(body.Orders))
	}
	if body.Orders[0]["order_id"] != "order-1" {
		t.Errorf("order_id 未映射进 DTO：%v", body.Orders[0])
	}
}

// TestListOrders_非法分页参数_400且不查账本 覆盖 queryInt 的失败分支。
//
// 非法 offset 必须是客户端错误（400），而不是默默按 0 处理——静默修正
// 会让调用方以为翻到了页，却一直在看第一页；同时它不该触碰账本。
func TestListOrders_非法分页参数_400且不查账本(t *testing.T) {
	st := &fakeStore{}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodGet, "/orders?offset=abc", nil)
	req = withIdentity(req, "buyer-1", "sess-1")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 offset 应 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_offset") {
		t.Errorf("响应应含 invalid_offset，实际 %s", rec.Body.String())
	}
	if st.ordersCalls != 0 {
		t.Errorf("参数非法时不应查询账本，实际调用 %d 次", st.ordersCalls)
	}
}

// ---- 订单读路径 ----------------------------------------------------------

// TestGetOrder_成功映射订单快照 覆盖 GET /orders/{orderID} 的完整链路。
//
// 一次断言三件事：chi 的路径参数被正确取出、买家来自中间件注入的 context
// （而不是任何请求参数）、订单行被映射进 DTO。任一环节错位，查单就会返回
// 空明细或别人的订单，而只断言 200 的测试看不出来。
func TestGetOrder_成功映射订单快照(t *testing.T) {
	st := &fakeStore{
		orderResp: trade.OrderSnapshot{
			OrderID:          "order-1",
			BuyerID:          "buyer-1",
			Status:           order.StatusConfirmed,
			TotalAmountMajor: "59.98",
			TotalAmountMinor: 5998,
			Currency:         catalog.USD,
			CreatedAt:        fixedNow,
			Lines: []trade.LineSnapshot{{
				ProductID:      "product-1",
				SKUID:          "sku-1",
				Title:          "登山包（60L）",
				UnitPriceMajor: "29.99",
				Quantity:       2,
				Currency:       catalog.USD,
			}},
		},
	}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodGet, "/orders/order-1", nil)
	req = withIdentity(req, "buyer-1", "sess-1")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	if st.lastOrderID != "order-1" {
		t.Errorf("路径参数 order_id 未透传，实际 %q", st.lastOrderID)
	}
	if st.lastOrderBuy != "buyer-1" {
		t.Errorf("订单归属买家应来自 context，实际 %q", st.lastOrderBuy)
	}
	var body struct {
		OrderID string `json:"order_id"`
		BuyerID string `json:"buyer_id"`
		Lines   []struct {
			SKUID    string `json:"sku_id"`
			Quantity int64  `json:"quantity"`
		} `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if body.OrderID != "order-1" || body.BuyerID != "buyer-1" {
		t.Errorf("订单标识或买家未映射：%+v", body)
	}
	if len(body.Lines) != 1 || body.Lines[0].SKUID != "sku-1" || body.Lines[0].Quantity != 2 {
		t.Errorf("订单行未正确映射：%+v", body.Lines)
	}
}

// TestGetOrder_服务层NotFound_404 覆盖读路径的账本错误映射。
//
// 查一张不存在的订单必须是 404；若这里漏判，前端会把「查无此单」当成
// 服务故障，反复重试同一请求。
func TestGetOrder_服务层NotFound_404(t *testing.T) {
	st := &fakeStore{orderErr: trade.Errorf(trade.CodeNotFound, "订单不存在")}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodGet, "/orders/order-404", nil)
	req = withIdentity(req, "buyer-1", "sess-1")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("订单不存在应 404，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), trade.CodeNotFound) {
		t.Errorf("响应应含错误码 %s，实际 %s", trade.CodeNotFound, rec.Body.String())
	}
}

// ---- 取消订单 ------------------------------------------------------------

// TestCancelOrder_成功创建取消确认单 覆盖 POST /orders/{orderID}/cancel。
//
// 取消是两阶段动作，HTTP 层必须把 order_id 从路径、reason 从请求体、
// buyer/session 从 context 三处拼装后交给服务层。这条逐一核对拼装结果：
// 任何一处写反（比如把 session 当 buyer 传），都会让账本认错主人。
func TestCancelOrder_成功创建取消确认单(t *testing.T) {
	st := &fakeStore{
		prepareResp: trade.Confirmation{
			ConfirmationID: "conf-cancel",
			OperationID:    "op-cancel",
			Action:         trade.ActionCancel,
			Status:         trade.StatusPending,
			ExpiresAt:      fixedNow.Add(time.Minute),
			CreatedAt:      fixedNow,
		},
	}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodPost, "/orders/order-1/cancel",
		strings.NewReader(`{"operation_id":"op-cancel","reason":"买错了"}`))
	req.Header.Set("Content-Type", "application/json")
	req = withIdentity(req, "buyer-1", "sess-1")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	if st.prepareCalls != 1 {
		t.Fatalf("应恰好调用一次账本 Prepare，实际 %d 次", st.prepareCalls)
	}
	got := st.lastPrepare
	if got.Action != trade.ActionCancel {
		t.Errorf("动作应为取消，实际 %q", got.Action)
	}
	if got.OrderID != "order-1" || got.Reason != "买错了" {
		t.Errorf("订单号或取消原因未透传：order=%q reason=%q", got.OrderID, got.Reason)
	}
	if got.BuyerID != "buyer-1" || got.SessionID != "sess-1" {
		t.Errorf("买家或会话未来自 context：buyer=%q session=%q", got.BuyerID, got.SessionID)
	}
	if got.OperationID != "op-cancel" {
		t.Errorf("幂等键未透传，实际 %q", got.OperationID)
	}
	if !strings.Contains(rec.Body.String(), "conf-cancel") {
		t.Errorf("响应应含确认单号 conf-cancel，实际 %s", rec.Body.String())
	}
}

// TestCancelOrder_非法JSON_400且不触达服务层 覆盖可选 body 的解析失败。
//
// 取消允许空 body，但畸形 JSON 必须被拒绝——否则一次拼错的请求会静默
// 变成「无原因取消」，而原因恰恰是账本审计需要的字段。
func TestCancelOrder_非法JSON_400且不触达服务层(t *testing.T) {
	st := &fakeStore{}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodPost, "/orders/order-1/cancel", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	req = withIdentity(req, "buyer-1", "sess-1")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_body") {
		t.Errorf("响应应含 invalid_body，实际 %s", rec.Body.String())
	}
	if st.prepareCalls != 0 {
		t.Errorf("body 非法时不应触达账本，实际 %d 次", st.prepareCalls)
	}
}

// ---- 确认单列表 ----------------------------------------------------------

// TestListConfirmations_透传身份会话与分页 覆盖 GET /confirmations。
//
// 确认单列表按 (买家, 会话) 隔离：买家或会话任一取错，列表就会串号。
// 同时限制条数必须原样进账本端口——多给一条就是多泄露一条他人决议。
func TestListConfirmations_透传身份会话与分页(t *testing.T) {
	st := &fakeStore{
		confirmationsResp: []trade.Confirmation{{
			ConfirmationID: "conf-1",
			OperationID:    "op-1",
			BuyerID:        "buyer-1",
			SessionID:      "sess-1",
			Action:         trade.ActionCreate,
			Status:         trade.StatusPending,
			ExpiresAt:      fixedNow.Add(time.Minute),
			CreatedAt:      fixedNow,
		}},
	}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodGet, "/confirmations?limit=3", nil)
	req = withIdentity(req, "buyer-1", "sess-1")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	if st.confirmationsCalls != 1 {
		t.Fatalf("应恰好查询一次账本，实际 %d 次", st.confirmationsCalls)
	}
	if st.lastConfirmationsBuyer != "buyer-1" || st.lastConfirmationsSess != "sess-1" {
		t.Errorf("买家/会话未来自 context：buyer=%q session=%q",
			st.lastConfirmationsBuyer, st.lastConfirmationsSess)
	}
	if st.lastConfirmationsLimit != 3 {
		t.Errorf("分页上限未透传，实际 %d", st.lastConfirmationsLimit)
	}
	if !strings.Contains(rec.Body.String(), "conf-1") {
		t.Errorf("响应应含确认单号，实际 %s", rec.Body.String())
	}
}

// TestListConfirmations_服务层错误_404 覆盖列表路径的错误映射。
func TestListConfirmations_服务层错误_404(t *testing.T) {
	st := &fakeStore{confirmationsErr: trade.Errorf(trade.CodeNotFound, "会话不存在")}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodGet, "/confirmations", nil)
	req = withIdentity(req, "buyer-1", "sess-1")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("账本 NotFound 应 404，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), trade.CodeNotFound) {
		t.Errorf("响应应含错误码 %s，实际 %s", trade.CodeNotFound, rec.Body.String())
	}
}

// ---- 下单 -----------------------------------------------------------------

// placeOrderBody 是一份合法下单请求：两个字段的来源各自可核。
const placeOrderBody = `{
	"operation_id": "op-order",
	"items": [{"product_id": "product-1", "sku_id": "sku-1", "quantity": 2}],
	"shipping_address": {
		"recipient": "张三", "phone": "13800000000", "country": "CN",
		"province": "广东", "city": "深圳", "line1": "某某路 1 号", "postal_code": "518000"
	}
}`

// TestPlaceOrder_成功下单并透传权威报价与地址 覆盖 POST /confirmations/orders。
//
// 下单是整条交易链路里最贵的一步，HTTP 层必须：把路径/体/context 三处输入
// 正确拼装、把地址经 toDomain 落到领域类型、把价格交给应用层从目录解析
// （而不是信调用方）。这条同时钉住 Items 的规格、数量、单价与币种，
// 以及 ShippingAddress 的收件人——只断言 200 会让「地址丢失」被放过。
func TestPlaceOrder_成功下单并透传权威报价与地址(t *testing.T) {
	cat := &fakeCatalog{
		found: true,
		product: catalog.OrderableProduct{
			ProductID: "product-1",
			Title:     "登山包",
			SKUs: []catalog.OrderableSKU{{
				SKUID: "sku-1",
				Spec:  "60L",
				Price: catalog.MustMoney("29.99", catalog.USD),
			}},
		},
	}
	st := &fakeStore{
		prepareResp: trade.Confirmation{
			ConfirmationID: "conf-order",
			OperationID:    "op-order",
			Action:         trade.ActionCreate,
			Status:         trade.StatusPending,
			ExpiresAt:      fixedNow.Add(time.Minute),
			CreatedAt:      fixedNow,
		},
	}
	h := newTestHandlerWithCatalog(t, st, cat)

	req := httptest.NewRequest(http.MethodPost, "/confirmations/orders", strings.NewReader(placeOrderBody))
	req.Header.Set("Content-Type", "application/json")
	req = withIdentity(req, "buyer-1", "sess-1")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	if st.prepareCalls != 1 {
		t.Fatalf("应恰好调用一次账本 Prepare，实际 %d 次", st.prepareCalls)
	}
	got := st.lastPrepare
	if got.Action != trade.ActionCreate {
		t.Errorf("动作应为创建，实际 %q", got.Action)
	}
	if got.BuyerID != "buyer-1" || got.SessionID != "sess-1" || got.OperationID != "op-order" {
		t.Errorf("身份或幂等键未正确透传：%+v", got)
	}
	if len(got.Items) != 1 {
		t.Fatalf("应透传一条下单明细，实际 %d 条", len(got.Items))
	}
	item := got.Items[0]
	if item.SKUID != "sku-1" || item.Quantity != 2 {
		t.Errorf("规格或数量未透传：%+v", item)
	}
	// 单价必须由目录解析得出（29.99 USD → 2999 分），而不是调用方给的。
	if item.UnitPriceMinor != 2999 || item.Currency != catalog.USD {
		t.Errorf("权威报价未正确解析：minor=%d currency=%q", item.UnitPriceMinor, item.Currency)
	}
	// toDomain 必须把地址搬进领域类型，收件人丢失会让包裹无处可送。
	if got.ShippingAddress.Recipient != "张三" || got.ShippingAddress.City != "深圳" {
		t.Errorf("收货地址未映射进领域类型：%+v", got.ShippingAddress)
	}
	if !strings.Contains(rec.Body.String(), "conf-order") {
		t.Errorf("响应应含确认单号，实际 %s", rec.Body.String())
	}
}

// TestPlaceOrder_非法JSON_400且不触达服务层 覆盖必填 body 的解析失败。
func TestPlaceOrder_非法JSON_400且不触达服务层(t *testing.T) {
	st := &fakeStore{}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodPost, "/confirmations/orders", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	req = withIdentity(req, "buyer-1", "sess-1")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_body") {
		t.Errorf("响应应含 invalid_body，实际 %s", rec.Body.String())
	}
	if st.prepareCalls != 0 {
		t.Errorf("body 非法时不应触达账本，实际 %d 次", st.prepareCalls)
	}
}

// TestPlaceOrder_空商品清单_400 覆盖服务层的形状校验经 HTTP 暴露的分支。
//
// 「至少一件商品」是账本对齐的硬约束；这条确认 handler 不会把空清单
// 当成功转发，且不会去查目录。
func TestPlaceOrder_空商品清单_400(t *testing.T) {
	cat := &fakeCatalog{}
	st := &fakeStore{}
	h := newTestHandlerWithCatalog(t, st, cat)

	body := `{"operation_id":"op-order","items":[],"shipping_address":{"recipient":"张三"}}`
	req := httptest.NewRequest(http.MethodPost, "/confirmations/orders", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withIdentity(req, "buyer-1", "sess-1")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空清单应 400，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), trade.CodeInvalidArgument) {
		t.Errorf("响应应含 %s，实际 %s", trade.CodeInvalidArgument, rec.Body.String())
	}
	if st.prepareCalls != 0 {
		t.Errorf("空清单不应触达账本，实际 %d 次", st.prepareCalls)
	}
}

// ---- 确认单详情 ----------------------------------------------------------

// TestGetConfirmation_成功含决议结果与时间 覆盖 GET /confirmations/{id}。
//
// 这条特意用一张「已决议」的确认单，把 ResolvedAt 与 Result 两个可选
// 分支都打开：决议后的详情必须能带出结果订单，否则前端拿到一张
// 「已批准但没有订单号」的确认单，无从跳转。
func TestGetConfirmation_成功含决议结果与时间(t *testing.T) {
	resolvedAt := fixedNow.Add(30 * time.Second)
	st := &fakeStore{
		confirmationResp: trade.Confirmation{
			ConfirmationID: "conf-1",
			OperationID:    "op-1",
			BuyerID:        "buyer-1",
			SessionID:      "sess-1",
			Action:         trade.ActionCreate,
			Status:         trade.StatusApproved,
			SnapshotHash:   "hash-1",
			ExpiresAt:      fixedNow.Add(time.Minute),
			CreatedAt:      fixedNow,
			ResolvedAt:     &resolvedAt,
			Result: &trade.OrderSnapshot{
				OrderID:   "order-1",
				BuyerID:   "buyer-1",
				Status:    order.StatusConfirmed,
				CreatedAt: fixedNow,
			},
		},
	}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodGet, "/confirmations/conf-1", nil)
	req = withIdentity(req, "buyer-1", "sess-1")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	if st.confirmationCalls != 1 {
		t.Fatalf("应恰好查询一次账本，实际 %d 次", st.confirmationCalls)
	}
	var body struct {
		ConfirmationID string         `json:"confirmation_id"`
		Status         string         `json:"status"`
		ResolvedAt     string         `json:"resolved_at"`
		Result         map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if body.ConfirmationID != "conf-1" || body.Status != string(trade.StatusApproved) {
		t.Errorf("确认单标识或状态未映射：%+v", body)
	}
	if body.ResolvedAt == "" {
		t.Error("已决议的确认单必须带 resolved_at")
	}
	if body.Result == nil || body.Result["order_id"] != "order-1" {
		t.Errorf("决议结果订单未映射：%+v", body.Result)
	}
}

// ---- 边角与装配 ----------------------------------------------------------

// TestListOrders_服务层错误_404 覆盖列表路径的错误映射（非 400 分支）。
func TestListOrders_服务层错误_404(t *testing.T) {
	st := &fakeStore{ordersErr: trade.Errorf(trade.CodeNotFound, "买家不存在")}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req = withIdentity(req, "buyer-1", "sess-1")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("账本 NotFound 应 404，实际 %d", rec.Code)
	}
}

// TestListOrders_非法状态_400且不查账本 覆盖订单状态过滤的校验分支。
//
// 未知状态必须被挡在账本之前：否则「查已发货」这类拼错的过滤条件会
// 静默返回空列表，调用方以为「没有这样的订单」。
func TestListOrders_非法状态_400且不查账本(t *testing.T) {
	st := &fakeStore{}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodGet, "/orders?status=SHIPPED", nil)
	req = withIdentity(req, "buyer-1", "sess-1")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知状态应 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_status") {
		t.Errorf("响应应含 invalid_status，实际 %s", rec.Body.String())
	}
	if st.ordersCalls != 0 {
		t.Errorf("状态非法时不应查询账本，实际 %d 次", st.ordersCalls)
	}
}

// TestNewHandler_未提供logger仍可用 覆盖 NewHandler 的 nil logger 分支。
//
// 构造时传 nil 必须回退到丢弃日志，而不是留下一个解引用即 panic 的字段。
func TestNewHandler_未提供logger仍可用(t *testing.T) {
	st := &fakeStore{}
	svc, err := tradesvc.New(tradesvc.Config{Store: st, Clock: fakeClock{now: fixedNow}})
	if err != nil {
		t.Fatalf("构造交易服务失败：%v", err)
	}
	h := NewHandler(svc, nil) // 显式传 nil

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req = withIdentity(req, "buyer-1", "sess-1")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("nil logger 的 handler 应正常工作，状态码 = %d", rec.Code)
	}
}

// TestWriteJSON_序列化失败不改已发出的状态码 覆盖 writeJSON 的错误分支。
//
// 序列化发生在 WriteHeader 之后，此时状态码已经发给客户端，能做的只有
// 记日志。这条钉住「失败时不 panic、不试图改状态码」这一行为。
func TestWriteJSON_序列化失败不改已发出的状态码(t *testing.T) {
	rec := httptest.NewRecorder()
	// channel 无法序列化成 JSON，必定触发 Encode 错误。
	writeJSON(rec, discardLogger(), http.StatusOK, make(chan int))

	if rec.Code != http.StatusOK {
		t.Errorf("头部已发出，状态码应保持 200，实际 %d", rec.Code)
	}
}
