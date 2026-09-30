# P3：Agent 编排

日期：2026-09-30
范围：Agent 三层（MainAgent/SearchAgent/TradeAgent）、ShoppingContext 显式化、中间件链（Harness 外 + Resilience 内）、熔断器、ReAct 循环、`task_dispatch` 派发隔离。

对应 `docs/开发计划-核心链路与三件套.md` 中 P3 的验收项 D1–D4。

---

## 交付物

### 包结构

```
internal/agent/
├── breaker/           # 熔断器（区分瞬时 vs 业务错误）
├── fakemodel/         # 测试用脚本式假模型
├── middleware/        # Harness(外) → Resilience(内) 中间件链
├── orchestrator/      # ReAct 主循环 + 事件流
├── shopping/          # ShoppingContext 显式化（替代 Python 的 contextvars）
└── tools/             # 14 个工具 JSON Schema + Executor 运行时
```

---

## 验收

| # | 指标 | 状态 | 备注 |
|---|------|------|------|
| D1 | 端到端跑通 | ✅ | `TestD1_EndToEnd_PlaceOrder`：search → create_order → exit 全程流转 |
| D1 | 未注册工具不崩溃 | ✅ | `TestD1_MissingTool_GracefullyHandled`：未知工具名走 `ErrUnknownTool` 路径，不 panic |
| D2 | 派发隔离 | ✅ | `TestD2_TaskDispatch_Isolation`：3 个并发派发各自维护 prompt，互不污染 |
| D3 | 中间件顺序 | ✅ | `TestD3_AssertionRejectionBeforeTimeout`：断言先于超时；`TestD3_TimeoutDoesNotPolluteBreaker`：超时绕开熔断计数 |
| D3 | 顺序固定 | ✅ | `TestD3_OrderIsHarnessOuterResilienceInner`：handler 看到 Harness 设置的 deadline |
| D4 | 熔断只计瞬时 | ✅ | `TestBreaker_D4_DoesNotCountBusinessErrors`：10 次业务错误不推高熔断计数 |

### 关键设计决策

1. **熔断器分类器保守化**：默认 `IsClosedError` 仅识别 `Timeout() bool` / `Temporary() bool` / `ErrTransient` sentinel；其他错误一律按业务处理，避免吞掉真实 bug。

2. **超时绕开熔断计数**：Harness 包装的超时错误带 `*timeoutErr` 类型；Resilience 中 `IsTimeout(err)` 命中时直接 `return`，既不重试也不 `Breaker.Record`。

3. **ReAct 循环与模型协议解耦**：`DecisionProvider.Next(prompt)` 只接受字符串、与具体 LLM 协议无关；接入 tRPC-Agent-Go 时只需把 `EventStreamReader` 适配成本接口。

4. **工具调用串行而非并发**：第一版实现为保证事件流有序；P4 引入真正的并发派发时再改为 worker pool。

5. **ShoppingContext 显式化**：所有 shopping 包函数都接收并返回 Context 值；`IntoContext` 仅作为第三方中间件（tRPC）读值的便利方法，业务代码不依赖。

---

## 测试覆盖

```
internal/agent/breaker         6 tests  PASS
internal/agent/middleware      6 tests  PASS  (含 D3 三个验收)
internal/agent/orchestrator    3 tests  PASS  (D1 × 2, D2 × 1)
internal/agent/tools          14 tests  PASS  (来自 P2)
```

---

## 与 P1/P2 的依赖

- P1 账本未动（D1 测试用 fake handler 隔离）
- P2 工具 schema 直接被 Executor 消费
- 后续 P4 (AG-UI SSE) 会订阅 orchestrator 事件流并补 run_start/run_finished 之外的事件类型