package observability_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
	"github.com/NicoYazawa/crosspilot/internal/application/observability"
)

// fakeJournal 是 observability.JournalStore 的测试替身。
type fakeJournal struct {
	mu      sync.Mutex
	events  map[string][]runevent.Event
	lastSeq map[string]int64
}

func newFakeJournal() *fakeJournal {
	return &fakeJournal{
		events:  map[string][]runevent.Event{},
		lastSeq: map[string]int64{},
	}
}

func (j *fakeJournal) Append(_ context.Context, ev runevent.Event) (int64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	last, seen := j.lastSeq[ev.RunID]
	if !seen && ev.Seq != 0 {
		return 0, errors.New("seq gap")
	}
	if seen && ev.Seq != last+1 {
		return last, errors.New("seq gap")
	}
	j.events[ev.RunID] = append(j.events[ev.RunID], ev)
	j.lastSeq[ev.RunID] = ev.Seq
	return ev.Seq, nil
}

func (j *fakeJournal) Since(_ context.Context, runID string, since int64, limit int) ([]runevent.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	src := j.events[runID]
	out := make([]runevent.Event, 0, len(src))
	for _, ev := range src {
		if ev.Seq >= since {
			out = append(out, ev)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (j *fakeJournal) LastSeq(_ context.Context, runID string) (int64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	last, ok := j.lastSeq[runID]
	if !ok {
		return -1, errors.New("unknown run")
	}
	return last, nil
}

// fakeCostStore 是 observability.CostStore 的测试替身。
type fakeCostStore struct {
	mu     sync.Mutex
	events map[string][]observability.CostEvent
}

func newFakeCostStore() *fakeCostStore {
	return &fakeCostStore{events: map[string][]observability.CostEvent{}}
}

func (c *fakeCostStore) CostOfRun(_ context.Context, runID string) ([]observability.CostEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.events[runID], nil
}

func (c *fakeCostStore) add(runID string, ev observability.CostEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events[runID] = append(c.events[runID], ev)
}

// fakeExperimentStore 是 observability.ExperimentStore 的测试替身。
type fakeExperimentStore struct {
	mu   sync.Mutex
	arms map[string]string // runID → arm
}

func newFakeExperimentStore() *fakeExperimentStore {
	return &fakeExperimentStore{arms: map[string]string{}}
}

func (e *fakeExperimentStore) ArmFor(_ context.Context, runID string) (string, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := e.arms[runID]
	return a, ok, nil
}

func (e *fakeExperimentStore) ArmsSummary(_ context.Context, _ string) ([]observability.ArmSummary, error) {
	// 简化：固定返回两臂
	return []observability.ArmSummary{
		{Arm: "A", Calls: 10, LatencyP95Ms: 1200, CostTotalMinor: 100},
		{Arm: "B", Calls: 10, LatencyP95Ms: 1500, CostTotalMinor: 150},
	}, nil
}

// makeSeqEvent 构造一条 runevent.Event。
func makeSeqEvent(runID string, seq int64, kind runevent.Kind, payload string) runevent.Event {
	seqr := runevent.NewSequencer(runID)
	for i := int64(0); i < seq; i++ {
		seqr.Next()
	}
	now := time.Unix(1700000000, 0).UTC()
	ev, err := seqr.Attach(seqr.Next(), kind, "main", []byte(payload), now)
	if err != nil {
		panic(err)
	}
	return ev
}

// TestReplay_ReturnsAllEventsInSeqOrder 验证回放按 seq 升序返回全部事件。
func TestReplay_ReturnsAllEventsInSeqOrder(t *testing.T) {
	t.Parallel()
	j := newFakeJournal()
	uc := observability.New(j, newFakeCostStore(), newFakeExperimentStore(), nil)

	ctx := context.Background()
	for i := int64(0); i < 5; i++ {
		_, _ = j.Append(ctx, makeSeqEvent("r1", i, runevent.KindModelTurn, `{"k":"v"}`))
	}

	got, err := uc.Replay(ctx, "r1", 0, 0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got.Events) != 5 {
		t.Fatalf("期望 5 条，实际 %d", len(got.Events))
	}
	for i, ev := range got.Events {
		if ev.Seq != int64(i) {
			t.Fatalf("第 %d 条 seq 应为 %d，实际 %d", i, i, ev.Seq)
		}
	}
	if got.HasMore {
		t.Errorf("已拉全部，HasMore 应为 false")
	}
}

// TestReplay_UnknownRunReturnsEmpty 验证未知 run 返回空切片而非错误。
func TestReplay_UnknownRunReturnsEmpty(t *testing.T) {
	t.Parallel()
	uc := observability.New(newFakeJournal(), newFakeCostStore(), newFakeExperimentStore(), nil)
	got, err := uc.Replay(context.Background(), "ghost", 0, 0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got.Events) != 0 {
		t.Fatalf("未知 run 应返回空切片，实际 %d 条", len(got.Events))
	}
}

// TestReplay_HasMoreWhenLimitTruncated 验证 limit 截断后 HasMore=true。
func TestReplay_HasMoreWhenLimitTruncated(t *testing.T) {
	t.Parallel()
	j := newFakeJournal()
	uc := observability.New(j, newFakeCostStore(), newFakeExperimentStore(), nil)
	ctx := context.Background()
	for i := int64(0); i < 5; i++ {
		_, _ = j.Append(ctx, makeSeqEvent("r1", i, runevent.KindModelTurn, `{}`))
	}
	got, err := uc.Replay(ctx, "r1", 0, 2)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got.Events) != 2 {
		t.Fatalf("期望 2 条，实际 %d", len(got.Events))
	}
	if !got.HasMore {
		t.Errorf("HasMore 应为 true")
	}
}

// TestDiff_DetectsAddedRemovedChanged 验证 diff 三种差异类型。
func TestDiff_DetectsAddedRemovedChanged(t *testing.T) {
	t.Parallel()
	j := newFakeJournal()
	uc := observability.New(j, newFakeCostStore(), newFakeExperimentStore(), nil)
	ctx := context.Background()

	// baseline: 0, 1, 2
	for _, seq := range []int64{0, 1, 2} {
		_, _ = j.Append(ctx, makeSeqEvent("base", seq, runevent.KindModelTurn, `{"k":"v"}`))
	}
	// against: 0, 1(same), 2(different), 3(extra)
	_, _ = j.Append(ctx, makeSeqEvent("agn", 0, runevent.KindModelTurn, `{"k":"v"}`))
	_, _ = j.Append(ctx, makeSeqEvent("agn", 1, runevent.KindModelTurn, `{"k":"v"}`))
	_, _ = j.Append(ctx, makeSeqEvent("agn", 2, runevent.KindModelTurn, `{"k":"DIFFERENT"}`))
	_, _ = j.Append(ctx, makeSeqEvent("agn", 3, runevent.KindModelTurn, `{"k":"v"}`))

	got, err := uc.Diff(ctx, "base", "agn")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got.Added != 1 || got.Removed != 0 || got.Changed != 1 || got.Unchanged != 2 {
		t.Fatalf("差异计数不对：added=%d removed=%d changed=%d unchanged=%d",
			got.Added, got.Removed, got.Changed, got.Unchanged)
	}
	if len(got.Items) != 2 {
		t.Fatalf("期望 2 条差异项，实际 %d", len(got.Items))
	}
	for _, item := range got.Items {
		switch item.Seq {
		case 2:
			if item.Kind != observability.DiffChanged {
				t.Errorf("seq=2 应为 changed，实际 %v", item.Kind)
			}
		case 3:
			if item.Kind != observability.DiffAdded {
				t.Errorf("seq=3 应为 added，实际 %v", item.Kind)
			}
		}
	}
}

// TestCostOfRun_AggregatesByProvider 是 F4 闸门的关键测试：
// 未定价调用必须标 unpriced，不能静默按 0。
func TestCostOfRun_AggregatesByProvider(t *testing.T) {
	t.Parallel()
	cs := newFakeCostStore()
	uc := observability.New(newFakeJournal(), cs, newFakeExperimentStore(), nil)
	cs.add("r1", observability.CostEvent{
		EventID: "e1", RunID: "r1", Provider: "qwen", Model: "qwen3-max",
		TokensIn: 100, TokensOut: 50, CostMinor: 1, Currency: "CNY",
	})
	cs.add("r1", observability.CostEvent{
		EventID: "e2", RunID: "r1", Provider: "qwen", Model: "qwen3-max",
		TokensIn: 200, TokensOut: 80, Unpriced: true, Currency: "CNY",
	})
	cs.add("r1", observability.CostEvent{
		EventID: "e3", RunID: "r1", Provider: "anthropic", Model: "claude-sonnet-5-5",
		TokensIn: 50, TokensOut: 30, CostMinor: 1, Currency: "CNY",
	})

	got, err := uc.CostOfRun(context.Background(), "r1")
	if err != nil {
		t.Fatalf("CostOfRun: %v", err)
	}
	if got.TotalCalls != 3 {
		t.Errorf("TotalCalls 应为 3，实际 %d", got.TotalCalls)
	}
	if got.UnpricedCount != 1 {
		t.Errorf("UnpricedCount 应为 1（F4 闸门），实际 %d", got.UnpricedCount)
	}
	if got.TotalCostMinor != 2 {
		t.Errorf("已定价成本应为 2（qwen 1 + anthropic 1），实际 %d", got.TotalCostMinor)
	}
	if got.Currency != "CNY" {
		t.Errorf("主币种应为 CNY，实际 %q", got.Currency)
	}
	if len(got.ByProvider) != 2 {
		t.Errorf("按 provider 应有 2 条，实际 %d", len(got.ByProvider))
	}
}

// TestStubJudge_ReturnsNotImplemented 验证 StubJudge 默认返回未启用。
func TestStubJudge_ReturnsNotImplemented(t *testing.T) {
	t.Parallel()
	var j observability.Judge = observability.StubJudge{}
	res, err := j.Score(observability.JudgeRequest{})
	if !errors.Is(err, observability.ErrJudgeNotImplemented) {
		t.Fatalf("期望 ErrJudgeNotImplemented，实际 %v", err)
	}
	if !res.Unjudged {
		t.Errorf("Unjudged 应为 true")
	}
}

// TestExperimentArms_EmptyKeyRejected 验证空 key 直接拒绝。
func TestExperimentArms_EmptyKeyRejected(t *testing.T) {
	t.Parallel()
	uc := observability.New(newFakeJournal(), newFakeCostStore(), newFakeExperimentStore(), nil)
	_, err := uc.ExperimentArms(context.Background(), "")
	if err == nil {
		t.Fatal("空 key 应报错")
	}
}
