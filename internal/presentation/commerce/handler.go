// Package commerce 是交易链路的 HTTP 适配器（附录 B 第 4–6、18–21 条路由）。
//
// 这里只做三件事：解析 HTTP 形状、调用 application/trade.Service、把领域类型
// 映射成对外的 DTO。任何业务判断都不在本层——包括「地址是否合法」「金额是否
// 自洽」，那些是领域层的职责，在这里重复一遍只会产生第二个真相。
//
// 身份不从这里取：买家来自 JWT 的 sub，会话来自 X-Session-ID，两者都由
// presentation/auth 的中间件放进 context。处理器只读 context，因此换一种
// 凭据方案（例如内部服务用 mTLS）不需要改这里。
package commerce

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	tradesvc "github.com/NicoYazawa/crosspilot/internal/application/trade"
	"github.com/NicoYazawa/crosspilot/internal/domain/order"
	"github.com/NicoYazawa/crosspilot/internal/domain/trade"
	presauth "github.com/NicoYazawa/crosspilot/internal/presentation/auth"
)

// 列表分页的默认值与上限。
//
// 上限与领域端口对外的契约一致：调用方可以要求「少一点」，但不能要求更多。
const (
	defaultListLimit = 20
	maxListLimit     = 20
)

// Handler 把 trade.Service 暴露成 HTTP。
type Handler struct {
	svc    *tradesvc.Service
	logger *slog.Logger
}

// NewHandler 构造 handler。
func NewHandler(svc *tradesvc.Service, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Handler{svc: svc, logger: logger}
}

// Routes 返回 commerce 子路由（附录 B 第 4–6、18–21 条），路径相对于挂载点。
//
// 前缀 /commerce 由装配层用 Mount 决定——chi 对同一个挂载路径只允许挂一次，
// 子路由各自声明绝对前缀会让第二个挂载点直接 panic。
//
// 身份中间件由调用方在挂载点外层挂上；本函数只负责路径。
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/orders", h.listOrders)
	r.Get("/orders/{orderID}", h.getOrder)
	r.Post("/orders/{orderID}/cancel", h.cancelOrder)

	r.Get("/confirmations", h.listConfirmations)
	r.Post("/confirmations/orders", h.placeOrder)
	r.Get("/confirmations/{confirmationID}", h.getConfirmation)
	r.Post("/confirmations/{confirmationID}/resolve", h.resolveConfirmation)
	return r
}

// ---- 订单 ----

// orderListResponse 是订单列表的响应体。
//
// Total 与 Orders 分开给：前端要翻页必须知道总数，只给本页条数会导致
// 分页控件只能「下一页」而不能「跳到第 N 页」。
type orderListResponse struct {
	Orders []orderSnapshotDTO `json:"orders"`
	Total  int                `json:"total"`
	Offset int                `json:"offset"`
	Limit  int                `json:"limit"`
}

func (h *Handler) listOrders(w http.ResponseWriter, r *http.Request) {
	buyer := buyerOf(r)
	if buyer == "" {
		writeError(w, h.logger, http.StatusUnauthorized, "unauthorized", nil)
		return
	}

	offset, err := queryInt(r, "offset", 0)
	if err != nil {
		writeError(w, h.logger, http.StatusBadRequest, "invalid_offset", err)
		return
	}
	limit, err := queryInt(r, "limit", defaultListLimit)
	if err != nil {
		writeError(w, h.logger, http.StatusBadRequest, "invalid_limit", err)
		return
	}

	status := order.Status(r.URL.Query().Get("status"))
	if status != "" && !status.Valid() {
		writeError(w, h.logger, http.StatusBadRequest, "invalid_status",
			errors.New("未知的订单状态: "+string(status)))
		return
	}

	page, err := h.svc.ListOrders(r.Context(), trade.OrderFilter{
		BuyerID: buyer,
		Status:  status,
		Offset:  offset,
		Limit:   limit,
	})
	if err != nil {
		writeServiceError(w, h.logger, err)
		return
	}

	out := make([]orderSnapshotDTO, 0, len(page.Orders))
	for i := range page.Orders {
		out = append(out, newOrderSnapshotDTO(page.Orders[i]))
	}
	writeJSON(w, h.logger, http.StatusOK, orderListResponse{
		Orders: out,
		Total:  page.Total,
		Offset: page.Offset,
		Limit:  page.Limit,
	})
}

func (h *Handler) getOrder(w http.ResponseWriter, r *http.Request) {
	buyer := buyerOf(r)
	if buyer == "" {
		writeError(w, h.logger, http.StatusUnauthorized, "unauthorized", nil)
		return
	}
	snap, err := h.svc.GetOrder(r.Context(), chi.URLParam(r, "orderID"), buyer)
	if err != nil {
		writeServiceError(w, h.logger, err)
		return
	}
	writeJSON(w, h.logger, http.StatusOK, newOrderSnapshotDTO(snap))
}

// cancelOrder 发起一张取消确认单。
//
// 它不直接改订单状态，而是返回一张待决议的确认单——取消与下单同样是
// 「先准备、后决议」的两阶段动作，跳过确认直接把订单改成已取消，等于给
// HTTP 层一个绕过账本的口子。
func (h *Handler) cancelOrder(w http.ResponseWriter, r *http.Request) {
	buyer, session := buyerOf(r), sessionOf(r)
	if buyer == "" || session == "" {
		writeError(w, h.logger, http.StatusUnauthorized, "unauthorized", nil)
		return
	}

	var body struct {
		OperationID string `json:"operation_id"`
		Reason      string `json:"reason"`
	}
	if !decodeOptionalBody(w, h.logger, r, &body) {
		return
	}

	conf, err := h.svc.CancelOrder(r.Context(), tradesvc.CancelOrderInput{
		OperationID: body.OperationID,
		BuyerID:     buyer,
		SessionID:   session,
		OrderID:     chi.URLParam(r, "orderID"),
		Reason:      body.Reason,
	})
	if err != nil {
		writeServiceError(w, h.logger, err)
		return
	}
	writeJSON(w, h.logger, http.StatusOK, newConfirmationDTO(conf))
}

// ---- 确认单 ----

func (h *Handler) listConfirmations(w http.ResponseWriter, r *http.Request) {
	buyer, session := buyerOf(r), sessionOf(r)
	if buyer == "" || session == "" {
		writeError(w, h.logger, http.StatusUnauthorized, "unauthorized", nil)
		return
	}
	limit, err := queryInt(r, "limit", maxListLimit)
	if err != nil {
		writeError(w, h.logger, http.StatusBadRequest, "invalid_limit", err)
		return
	}

	confs, err := h.svc.List(r.Context(), buyer, session, limit)
	if err != nil {
		writeServiceError(w, h.logger, err)
		return
	}
	out := make([]confirmationDTO, 0, len(confs))
	for i := range confs {
		out = append(out, newConfirmationDTO(confs[i]))
	}
	writeJSON(w, h.logger, http.StatusOK, map[string]any{"confirmations": out})
}

// placeOrderInput 是 POST /commerce/confirmations/orders 的请求体。
//
// 刻意不收价格：价格是商品目录的权威事实，让调用方传价格就等于允许调用方
// 定价，「确认单上的金额」也就失去了意义。
type placeOrderInput struct {
	OperationID string `json:"operation_id"`
	Items       []struct {
		ProductID string `json:"product_id"`
		SKUID     string `json:"sku_id"`
		Quantity  int64  `json:"quantity"`
	} `json:"items"`
	ShippingAddress addressDTO `json:"shipping_address"`
}

func (h *Handler) placeOrder(w http.ResponseWriter, r *http.Request) {
	buyer, session := buyerOf(r), sessionOf(r)
	if buyer == "" || session == "" {
		writeError(w, h.logger, http.StatusUnauthorized, "unauthorized", nil)
		return
	}

	var body placeOrderInput
	if !decodeRequiredBody(w, h.logger, r, &body) {
		return
	}

	items := make([]tradesvc.OrderIntent, 0, len(body.Items))
	for _, it := range body.Items {
		items = append(items, tradesvc.OrderIntent{
			ProductID: it.ProductID,
			SKUID:     it.SKUID,
			Quantity:  it.Quantity,
		})
	}

	conf, err := h.svc.PlaceOrder(r.Context(), tradesvc.PlaceOrderInput{
		OperationID:     body.OperationID,
		BuyerID:         buyer,
		SessionID:       session,
		Items:           items,
		ShippingAddress: body.ShippingAddress.toDomain(),
	})
	if err != nil {
		writeServiceError(w, h.logger, err)
		return
	}
	writeJSON(w, h.logger, http.StatusOK, newConfirmationDTO(conf))
}

func (h *Handler) getConfirmation(w http.ResponseWriter, r *http.Request) {
	buyer, session := buyerOf(r), sessionOf(r)
	if buyer == "" || session == "" {
		writeError(w, h.logger, http.StatusUnauthorized, "unauthorized", nil)
		return
	}
	conf, err := h.svc.Get(r.Context(), chi.URLParam(r, "confirmationID"), buyer, session)
	if err != nil {
		writeServiceError(w, h.logger, err)
		return
	}
	writeJSON(w, h.logger, http.StatusOK, newConfirmationDTO(conf))
}

// resolveConfirmation 执行买家对确认单的决议。
func (h *Handler) resolveConfirmation(w http.ResponseWriter, r *http.Request) {
	buyer, session := buyerOf(r), sessionOf(r)
	if buyer == "" || session == "" {
		writeError(w, h.logger, http.StatusUnauthorized, "unauthorized", nil)
		return
	}

	// Approved 用指针：缺字段与「显式传 false」是两回事。用布尔值接收时
	// 两者都变成 false，一次漏传 approved 的请求会被当成「买家拒绝了」，
	// 而这个误判会真的改变订单状态。
	var body struct {
		SnapshotHash string `json:"snapshot_hash"`
		Approved     *bool  `json:"approved"`
	}
	if !decodeRequiredBody(w, h.logger, r, &body) {
		return
	}
	if body.Approved == nil {
		writeError(w, h.logger, http.StatusBadRequest, "missing_approved",
			errors.New("approved 必填"))
		return
	}

	conf, err := h.svc.Resolve(r.Context(), tradesvc.ResolveInput{
		ConfirmationID: chi.URLParam(r, "confirmationID"),
		BuyerID:        buyer,
		SessionID:      session,
		SnapshotHash:   body.SnapshotHash,
		Approved:       *body.Approved,
	})
	if err != nil {
		writeServiceError(w, h.logger, err)
		return
	}
	writeJSON(w, h.logger, http.StatusOK, newConfirmationDTO(conf))
}

// ---- DTO ----------------------------------------------------------------

// addressDTO 是收货地址的对外形状。
type addressDTO struct {
	Recipient  string `json:"recipient"`
	Phone      string `json:"phone"`
	Country    string `json:"country"`
	Province   string `json:"province"`
	City       string `json:"city"`
	Line1      string `json:"line1"`
	Line2      string `json:"line2,omitempty"`
	PostalCode string `json:"postal_code"`
}

func (a addressDTO) toDomain() order.Address {
	return order.Address{
		Recipient:  a.Recipient,
		Phone:      a.Phone,
		Country:    a.Country,
		Province:   a.Province,
		City:       a.City,
		Line1:      a.Line1,
		Line2:      a.Line2,
		PostalCode: a.PostalCode,
	}
}

// orderSnapshotDTO 是订单的对外快照。
//
// 用 order_id / total_amount_minor 这样的 snake_case，与前端和 AG-UI 报文的
// 命名保持一致；领域类型不带 JSON tag，正是为了让「对外形状」只在这一层出现。
type orderSnapshotDTO struct {
	OrderID          string         `json:"order_id"`
	BuyerID          string         `json:"buyer_id"`
	Status           string         `json:"status"`
	TotalAmountMajor string         `json:"total_amount_major"`
	TotalAmountMinor int64          `json:"total_amount_minor"`
	Currency         string         `json:"currency"`
	AmountScope      string         `json:"amount_scope"`
	OrderKind        string         `json:"order_kind"`
	ShippingAddress  string         `json:"shipping_address"`
	Lines            []orderLineDTO `json:"lines"`
	CreatedAt        string         `json:"created_at"`
	CancelReason     string         `json:"cancel_reason,omitempty"`
}

// orderLineDTO 是订单行的对外形状。
//
// 不含行小计：单价是十进制字符串，乘数量是一次金额运算——金额运算属于领域层，
// 在 HTTP 适配器里用 float 乘一遍正是「第二个真相」的典型来源。订单级合计
// 已由 total_amount_major 给出。
type orderLineDTO struct {
	ProductID      string `json:"product_id"`
	SKUID          string `json:"sku_id"`
	Title          string `json:"title"`
	Quantity       int64  `json:"quantity"`
	UnitPriceMajor string `json:"unit_price_major"`
	Currency       string `json:"currency"`
}

func newOrderSnapshotDTO(s trade.OrderSnapshot) orderSnapshotDTO {
	lines := make([]orderLineDTO, 0, len(s.Lines))
	for _, l := range s.Lines {
		lines = append(lines, orderLineDTO{
			ProductID:      l.ProductID,
			SKUID:          l.SKUID,
			Title:          l.Title,
			Quantity:       l.Quantity,
			UnitPriceMajor: l.UnitPriceMajor,
			Currency:       string(l.Currency),
		})
	}
	return orderSnapshotDTO{
		OrderID:          s.OrderID,
		BuyerID:          s.BuyerID,
		Status:           string(s.Status),
		TotalAmountMajor: s.TotalAmountMajor,
		TotalAmountMinor: s.TotalAmountMinor,
		Currency:         string(s.Currency),
		AmountScope:      s.AmountScope,
		OrderKind:        s.OrderKind,
		ShippingAddress:  s.ShippingAddress,
		Lines:            lines,
		CreatedAt:        s.CreatedAt.UTC().Format(time.RFC3339),
		CancelReason:     s.CancelReason,
	}
}

// confirmationDTO 是确认单的对外形状。
//
// 暴露 snapshot_hash 是必须的：决议请求要把它原样带回，账本据此判断
// 「买家看到的那份内容」与「此刻要执行的内容」是否还是同一份。
type confirmationDTO struct {
	ConfirmationID string            `json:"confirmation_id"`
	OperationID    string            `json:"operation_id"`
	BuyerID        string            `json:"buyer_id"`
	SessionID      string            `json:"session_id"`
	Action         string            `json:"action"`
	Status         string            `json:"status"`
	SnapshotHash   string            `json:"snapshot_hash"`
	ExpiresAt      string            `json:"expires_at"`
	CreatedAt      string            `json:"created_at"`
	ResolvedAt     string            `json:"resolved_at,omitempty"`
	Result         *orderSnapshotDTO `json:"result,omitempty"`
}

func newConfirmationDTO(c trade.Confirmation) confirmationDTO {
	dto := confirmationDTO{
		ConfirmationID: c.ConfirmationID,
		OperationID:    c.OperationID,
		BuyerID:        c.BuyerID,
		SessionID:      c.SessionID,
		Action:         string(c.Action),
		Status:         string(c.Status),
		SnapshotHash:   c.SnapshotHash,
		ExpiresAt:      c.ExpiresAt.UTC().Format(time.RFC3339),
		CreatedAt:      c.CreatedAt.UTC().Format(time.RFC3339),
	}
	if c.ResolvedAt != nil {
		dto.ResolvedAt = c.ResolvedAt.UTC().Format(time.RFC3339)
	}
	if c.Result != nil {
		r := newOrderSnapshotDTO(*c.Result)
		dto.Result = &r
	}
	return dto
}

// ---- helpers ------------------------------------------------------------

func buyerOf(r *http.Request) string   { return presauth.BuyerFrom(r.Context()) }
func sessionOf(r *http.Request) string { return presauth.SessionFrom(r.Context()) }

// queryInt 解析整型查询参数，缺省时返回 def。
func queryInt(r *http.Request, name string, def int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New(name + " 不是整数: " + raw)
	}
	return v, nil
}

// decodeRequiredBody 解析请求体，空 body 视为错误。
func decodeRequiredBody(w http.ResponseWriter, logger *slog.Logger, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, logger, http.StatusBadRequest, "invalid_body", err)
		return false
	}
	return true
}

// decodeOptionalBody 解析请求体，空 body 视为「全部字段缺省」。
//
// cancel 的 reason 与 operation_id 都可能不传，此时 body 干脆是空的
// （Content-Length: 0）。把它当成解析失败会让「取消订单」这个最普通的
// 动作必须带一个空 JSON 才能通过。
func decodeOptionalBody(w http.ResponseWriter, logger *slog.Logger, r *http.Request, dst any) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, logger, http.StatusBadRequest, "invalid_body", err)
		return false
	}
	return true
}

// writeServiceError 把账本错误码映射成状态码。
//
// 错误码本身原样回给客户端：源契约里前端就是按 CodeInvalidArgument /
// CodeConfirmationExpired 这些字符串分支的，换成自造的 "conflict" 会让
// 「确认单过期」和「价格已变」两种完全不同的处置方式（前者重新下单、
// 后者提示改价）在前端塌缩成同一件事。
//
// 码 → 状态的对应关系只有一处判断：分类依据是「客户端重试同一个请求会怎样」，
// 而不是错误发生在哪一层。
func writeServiceError(w http.ResponseWriter, logger *slog.Logger, err error) {
	var se *trade.StoreError
	if !errors.As(err, &se) {
		// 不是账本的拒绝错误：要么是服务层的形状校验，要么是真故障。
		switch {
		case errors.Is(err, trade.ErrInvalidArgument),
			errors.Is(err, order.ErrInvalidOrder),
			errors.Is(err, order.ErrInvalidStatus),
			errors.Is(err, order.ErrEmptyLines),
			errors.Is(err, order.ErrCurrencyMismatch),
			errors.Is(err, order.ErrCancelReasonRequired):
			writeError(w, logger, http.StatusBadRequest, trade.CodeInvalidArgument, err)
		default:
			writeError(w, logger, http.StatusInternalServerError, "INTERNAL", err)
		}
		return
	}

	switch se.Code {
	case trade.CodeNotFound:
		// 也覆盖 order.ErrNotFound 的场景：商品/规格/订单不存在都归这里。
		writeError(w, logger, http.StatusNotFound, se.Code, err)
	case trade.CodeInvalidArgument, trade.CodeInvalidQuery:
		writeError(w, logger, http.StatusBadRequest, se.Code, err)
	case trade.CodeOwnerMismatch:
		// 403 而不是 404：调用方身份是合法的，只是不是这张单子的主人。
		// 用 404 遮掩会让真正的越权尝试在日志里与「输错 ID」无法区分。
		writeError(w, logger, http.StatusForbidden, se.Code, err)
	case trade.CodeTransactionConflictExhaust:
		// 事务冲突耗尽是可以重试的，503 + Retry-After 比 500 更准确。
		w.Header().Set("Retry-After", "1")
		writeError(w, logger, http.StatusServiceUnavailable, se.Code, err)
	default:
		// 其余都是「请求与当前状态对不上」：确认单过期、库存不足、价格已变、
		// 快照不匹配、决议冲突、订单已变更。重试同一个请求不会成功，
		// 但重新拉一次状态再发新请求会——409 表达的正是这个意思。
		writeError(w, logger, http.StatusConflict, se.Code, err)
	}
}

func writeJSON(w http.ResponseWriter, logger *slog.Logger, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// 头部已经发出，改不了状态码，只能记录——此时能做的只有让日志留下痕迹。
		logger.Error("commerce: 响应序列化失败", slog.Any("error", err))
	}
}

func writeError(w http.ResponseWriter, logger *slog.Logger, status int, code string, err error) {
	if err != nil && status >= http.StatusInternalServerError {
		logger.Error("commerce: 请求失败", slog.String("code", code), slog.Any("error", err))
	} else if err != nil {
		logger.Warn("commerce: 请求被拒绝", slog.String("code", code), slog.Any("error", err))
	}
	writeJSON(w, logger, status, map[string]string{"error": code})
}
