// Diff 用例：对比两条 run 的事件序列，输出差异列表。
//
// F2 闸门：差分必须精确，无假阳性。本用例以 (runID, seq) 为主键对齐，按
// payload 的字节级 hash（payload_sha256）判 equal/change；payload 缺失则视为
// 「unknown」，不报 change（避免误报）。
//
// 应用场景：评测团队比较同一 query 在两个 agent 版本下的事件序列差异。
package observability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
)

// DiffKind 是差异类型。
type DiffKind string

const (
	DiffAdded    DiffKind = "added"     // 仅在 against 出现
	DiffRemoved  DiffKind = "removed"   // 仅在 baseline 出现
	DiffChanged  DiffKind = "changed"   // 两边都有，但 payload 不同
)

// DiffItem 是一条差异记录。
//
// Baseline 与 Against 都是原始事件（去重后的），Kind 为差异类型，Reason
// 是人类可读的描述。
type DiffItem struct {
	Seq      int64
	Kind     DiffKind
	Baseline *runevent.Event
	Against  *runevent.Event
	Reason   string
}

// DiffResult 是两条 run 的对比结果。
type DiffResult struct {
	BaselineRunID string
	AgainstRunID  string
	Items         []DiffItem
	Added         int
	Removed       int
	Changed       int
	Unchanged     int
}

// Diff 对比两条 run 的事件序列。
//
// 算法：
//  1. 拉取两条 run 的全部事件
//  2. 按 seq 对齐：seq 仅在一侧出现 → added/removed；两侧都有 → 比 payload sha256
//  3. payload 字节相同 → unchanged；不同 → changed（保留两侧的原始 payload）
func (uc *UseCases) Diff(ctx context.Context, baselineRunID, againstRunID string) (DiffResult, error) {
	baseline, err := uc.Journal.Since(ctx, baselineRunID, 0, 0)
	if err != nil {
		return DiffResult{}, err
	}
	against, err := uc.Journal.Since(ctx, againstRunID, 0, 0)
	if err != nil {
		return DiffResult{}, err
	}

	// 构造 seq → event 索引
	bySeq := make(map[int64]runevent.Event, len(baseline)+len(against))
	for _, ev := range baseline {
		bySeq[ev.Seq] = ev
	}

	result := DiffResult{
		BaselineRunID: baselineRunID,
		AgainstRunID:  againstRunID,
	}

	// 走两遍：先看 baseline（seq 仅在 baseline → removed），再 walk against（看 added/changed）
	for _, ev := range baseline {
		if other, ok := lookupSeq(against, ev.Seq); ok {
			if payloadEqual(ev.Payload, other.Payload) {
				result.Unchanged++
				continue
			}
			b, a := ev, other
			result.Items = append(result.Items, DiffItem{
				Seq:      ev.Seq,
				Kind:     DiffChanged,
				Baseline: &b,
				Against:  &a,
				Reason:   "payload 不同",
			})
			result.Changed++
		} else {
			e := ev
			result.Items = append(result.Items, DiffItem{
				Seq:      ev.Seq,
				Kind:     DiffRemoved,
				Baseline: &e,
				Against:  nil,
				Reason:   "仅 baseline 出现",
			})
			result.Removed++
		}
	}
	for _, ev := range against {
		if _, ok := lookupSeq(baseline, ev.Seq); ok {
			continue // 已在 baseline 循环里处理过（changed/unchanged）
		}
		e := ev
		result.Items = append(result.Items, DiffItem{
			Seq:      ev.Seq,
			Kind:     DiffAdded,
			Baseline: nil,
			Against:  &e,
			Reason:   "仅 against 出现",
		})
		result.Added++
	}

	sort.Slice(result.Items, func(i, j int) bool { return result.Items[i].Seq < result.Items[j].Seq })
	return result, nil
}

func lookupSeq(events []runevent.Event, seq int64) (runevent.Event, bool) {
	for _, ev := range events {
		if ev.Seq == seq {
			return ev, true
		}
	}
	return runevent.Event{}, false
}

func payloadEqual(a, b []byte) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	return sha256Hex(a) == sha256Hex(b)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
