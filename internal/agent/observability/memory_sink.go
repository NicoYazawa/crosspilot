// memory_sink.go 是 agent/observability 包的测试辅助 sink。
//
// 设计：MemorySink 放在 agent 层（不是 infra 层），是因为：
//   - Sink 接口与 SinkRecord 类型都在 agent 层
//   - infra 层不能 import agent 层（arch 规则）；所以 infra 层无法直接
//     实现 agent 层的 Sink 接口
//   - MemorySink 是 agent 层单测/F6/F7 闸门测试的事实标准实现
//
// 生产用的 Postgres sink 不在本文件范围；它在容器装配阶段由独立的适配器实现
// （PgSink 走 own type + adapter 模式，详见 container/observability/）。

package observability

import (
	"context"
	"errors"
	"sync"
)

// ErrSinkClosed 表示 Sink 已关闭，不应再写入。
var ErrSinkClosed = errors.New("observability: sink 已关闭")

// MemorySink 是测试用 sink：保留全部记录在内存中，可直接断言内容。
//
// 并发安全：Append 与 Records 调用都拿同一把互斥锁。
type MemorySink struct {
	mu      sync.Mutex
	records []SinkRecord
	closed  bool
	failN   int // 模拟前 N 次 Append 失败（测试注入）
}

// NewMemorySink 创建空内存 sink。
func NewMemorySink() *MemorySink {
	return &MemorySink{}
}

// Append 实现 Sink 接口。
func (m *MemorySink) Append(_ context.Context, batch []SinkRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrSinkClosed
	}
	if m.failN > 0 {
		m.failN--
		return errors.New("memory sink: 模拟失败")
	}
	m.records = append(m.records, batch...)
	return nil
}

// Close 实现 Sink 接口。
func (m *MemorySink) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// Records 返回当前所有记录的快照（按追加顺序）。
func (m *MemorySink) Records() []SinkRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SinkRecord, len(m.records))
	copy(out, m.records)
	return out
}

// SetFailN 设置前 N 次 Append 失败。测试用。
func (m *MemorySink) SetFailN(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failN = n
}
