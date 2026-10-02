package observability

import "errors"

// ErrExperimentNotFound 表示实验 key 未注册。
//
// 与「该 key 已注册但还没有 run 进入」是两回事：前者是运营把 key 拼错了，
// UI 应当提示「实验不存在」；后者是实验刚建好还没跑出数据，UI 应当显示
// 「暂无数据」。把两者合成一个「查不到」，前一种错误就永远得不到纠正。
//
// 定义放在 domain 而不是 application：判定它的是基础设施层的适配器，
// 而 infra 不允许依赖 application。application 侧以别名引用同一个值，
// errors.Is 因此照常成立。
var ErrExperimentNotFound = errors.New("observability: 实验 key 未注册")
