-- 0003_catalog_products
--
-- 商品目录表，存储从 catalog-v3.jsonl 导入的 7,105 件 SKU。
-- 金额以字符串形式存储以保留精度，由领域层在读写时做 decimal 转换。

CREATE SCHEMA IF NOT EXISTS catalog;

CREATE TABLE catalog.products (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT,
    category_id TEXT NOT NULL,
    price_amount_major TEXT NOT NULL,  -- 十进制字符串，保留精度
    price_currency TEXT NOT NULL,
    image_url TEXT,
    rating REAL,
    review_count INTEGER,
    in_stock BOOLEAN DEFAULT true,
    attributes JSONB,
    tags TEXT[],
    origin_country TEXT DEFAULT '',
    brand TEXT DEFAULT '',
    material_tags TEXT[],
    weight_kg REAL DEFAULT 0,
    ships_to TEXT[],
    canonical_product_id TEXT,
    source_platform TEXT DEFAULT '',
    updated_at TIMESTAMPTZ DEFAULT now(),
    default_sku_id TEXT,
    source_language TEXT DEFAULT '',
    source_locale TEXT DEFAULT '',
    data_provenance TEXT DEFAULT '',
    created_at TIMESTAMPTZ DEFAULT now()
);

CREATE INDEX idx_products_category ON catalog.products(category_id);
CREATE INDEX idx_products_in_stock ON catalog.products(in_stock);
