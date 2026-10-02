-- 0005_cost_unit_micro.down
--
-- 恢复 0004 里的原始注释（口径回到「分」）。
-- 只回滚注释，不碰数据：本迁移本身也没有改过任何行。

COMMENT ON COLUMN observability.cost.cost_minor IS '累计最小货币单位（分）';
COMMENT ON COLUMN observability.cost.currency IS NULL;
COMMENT ON COLUMN observability.cost.priced_at IS NULL;
