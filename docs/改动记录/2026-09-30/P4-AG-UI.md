# P4：AG-UI SSE 服务端

日期：2026-09-30
范围：AG-UI v0.9 三报文契约、journal 与可恢复 SSE、HMAC 鉴权、`runevent` 基座、A2UI 翻译器、Agent → SSE 衔接层 `orderflow`。
对应 [docs/开发计划-核心链路与三件套.md](../../开发计划-核心链路与三件套.md) 中 P4 的核心必须项 E1–E6（E7/E8/E9 留 P6/P7）。

---

## 交付物

```
internal/agent/runevent/
├── runevent.go            # Event / Sequencer / VerifyGap；ULID 形态 event_id
├── a2ui.go                # CatalogId="globex.local/shopping-v2"；v0.9 三报文校验
├── runevent_test.go       # Sequencer / Attach / VerifyGap
└── a2ui_test.go           # 三报文契约

internal/application/orderflow/
├── ports.go               # Bridge：orchestrator 事件 → RunEvent → Journal
├── a2ui.go                # Confirmation → A2UI v0.9 三报文
├── bridge_test.go         # Bridge.Run + 错误传播 + 事件转发
└── a2ui_test.go           # 三报文形态

internal/presentation/agui/
├── journal.go             # JournalStore 端口 + MemoryJournal
├── replay.go              # Cursor / BindToRun / CheckGap
├── handler.go             # POST /runs, GET /runs/{id}/events, /confirm
├── auth.go                # X-HMAC-Sign 简化校验
├── submitter.go           # application → presentation 适配
├── handler_test.go        # E1-E6 + HMAC
├── journal_test.go        # Append 单调性 / Since / ListRuns
└── replay_test.go         # ParseCursor / BindToRun / CheckGap
```

---

## 验收

| # | 指标 | 状态 | 备注 |
|---|---|---|---|
| E1 | 断流续传 | ✅ | `TestE1_ResumeContinuity`：cursor=`run:2` → 从 seq=3 起续 |
| E2 | 游标连续性 | ✅ | `TestE2_GapRejected`：cursor=`run:7` 但 lastSeq=4 → 400 seq_gap |
| E3 | 跨 run 游标拒绝 | ✅ | `TestE3_CrossRunRejected`：cursor.runID ≠ target → 400 cursor_mismatch |
| E4 | 重连不重计费 | ✅ | `TestE4_ReconnectDoesNotCallModel`：3 次 GET 后 Submitter 调用次数 = 0 |
| E5 | 崩溃恢复 | ✅ | `TestE5_ServerRestartSentinel`：未关闭 run 补哨兵，run_finished 跳过 |
| E6 | A2UI v0.9 契约 | ✅ | `TestE6_A2UI_ThreeMessagesAreValid`：三报文 schema + catalogId 校验 |

### 关键设计决策

1. **「先 journal 后 SSE」物理保证**：handler 不持有 model 引用；`Bridge.Run` 把每条 orchestrator 事件同步 Append 到 journal 后才返回给客户端。重连路径只调 `Journal.Since`，绝不可能触发模型重跑。

2. **RunEvent 与 orchestrator.Event 分离**：
   - `orchestrator.Event` 含 `Err`、`Iteration` 等中间状态，**不可序列化**到 journal。
   - `runevent.Event` 是不可变记录：`EventID / RunID / Seq / Kind / Payload / CreatedAt`。
   - `runevent.Sequencer` 强制单调 Seq；调用方拿不到未编号事件。

3. **Cursor 语义**：`Last-Event-ID: run:N` 表示「客户端已处理到 seq=N」，重连从 `N+1` 起。
   - `since == 0`：首次订阅
   - `since <= lastSeq+1`：客户端至多落后一格（断电重启后失忆也算这一类），允许重发
   - `since >  lastSeq+1`：缺口，400 seq_gap

4. **JournalStore 接口隔离**：测试用 `MemoryJournal`，生产用 Postgres 实现。
   - `Append / Since / LastSeq / ListRuns` 四方法覆盖 E1/E4/E5 全部分支。
   - **未实现**前 `streamHandler` 不挂发订阅尾巴（E9 留给 P5 publisher）。

5. **A2UI v0.9 严格校验**：
   - `CreateSurface` 必须包含 `path="/requirements"` 根组件
   - `UpdateComponents` 每条必须有 `id` 与 `type`
   - `UpdateDataModel` 必须有 `value.path` 与 `value.data`
   - `catalogId` 硬编码为 `"globex.local/shopping-v2"`（A2UI v0.8 vs v0.9 不兼容的源头）

6. **HMAC 简化方案**：`X-HMAC-Sign: hex(HMAC-SHA256(secret, method + "\n" + path + "\n" + ts))`。
   - 时间偏差 ≤5min；空密钥放行（开发期开关）。
   - 严格的键集校验（E8）留 P7。

---

## 测试覆盖

```
internal/agent/runevent       12 tests  PASS
internal/application/orderflow 5 tests  PASS
internal/presentation/agui    21 tests  PASS  (E1-E6 + journal + replay + auth)
```

全量 26 个 internal 包通过。

---

## 不在本阶段（TODO 给 P6/P7）

- E7 前端拒渲染闸门：依赖前端组件运行时，本阶段仅 Go 服务端
- E8 严格身份键集（X-HMAC-* 必填集合）：当前实现已含基本签名校验
- E9 慢客户端 lag 丢弃：当前 publisher 未实现；P5 阶段接入 publisher 时一起补
- 22 条 REST 全量路由（附录 B）：演示用最小面 4 条路由（POST /runs, GET /runs/{id}, GET /runs/{id}/events, POST /runs/{id}/confirm）
- 取消/屏蔽运行控制：handler 挂点已留，`confirmHandler` 占位实现
- Postgres journal 实现：当前 `MemoryJournal` 满足测试与单进程演示；生产实现是 `internal/infra/persistence/pg/agui_journal.go` 的接口待补