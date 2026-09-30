-- 0004_observability.down
--
-- 回滚 P5 observability schema。注意 ON DELETE CASCADE 在 event/cost/latency
-- 表上是必要的，否则 down 会因为有 run 行引用而失败。

DROP TABLE IF EXISTS observability.rollup_hourly;
DROP TABLE IF EXISTS observability.dropped;
DROP TABLE IF EXISTS observability.latency;
DROP TABLE IF EXISTS observability.cost;
DROP TABLE IF EXISTS observability.event;
DROP TABLE IF EXISTS observability.run;
DROP SCHEMA IF EXISTS observability;