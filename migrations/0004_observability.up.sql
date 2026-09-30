-- 0004_observability
--
-- 可观测三件套（P5）的存储层：与交易账本同 Postgres 实例、不同 schema、
-- 不同 pgxpool —— 这是 F9「观测写入不影响交易事务」的物理保证。
--
-- 关键设计：
--   1. observability.run / .event 是事实源（raw 事件）；cost / latency 是归因
--   2. payload_redacted 是写路径脱敏后的对象，用来回放/差分 —— 全文 blob 不存
--   3. payload_sha256 是完整性校验 + 去重键
--   4. dropped 表登记队列满丢弃的事件数，绝不静默吞掉（F6 闸门）
--   5. rollup_hourly 是物化层，给 P6 前端看板用；F5 P95 < 500ms 实测待 P5 实跑

CREATE SCHEMA IF NOT EXISTS observability;

-- 一条 run 的元信息：买家、会话、起止、A/B 臂。
CREATE TABLE IF NOT EXISTS observability.run (
    run_id         TEXT PRIMARY KEY,
    buyer_id       TEXT NOT NULL,
    session_id     TEXT NOT NULL,
    agent          TEXT NOT NULL DEFAULT 'main',
    started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at    TIMESTAMPTZ,
    status         TEXT,                         -- completed | errored | server_restart
    experiment_key TEXT,                         -- F8：实验键；NULL 表示不在实验中
    experiment_arm TEXT                          -- F8：arm 标识（a / b）
);

CREATE INDEX IF NOT EXISTS idx_obs_run_session ON observability.run(session_id);
CREATE INDEX IF NOT EXISTS idx_obs_run_started  ON observability.run(started_at);
CREATE INDEX IF NOT EXISTS idx_obs_run_arm      ON observability.run(experiment_key, experiment_arm);

-- 事件事实表：payload_redacted 是脱敏后的对象，足够做回放/差分；
-- 原始 blob 故意不存（F4 内容寻址）—— sha256 + size 已足够验证完整性。
CREATE TABLE IF NOT EXISTS observability.event (
    event_id         TEXT PRIMARY KEY,
    run_id           TEXT NOT NULL REFERENCES observability.run(run_id) ON DELETE CASCADE,
    seq              BIGINT NOT NULL,
    kind             TEXT NOT NULL,
    agent            TEXT,
    payload_sha256   TEXT NOT NULL,               -- 内容寻址
    payload_size     INT  NOT NULL,              -- 原始 payload 字节数
    payload_redacted JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_obs_event_kind ON observability.event(kind);
CREATE INDEX IF NOT EXISTS idx_obs_event_created ON observability.event(created_at);

-- 成本归因：按事件挂费用。unpriced=true 表示未命中价格表（F4 闸门），
-- 绝不允许「未定价就当 0 元」。
CREATE TABLE IF NOT EXISTS observability.cost (
    event_id          TEXT PRIMARY KEY REFERENCES observability.event(event_id) ON DELETE CASCADE,
    provider          TEXT NOT NULL,             -- qwen | minimax | deepseek | anthropic
    model             TEXT NOT NULL,
    tokens_in         BIGINT NOT NULL DEFAULT 0,
    tokens_out        BIGINT NOT NULL DEFAULT 0,
    tokens_cached     BIGINT NOT NULL DEFAULT 0,
    tokens_reasoning  BIGINT NOT NULL DEFAULT 0,
    cost_minor        BIGINT NOT NULL DEFAULT 0, -- 累计最小货币单位（分）
    currency          TEXT NOT NULL DEFAULT 'CNY',
    unpriced          BOOLEAN NOT NULL DEFAULT false,
    priced_at         TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_obs_cost_provider ON observability.cost(provider, model);
CREATE INDEX IF NOT EXISTS idx_obs_cost_unpriced ON observability.cost(unpriced) WHERE unpriced = true;

-- 延迟分层：按 span 记一段操作的起止，便于 F10「跨协议族可比」。
CREATE TABLE IF NOT EXISTS observability.latency (
    event_id     TEXT PRIMARY KEY REFERENCES observability.event(event_id) ON DELETE CASCADE,
    span_name    TEXT NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL,
    ended_at     TIMESTAMPTZ NOT NULL,
    duration_ms  INT NOT NULL,
    CHECK (ended_at >= started_at)
);

CREATE INDEX IF NOT EXISTS idx_obs_latency_span ON observability.latency(span_name);

-- 队列丢弃计数：F6 闸门。任何 backend worker 在队列满时记一行；
-- 这张表永远只有聚合行，绝不漏掉（F6 的本质要求）。
CREATE TABLE IF NOT EXISTS observability.dropped (
    bucket_ts  TIMESTAMPTZ NOT NULL,
    reason     TEXT NOT NULL,                   -- queue_full | sink_error | redact_error | sink_timeout
    count      INT NOT NULL,
    PRIMARY KEY (bucket_ts, reason)
);

-- 物化层：每小时每个 provider×model 聚合一次，给 P6 看板用。
-- 刷新策略（5 分钟一次）留 P8 cron job；本阶段只建表。
CREATE TABLE IF NOT EXISTS observability.rollup_hourly (
    hour_ts      TIMESTAMPTZ NOT NULL,
    provider     TEXT NOT NULL,
    model        TEXT NOT NULL,
    calls        INT NOT NULL DEFAULT 0,
    tokens_in    BIGINT NOT NULL DEFAULT 0,
    tokens_out   BIGINT NOT NULL DEFAULT 0,
    cost_minor   BIGINT NOT NULL DEFAULT 0,
    unpriced     INT NOT NULL DEFAULT 0,
    PRIMARY KEY (hour_ts, provider, model)
);

CREATE INDEX IF NOT EXISTS idx_obs_rollup_hour ON observability.rollup_hourly(hour_ts);