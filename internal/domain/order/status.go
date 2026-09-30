package order

import (
	"encoding/json"
	"fmt"
)

// Status 是订单的状态。
//
// 取值是大写英文单词而非小写：线上契约里它就是这几个裸字符串，前端与既有的
// 评测数据都按大写比较。改成小写不会让任何测试失败，却会静默改变对外契约。
type Status string

const (
	// StatusDraft 已建单，尚未确认。订单创建后即处于此状态。
	StatusDraft Status = "DRAFT"

	// StatusConfirmed 交易成立。可以取消，但不能回到草稿。
	StatusConfirmed Status = "CONFIRMED"

	// StatusCancelled 已取消，库存已回补。终态。
	StatusCancelled Status = "CANCELLED"
)

// validStatuses 是全量合法状态，供校验与测试遍历使用。
var validStatuses = []Status{
	StatusDraft,
	StatusConfirmed,
	StatusCancelled,
}

// transitions 描述状态机的合法迁移：草稿可以确认或直接取消，
// 已确认只能取消，取消是终态。
var transitions = map[Status][]Status{
	StatusDraft:     {StatusConfirmed, StatusCancelled},
	StatusConfirmed: {StatusCancelled},
	StatusCancelled: nil,
}

// AllStatuses 返回全部合法状态的副本。
func AllStatuses() []Status {
	out := make([]Status, len(validStatuses))
	copy(out, validStatuses)
	return out
}

// Valid 报告该状态是否为已知取值。
func (s Status) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// IsTerminal 报告该状态是否为终态，即不再有出边。
func (s Status) IsTerminal() bool {
	targets, ok := transitions[s]
	return ok && len(targets) == 0
}

// CanTransitionTo 报告从当前状态迁移到 next 是否合法。
func (s Status) CanTransitionTo(next Status) bool {
	for _, candidate := range transitions[s] {
		if candidate == next {
			return true
		}
	}
	return false
}

// MarshalJSON 实现 json.Marshaler，拒绝写出未知状态。
func (s Status) MarshalJSON() ([]byte, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidStatus, string(s))
	}
	return json.Marshal(string(s))
}

// UnmarshalJSON 实现 json.Unmarshaler，拒绝读入未知状态。
//
// 只接受大写取值：把 "confirmed" 也当合法会让契约悄悄放宽，
// 而放宽之后再收紧就是破坏性变更。
func (s *Status) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidStatus, err)
	}
	parsed := Status(raw)
	if !parsed.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidStatus, raw)
	}
	*s = parsed
	return nil
}

// String 实现 fmt.Stringer。
func (s Status) String() string { return string(s) }
