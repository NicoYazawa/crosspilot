package orderflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/orchestrator"
	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
	"github.com/NicoYazawa/crosspilot/internal/application/orderflow"
)

// blockingOrch 是一个「会一直跑、直到 ctx 被取消才返回」的假 orchestrator。
//
// 它刻意不自己伪造结束：只有真正观察到 ctx.Done() 才会 return。这样一来，
// 如果 Bridge.Cancel 没调用登记在表里的 CancelFunc，Run 就会永远挂住——
// 这正是「取消是否真的生效」唯一可靠的观测点。
type blockingOrch struct {
	started chan struct{} // Run 进入后关闭，供测试同步「run 已开始」

	// observedCtxErr 是 Run 观察到的 ctx.Err()，在 <-ctx.Done() 之后写入。
	// 它由 channel（done）建立 happens-before，读侧无需加锁。
	observedCtxErr error
}

func (o *blockingOrch) Run(ctx context.Context, _ orchestrator.Config) (orchestrator.Result, error) {
	close(o.started) // 只会被调用一次（一个 run 一个 orch 实例）
	<-ctx.Done()
	o.observedCtxErr = ctx.Err()
	return orchestrator.Result{}, ctx.Err()
}

// TestBridge_Cancel_UnknownRunIDReturnsFalse 验证未登记的 runID 返回 false。
func TestBridge_Cancel_UnknownRunIDReturnsFalse(t *testing.T) {
	b := &orderflow.Bridge{J: newFakeJournal(), DefaultAgent: "main"}

	if b.Cancel("从未存在的-run") {
		t.Fatal("对未登记的 runID，Cancel 应返回 false")
	}
}

// TestBridge_Cancel_InterruptsRunningRun 是本任务的核心用例：
// Cancel 必须真正打断一次正在跑的推理，而不是只改个状态位返回 true。
//
// 断言的是可观测后果：Run 确实返回了、底层 context 真的被取消、journal 里
// 落的是 run_error（而不是因为挂住而什么都没写）。
func TestBridge_Cancel_InterruptsRunningRun(t *testing.T) {
	o := &blockingOrch{started: make(chan struct{})}
	j := newFakeJournal()
	b := &orderflow.Bridge{Orch: o, J: j, DefaultAgent: "main"}

	type runResult struct {
		evs []runevent.Event
		err error
	}
	done := make(chan runResult, 1)

	go func() {
		evs, err := b.Run(context.Background(), orderflow.RunnerConfig{
			RunID: "r-cancel", SessionID: "s", Query: "买登山包",
		})
		done <- runResult{evs: evs, err: err}
	}()

	// 等 orchestrator 真正进入 Run（此时 running 表里已登记了 CancelFunc）。
	select {
	case <-o.started:
	case <-time.After(2 * time.Second):
		t.Fatal("orchestrator 在 2s 内未启动，测试前置条件不成立")
	}

	// Cancel 之前，Run 必须还在挂着——否则这个用例根本没在测「打断」。
	select {
	case <-done:
		t.Fatal("Cancel 之前 Run 不应返回")
	case <-time.After(20 * time.Millisecond):
	}

	if !b.Cancel("r-cancel") {
		t.Fatal("对正在跑的 run，Cancel 应返回 true")
	}

	select {
	case r := <-done:
		// 1) 底层 context 真的被取消了：orchestrator 观察到的就是 context.Canceled
		if !errors.Is(o.observedCtxErr, context.Canceled) {
			t.Errorf("orchestrator 观察到的 ctx.Err() 应为 context.Canceled，实际 %v", o.observedCtxErr)
		}
		// 2) Run 返回了错误（取消导致的失败），不是超时挂死
		if !errors.Is(r.err, context.Canceled) {
			t.Errorf("Run 应返回 context.Canceled，实际 %v", r.err)
		}
		// 3) journal 里落的是 run_error，而不是一直挂着什么都没写
		evs, _ := j.Since(context.Background(), "r-cancel", 0, 0)
		if len(evs) < 2 {
			t.Fatalf("journal 应有 run_start + run_error，实际 %d 条", len(evs))
		}
		if evs[0].Kind != runevent.KindRunStart {
			t.Errorf("首事件应为 run_start，实际 %v", evs[0].Kind)
		}
		last := evs[len(evs)-1]
		if last.Kind != runevent.KindRunError {
			t.Errorf("末事件应为 run_error（被取消），实际 %v", last.Kind)
		}
		var payload map[string]any
		if err := json.Unmarshal(last.Payload, &payload); err != nil {
			t.Fatalf("run_error payload 无法解析：%v", err)
		}
		if msg, _ := payload["error"].(string); !strings.Contains(msg, "canceled") {
			t.Errorf("run_error payload.error 应携带取消原因，实际 %q", msg)
		}
	case <-time.After(2 * time.Second):
		// 这一分支正是「实现被改坏」时会命中的地方：Cancel 返回了 true，
		// 但底层 ctx 从未被取消，Run 永远卡在 <-ctx.Done() 上。
		t.Fatal("Cancel 未真正中断 Run：Run 一直挂着未返回")
	}
}

// TestBridge_Cancel_AfterRunCompletesReturnsFalse 验证 Run 结束后（defer 注销那条路径）
// 对同一 runID 再 Cancel 返回 false——否则会取消到一个已结束的旧 ctx。
func TestBridge_Cancel_AfterRunCompletesReturnsFalse(t *testing.T) {
	o := &fakeOrch{}
	j := newFakeJournal()
	b := &orderflow.Bridge{Orch: o, J: j, DefaultAgent: "main"}

	_, err := b.Run(context.Background(), orderflow.RunnerConfig{
		RunID: "r-done", SessionID: "s", Query: "test",
	})
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	if b.Cancel("r-done") {
		t.Fatal("Run 已结束后，对同一 runID 再 Cancel 应返回 false")
	}
}

// TestBridge_RunStartAppendFails 覆盖 Run 中 run_start 落 journal 失败的分支：
// 此时应立即返回错误、不调用 orchestrator、journal 里不残留任何事件。
func TestBridge_RunStartAppendFails(t *testing.T) {
	o := &fakeOrch{}
	// failFrom=0：第 1 次 Append（run_start）即失败。
	j := newPartialFailJournal(0)
	b := &orderflow.Bridge{Orch: o, J: j, DefaultAgent: "main"}

	evs, err := b.Run(context.Background(), orderflow.RunnerConfig{
		RunID: "r-start-fail", SessionID: "s", Query: "test",
	})
	if err == nil {
		t.Fatal("run_start 落 journal 失败时 Run 应返回错误")
	}
	if evs != nil {
		t.Errorf("run_start 写入失败时应返回 nil 事件，实际 %d 条", len(evs))
	}
	if o.called != 0 {
		t.Errorf("run_start 写入失败后不应调用 orchestrator，实际调用 %d 次", o.called)
	}
	j.mu.Lock()
	written := len(j.events["r-start-fail"])
	j.mu.Unlock()
	if written != 0 {
		t.Errorf("run_start 写入失败时 journal 不应有记录，实际 %d 条", written)
	}
}

// TestBridge_Forward_AppendErrorSilent 覆盖 forward 中 J.Append 出错的静默 return 分支：
// 该条 orchestrator 事件不应落进 journal，且不 panic、Run 仍能继续走到收尾。
func TestBridge_Forward_AppendErrorSilent(t *testing.T) {
	o := &fakeOrch{
		events: []orchestrator.Event{
			{Kind: orchestrator.KindModelTurn, Agent: "main", Content: "thinking"},
		},
	}
	// failFrom=1：run_start(第 1 次)成功；forward 的 model_turn(第 2 次)失败 → 静默 return；
	// run_finished(第 3 次)也失败 → 覆盖 err。
	j := newPartialFailJournal(1)
	b := &orderflow.Bridge{Orch: o, J: j, DefaultAgent: "main"}

	evs, err := b.Run(context.Background(), orderflow.RunnerConfig{
		RunID: "r-fwd-fail", SessionID: "s", Query: "test",
	})
	// run_finished 的 Append 失败且 orchestrator 无错，err 被覆盖为非 nil。
	if err == nil {
		t.Fatal("run_finished 写入失败时 Run 应返回错误")
	}
	// 关键断言：看 journal 里真正落下的内容——只有 run_start，
	// 失败的那条 model_turn 事件绝不能出现在 journal 里。
	if len(evs) != 1 {
		t.Fatalf("journal 应只有 run_start 一条，实际 %d 条", len(evs))
	}
	if evs[0].Kind != runevent.KindRunStart {
		t.Errorf("唯一的事件应为 run_start，实际 %v", evs[0].Kind)
	}
	for _, ev := range evs {
		if ev.Kind == runevent.KindModelTurn {
			t.Fatal("Append 失败的 model_turn 事件不应出现在 journal 里")
		}
	}
}
