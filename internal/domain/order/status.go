package order

import (
	"encoding/json"
	"fmt"
)

// Status 是订单的状态。
type Status string

const (
	// StatusPending 已下单，等待卖家确认。订单创建后即处于此状态。
	StatusPending Status = "pending"

	// StatusConfirmed 卖家已确认，交易成立。终态。
	StatusConfirmed Status = "confirmed"

	// StatusCancelled 买家或卖家主动取消。终态。
	StatusCancelled Status = "cancelled"

	// StatusExpired 超时未确认，自动关闭。终态。
	StatusExpired Status = "expired"
)

// validStatuses 是全量合法状态，供校验与测试遍历使用。
var validStatuses = []Status{
	StatusPending,
	StatusConfirmed,
	StatusCancelled,
	StatusExpired,
}

// transitions 描述状态机的合法迁移：pending 可以走向三个终态，终态无出边。
var transitions = map[Status][]Status{
	StatusPending:   {StatusConfirmed, StatusCancelled, StatusExpired},
	StatusConfirmed: nil,
	StatusCancelled: nil,
	StatusExpired:   nil,
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
