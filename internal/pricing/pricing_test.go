package pricing

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/govalues/decimal"
)

// 注：本文件刻意不用 testify，与 infra 层保持一致（vendorDeps 不含 testify）。
// 工具函数 equal/contains/assertError 模拟 testify 的常用子集，避免逐个 if err != nil。

func equal[T comparable](t *testing.T, name string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s 不一致：got=%v want=%v", name, got, want)
	}
}

func assertTrue(t *testing.T, name string, cond bool) {
	t.Helper()
	if !cond {
		t.Fatalf("%s 期望为 true，实际为 false", name)
	}
}

func assertFalse(t *testing.T, name string, cond bool) {
	t.Helper()
	if cond {
		t.Fatalf("%s 期望为 false，实际为 true", name)
	}
}

func assertNoError(t *testing.T, name string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s 期望无错，实际 %v", name, err)
	}
}

func assertError(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s 期望有错，实际无错", name)
	}
}

func assertErrorAs(t *testing.T, name string, err error, target any) {
	t.Helper()
	if !errors.As(err, target) {
		t.Fatalf("%s 期望错误可转为 %T，实际 %T（%v）", name, target, err, err)
	}
}

func TestIsValidProvider(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"qwen", true},
		{"minimax", true},
		{"deepseek", true},
		{"anthropic", true},
		{"", false},
		{"openai", false},
		{"QWEN", false}, // 严格区分大小写
	}
	for _, c := range cases {
		got := IsValidProvider(c.in)
		if got != c.want {
			t.Fatalf("IsValidProvider(%q): got=%v want=%v", c.in, got, c.want)
		}
	}
}

func TestLoadFromReader_BasicLookup(t *testing.T) {
	t.Parallel()
	yml := `
version: "2026-09-30"
entries:
  - {provider: qwen, model: qwen3-max, kind: input,  unit_minor: 4,    currency: CNY}
  - {provider: qwen, model: qwen3-max, kind: output, unit_minor: 12,   currency: CNY}
  - {provider: anthropic, model: claude-sonnet-5-5, kind: input, unit_minor: 7, currency: CNY}
`
	book, err := LoadFromReader(strings.NewReader(yml), "test://basic")
	assertNoError(t, "LoadFromReader", err)
	equal(t, "Version", book.Version, "2026-09-30")
	equal(t, "Size", book.Size(), 3)

	cost, ok := book.Lookup("qwen", "qwen3-max", KindInput)
	assertTrue(t, "qwen input 命中", ok)
	equal(t, "qwen input cost", cost.String(), "4")

	cost, ok = book.Lookup("qwen", "qwen3-max", KindOutput)
	assertTrue(t, "qwen output 命中", ok)
	equal(t, "qwen output cost", cost.String(), "12")

	cost, ok = book.Lookup("anthropic", "claude-sonnet-5-5", KindInput)
	assertTrue(t, "anthropic input 命中", ok)
	equal(t, "anthropic input cost", cost.String(), "7")
}

func TestLoadFromReader_MissReturnsFalse(t *testing.T) {
	t.Parallel()
	yml := `
version: "v1"
entries:
  - {provider: qwen, model: qwen3-max, kind: input, unit_minor: 4, currency: CNY}
`
	book, err := LoadFromReader(strings.NewReader(yml), "test://miss")
	assertNoError(t, "LoadFromReader", err)

	_, ok := book.Lookup("qwen", "qwen3-max-not-exist", KindInput)
	assertFalse(t, "未知 model 应返回 false", ok)

	_, ok = book.Lookup("qwen", "qwen3-max", KindCached)
	assertFalse(t, "未知 kind 应返回 false", ok)

	_, ok = book.Lookup("anthropic", "qwen3-max", KindInput)
	assertFalse(t, "未知 provider 应返回 false", ok)
}

func TestLoadFromReader_F4_EmptyMeansUnpricedNotError(t *testing.T) {
	t.Parallel()
	book, err := LoadFromReader(strings.NewReader("version: \"empty\"\nentries: []\n"), "test://empty")
	assertNoError(t, "LoadFromReader", err)
	equal(t, "空表 Size", book.Size(), 0)

	cost, ok := book.Lookup("qwen", "qwen3-max", KindInput)
	assertFalse(t, "空表应返回未命中", ok)
	equal(t, "空表成本", cost.String(), decimal.Zero.String())
}

func TestLoadFromReader_NilBookIsUnpriced(t *testing.T) {
	t.Parallel()
	var nilBook *PriceBook
	_, ok := nilBook.Lookup("qwen", "qwen3-max", KindInput)
	assertFalse(t, "nil PriceBook 应返回未命中而不是 panic", ok)
}

func TestLoadFromReader_InvalidProviderRejected(t *testing.T) {
	t.Parallel()
	yml := `
version: "v1"
entries:
  - {provider: openai, model: gpt-4, kind: input, unit_minor: 1, currency: USD}
`
	_, err := LoadFromReader(strings.NewReader(yml), "test://invalid-provider")
	assertError(t, "LoadFromReader 期望拒绝非法 provider", err)
	var typed *ErrInvalidPriceEntry
	assertErrorAs(t, "错误类型", err, &typed)
}

func TestLoadFromReader_InvalidKindRejected(t *testing.T) {
	t.Parallel()
	yml := `
version: "v1"
entries:
  - {provider: qwen, model: qwen3-max, kind: hallucinated, unit_minor: 1, currency: CNY}
`
	_, err := LoadFromReader(strings.NewReader(yml), "test://invalid-kind")
	assertError(t, "LoadFromReader 期望拒绝非法 kind", err)
}

func TestLoadFromReader_UnitMinorAsString(t *testing.T) {
	t.Parallel()
	yml := `
version: "v1"
entries:
  - {provider: qwen, model: qwen3-max, kind: cached_input, unit_minor: "0.0007", currency: CNY}
`
	book, err := LoadFromReader(strings.NewReader(yml), "test://string")
	assertNoError(t, "LoadFromReader", err)
	cost, ok := book.Lookup("qwen", "qwen3-max", KindCached)
	assertTrue(t, "cached_input 命中", ok)
	equal(t, "cached_input cost", cost.String(), "0.0007")
}

func TestLoadFromFile_EmptyPathReturnsEmptyBook(t *testing.T) {
	t.Parallel()
	book, err := LoadFromFile("")
	assertNoError(t, "LoadFromFile", err)
	equal(t, "空路径 Size", book.Size(), 0)
	equal(t, "空路径 Version", book.Version, "empty")
}

func TestHasModel(t *testing.T) {
	t.Parallel()
	yml := `
version: "v1"
entries:
  - {provider: qwen, model: qwen3-max, kind: input, unit_minor: 4, currency: CNY}
`
	book, err := LoadFromReader(strings.NewReader(yml), "test://has-model")
	assertNoError(t, "LoadFromReader", err)

	assertTrue(t, "qwen3-max 应在表中", book.HasModel("qwen", "qwen3-max"))
	assertFalse(t, "未知 model 不在表中", book.HasModel("qwen", "qwen3-max-not-exist"))
	assertFalse(t, "anthropic 不应有 qwen3-max", book.HasModel("anthropic", "qwen3-max"))

	// nil PriceBook
	var nilBook *PriceBook
	assertFalse(t, "nil PriceBook 不应有 model", nilBook.HasModel("qwen", "qwen3-max"))
}

func TestSize(t *testing.T) {
	t.Parallel()
	var nilBook *PriceBook
	equal(t, "nil PriceBook Size", nilBook.Size(), 0)

	yml := `
version: "v1"
entries:
  - {provider: qwen, model: qwen3-max, kind: input, unit_minor: 4, currency: CNY}
  - {provider: qwen, model: qwen3-max, kind: output, unit_minor: 12, currency: CNY}
`
	book, err := LoadFromReader(strings.NewReader(yml), "test://size")
	assertNoError(t, "LoadFromReader", err)
	equal(t, "book Size", book.Size(), 2)
}

func TestErrInvalidPriceEntry_Error(t *testing.T) {
	t.Parallel()
	err := &ErrInvalidPriceEntry{Index: 3, Reason: "provider 不合法"}
	got := err.Error()
	if !strings.Contains(got, "第 3") || !strings.Contains(got, "provider") {
		t.Fatalf("错误消息缺字段：%s", got)
	}
}

func TestLoadFromFile_NonExistentReturnsMissingBook(t *testing.T) {
	t.Parallel()
	// 指向不存在的路径 → 返回 version="missing" 的空表，不报错
	book, err := LoadFromFile("/nonexistent/path/pricing.yaml")
	assertNoError(t, "LoadFromFile 不存在路径应不报错", err)
	equal(t, "missing 版本", book.Version, "missing")
	equal(t, "missing Size", book.Size(), 0)
}

func TestLoadFromFile_MalformedYAMLReturnsError(t *testing.T) {
	t.Parallel()
	// 通过临时文件测试真实文件存在但 YAML 解析失败
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("not: valid: yaml: at: all"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	_, err := LoadFromFile(path)
	assertError(t, "LoadFromFile 错误 YAML 应报错", err)
}

func TestEntryFromMap_UnitMinorAsFloat(t *testing.T) {
	t.Parallel()
	// YAML 解码数字默认走 float64：测试这条分支
	yml := `
version: "v1"
entries:
  - {provider: qwen, model: qwen3-max, kind: input, unit_minor: 4.0, currency: CNY}
`
	book, err := LoadFromReader(strings.NewReader(yml), "test://float")
	assertNoError(t, "LoadFromReader", err)
	cost, ok := book.Lookup("qwen", "qwen3-max", KindInput)
	assertTrue(t, "float 应解析", ok)
	equal(t, "float cost", cost.String(), "4")
}

func TestEntryFromMap_DefaultCurrency(t *testing.T) {
	t.Parallel()
	yml := `
version: "v1"
entries:
  - {provider: qwen, model: qwen3-max, kind: input, unit_minor: 4}
`
	book, err := LoadFromReader(strings.NewReader(yml), "test://default-currency")
	assertNoError(t, "LoadFromReader", err)
	// 找不到 currency 字段时默认 CNY
	_, ok := book.Lookup("qwen", "qwen3-max", KindInput)
	assertTrue(t, "默认币种应命中", ok)
}

func TestEntryFromMap_UnknownFieldIgnored(t *testing.T) {
	t.Parallel()
	// 注：yaml.v3 的 KnownFields(true) 对顶层结构体生效，对 map[string]any
	// 不生效（map 没有"未知字段"概念）。这里只确认多余字段不会让查找崩。
	yml := `
version: "v1"
entries:
  - {provider: qwen, model: qwen3-max, kind: input, unit_minor: 4, currency: CNY, unknown_field: oops}
`
	book, err := LoadFromReader(strings.NewReader(yml), "test://unknown-field")
	assertNoError(t, "未知字段在 map 模式下应忽略", err)
	cost, ok := book.Lookup("qwen", "qwen3-max", KindInput)
	assertTrue(t, "查找仍成功", ok)
	equal(t, "cost", cost.String(), "4")
}
