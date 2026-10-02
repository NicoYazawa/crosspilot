// Package orderflow 是把 Agent 编排结果与购物流衔接的应用层。
//
// 名称 orderflow 而非 trade：trade 是 P1 已有的「账本 + 下单 service」，
// orderflow 是「agent run → SSE 事件 + A2UI 报文 → journal」的衔接层。
// 它消费 orchestrator 事件、做格式转换、落 journal、对外广播 SSE。
//
// 这一层故意不依赖 trade service：P4 阶段目标是「端到端 SSE + journal 跑通」，
// trade 决议接入留 P7。
//
// P5 阶段新增：Bridge 每 Append 一条事件到 journal 后，**额外**调一次
// Emitter.Emit(&ev)，把事件送进观测通道。Emitter 是可选字段——单测可以不装。
// 这条路径不影响 P4 E1–E6 的任何验收（journal 写仍同步完成）。
package orderflow

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/observability"
	"github.com/NicoYazawa/crosspilot/internal/agent/orchestrator"
	"github.com/NicoYazawa/crosspilot/internal/agent/protocol"
	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
	"github.com/NicoYazawa/crosspilot/internal/agent/tools"
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
// 除编排外它还维护「在跑的 run」:每个 run 的 context 由 Bridge 派生并登记，
// 使 POST /runs/{id}/cancel 能真正中断一次正在进行的推理，而不是只写一条
// 事件假装取消。取消是运维手段（跑飞的循环、用户关掉页面），必须能让底层
// 工作停下——只改状态位的「取消」会让 token 继续烧。
//
// 一次 Run 的流程：
//  1. 构造 Sequencer，从 seq=0 开始。
//  2. 调用 orchestrator.Run，传 OnEvent 回调：每条 orchestrator 事件都变成
//     RunEvent、Append 到 journal。
//  3. Run 完成后追加 KindRunFinished 哨兵事件。
//  4. 返回 journal 里全部 RunEvent 给 SSE handler，由它写给订阅者。
//
// 「先 journal 后 SSE」的写入顺序由本类强制——handler 不会绕过它去触发模型。
// 「先 journal 后 Emit」：P5 阶段新增——Append 成功后**额外**调用 Emitter.Emit，
// 把事件送进观测通道。Emitter 自身保证不阻塞（队列满即 drop）。
type Bridge struct {
	Orch Orchestrator
	J    Journal
	C    Clock
	// DefaultAgent 是默认 agent 名（不通过 RunnerConfig 指定时使用）。
	DefaultAgent string
	// Emitter 是可选的观测通道发射器。nil 时 Bridge 不发观测事件（兼容单测）。
	// 装上后每次 Append 成功后调用一次 Emit(&ev)。
	Emitter *observability.Emitter

	// running 登记在跑的 run → 取消函数。
	//
	// 用 sync.Map 而不是 map+Mutex：Bridge 在测试里是按结构体字面量构造的
	// （零值可用），sync.Map 的零值本来就是可用的，map 需要在每个入口做
	// 「为 nil 就初始化」的判断——那种判断漏一处就是运行期 panic。
	running sync.Map
}

// Cancel 中断一个正在跑的 run。
//
// 返回 false 表示该 run 不在跑：可能已经结束，也可能从未存在。两种情况对
// 调用方是同一件事（没什么可取消的），因此不区分——真正需要区分的是
// 「没找到」与「找到了但取消失败」，而后者在本实现里不存在。
func (b *Bridge) Cancel(runID string) bool {
	v, ok := b.running.Load(runID)
	if !ok {
		return false
	}
	v.(context.CancelFunc)()
	return true
}

// Run 同步驱动一次 agent run 并把全部事件落 journal。
//
// 返回 []Event 是「当前 journal 中从 seq=0 到本次 run 末的全部事件」。
func (b *Bridge) Run(ctx context.Context, cfg RunnerConfig) ([]runevent.Event, error) {
	// 派生可取消的 context 并登记，让 Cancel 能真正打断推理循环。
	// defer 注销是必须的：不注销的话同一个 run_id 被复用时会取消到已经
	// 结束的旧 ctx，而新的这次继续跑——「取消没生效」最难查的一种形态。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if cfg.RunID != "" {
		b.running.Store(cfg.RunID, cancel)
		defer b.running.Delete(cfg.RunID)
	}

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
	b.observe(ev)

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
	b.observe(finishEv)

	out, _ := b.J.Since(ctx, cfg.RunID, 0, 0)
	return out, err
}

// ModelTurnPayload 是事件 payload 的形态。
//
// 用结构体而不是就地手拼 map：成本归因要**读回**这个 payload（解析 usage /
// provider / model 去查价格表），写侧与读侧如果各写一份键名，加字段时必有一侧
// 静默漏掉——那正是「用量采到了却算不出成本」最可能的成因。共用一份定义，
// 编译器替我们保证两侧一致。
//
// 前四个字段不带 omitempty：它们在改造前由 map 恒定写出（值为空串也写），
// 加上 omitempty 会悄悄改变既有事件的线上字节。
type ModelTurnPayload struct {
	Agent     string `json:"agent"`
	Content   string `json:"content"`
	ToolName  string `json:"tool_name"`
	Iteration int    `json:"iteration"`
	// 以下三个只对 model_turn 有值；其余事件不写这三个键。
	Provider string          `json:"provider,omitempty"`
	Model    string          `json:"model,omitempty"`
	Usage    *protocol.Usage `json:"usage,omitempty"`
}

func (b *Bridge) forward(ctx context.Context, seqr *runevent.Sequencer, oe orchestrator.Event) {
	payload, err := json.Marshal(ModelTurnPayload{
		Agent:     oe.Agent,
		Content:   oe.Content,
		ToolName:  oe.ToolName,
		Iteration: oe.Iteration,
		Provider:  oe.Provider,
		Model:     oe.Model,
		Usage:     oe.Usage,
	})
	if err != nil {
		// 与改造前的 `payload, _ :=` 不同：序列化失败就整条丢弃，而不是落一个
		// 空 payload 进 journal——空 payload 会被下游当成「这条事件本来就没内容」。
		return
	}
	ev, err := seqr.Attach(seqr.Next(), runevent.Kind(oe.Kind), oe.Agent, payload, b.now())
	if err != nil {
		return
	}
	if _, err := b.J.Append(ctx, ev); err != nil {
		return
	}
	b.observe(ev)

	b.forwardSearchA2UI(ctx, seqr, oe)
}

// forwardSearchA2UI 在商品检索返回后补发一条 A2UI 报文。
//
// 为什么在 Bridge 做而不是让 handler 直接返回报文：handler 是业务的适配器，
// 只该回答「检索到了什么」；「怎么画」是展示层的事。让工具返回 UI 报文会把
// 渲染契约焊进业务层，之后换前端就得改工具。
//
// 报文生成失败只跳过、不报错：卡片画不出来不该让整轮对话失败，
// 模型给买家的文字回复与工具结果都已经在 journal 里了。
func (b *Bridge) forwardSearchA2UI(
	ctx context.Context,
	seqr *runevent.Sequencer,
	oe orchestrator.Event,
) {
	if oe.Kind != orchestrator.KindToolResult || oe.ToolName != tools.ToolProductSearch {
		return
	}

	// 失败态的工具结果 content 是 "error: ..."，解析必然失败——直接跳过。
	hits, err := HitsFromToolResult([]byte(oe.Content))
	if err != nil || len(hits) == 0 {
		return
	}

	msgs, err := NewSearchEmitter().Emit(hits)
	if err != nil || len(msgs) == 0 {
		return
	}

	// 一条报文一个事件，不打包成 {"messages":[...]}。
	//
	// 前端的 A2UIRenderer 对每个事件载荷直接跑 Zod 的 discriminatedUnion
	// ('action') 校验——载荷必须**就是**一条报文，有顶层 action。包一层
	// messages 数组会被判成「A2UI 报文不合规」并整块渲染失败，而且失败原因
	// 指向前端 schema，看不出是后端多发了一层壳。
	for _, msg := range msgs {
		payload, err := json.Marshal(msg)
		if err != nil {
			return
		}
		ev, err := seqr.Attach(seqr.Next(), runevent.KindA2UI, oe.Agent, payload, b.now())
		if err != nil {
			return
		}
		if _, err := b.J.Append(ctx, ev); err != nil {
			return
		}
		b.observe(ev)
	}
}

// observe 把事件送进观测通道。Emitter 为 nil 时是 no-op（单测兼容）。
//
// 注意：这里是同步调用 Emitter.Emit；Emitter 内部保证「同步只塞 channel，
// 绝不阻塞」（F6 闸门），所以 Bridge 不需要异步处理。
func (b *Bridge) observe(ev runevent.Event) {
	if b.Emitter == nil {
		return
	}
	b.Emitter.Emit(&ev)
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
