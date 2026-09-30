-- 0002_trade_ledger 回滚
--
-- 顺序与外键相反：先删引用方，再删被引用方。
DROP TABLE IF EXISTS agent_session_states;
DROP TABLE IF EXISTS session_write_claims;
DROP TABLE IF EXISTS trade_operations;
DROP TABLE IF EXISTS order_lines;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS trade_confirmations;
DROP TABLE IF EXISTS trade_sku_inventory;
