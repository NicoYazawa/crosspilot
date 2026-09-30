-- 0002_trade_ledger
--
-- 交易账本的四张聚合表与两张会话表。
--
-- 与源实现的差别不在字段，而在并发原语：源实现依赖 SQLite 的 BEGIN IMMEDIATE
-- 单写锁——「先拿写锁，再检查库存、状态与幂等键」在同一时刻只有一个写者，
-- 因此检查与写入之间不存在窗口。Postgres 是 MVCC，没有这个原语，写锁不存在。
--
-- 因此这里把正确性交给三样东西，每一张表都要能被单独推理：
--
--   1. 唯一约束：operation_id 唯一 → 同一操作编号只可能有一张确认单；
--   2. 条件更新：扣减库存带 stock >= qty 与报价条件，rowcount != 1 即失败；
--   3. 显式行锁与咨询锁：确认单走 pg_advisory_xact_lock(operation_id)，
--      把「检查后写入」的窗口在数据库层关闭，而不是靠应用层假设。
--
-- 所有金额都是最小货币单位（分）的整数。浮点不进数据库，也不进账本。

-- ---------------------------------------------------------------------------
-- 库存：唯一权威报价 + 可售数量
-- ---------------------------------------------------------------------------
--
-- 报价与库存同处一行是刻意的：确认单展示的金额与扣减时校验的金额必须同源，
-- 分成两张表就会产生「读报价时是旧价、扣库存时是新价」的窗口。
CREATE TABLE trade_sku_inventory (
    sku_id           text   PRIMARY KEY,
    product_id       text   NOT NULL,
    title            text   NOT NULL,
    stock            bigint NOT NULL,
    unit_price_minor bigint NOT NULL,
    currency         text   NOT NULL,

    -- 库存不可能为负：应用层的比较更新是主要防线，约束是最后一道，
    -- 它保证「即使有人绕过应用层直连数据库」也不会出现负库存。
    CONSTRAINT ck_trade_stock_nonnegative CHECK (stock >= 0),
    CONSTRAINT ck_trade_price_nonnegative CHECK (unit_price_minor >= 0),
    CONSTRAINT ck_trade_currency_shape CHECK (currency ~ '^[A-Z]{3}$')
);

-- ---------------------------------------------------------------------------
-- 确认单：两阶段提交的第一阶段产物
-- ---------------------------------------------------------------------------
CREATE TABLE trade_confirmations (
    confirmation_id  text        PRIMARY KEY,
    -- 幂等键。唯一约束是「同一操作编号只有一笔交易」的最终保证。
    operation_id     text        NOT NULL UNIQUE,
    buyer_id         text        NOT NULL,
    session_id       text        NOT NULL,
    action           text        NOT NULL,
    -- 请求内容摘要：同一操作编号换了商品、数量、地址或动作时用它识别冲突。
    request_hash     text        NOT NULL,
    -- 规范化后的载荷。决议时按它执行，而不是按当时的输入重算。
    payload          jsonb       NOT NULL,
    -- 覆盖买方、会话、动作、载荷与有效期的摘要。
    snapshot_hash    text        NOT NULL,
    expires_at       timestamptz NOT NULL,
    status           text        NOT NULL DEFAULT 'pending',
    result           jsonb,
    created_at       timestamptz NOT NULL,
    resolved_at      timestamptz,

    CONSTRAINT ck_trade_confirmation_status CHECK (status IN ('pending', 'approved', 'rejected')),
    CONSTRAINT ck_trade_confirmation_action CHECK (action IN ('create', 'cancel')),
    CONSTRAINT ck_trade_confirmation_snapshot_hash CHECK (snapshot_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT ck_trade_confirmation_request_hash CHECK (request_hash ~ '^[0-9a-f]{64}$')
);

-- 列表按「当前买家 + 当前会话 + 时间倒序」取，索引照着这个形状建
CREATE INDEX ix_trade_confirmation_owner
    ON trade_confirmations (buyer_id, session_id, created_at DESC, confirmation_id DESC);

-- ---------------------------------------------------------------------------
-- 订单与订单行
-- ---------------------------------------------------------------------------
CREATE TABLE orders (
    order_id              text        PRIMARY KEY,
    buyer_id              text        NOT NULL,
    status                text        NOT NULL,
    -- 币种与总额冗余在订单头上：订单快照与列表都要用到，
    -- 每次都从订单行重算会让「订单总额」有两个来源。
    currency              text        NOT NULL,
    total_amount_minor    bigint      NOT NULL,
    shipping_address_json jsonb       NOT NULL,
    created_at            timestamptz NOT NULL,
    confirmed_at          timestamptz,
    cancelled_at          timestamptz,
    cancel_reason         text,

    CONSTRAINT ck_orders_status CHECK (status IN ('DRAFT', 'CONFIRMED', 'CANCELLED')),
    CONSTRAINT ck_orders_total_nonnegative CHECK (total_amount_minor >= 0),
    CONSTRAINT ck_orders_cancelled_has_reason CHECK (
        status <> 'CANCELLED' OR (cancelled_at IS NOT NULL AND cancel_reason IS NOT NULL)
    )
);

CREATE INDEX ix_orders_buyer
    ON orders (buyer_id, created_at DESC, order_id DESC);
CREATE INDEX ix_orders_buyer_status
    ON orders (buyer_id, status, created_at DESC, order_id DESC);

CREATE TABLE order_lines (
    order_id         text   NOT NULL REFERENCES orders (order_id) ON DELETE CASCADE,
    sku_id           text   NOT NULL,
    product_id       text   NOT NULL,
    title            text   NOT NULL,
    unit_price_minor bigint NOT NULL,
    currency         text   NOT NULL,
    quantity         bigint NOT NULL,

    -- 一个订单里同一个 SKU 只能有一行：拆成两行会让「取消时回补多少」
    -- 出现两种答案。应用层已经合并，这里是最终约束。
    PRIMARY KEY (order_id, sku_id),
    CONSTRAINT ck_order_lines_quantity_positive CHECK (quantity > 0),
    CONSTRAINT ck_order_lines_price_nonnegative CHECK (unit_price_minor >= 0)
);

-- ---------------------------------------------------------------------------
-- 已执行的决议
-- ---------------------------------------------------------------------------
--
-- 这张表存在的意义是「决议只执行一次」的可审计证据：确认单的状态字段说明
-- 结果，而这张表记录结果是在哪一刻、以什么方式产生的。
CREATE TABLE trade_operations (
    operation_id    text        PRIMARY KEY,
    confirmation_id text        NOT NULL UNIQUE REFERENCES trade_confirmations (confirmation_id),
    approved        boolean     NOT NULL,
    -- 决议的三态：approved / rejected。放在这里而不是从确认单推断，
    -- 是为了让这张表单独可读。
    decision        text        NOT NULL,
    result          jsonb,
    resolved_at     timestamptz NOT NULL,

    CONSTRAINT ck_trade_operations_decision CHECK (decision IN ('approved', 'rejected'))
);

-- ---------------------------------------------------------------------------
-- 会话：执行权票据与全量快照
-- ---------------------------------------------------------------------------
--
-- claim 每次让 fence 前进，save 用 (owner_id, revision, fence) 条件更新。
-- 两者一起保证「旧执行者回来写」必被拒绝。
CREATE TABLE session_write_claims (
    session_id text   PRIMARY KEY,
    owner_id   text,
    revision   bigint NOT NULL DEFAULT 0,
    fence      bigint NOT NULL DEFAULT 0,

    CONSTRAINT ck_session_revision_nonnegative CHECK (revision >= 0),
    CONSTRAINT ck_session_fence_nonnegative CHECK (fence >= 0)
);

CREATE TABLE agent_session_states (
    session_id text        PRIMARY KEY,
    state_json text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- 快照必须是 JSON **对象**：数组、字符串、数字都是合法 JSON，
    -- 但读回来之后按对象取字段会全部落空，会话等于永久损坏。
    -- 只写 `state_json::jsonb IS NOT NULL` 拦不住这些——它只校验「是不是合法 JSON」。
    CONSTRAINT ck_agent_session_state_is_object
        CHECK (jsonb_typeof(state_json::jsonb) = 'object')
);
