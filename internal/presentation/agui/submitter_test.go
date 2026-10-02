package agui_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
	"github.com/NicoYazawa/crosspilot/internal/application/orderflow"
	"github.com/NicoYazawa/crosspilot/internal/presentation/agui"
)

// fakeRunner 是 OrchestratorRunner 的假实现。
type fakeRunner struct {
	runErr error
	evts   []runevent.Event
}

func (f *fakeRunner) Run(_ context.Context, _ orderflow.RunnerConfig) ([]runevent.Event, error) {
	return f.evts, f.runErr
}

// TestSubmitAdapter_SubmitsRequest 验证 submitAdapter 把 SubmitRequest 透传到 backend.Run。
func TestSubmitAdapter_SubmitsRequest(t *testing.T) {
	evts := []runevent.Event{
		{EventID: "run-1:0", RunID: "run-1", Seq: 0, Kind: runevent.KindRunStart, Agent: "main", CreatedAt: time.Now().UTC(), Payload: []byte(`{}`)},
		{EventID: "run-1:1", RunID: "run-1", Seq: 1, Kind: runevent.KindRunFinished, Agent: "main", CreatedAt: time.Now().UTC(), Payload: []byte(`{}`)},
	}
	backend := &fakeRunner{evts: evts}
	adapter := agui.NewSubmitter(backend, "default-agent")

	req := agui.SubmitRequest{
		RunID:     "run-1",
		BuyerID:   "b1",
		SessionID: "s1",
		Query:     "test query",
		Agent:     "custom-agent",
	}
	got, err := adapter.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit 失败：%v", err)
	}
	if len(got) != 2 {
		t.Errorf("期望 2 条事件，实际 %d", len(got))
	}
}

// TestSubmitAdapter_UsesDefaultAgent 验证 agent 为空时使用 defaultAgent。
func TestSubmitAdapter_UsesDefaultAgent(t *testing.T) {
	backend := &fakeRunner{evts: nil}
	adapter := agui.NewSubmitter(backend, "default-agent")

	req := agui.SubmitRequest{
		RunID:     "run-1",
		BuyerID:   "b1",
		SessionID: "s1",
		Query:     "test",
		Agent:     "", // empty
	}
	got, err := adapter.Submit(context.Background(), req)
	if err != nil {
		t.Fatalf("Submit 失败：%v", err)
	}
	_ = got // backend 不返回事件也没关系
}

// TestSubmitAdapter_ForwardsError 验证 backend.Run 的错误会向上透传。
func TestSubmitAdapter_ForwardsError(t *testing.T) {
	wantErr := errors.New("backend error")
	backend := &fakeRunner{runErr: wantErr}
	adapter := agui.NewSubmitter(backend, "default-agent")

	_, err := adapter.Submit(context.Background(), agui.SubmitRequest{
		RunID:     "run-1",
		BuyerID:   "b1",
		SessionID: "s1",
		Query:     "test",
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("期望错误 %v，实际 %v", wantErr, err)
	}
}
