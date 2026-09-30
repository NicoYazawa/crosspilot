// Package orderflow 是把 Agent 编排结果与购物流衔接的应用层。
//
// 名称 orderflow 而非 trade：trade 是 P1 已有的「账本 + 下单 service」，
// orderflow 是「agent run → SSE 事件 + A2UI 报文 → journal」的衔接层。
// 它消费 orchestrator 事件、做格式转换、落 journal、对外广播 SSE。
//
// 这一层故意不依赖 trade service：P4 阶段目标是「端到端 SSE + journal 跑通」，
// trade 决议接入留 P7。
package orderflow

import (
	"context"
	"encoding/json"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/orchestrator"
	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
)

// OrchestratorRunner 抽象 orchestrator 的运行入口。
//
// 它与 agui.OrchestratorRunner 同名同语义——本包提供默认实现 (Bridge)，
// 让 SSE handler 通过 RunSubmitter 端口调用。
type OrchestratorRunner interface {
	Run(ctx context.Context, cfg RunnerConfig) ([]runevent.Event, error)
}

// RunnerConfig 是传给 OrchestratorRunner 的配置。
type RunnerConfig struct {
	RunID     string
	BuyerID   string
	SessionID string
	Query     string
	Agent     string
}

// Clock 提供当前时间。
type Clock interface {
	Now() time.Time
}

// Orchestrator 是 P3 orchestrator 的窄端口。
type Orchestrator interface {
	Run(ctx context.Context, cfg orchestrator.Config) (orchestrator.Result, error)
}

// Journal 与 agui.JournalStore 同语义；这里重新声明是因为应用层不应该 import
// presentation 包。
type Journal interface {
	Append(ctx context.Context, ev runevent.Event) (int64, error)
	Since(ctx context.Context, runID string, since int64, limit int) ([]runevent.Event, error)
}

// Bridge 把 orchestrator 事件转成 runevent 并写入 journal。
//
// 一次 Run 的流程：
//  1. 构造 Sequencer，从 seq=0 开始。
//  2. 调用 orchestrator.Run，传 OnEvent 回调：每条 orchestrator 事件都变成
//     RunEvent、Append 到 journal。
//  3. Run 完成后追加 KindRunFinished 哨兵事件。
//  4. 返回 journal 里全部 RunEvent 给 SSE handler，由它写给订阅者。
//
// 「先 journal 后 SSE」的写入顺序由本类强制——handler 不会绕过它去触发模型。
type Bridge struct {
	Orch Orchestrator
	J     Journal
	C     Clock
	// DefaultAgent 是默认 agent 名（不通过 RunnerConfig 指定时使用）。
	DefaultAgent string
}

// Run 同步驱动一次 agent run 并把全部事件落 journal。
//
// 返回 []Event 是「当前 journal 中从 seq=0 到本次 run 末的全部事件」。
func (b *Bridge) Run(ctx context.Context, cfg RunnerConfig) ([]runevent.Event, error) {
	seqr := runevent.NewSequencer(cfg.RunID)
	startAt := b.now()
	ev, err := seqr.Attach(seqr.Next(), runevent.KindRunStart, cfg.Agent,
		mustJSON(map[string]any{
			"buyer_id":   cfg.BuyerID,
			"session_id": cfg.SessionID,
			"query":      cfg.Query,
		}), startAt)
	if err != nil {
		return nil, err
	}
	if _, err := b.J.Append(ctx, ev); err != nil {
		return nil, err
	}

	agent := cfg.Agent
	if agent == "" {
		agent = b.DefaultAgent
	}
	_, err = b.Orch.Run(ctx, orchestrator.Config{
		SessionID:     cfg.SessionID,
		Query:         cfg.Query,
		Agent:         agent,
		MaxIterations: 8,
		OnEvent: func(oe orchestrator.Event) {
			b.forward(ctx, seqr, oe)
		},
	})

	finishKind := runevent.KindRunFinished
	if err != nil {
		finishKind = runevent.KindRunError
	}
	finishEv, _ := seqr.Attach(seqr.Next(), finishKind, agent,
		mustJSON(map[string]any{"error": errMsg(err)}), b.now())
	if _, err2 := b.J.Append(ctx, finishEv); err2 != nil && err == nil {
		err = err2
	}

	out, _ := b.J.Since(ctx, cfg.RunID, 0, 0)
	return out, err
}

func (b *Bridge) forward(ctx context.Context, seqr *runevent.Sequencer, oe orchestrator.Event) {
	payload, _ := json.Marshal(map[string]any{
		"agent":     oe.Agent,
		"content":   oe.Content,
		"tool_name": oe.ToolName,
		"iteration": oe.Iteration,
	})
	ev, err := seqr.Attach(seqr.Next(), runevent.Kind(oe.Kind), oe.Agent, payload, b.now())
	if err != nil {
		return
	}
	_, _ = b.J.Append(ctx, ev)
}

func (b *Bridge) now() time.Time {
	if b.C != nil {
		return b.C.Now().UTC()
	}
	return time.Now().UTC()
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}