// Package trade 是交易账本的领域模型：库存、交易确认单，以及「先准备、后决议」
// 两阶段提交的纯逻辑部分。
//
// 本包不碰数据库：所有写入都要靠外层的单个事务原子完成，因此这里只回答
// 「这次请求合法吗」「规范化之后的内容长什么样」「摘要应该是多少」，
// 并把结果交给持久化层。这样并发与事务的失败模式在数据库层验证，
// 而校验、规范化与摘要这些最容易写错的地方可以在毫秒级单测里穷举。
package trade

import (
	"crypto/hmac"
	"errors"
	"fmt"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/domain/canonical"
)

// AmountScope 是本轮交易账本唯一的金额口径：只结算商品金额，
// 不含运费与关税。做成常量而不是自由文本，避免口径变成调用方随手填的字符串。
const AmountScope = "merchandise_only"

// OrderKind 表示这张单据的用途是购买意向，而不是已支付订单。
const OrderKind = "purchase_intent"

// Action 是确认单要执行的交易动作。
type Action string

const (
	// ActionCreate 创建订单并扣减库存。
	ActionCreate Action = "create"

	// ActionCancel 取消订单并回补库存。
	ActionCancel Action = "cancel"
)

// Valid 报告动作是否为已知取值。
func (a Action) Valid() bool { return a == ActionCreate || a == ActionCancel }

// ConfirmationStatus 是确认单的决议状态。
type ConfirmationStatus string

const (
	// StatusPending 等待用户决议。只有这个状态可以改变。
	StatusPending ConfirmationStatus = "pending"

	// StatusApproved 已批准并已执行。终态。
	StatusApproved ConfirmationStatus = "approved"

	// StatusRejected 已拒绝，未产生任何交易。终态。
	StatusRejected ConfirmationStatus = "rejected"
)

// Valid 报告决议状态是否为已知取值。
func (s ConfirmationStatus) Valid() bool {
	return s == StatusPending || s == StatusApproved || s == StatusRejected
}

// Decision 是用户对确认单的决议。
//
// 用枚举而不是 bool：布尔参数在调用点上无法自证含义，
// 「拒绝」与「尚未决议」的区分也会随之丢失。
type Decision string

const (
	// DecisionApprove 批准并执行交易。
	DecisionApprove Decision = "approved"

	// DecisionReject 拒绝，不产生交易。
	DecisionReject Decision = "rejected"
)

// DecisionOf 把用户的批准/拒绝转成决议状态。
func DecisionOf(approved bool) Decision {
	if approved {
		return DecisionApprove
	}
	return DecisionReject
}

// Status 返回该决议对应的终态。
func (d Decision) Status() ConfirmationStatus { return ConfirmationStatus(d) }

// Confirmation 是一张交易确认单。
//
// 它把「用户看到的内容」与「设备将要执行的内容」钉在一起：
// SnapshotHash 覆盖买方、会话、动作、规范化载荷与有效期，
// 因此任何一项在决议前发生变化，决议都会被拒绝。
type Confirmation struct {
	ConfirmationID string
	OperationID    string
	BuyerID        string
	SessionID      string
	Action         Action
	RequestHash    string
	Payload        Payload
	SnapshotHash   string
	ExpiresAt      time.Time
	Status         ConfirmationStatus
	Result         *OrderSnapshot
	CreatedAt      time.Time
	ResolvedAt     *time.Time
}

// Expired 报告确认单在给定时刻是否已过期。
//
// 只对未决议的确认单有意义：已决议的确认单无论过了多久都应当原样返回，
// 否则重试会看到「已过期」，而交易其实早已完成。
func (c Confirmation) Expired(now time.Time) bool {
	return c.Status == StatusPending && !c.ExpiresAt.After(now)
}

// Binding 返回参与快照摘要计算的内容。
//
// 时间字段先经 canonical.Time 归一，保证同一时刻的不同表示得到同一摘要。
func (c Confirmation) Binding() map[string]any {
	return map[string]any{
		"buyer_id":   c.BuyerID,
		"session_id": c.SessionID,
		"action":     string(c.Action),
		"payload":    c.Payload.canonical(),
		"expires_at": canonical.Time(c.ExpiresAt),
	}
}

// ComputeSnapshotHash 计算确认单的快照摘要。
//
// 决议时重新计算并与存储值比对，而不是只比对存储值是否相等：
// 后者只能发现「传错了值」，前者还能发现「载荷被改过」。
func (c Confirmation) ComputeSnapshotHash() (string, error) {
	return canonical.Hash(c.Binding())
}

// RequestHash 计算「同一操作编号下请求内容是否一致」所用的摘要。
//
// 它只覆盖动作与规范化载荷，不覆盖有效期与身份：重试同一笔请求时
// 有效期会被重新计算，若把它纳入进来，重试就会被误判成内容变更。
func RequestHash(action Action, payload Payload) (string, error) {
	return canonical.Hash(map[string]any{
		"action":  string(action),
		"payload": payload.canonical(),
	})
}

// SnapshotMatches 以恒时方式比较两个快照摘要。
//
// 用恒时比较而不是 == ：摘要是可猜测的短串，比较耗时应与内容无关。
func SnapshotMatches(stored, candidate string) bool {
	return hmac.Equal([]byte(stored), []byte(candidate))
}

// ErrInvalidArgument 是「调用方给错了」这一类错误的基类。
var ErrInvalidArgument = errors.New("trade: 参数非法")

// StoreError 是一次被拒绝的交易操作。
//
// 它同时携带稳定的错误码与面向用户的说明：错误码供接口层映射 HTTP 状态，
// 说明可以直接展示给买家。
type StoreError struct {
	Code    string
	Message string
	// cause 保留底层原因，便于日志排查，不对外展示。
	cause error
}

// Error 实现 error。
func (e *StoreError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Unwrap 暴露底层原因。
func (e *StoreError) Unwrap() error { return e.cause }

// Is 让 errors.Is(err, ErrInvalidArgument) 对参数类错误成立。
func (e *StoreError) Is(target error) bool {
	return target == ErrInvalidArgument && e.Code == CodeInvalidArgument
}

// 交易账本的错误码。与源契约逐字一致，前端按这些字符串分支。
const (
	CodeInvalidArgument            = "INVALID_ARGUMENT"
	CodeInvalidQuery               = "INVALID_QUERY"
	CodeNotFound                   = "NOT_FOUND"
	CodeOwnerMismatch              = "OWNER_MISMATCH"
	CodeOperationConflict          = "OPERATION_CONFLICT"
	CodeConfirmationExpired        = "CONFIRMATION_EXPIRED"
	CodeInsufficientStock          = "INSUFFICIENT_STOCK"
	CodePriceChanged               = "PRICE_CHANGED"
	CodeSnapshotMismatch           = "SNAPSHOT_MISMATCH"
	CodeDecisionConflict           = "DECISION_CONFLICT"
	CodeOrderChanged               = "ORDER_CHANGED"
	CodeInventoryMigrationRequired = "INVENTORY_MIGRATION_REQUIRED"
	CodeDuplicateSKU               = "DUPLICATE_SKU"
	CodeSKUMigrationNotAllowed     = "SKU_MIGRATION_NOT_ALLOWED"
	CodeTransactionConflictExhaust = "TRANSACTION_CONFLICT_EXHAUSTED"
)

// Errorf 构造一个带错误码的拒绝错误。
func Errorf(code, format string, args ...any) *StoreError {
	return &StoreError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrapf 构造一个带错误码并保留底层原因的拒绝错误。
func Wrapf(code string, cause error, format string, args ...any) *StoreError {
	return &StoreError{Code: code, Message: fmt.Sprintf(format, args...), cause: cause}
}
