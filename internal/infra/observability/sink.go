// Package observability 提供可观测三件套的基础设施：脱敏器、原始 Sink 实现。
//
// 关键不变量（跨整个 P5 阶段共用）：
//   - 脱敏在写路径上，不在读路径上（F7 闸门）
//   - Sink 写入失败绝不影响交易事务（F9 闸门）
//   - 队列满时丢弃并计数，绝不阻塞 Agent（F6 闸门）
//
// 本文件保留：未来 Postgres sink 实现（sink_pg.go）的占位 + 类型别名。
// 当前阶段：MemorySink 在 agent/observability 包内（与 Sink 接口同侧），
// Postgres sink 留待 P5 收尾阶段以适配器形态接入。
//
// 本包唯一对外暴露的是 Redactor（脱敏器）的具体实现，它满足 agent/observability
// 包里的 Redactor 端口。
package observability

// 本包当前不导出任何类型（Redactor 在 redact.go 里）。
// 保留 sink.go 文件名是为了后续 Postgres sink 实现有固定入口；
// 现阶段是占位，编译期什么都不挂。
var _ = struct{}{}
