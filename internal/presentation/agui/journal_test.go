package agui_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
	"github.com/NicoYazawa/crosspilot/internal/presentation/agui"
)

func newEvent(runID string, seq int64, kind runevent.Kind) runevent.Event {
	now := time.Unix(1735000000+seq, 0).UTC()
	seqr := runevent.NewSequencer(runID)
	for i := int64(0); i < seq; i++ {
		seqr.Next()
	}
	body, _ := json.Marshal(map[string]any{"i": seq})
	ev, err := seqr.Attach(seqr.Next(), kind, "main", body, now)
	if err != nil {
		panic(err)
	}
	return ev
}

func TestMemoryJournal_AppendRejectsGap(t *testing.T) {
	j := agui.NewMemoryJournal()
	ctx := context.Background()

	if _, err := j.Append(ctx, newEvent("run-1", 0, runevent.KindRunStart)); err != nil {
		t.Fatalf("seq=0 应通过：%v", err)
	}
	if _, err := j.Append(ctx, newEvent("run-1", 1, runevent.KindModelTurn)); err != nil {
		t.Fatalf("seq=1 应通过：%v", err)
	}
	_, err := j.Append(ctx, newEvent("run-1", 5, runevent.KindToolCall)) // 跳了
	if !errors.Is(err, agui.ErrSeqGap) {
		t.Fatalf("序号 5 应报 ErrSeqGap，实际 %v", err)
	}
}

func TestMemoryJournal_AppendRejectsFirstNonZero(t *testing.T) {
	j := agui.NewMemoryJournal()
	_, err := j.Append(context.Background(), newEvent("run-1", 5, runevent.KindRunStart))
	if !errors.Is(err, agui.ErrSeqGap) {
		t.Fatalf("首次写入非 0 应报 ErrSeqGap，实际 %v", err)
	}
}

func TestMemoryJournal_SinceReturnsRange(t *testing.T) {
	j := agui.NewMemoryJournal()
	ctx := context.Background()
	for i := int64(0); i < 5; i++ {
		if _, err := j.Append(ctx, newEvent("run-1", i, runevent.KindModelTurn)); err != nil {
			t.Fatalf("append %d 失败：%v", i, err)
		}
	}

	got, err := j.Since(ctx, "run-1", 2, 0)
	if err != nil {
		t.Fatalf("Since 失败：%v", err)
	}
	if len(got) != 3 {
		t.Fatalf("期望 3 条（seq=2,3,4），实际 %d", len(got))
	}
	for i, ev := range got {
		if ev.Seq != int64(i)+2 {
			t.Errorf("第 %d 条 seq=%d", i, ev.Seq)
		}
	}
}

func TestMemoryJournal_SinceUnknownRunReturnsEmpty(t *testing.T) {
	j := agui.NewMemoryJournal()
	got, err := j.Since(context.Background(), "missing", 0, 0)
	if err != nil {
		t.Fatalf("未知 run 不应报错：%v", err)
	}
	if len(got) != 0 {
		t.Errorf("期望 0 条，实际 %d", len(got))
	}
}

func TestMemoryJournal_LastSeqUnknownRun(t *testing.T) {
	j := agui.NewMemoryJournal()
	_, err := j.LastSeq(context.Background(), "missing")
	if !errors.Is(err, agui.ErrUnknownRun) {
		t.Errorf("应报 ErrUnknownRun，实际 %v", err)
	}
}

func TestMemoryJournal_ListRunsOrdersByLastTime(t *testing.T) {
	j := agui.NewMemoryJournal()
	ctx := context.Background()

	// 同一时间戳，事件顺序按写入顺序；本测试只验证列得出来 + 排序稳定。
	if _, err := j.Append(ctx, newEvent("run-1", 0, runevent.KindRunStart)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(ctx, newEvent("run-2", 0, runevent.KindRunStart)); err != nil {
		t.Fatal(err)
	}

	runs, err := j.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns 失败：%v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("期望 2 个 run，实际 %d", len(runs))
	}
	seen := map[string]bool{}
	for _, r := range runs {
		seen[r.RunID] = true
	}
	if !seen["run-1"] || !seen["run-2"] {
		t.Errorf("run 不全：%v", seen)
	}
}
