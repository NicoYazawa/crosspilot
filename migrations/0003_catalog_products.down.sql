-- 0003_catalog_products.down.sql

DROP INDEX IF EXISTS catalog.idx_products_in_stock;
DROP INDEX IF EXISTS catalog.idx_products_category;
DROP TABLE IF EXISTS catalog.products;
DROP SCHEMA IF EXISTS catalog;
