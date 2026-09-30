// Replay 用例：从 journal 拉取某 run 的全部事件，返回给前端。
//
// F1 闸门：回放必须 100% 复现原始事件序列。本用例直接消费 journal.Since，
// journal 是「先落库再推送」的权威源——它与 SSE 写入路径是同一份数据，
// 回放不可能与原始事件序列不一致。
//
// D7 决策：不另建 event 表——observability schema 只存成本/归因/diff 标记。
package observability

import (
	"context"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
)

// ReplayResult 是回放的返回值。
//
// Events 是按 seq 升序的全部事件；TotalSeq 是最后一条事件的 seq 序号。
type ReplayResult struct {
	RunID    string
	Events   []runevent.Event
	TotalSeq int64
	// HasMore 表示 from < lastSeq 时是否还有未拉取的事件。
	HasMore bool
}

// Replay 拉取指定 run 的一段事件。
//
// from 是「起点 seq」——0 表示从头开始；> 0 表示「已知 lastSeq=N，从 N+1 续」。
// limit <= 0 表示不限制（拉全部）。
//
// 行为保证：
//   - 返回的事件严格按 seq 升序
//   - runID 不存在返回 ReplayResult{Events: nil}，不返回错误
//     （前端"刷新页面"是合法用法，与 agui.JournalStore.Since 一致）
//   - 返回错误只发生在 journal 调用本身失败（连接断开等）
func (uc *UseCases) Replay(ctx context.Context, runID string, from int64, limit int) (ReplayResult, error) {
	events, err := uc.Journal.Since(ctx, runID, from, limit)
	if err != nil {
		return ReplayResult{}, err
	}
	if len(events) == 0 {
		return ReplayResult{RunID: runID}, nil
	}

	lastSeq, err := uc.Journal.LastSeq(ctx, runID)
	if err != nil {
		// run 不存在时 LastSeq 返回 ErrUnknownRun；这里容忍它
		lastSeq = events[len(events)-1].Seq
	}

	totalSeq := events[len(events)-1].Seq
	hasMore := lastSeq > totalSeq

	return ReplayResult{
		RunID:    runID,
		Events:   events,
		TotalSeq: totalSeq,
		HasMore:  hasMore,
	}, nil
}
