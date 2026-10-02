package persistence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// 真实引导文件相对本测试文件的位置。
const realCatalogPath = "../../../data/catalog-v3.jsonl.gz"

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// decodeOne 用与 readCatalog 相同的解码方式解析一行，供用例复用。
//
// 刻意与 readCatalog 走同一条解码路径（json.Decoder + UseNumber）而不是
// json.Unmarshal：一旦两条路径出现偏差，用例就会在验证一件实现里并不存在的
// 事——之前那版导入器正是「测试很绿、代码读不出数据」。
func decodeOne(t *testing.T, line string) jsonProduct {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()

	var p jsonProduct
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("解析样例失败: %v\n%s", err, line)
	}
	return p
}

func TestJSONProductToRow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		line  string
		check func(t *testing.T, r productRow)
	}{
		{
			name: "字段名按源文件映射而非想象中的扁平结构",
			line: `{"product_id":"P9","title":"标题","description":"描述","category":"旅行装备",
			        "brand":"Nomadica","origin_country":"VN","material_tags":["合成聚合物"],
			        "weight_kg":0.15,"ships_to":["CN","US"],"canonical_product_id":"CAN-1",
			        "source_platform":"amazon","source_language":"zh","source_locale":"zh-CN",
			        "data_provenance":"legacy",
			        "rating_summary":{"average":4.2,"review_count":37},
			        "skus":[{"sku_id":"P9-S1","spec":"军绿","currency":"CNY","price_major":189.0,"stock":5}]}`,
			check: func(t *testing.T, r productRow) {
				if r.ID != "P9" {
					t.Errorf("ID = %q, 期望 P9（源文件里叫 product_id）", r.ID)
				}
				if r.CategoryID != "旅行装备" {
					t.Errorf("CategoryID = %q, 期望取 category", r.CategoryID)
				}
				if r.Rating != 4.2 || r.ReviewCount != 37 {
					t.Errorf("评分 = %v/%d, 期望从 rating_summary 取到 4.2/37", r.Rating, r.ReviewCount)
				}
				if r.Brand != "Nomadica" || r.OriginCountry != "VN" {
					t.Errorf("品牌/产地 = %q/%q", r.Brand, r.OriginCountry)
				}
				if !r.InStock {
					t.Error("InStock = false, 有一件规格库存 5 应为在售")
				}
			},
		},
		{
			name: "金额保留文件里的字面量，不经过 float64",
			line: `{"product_id":"P1","category":"c","skus":[
			         {"sku_id":"P1-S1","currency":"CNY","price_major":189.0,"stock":1}]}`,
			check: func(t *testing.T, r productRow) {
				// 189.0 若被解成 float64 再转字符串会得到 "189"，丢掉小数位；
				// 更糟的是 0.1+0.2 那类价格会出现二进制误差。
				if r.PriceAmountMajor != "189.0" {
					t.Errorf("PriceAmountMajor = %q, 期望字面量 189.0", r.PriceAmountMajor)
				}
				if r.PriceCurrency != "CNY" {
					t.Errorf("PriceCurrency = %q, 期望 CNY", r.PriceCurrency)
				}
			},
		},
		{
			name: "整数价格同样原样保留",
			line: `{"product_id":"P2","category":"c","skus":[
			         {"sku_id":"P2-S1","currency":"JPY","price_major":2980,"stock":3}]}`,
			check: func(t *testing.T, r productRow) {
				if r.PriceAmountMajor != "2980" {
					t.Errorf("PriceAmountMajor = %q, 期望 2980", r.PriceAmountMajor)
				}
			},
		},
		{
			name: "默认规格与报价取自同一个 SKU",
			line: `{"product_id":"P3","category":"c","skus":[
			         {"sku_id":"P3-S1","spec":"军绿","currency":"CNY","price_major":189.0,"stock":50},
			         {"sku_id":"P3-S2","spec":"沙漠黄","currency":"CNY","price_major":199.0,"stock":30}]}`,
			check: func(t *testing.T, r productRow) {
				// 报价取首规格、规格 ID 若取最低价，两者会指向不同 SKU，
				// 下单时价格与规格就对不上了。
				if r.DefaultSkuID != "P3-S1" || r.PriceAmountMajor != "189.0" {
					t.Errorf("默认规格/价格 = %q/%q, 期望 P3-S1/189.0",
						r.DefaultSkuID, r.PriceAmountMajor)
				}
				skus, ok := r.Attributes["skus"].([]jsonSKU)
				if !ok || len(skus) != 2 {
					t.Fatalf("attributes.skus = %#v, 期望完整两个规格", r.Attributes["skus"])
				}
				if skus[1].SKUID != "P3-S2" || skus[1].PriceMajor.String() != "199.0" {
					t.Errorf("第二个规格丢了: %#v", skus[1])
				}
			},
		},
		{
			name: "全部规格无货时商品为缺货",
			line: `{"product_id":"P4","category":"c","skus":[
			         {"sku_id":"P4-S1","currency":"CNY","price_major":10,"stock":0},
			         {"sku_id":"P4-S2","currency":"CNY","price_major":12,"stock":0}]}`,
			check: func(t *testing.T, r productRow) {
				if r.InStock {
					t.Error("InStock = true, 全部规格库存为 0 应为缺货")
				}
			},
		},
		{
			name: "卖点与尺寸进 attributes",
			line: `{"product_id":"P5","category":"c",
			        "highlights":[{"label":"材质","detail":"帆布"}],
			        "dimensions_cm":{"length":12,"width":8,"height":4},
			        "tax_category":"旅行装备","external_product_id":"amazon-P5",
			        "updated_at":"2026-08-01",
			        "skus":[{"sku_id":"P5-S1","currency":"CNY","price_major":10,"stock":1}]}`,
			check: func(t *testing.T, r productRow) {
				if _, ok := r.Attributes["highlights"]; !ok {
					t.Error("attributes 缺 highlights")
				}
				if _, ok := r.Attributes["dimensions_cm"]; !ok {
					t.Error("attributes 缺 dimensions_cm")
				}
				if r.Attributes["tax_category"] != "旅行装备" {
					t.Errorf("tax_category = %v", r.Attributes["tax_category"])
				}
				if r.Attributes["external_product_id"] != "amazon-P5" {
					t.Errorf("external_product_id = %v", r.Attributes["external_product_id"])
				}
				if r.Attributes["source_updated_at"] != "2026-08-01" {
					t.Errorf("source_updated_at = %v", r.Attributes["source_updated_at"])
				}
			},
		},
		{
			name: "源文件没有的字段留空而不是编造",
			line: `{"product_id":"P6","category":"c","skus":[
			         {"sku_id":"P6-S1","currency":"CNY","price_major":10,"stock":1}]}`,
			check: func(t *testing.T, r productRow) {
				if r.ImageURL != "" {
					t.Errorf("ImageURL = %q, 源文件无图片字段应为空", r.ImageURL)
				}
				if r.Tags != nil {
					t.Errorf("Tags = %#v, 源文件无 tags 应为 nil", r.Tags)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			row, err := decodeOne(t, tt.line).toRow()
			if err != nil {
				t.Fatalf("toRow 失败: %v", err)
			}
			tt.check(t, row)
		})
	}
}

func TestJSONProductToRow_拒绝无法形成价格的行(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
	}{
		{"没有 product_id", `{"title":"x","skus":[{"sku_id":"S","currency":"CNY","price_major":1,"stock":1}]}`},
		{"没有规格", `{"product_id":"P1","title":"x","skus":[]}`},
		{"规格缺价格", `{"product_id":"P1","skus":[{"sku_id":"P1-S1","currency":"CNY","stock":1}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := decodeOne(t, tt.line).toRow(); err == nil {
				t.Error("期望返回错误，实际为 nil")
			}
		})
	}
}

// TestReadCatalog_真实目录文件全部可映射 是这个缺陷的回归守卫。
//
// 它断言的不是「函数没报错」，而是「3,700 行里一行都没被跳过」——上一版导入器
// 的失败方式正是每行都映射失败却一路安静，只要断言写在「调用没返回错误」上就
// 永远发现不了。用例直接读仓库里的真实文件，不需要数据库。
func TestReadCatalog_真实目录文件全部可映射(t *testing.T) {
	t.Parallel()

	// 走 openCatalogFile 而不是直接 os.Open：压缩文件的解压是导入路径的
	// 一部分，绕过它测的就是一份比真实调用少一层的实现。
	f, err := openCatalogFile(realCatalogPath)
	if err != nil {
		t.Fatalf("打开真实目录文件失败: %v", err)
	}
	defer func() { _ = f.Close() }()

	rows := map[string]productRow{}
	imported, skipped, err := readCatalog(context.Background(), f, discardLogger(), func(r productRow) error {
		if _, dup := rows[r.ID]; dup {
			t.Errorf("商品 ID 重复: %s", r.ID)
		}
		rows[r.ID] = r
		return nil
	})
	if err != nil {
		t.Fatalf("readCatalog 失败: %v", err)
	}

	// 源文件 3,700 个 SPU。数量对不上说明文件换了或被截断。
	const wantProducts = 3700
	if imported != wantProducts {
		t.Errorf("成功映射 %d 件, 期望 %d 件", imported, wantProducts)
	}
	if skipped != 0 {
		t.Errorf("跳过 %d 行, 期望 0 行（跳过说明映射规则与文件结构又不匹配了）", skipped)
	}

	// 逐件断言「有没有形成可下单的商品」——单看行数会被「行数对、字段全空」骗过。
	// 价格交给领域层的 ParseMoney 校验而不只判空：金额与币种要能被下单链路
	// 直接消费，才算真的可用。
	for id, r := range rows {
		if r.DefaultSkuID == "" {
			t.Fatalf("商品 %s 没有默认规格", id)
		}
		if _, err := catalog.ParseMoney(r.PriceAmountMajor, catalog.Currency(r.PriceCurrency)); err != nil {
			t.Fatalf("商品 %s 的价格 %q %s 无法被领域层解析: %v",
				id, r.PriceAmountMajor, r.PriceCurrency, err)
		}
	}

	// P1001 是源文件第一行，两个规格 189.0 / 199.0，库存 50/30。
	p1001, ok := rows["P1001"]
	if !ok {
		t.Fatal("缺少 P1001")
	}
	if p1001.PriceAmountMajor != "189.0" || p1001.PriceCurrency != "CNY" {
		t.Errorf("P1001 价格 = %s %s, 期望 189.0 CNY", p1001.PriceAmountMajor, p1001.PriceCurrency)
	}
	if p1001.DefaultSkuID != "P1001-S1" {
		t.Errorf("P1001 默认规格 = %q, 期望 P1001-S1", p1001.DefaultSkuID)
	}
	if p1001.Title == "" || p1001.CategoryID == "" || p1001.Description == "" {
		t.Errorf("P1001 文本字段为空: %#v", p1001)
	}
	// 源文件里 P1001 的规格是「军绿色 / 沙漠黄」，全量入 attributes 后应能读回。
	if skus, ok := p1001.Attributes["skus"].([]jsonSKU); !ok || len(skus) != 2 {
		t.Errorf("P1001 的 attributes.skus = %#v, 期望两个规格", p1001.Attributes["skus"])
	}

	// 抽查缺货商品：源文件有 370 件全部规格库存为 0。
	inStock, outOfStock := 0, 0
	for _, r := range rows {
		if r.InStock {
			inStock++
		} else {
			outOfStock++
		}
	}
	if outOfStock == 0 {
		t.Error("没有任何缺货商品：in_stock 很可能是恒定 true，没有真的看库存")
	}
	if inStock == 0 {
		t.Error("没有任何在售商品")
	}
}

// TestOpenCatalogFile_gzip与明文都能读 断言魔数嗅探两条分支都真在工作。
func TestOpenCatalogFile_gzip与明文都能读(t *testing.T) {
	t.Parallel()

	gz, err := os.ReadFile(realCatalogPath)
	if err != nil {
		t.Fatalf("读取压缩文件失败: %v", err)
	}

	dir := t.TempDir()

	gzPath := filepath.Join(dir, "catalog.jsonl.gz")
	// #nosec G703 -- 路径来自 t.TempDir() 加常量文件名，不含任何外部输入
	if err := os.WriteFile(gzPath, gz, 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := openCatalogFile(gzPath)
	if err != nil {
		t.Fatalf("打开 gzip 文件失败: %v", err)
	}
	got, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		t.Fatalf("读取 gzip 文件失败: %v", err)
	}
	if bytes.Count(got, []byte("\n")) != 3700 {
		t.Errorf("解压后 %d 行, 期望 3700 行", bytes.Count(got, []byte("\n")))
	}

	// 同一份数据以明文放出，sniff 必须走「不套解压」那一支。
	plainPath := filepath.Join(dir, "catalog.jsonl")
	if err := os.WriteFile(plainPath, got, 0o600); err != nil {
		t.Fatal(err)
	}

	f, err = openCatalogFile(plainPath)
	if err != nil {
		t.Fatalf("打开明文文件失败: %v", err)
	}
	plain, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		t.Fatalf("读取明文文件失败: %v", err)
	}
	if !bytes.Equal(plain, got) {
		t.Error("明文读取结果与解压结果不一致")
	}
}

func TestOpenCatalogFile_文件不存在时报错(t *testing.T) {
	t.Parallel()

	if _, err := openCatalogFile(filepath.Join(t.TempDir(), "缺失.jsonl.gz")); err == nil {
		t.Error("期望返回错误，实际为 nil")
	}
}

// TestReadCatalog_全是坏行时计数如实上报 断言跳过计数不是摆设。
func TestReadCatalog_全是坏行时计数如实上报(t *testing.T) {
	t.Parallel()

	r := strings.NewReader("{\"坏的\n\n{\"product_id\":\"P1\"}\n")
	imported, skipped, err := readCatalog(
		context.Background(), r, discardLogger(), func(productRow) error { return nil })
	if err != nil {
		t.Fatalf("readCatalog 失败: %v", err)
	}
	if imported != 0 {
		t.Errorf("imported = %d, 期望 0", imported)
	}
	// 第 1 行 JSON 残缺；第 2 行是空行不计；第 3 行没有规格。
	if skipped != 2 {
		t.Errorf("skipped = %d, 期望 2", skipped)
	}
}

// TestReadCatalog_yield 出错时立即中止 断言落库失败不会被吞掉。
func TestReadCatalog_yield出错时立即中止(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("写库炸了")
	lines := strings.Repeat(
		"{\"product_id\":\"P1\",\"skus\":[{\"sku_id\":\"S\",\"currency\":\"CNY\",\"price_major\":1,\"stock\":1}]}\n", 5)

	calls := 0
	_, _, err := readCatalog(context.Background(), strings.NewReader(lines), discardLogger(),
		func(productRow) error {
			calls++
			return sentinel
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, 期望 %v", err, sentinel)
	}
	if calls != 1 {
		t.Errorf("yield 被调用 %d 次, 期望出错后立即停止", calls)
	}
}
