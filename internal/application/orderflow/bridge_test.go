package orderflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	agentobs "github.com/NicoYazawa/crosspilot/internal/agent/observability"
	"github.com/NicoYazawa/crosspilot/internal/agent/orchestrator"
	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
	"github.com/NicoYazawa/crosspilot/internal/application/orderflow"
)

// noopRedactor 是测试用脱敏器，原样返回。
type noopRedactor struct{}

func (noopRedactor) Apply(in []byte) ([]byte, error) { return in, nil }

// recSink 是测试用 sink：把 Append 的所有记录都保存到内存中。
//
// 实现 agent/observability.Sink 接口，避免 application 层 import infra/observability。
type recSink struct {
	mu      sync.Mutex
	records []agentobs.SinkRecord
}

func (s *recSink) Append(_ context.Context, batch []agentobs.SinkRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, batch...)
	return nil
}

func (s *recSink) Close() error { return nil }

func (s *recSink) Records() []agentobs.SinkRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]agentobs.SinkRecord, len(s.records))
	copy(out, s.records)
	return out
}

// fakeOrch 模拟 P3 orchestrator，按 Run 调用次数返回预定义事件。
type fakeOrch struct {
	mu     sync.Mutex
	runErr error
	events []orchestrator.Event
	called int
}

func (f *fakeOrch) Run(_ context.Context, cfg orchestrator.Config) (orchestrator.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	for _, ev := range f.events {
		if cfg.OnEvent != nil {
			cfg.OnEvent(ev)
		}
	}
	if f.runErr != nil {
		return orchestrator.Result{}, f.runErr
	}
	return orchestrator.Result{
		Transcript: []orchestrator.Turn{{Content: "done"}},
		FinalText:  "done",
	}, nil
}

// fakeJournal 记录所有 Append，按 Since 返回事件。
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

func (j *fakeJournal) Since(_ context.Context, runID string, since int64, _ int) ([]runevent.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	src := j.events[runID]
	out := make([]runevent.Event, 0, len(src))
	for _, ev := range src {
		if ev.Seq >= since {
			out = append(out, ev)
		}
	}
	return out, nil
}

// TestBridge_WritesRunStartAndFinished 验证 Bridge 至少写 run_start + run_finished 两条事件。
func TestBridge_WritesRunStartAndFinished(t *testing.T) {
	o := &fakeOrch{}
	j := newFakeJournal()
	b := &orderflow.Bridge{Orch: o, J: j, DefaultAgent: "main"}

	events, err := b.Run(context.Background(), orderflow.RunnerConfig{
		RunID: "r1", SessionID: "s1", Query: "test", Agent: "main",
	})
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	if o.called != 1 {
		t.Errorf("orchestrator 应被调用 1 次，实际 %d", o.called)
	}
	if len(events) < 2 {
		t.Fatalf("事件序列至少 2 条（run_start + run_finished），实际 %d", len(events))
	}
	if events[0].Kind != runevent.KindRunStart {
		t.Errorf("首事件应为 run_start，实际 %v", events[0].Kind)
	}
	last := events[len(events)-1]
	if last.Kind != runevent.KindRunFinished {
		t.Errorf("末事件应为 run_finished，实际 %v", last.Kind)
	}
}

// TestBridge_PropagatesOrchestratorError 验证 orchestrator 报错时，Bridge 落 run_error 事件。
func TestBridge_PropagatesOrchestratorError(t *testing.T) {
	o := &fakeOrch{runErr: errors.New("模型超时")}
	j := newFakeJournal()
	b := &orderflow.Bridge{Orch: o, J: j, DefaultAgent: "main"}

	_, err := b.Run(context.Background(), orderflow.RunnerConfig{
		RunID: "r2", SessionID: "s2", Query: "test",
	})
	if err == nil {
		t.Fatal("Run 应返回错误")
	}

	evs, _ := j.Since(context.Background(), "r2", 0, 0)
	if len(evs) == 0 {
		t.Fatal("journal 中应有事件")
	}
	last := evs[len(evs)-1]
	if last.Kind != runevent.KindRunError {
		t.Errorf("末事件应为 run_error，实际 %v", last.Kind)
	}
	// payload 应携带错误信息
	var payload map[string]any
	_ = json.Unmarshal(last.Payload, &payload)
	if payload["error"] != "模型超时" {
		t.Errorf("payload.error 应为错误描述，实际 %v", payload["error"])
	}
}

// TestBridge_ForwardsOrchestratorEvents 验证 orchestrator 事件被原样写为 RunEvent。
func TestBridge_ForwardsOrchestratorEvents(t *testing.T) {
	o := &fakeOrch{
		events: []orchestrator.Event{
			{Kind: orchestrator.KindModelTurn, Agent: "main", Content: "thinking"},
			{Kind: orchestrator.KindToolCall, Agent: "main", ToolName: "search"},
		},
	}
	j := newFakeJournal()
	b := &orderflow.Bridge{Orch: o, J: j, DefaultAgent: "main"}

	_, err := b.Run(context.Background(), orderflow.RunnerConfig{
		RunID: "r3", SessionID: "s3", Query: "test",
	})
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	evs, _ := j.Since(context.Background(), "r3", 0, 0)
	// 期望：run_start, model_turn, tool_call, run_finished
	if len(evs) != 4 {
		t.Fatalf("期望 4 条，实际 %d", len(evs))
	}
	if evs[1].Kind != runevent.KindModelTurn {
		t.Errorf("第 2 条应为 model_turn，实际 %v", evs[1].Kind)
	}
	if evs[2].Kind != runevent.KindToolCall {
		t.Errorf("第 3 条应为 tool_call，实际 %v", evs[2].Kind)
	}
}

// TestBridge_EmitterReceivesEveryJournalEvent 是 P5 桥接测试：
// 每条 Append 的事件都必须**额外**进 Emitter。
func TestBridge_EmitterReceivesEveryJournalEvent(t *testing.T) {
	o := &fakeOrch{
		events: []orchestrator.Event{
			{Kind: orchestrator.KindModelTurn, Agent: "main", Content: "thinking"},
			{Kind: orchestrator.KindToolCall, Agent: "main", ToolName: "search"},
		},
	}
	j := newFakeJournal()
	sink := &recSink{}
	emitter := agentobs.NewEmitter(sink, agentobs.EmitterConfig{
		QueueSize:  64,
		BatchSize:  1,
		FlushEvery: 50 * time.Millisecond,
		Redactor:   noopRedactor{},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go emitter.Run(ctx)

	b := &orderflow.Bridge{
		Orch: o, J: j, DefaultAgent: "main",
		Emitter: emitter,
	}

	_, err := b.Run(context.Background(), orderflow.RunnerConfig{
		RunID: "r-emit", SessionID: "s", Query: "test",
	})
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	// 等所有事件落 sink
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.Records()) >= 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	recs := sink.Records()
	// 期望：run_start, model_turn, tool_call, run_finished —— 与 journal 一致
	if len(recs) != 4 {
		t.Fatalf("Emitter 应收到 4 条，实际 %d", len(recs))
	}
	wantSeqs := []int64{0, 1, 2, 3}
	for i, rec := range recs {
		if rec.Seq != wantSeqs[i] {
			t.Errorf("第 %d 条 seq 应为 %d，实际 %d", i, wantSeqs[i], rec.Seq)
		}
		if rec.RunID != "r-emit" {
			t.Errorf("第 %d 条 run_id 不对，实际 %q", i, rec.RunID)
		}
	}
}

// TestBridge_NilEmitterIsNoop 验证 Emitter 为 nil 时 Bridge 仍能正常工作
// （单测兼容，不强制依赖 Emitter）。
func TestBridge_NilEmitterIsNoop(t *testing.T) {
	o := &fakeOrch{}
	j := newFakeJournal()
	b := &orderflow.Bridge{Orch: o, J: j, DefaultAgent: "main"} // 无 Emitter

	_, err := b.Run(context.Background(), orderflow.RunnerConfig{
		RunID: "r-noemit", SessionID: "s", Query: "test",
	})
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	evs, _ := j.Since(context.Background(), "r-noemit", 0, 0)
	if len(evs) < 2 {
		t.Fatalf("无 Emitter 时 Bridge 仍应落 journal，实际 %d 条", len(evs))
	}
}
