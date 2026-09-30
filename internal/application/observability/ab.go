// A/B 框架用例：实验臂聚合与查询。
//
// D6 决策：A/B 分组由 ExperimentStore 决定，本用例只做聚合查询。
// LLM-judge 留接口位（Judge interface + StubJudge），等 judge 模型就绪后再实跑。
// F8 实测需外部 judge 模型 + 标注集 → 留缺口。
package observability

import (
	"context"
	"errors"
)

// ErrExperimentNotFound 表示 key 不存在。
//
// 与「该 key 没有 run」的区别：NotFound 表示 key 未注册；Empty 表示注册了但
// 还没有 run 进入。两种情况下返回的 ArmSummary 切片都是空，错误用于让上层区分。
var ErrExperimentNotFound = errors.New("observability: 实验 key 未注册")

// ExperimentArms 把某个实验的各臂聚合返回给面板。
//
// 不存在的实验 → 返回空切片与 nil 错误（"该 key 没有 run" 合法）。
// 未注册的 key → 返回 ErrExperimentNotFound。
//
// 这是 F8「实验未注册与实验无数据是两个不同错误」的物理保证——运营把 key
// 拼错时 UI 应能识别"是不是没创建实验"，不能只显示"暂无数据"。
func (uc *UseCases) ExperimentArms(ctx context.Context, key string) ([]ArmSummary, error) {
	if key == "" {
		return nil, errors.New("observability: 实验 key 不能为空")
	}
	return uc.Experiments.ArmsSummary(ctx, key)
}

// ArmForRun 查某 run 所属的实验臂。
//
// run 未注册到任何实验 → 返回 ("", false)，无错误；这与 ExperimentArms 的策略一致。
func (uc *UseCases) ArmForRun(ctx context.Context, runID string) (string, bool, error) {
	return uc.Experiments.ArmFor(ctx, runID)
}
