// Cursor 与重连逻辑。
//
// SSE 协议把 cursor 放在 `Last-Event-ID` 头或查询参数 cursor 里，形式是
// `{runId}:{seq}`。客户端与服务端用这条信息续传；任何错位都必须在协议层
// 显式拒绝，而不是悄悄接受造成数据不一致。
package agui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Cursor 是解析后的「在某 run 上的某序号之后继续」。
//
// LastEventID 与 query 的 cursor 二选一：LastEventID 优先级更高，因为它来自
// EventSource 协议本身，前端代码几乎不会手动拼接。
type Cursor struct {
	RunID string
	Seq   int64
}

// ErrCursorInvalid 表示格式不合规。
//
// 调用方应直接返回 400 而不是 401：cursor 是协议层错误，不是身份错误。
var ErrCursorInvalid = errors.New("agui: cursor 格式不合法")

// ErrCursorMismatch 表示 run_id 与路径参数不一致。
//
// 客户端代码 bug 或前端用了错误的路由参数；不能让游标「自动校正」
// 跑去另一个 run——那是跨 run 数据泄漏。
var ErrCursorMismatch = errors.New("agui: cursor 与目标 run 不匹配")

// ParseCursor 把 `{runId}:{seq}` 拆开。
//
// seq 必须 >= 0：负 seq 是 journal 不可能产出的值，出现即协议层错误。
func ParseCursor(raw string) (Cursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Cursor{}, fmt.Errorf("%w：cursor 为空", ErrCursorInvalid)
	}
	idx := strings.LastIndex(raw, ":")
	if idx <= 0 || idx == len(raw)-1 {
		return Cursor{}, fmt.Errorf("%w：缺 runId 或 seq（%q）", ErrCursorInvalid, raw)
	}
	runID := raw[:idx]
	seqStr := raw[idx+1:]
	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil || seq < 0 {
		return Cursor{}, fmt.Errorf("%w：seq 应为非负整数（%q）", ErrCursorInvalid, raw)
	}
	return Cursor{RunID: runID, Seq: seq}, nil
}

// BindToRun 校验 cursor 与目标 run 是否匹配，并返回「起点 seq（cursor.Seq+1）」。
//
// 调用方传入「客户端声明的 cursor」与「本请求要订阅的 run_id」，
// 一致才返回续传起点；不一致时返回 ErrCursorMismatch。
//
// 起点是 cursor.Seq+1：cursor.Seq 表示「客户端已收到的最后序号」，
// 重连自然要从下一条开始。
//
// 当 cursor 是空的（首次连接 / 无 Last-Event-ID），返回 0 表示「从头开始」。
func BindToRun(c Cursor, expectedRunID string) (int64, error) {
	if c.RunID == "" {
		return 0, nil
	}
	if c.RunID != expectedRunID {
		return 0, fmt.Errorf("%w：cursor=%s target=%s", ErrCursorMismatch, c.RunID, expectedRunID)
	}
	return c.Seq + 1, nil
}

// GapError 是重连时检测到的「客户端 cursor 与服务端最新序号不一致」错误。
//
// E2 验收信号：当 cursor.Seq+1 != journal.LastSeq 时拒绝重连。
// 之所以写成独立类型而非复用 runevent.ErrGap，是为了让错误信息明确说
// 「重连时被拒」——客户端会换策略重新订阅。
type GapError struct {
	Expected int64
	Actual   int64
}

func (e *GapError) Error() string {
	return fmt.Sprintf("agui: 重连点 %d 与服务端最后序号 %d 不连续", e.Expected, e.Actual)
}

// CheckGap 校验客户端请求的起点是否合法。
//
// SSE `Last-Event-ID: run:N` 表示「我已经处理到 seq=N」，重连从 N+1 起。
// 因此 since == cursor.Seq+1，可能落在三种位置：
//
//	since == 0           → 首次订阅，从头开始，合法
//	since <= lastSeq+1   → 客户端至多落后一格，合法
//	since >  lastSeq+1   → 客户端声称已处理到 N，但服务端只到 lastSeq，
//	                       N+1 > lastSeq+1，缺口在 [lastSeq+1, since) 之间——E2 强信号
func CheckGap(lastSeq int64, since int64) error {
	if since == 0 {
		return nil
	}
	if since <= lastSeq+1 {
		return nil
	}
	return &GapError{Expected: lastSeq + 1, Actual: since}
}