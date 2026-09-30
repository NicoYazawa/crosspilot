// UseCases 是 P5 可观测用例的统一入口。
//
// 把所有用例（Replay / Diff / Cost / A/B）挂在同一结构上，是为了在容器装配
// 时只注入一次（同一个 Journal、CostStore、ExperimentStore）。
//
// 单方法用例比单独的结构体更适合本场景：
//   - 它们共享同一组依赖
//   - HTTP 处理器把 uc 整个注入，按需调方法
//   - 测试时构造一个 usecases 替身就能替代全部用例
package observability

// UseCases 是所有 P5 用例的载体。
type UseCases struct {
	Journal     JournalStore
	Cost        CostStore
	Experiments ExperimentStore
	Clock       Clock
}

// New 构造用例入口。Clock 为 nil 时使用 wallClock。
func New(journal JournalStore, cost CostStore, exp ExperimentStore, clock Clock) *UseCases {
	if clock == nil {
		clock = wallClock{}
	}
	return &UseCases{
		Journal:     journal,
		Cost:        cost,
		Experiments: exp,
		Clock:       clock,
	}
}
