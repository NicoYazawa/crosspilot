-- 0005_cost_unit_micro.up
--
-- 只改注释，不改结构：把 observability.cost.cost_minor 的「最小货币单位」由
-- 「分」订正为 1e-6 元。
--
-- 为什么必须改口径而不是将错就错：按各家公开价目，一次真实的下单查询总成本
-- 只有零点几分（DeepSeek 量级：数千 input + 数百 output）。若最小单位是分，
-- 每一行取整后都是 0，成本看板会长期显示「没花钱」——而这与「我们知道这次
-- 调用不花钱」是两件事，正是 unpriced 机制要区分的那个区别。
--
-- 列名与 Go 字段名一律不改（cost_minor 本来就是「以最小货币单位计的金额」，
-- 变的只是最小单位是什么），因此不产生跨端改名波及。
--
-- 附带的好处：厂商按「元 / 百万 tokens」报价，在最小单位 = 1e-6 元 下，
-- 该数字与「每 token 多少最小单位」逐个相等，价格表可以原样照抄价目页。
--
-- 历史数据：本迁移上线前 observability.cost 恒为 0 行（写入方从未接线），
-- 不存在需要换算的旧数据。

COMMENT ON COLUMN observability.cost.cost_minor IS
    '本次调用的费用，单位是最小货币单位 = 1e-6 元（即「微元」）。'
    'unpriced=true 时恒为 0。注意：不是「分」——1 分 = 10000 个最小单位。';

COMMENT ON COLUMN observability.cost.currency IS
    '与 cost_minor 配套的币种。不同 provider 可以不同（Anthropic 按美元报价），'
    '读取端按币种聚合，不跨币种求和。';

COMMENT ON COLUMN observability.cost.priced_at IS
    '定价发生的时刻；unpriced=true 时为 NULL，用于区分「尚未定价」与「定价后为 0」。';
