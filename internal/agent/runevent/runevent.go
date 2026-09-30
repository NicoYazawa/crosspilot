// Package runevent 是所有 Agent 事件的统一基座。
//
// 任何要进入 journal / SSE / 可观测的事件都必须先构造成 RunEvent：
//
//	RunEvent{EventID, RunID, Seq, Kind, Agent, Payload, CreatedAt}
//
// Seq 是 run 内单调自增序号，是 SSE `Last-Event-ID: {runId}:{seq}` 的右半段。
// 单调性由 NewSequencer 强制：调用方拿不到未编号事件。
//
// 为什么不让 orchestrator 直接吐事件：orchestrator 的 Event 是为了把控制流讲清楚
// 的 DTO，含迭代次数、错误对象等中间状态；RunEvent 是要落到 journal 里、要让前端
// 按 seq 续传、要被可观测通道聚合的不可变记录。中间状态一旦写下去，回放就要做
// 一堆「当时的 Err 是不是临时包装」这种没意义的判断。
package runevent

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Kind 是事件类型。
//
// 任何新增类型必须显式加常量而不是字符串字面量——A2UI 报文与 SSE 路由都会按 kind
// 路由分发，写错了会被静默丢弃。
type Kind string

const (
	KindRunStart     Kind = "run_start"
	KindModelTurn    Kind = "model_turn"
	KindToolCall     Kind = "tool_call"
	KindToolResult   Kind = "tool_result"
	KindRunFinished  Kind = "run_finished"
	KindRunError     Kind = "run_error"
	KindServerRestart Kind = "server_restart"
	KindA2UI         Kind = "a2ui"
	KindHeartbeat    Kind = "heartbeat"
)

// Event 是不可变事件记录。
//
// JSON tag 是稳定的对外契约：SSE 客户端按这些字段读，journal 按这些字段写。
// 一旦发布就不重命名、不改 JSON tag 拼写。
type Event struct {
	EventID   string          `json:"event_id"`
	RunID     string          `json:"run_id"`
	Seq       int64           `json:"seq"`
	Kind      Kind            `json:"kind"`
	Agent     string          `json:"agent,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// ErrGap 表示事件序号出现缺口。
//
// 缺口是 E2 验收的强信号：要么上游漏写、要么 journal 写入失败后丢失回包。
// 无论哪种，订阅者都不能继续——客户端不知道缺口里有什么消息，强行续传等于
// 把不一致状态传染给用户。
var ErrGap = errors.New("runevent: 事件序号出现缺口")

// Sequencer 按 RunID 单调分配 Seq。
//
// 一个 run 一个 sequencer；run 结束（KindRunFinished / KindRunError / KindServerRestart）
// 后不应再创号。New 永远返回非 nil 的 *Sequencer：调用方按构造期检查即可。
type Sequencer struct {
	runID string
	next  int64
	last  int64
}

// NewSequencer 创建一个以 0 为下一序号、以 -1 为最后序号的 sequencer。
//
// 选择 -1 是为了让首次 Next 返回 0：seq=0 表示「run 的第一个事件」，跨语言解析时
// 不会出现 -1 引发「负序号是否合法」的语义分歧。
func NewSequencer(runID string) *Sequencer {
	return &Sequencer{runID: runID, next: 0, last: -1}
}

// Next 返回下一序号并推进内部状态。
//
// 调用方拿到序号后再写 journal / Payload / Kind，最后调 Attach 把它缝到 Event 上。
// 拆成两步而不是一步返回 Event，是为了让调用方在「拿到序号之后、写之前」可以校验
// 比如 `Seq != cursor+1` 这类交叉检查。
func (s *Sequencer) Next() int64 {
	seq := s.next
	s.next++
	s.last = seq
	return seq
}

// Last 返回上一次 Next 返回的序号。Sequencer 刚创建时返回 -1。
func (s *Sequencer) Last() int64 { return s.last }

// RunID 返回本 sequencer 所属的 run 标识。
func (s *Sequencer) RunID() string { return s.runID }

// Attach 把给定的序号缝进 Event 并校验 RunID 一致。
//
// 校验显式：错配的 RunID 是上游 bug，吞掉只会让事件落到错误 run 的 journal 里，
// 然后再花两个工时排查「为什么客户端用对的 cursor 拉不到对的事」。
func (s *Sequencer) Attach(seq int64, kind Kind, agent string, payload []byte, now time.Time) (Event, error) {
	if seq < 0 {
		return Event{}, fmt.Errorf("runevent: 非法序号 %d", seq)
	}
	if payload == nil {
		payload = []byte("{}")
	}
	if !json.Valid(payload) {
		return Event{}, fmt.Errorf("runevent: payload 不是合法 JSON")
	}
	return Event{
		EventID:   newEventID(s.runID, seq, now),
		RunID:     s.runID,
		Seq:       seq,
		Kind:      kind,
		Agent:     agent,
		Payload:   json.RawMessage(payload),
		CreatedAt: now,
	}, nil
}

// AttachWithID 在外部已有 EventID 时使用。
//
// 当前只在 SSE 重连回放处用到：客户端传过来的 EventID 需要原样保留，避免重连后
// 客户端的事件去重表把同一条事件当成「重复」丢掉。
func (s *Sequencer) AttachWithID(eventID string, seq int64, kind Kind, agent string, payload []byte, now time.Time) (Event, error) {
	if eventID == "" {
		return Event{}, fmt.Errorf("runevent: EventID 不能为空")
	}
	ev, err := s.Attach(seq, kind, agent, payload, now)
	if err != nil {
		return Event{}, err
	}
	ev.EventID = eventID
	return ev, nil
}

// VerifyGap 检查新事件序号是否紧贴已确认的最后序号。
//
// 重连场景下用：journal 写到 seq=N，重连请求带 cursor=N+1；如果 journal 看到的
// 实际最后一条是 M ≥ N+1 就拒绝——这个 cursor 已经过期或被造假。
func VerifyGap(prev, next int64) error {
	if next != prev+1 {
		return fmt.Errorf("%w：prev=%d next=%d", ErrGap, prev, next)
	}
	return nil
}

// newEventID 生成可读且单调的事件标识。
//
// 形式：`{runID}:{seq}:{unixNanos}`。人类可读、调试时一眼就能定位事件位置；
// 单调性由 unixNanos 兜底（同一毫秒多次写入时 seq 仍单调）。SSE 协议把它原样
// 放在 `id:` 行，客户端也按它做去重键。
func newEventID(runID string, seq int64, now time.Time) string {
	return fmt.Sprintf("%s:%d:%d", runID, seq, now.UnixNano())
}