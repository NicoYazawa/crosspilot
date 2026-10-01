// Judge 接口与 Stub 实现：LLM-judge 的入口位。
//
// F8「LLM-judge κ ≥ 0.7 才启用」是接口契约。本文件留接口位与 StubJudge，
// 实现在 judge 模型就绪后接入（位置对调、异源模型判定一起做）。
//
// StubJudge 行为：所有调用返回 Score=0 与 ErrNotImplemented，由调用方决定
// 如何处理「未就绪」——前端面板直接隐藏 judge 列；后端报表只统计未 judge 的样本数。
package observability

import "errors"

// ErrJudgeNotImplemented 表示 judge 模型尚未接入。
//
// 调用方收到本错误应：
//   - 前端面板：隐藏 judge 列，展示"未启用"
//   - 后端报表：标记为 unjudged，不参与 κ 统计
//   - 实验分析：不使用 judge 分数，仅用 arm × calls × latency × cost
var ErrJudgeNotImplemented = errors.New("observability: judge 模型尚未接入（F8 待外部资源）")

// JudgeRequest 是 judge 的入参。
//
// 字段命名遵循「给 judge 看的内容是已脱敏的」原则——Judge 输入与 Emitter
// 写入的 SinkRecord 共享脱敏器，避免泄漏 PII 到 judge 供应商。
type JudgeRequest struct {
	RunID       string
	BaselineSeq int64
	AgainstSeq  int64
	// 两侧事件 payload（已脱敏）；由 caller 传入
	BaselinePayload []byte
	AgainstPayload  []byte
}

// JudgeResult 是 judge 的返回值。
type JudgeResult struct {
	Score    float64 // 0–1；1 表示完全一致，0 表示完全不一致
	Reason   string  // 模型给的文字解释（用于审计）
	Unjudged bool    // true 表示 judge 失败/未启用，UI 应降级
}

// Judge 是 LLM-judge 的端口。
//
// 设计为 interface 是为了让实现可替换（位置对调 / 异源模型对照）——单元测试
// 注入假 Judge，验证「接口契约」而非「真实 judge 行为」。
type Judge interface {
	// Score 给两条事件打分。
	//
	// 返回 ErrJudgeNotImplemented 时 caller 应跳过该样本（不计入 κ 统计）。
	Score(req JudgeRequest) (JudgeResult, error)
}

// StubJudge 是 Judge 的 no-op 实现。
//
// 所有调用返回 Score=0, Unjudged=true, Err=ErrJudgeNotImplemented。
// 用于「judge 未接入时」的面板降级；上线后替换为真实 judge 实现。
type StubJudge struct{}

// Score 实现 Judge 接口。
func (StubJudge) Score(_ JudgeRequest) (JudgeResult, error) {
	return JudgeResult{Score: 0, Unjudged: true}, ErrJudgeNotImplemented
}
