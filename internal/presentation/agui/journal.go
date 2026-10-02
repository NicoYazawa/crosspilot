// Package agui 是 AG-UI SSE 服务的实现入口。
//
// 本文件定义运行事件的 journal 接口与内存实现（测试用）。
//
// 为什么 journal 单独抽出：
//
//	「先落库再推送」是 E1/E4/E5 的实现依据。落库模型是 journal：每条事件都
//	按 (run_id, seq) 唯一索引，先持久化再对外广播；订阅者掉线重连只读
//	journal，绝不触发模型重跑。
//
//	把 journal 抽成接口，是为了让 handler / orchestrator / P5 的统一流都能用
//	同一份形状：在测试里换 MemoryJournal，在生产里换成 Postgres 实现。
package agui

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
)

// JournalStore 是事件 journal 的端口。
//
// Append 与 Since 是互斥的两条路径：Append 是「先写后推」的上游，Since 是
// 「重连时按 seq 续传」的下游。两条路径都不应触发模型调用。
type JournalStore interface {
	// Append 把事件写入 journal。
	//
	// 返回的 lastSeq 是「写入后本 run 的最新序号」——调用方推送给客户端时
	// 可以直接用，不需重新查询。
	//
	// 违反 (run_id, seq) 单调性的事件会被拒绝（返回 ErrSeqGap），调用方应
	// 终止当前流并发出 run_error 而不是把不一致状态传染给订阅者。
	Append(ctx context.Context, ev runevent.Event) (lastSeq int64, err error)

	// Since 按 run_id 与起点 seq 返回该 run 后续事件（seq >= since），按 seq 升序。
	//
	// run_id 不存在时返回空切片而非 error——前端「刷新了一下页面」是合法用法。
	Since(ctx context.Context, runID string, since int64, limit int) ([]runevent.Event, error)

	// LastSeq 返回 run 已知的最后序号；run 不存在返回 -1。
	LastSeq(ctx context.Context, runID string) (int64, error)

	// ListRuns 列出 journal 里所有 run_id，按最近事件时间倒序。
	//
	// E5 启动时用它扫「未关闭的 run」并补 SERVER_RESTART 哨兵。
	ListRuns(ctx context.Context) ([]RunMeta, error)
}

// RunMeta 是 journal 维度的 run 元信息。
type RunMeta struct {
	RunID       string
	LastSeq     int64
	LastKind    runevent.Kind
	LastEventAt time.Time
}

// ErrSeqGap 标识 Append 时序号出现缺口。
var ErrSeqGap = errors.New("agui: 事件序号缺口")

// ErrUnknownRun 标识 LastSeq / Since 找不到对应 run。
//
// 用 error 而不是 (val, ok) 双返回值：调用方希望把 ErrUnknownRun 当成
// 「这个 run 不存在」，但「调用本身失败」是另一回事（也要 error）。让两条
// 路径返回不同错误，调用方就不会误把「调用失败」当成「run 不存在」。
var ErrUnknownRun = errors.New("agui: 未知 run_id")

// MemoryJournal 是 JournalStore 的进程内实现。
//
// 真实实现会用 Postgres：表 `run_events (run_id text, seq bigint, event_id text,
// kind text, agent text, payload jsonb, created_at timestamptz, primary key(run_id, seq))`。
//
// 接口先稳定下来，Postgres 实现可以在不改 SSE handler 的情况下补上。
type MemoryJournal struct {
	mu      sync.Mutex
	events  map[string][]runevent.Event
	lastSeq map[string]int64
}

// NewMemoryJournal 构造一个空 journal。
func NewMemoryJournal() *MemoryJournal {
	return &MemoryJournal{
		events:  make(map[string][]runevent.Event),
		lastSeq: make(map[string]int64),
	}
}

// Append 实现 JournalStore.Append。
func (m *MemoryJournal) Append(_ context.Context, ev runevent.Event) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	last, seen := m.lastSeq[ev.RunID]
	if !seen {
		// 第一次写入该 run，序号必须是 0。
		if ev.Seq != 0 {
			return 0, ErrSeqGap
		}
	} else {
		// 后续写入必须严格 +1。
		if ev.Seq != last+1 {
			return last, ErrSeqGap
		}
	}

	m.events[ev.RunID] = append(m.events[ev.RunID], ev)
	m.lastSeq[ev.RunID] = ev.Seq
	return ev.Seq, nil
}

// Since 实现 JournalStore.Since。
func (m *MemoryJournal) Since(_ context.Context, runID string, since int64, limit int) ([]runevent.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	src := m.events[runID]
	if src == nil {
		return nil, nil
	}

	out := make([]runevent.Event, 0, len(src))
	for _, ev := range src {
		if ev.Seq >= since {
			out = append(out, ev)
		}
	}
	// already in seq order from Append; sort defensively
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// LastSeq 实现 JournalStore.LastSeq。
func (m *MemoryJournal) LastSeq(_ context.Context, runID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	last, ok := m.lastSeq[runID]
	if !ok {
		return -1, ErrUnknownRun
	}
	return last, nil
}

// ListRuns 实现 JournalStore.ListRuns。
func (m *MemoryJournal) ListRuns(_ context.Context) ([]RunMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]RunMeta, 0, len(m.events))
	for runID, evs := range m.events {
		if len(evs) == 0 {
			continue
		}
		last := evs[len(evs)-1]
		out = append(out, RunMeta{
			RunID:       runID,
			LastSeq:     last.Seq,
			LastKind:    last.Kind,
			LastEventAt: last.CreatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastEventAt.After(out[j].LastEventAt)
	})
	return out, nil
}
