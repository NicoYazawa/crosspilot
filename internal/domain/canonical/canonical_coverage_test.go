package canonical

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件补齐 canonical 包的分支覆盖。每个用例都断言规范编码的**逐字节**结果，
// 而不是只断言「没有报错」：规范编码多一个字节、少一个转义，哈希就会分叉，
// 而「能编码出来」这件事本身说明不了任何问题。

// CoverageTagged 覆盖 json 标签的四种写法：改名加选项、只有选项、完全没有标签，
// 以及必须被跳过的 json:"-"；外加一个私有成员，验证它永远不参与编码。
type CoverageTagged struct {
	Named    string `json:"renamed,omitempty"`
	Bare     string `json:",omitempty"`
	Untagged string
	Skipped  string `json:"-"`
	private  string
}

// CoverageEmbedded 是被匿名嵌入的成员类型。名字必须首字母大写：
// 嵌入字段的字段名就是类型名，未导出类型会被反射当成私有成员跳过。
type CoverageEmbedded struct {
	B string `json:"b"`
}

// CoverageEmbedding 的匿名成员没有标签：按 encoding/json 的约定，
// JSON 名取类型名而不是字段名。
type CoverageEmbedding struct {
	CoverageEmbedded
	C string `json:"c"`
}

// CoverageEmbeddingTagged 的匿名成员带标签，名字必须取标签里的名字。
type CoverageEmbeddingTagged struct {
	CoverageEmbedded `json:"inner"`
	D                string `json:"d"`
}

// CoverageAllPrivate 没有任何可导出成员，规范编码必须是空对象 {}，
// 而不是 panic，也不是漏掉大括号。
type CoverageAllPrivate struct {
	hidden int
	note   string
}

// CoverageWithPointer 覆盖结构体里的指针成员：nil 写 null，非 nil 写指向的值。
type CoverageWithPointer struct {
	Count *int    `json:"count"`
	Note  *string `json:"note"`
}

// CoverageWithFloat 的成员含浮点：成员编码失败必须让整次编码失败，
// 不能写出半个对象之后当作成功返回。
type CoverageWithFloat struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
}

// CoverageWithChan 的成员属于无法规范编码的类型。
type CoverageWithChan struct {
	Ch chan int `json:"ch"`
}

// CoverageWithNumber 的成员是 json.Number：整数字面量必须原样输出成数字，
// 小数与指数必须被拒绝。
type CoverageWithNumber struct {
	Q json.Number `json:"q"`
}

// CoverageWithTime 的成员是 time.Time：嵌套在结构体里也要走规范的 Time。
type CoverageWithTime struct {
	At time.Time `json:"at"`
}

// CoverageNode 自引用，用来构造超过 maxDepth 的嵌套链。
type CoverageNode struct {
	Next *CoverageNode `json:"next"`
}

// coverageCachePayload 用于验证成员布局缓存：同一类型会被反复编码，
// 第二次起必须命中缓存并给出完全相同的字节。
type coverageCachePayload struct {
	OperationID string `json:"operation_id"`
	BuyerID     string `json:"buyer_id"`
	Quantity    int    `json:"quantity"`
}

func TestJSONEncodesScalarKinds(t *testing.T) {
	// 既有用例只覆盖了 true。false 是另一条分支：写反了会把「假」编成 true，
	// 两种内容拿到同一个摘要。无符号整数同理——负数走的是有符号分支，
	// 若把 uint 误当 int 处理，超过 int64 的值会变成负数。
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"真", true, "true"},
		{"假", false, "false"},
		{"int", int(-7), "-7"},
		{"int8", int8(-128), "-128"},
		{"int16", int16(-32768), "-32768"},
		{"int32", int32(-2147483648), "-2147483648"},
		{"int64 上限", int64(9223372036854775807), "9223372036854775807"},
		{"uint", uint(7), "7"},
		{"uint8 上限", uint8(255), "255"},
		{"uint16 上限", uint16(65535), "65535"},
		{"uint32 上限", uint32(4294967295), "4294967295"},
		{"uint64 上限", uint64(18446744073709551615), "18446744073709551615"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JSON(tc.in)
			if err != nil {
				t.Fatalf("编码 %v 失败：%v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Errorf("编码 %v = %s，期望 %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestJSONEncodesFixedSizeArrays(t *testing.T) {
	// 定长数组与切片共用一个分支，但数组没有「nil」的概念：若把数组也当成
	// 可空切片处理，长度为 0 的数组会被编成 null 而不是 []。
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"三元素数组", [3]int{1, 2, 3}, "[1,2,3]"},
		{"零长度数组", [0]int{}, "[]"},
		{"单元素数组", [1]string{"a"}, `["a"]`},
		{"字节数组按数字输出", [3]byte{'a', 'b', 'c'}, "[97,98,99]"},
		{"数组套数组", [2][2]string{{"a", "b"}, {"c", "d"}}, `[["a","b"],["c","d"]]`},
		{"数组与切片混用", []any{[2]int{1, 2}, []int{3}}, "[[1,2],[3]]"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JSON(tc.in)
			if err != nil {
				t.Fatalf("编码失败：%v", err)
			}
			if string(got) != tc.want {
				t.Errorf("编码 = %s，期望 %s", got, tc.want)
			}
		})
	}
}

func TestJSONUnwrapsPointersAndNilInterfaces(t *testing.T) {
	// 指针与接口必须先展开再判断类型。nil 指针若直接 Elem() 会 panic，
	// 若跳过不写又会让对象少一个成员——两者都会破坏哈希的可比性。
	count := 3
	note := "n"
	inner := struct {
		ID int `json:"id"`
	}{ID: 9}
	double := func() **int { p := &count; return &p }()

	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil 字符串指针", (*string)(nil), "null"},
		{"nil 切片指针", (*[]int)(nil), "null"},
		{"nil 映射指针", (*map[string]int)(nil), "null"},
		{"nil 结构体指针", (*CoverageNode)(nil), "null"},
		{"非 nil 指针指向标量", &count, "3"},
		{"非 nil 指针指向切片", &[]int{1, 2}, "[1,2]"},
		{"非 nil 指针指向映射", &map[string]int{"a": 1}, `{"a":1}`},
		{"非 nil 指针指向结构体", &inner, `{"id":9}`},
		{"多级指针", double, "3"},
		{"结构体指针成员为 nil", CoverageWithPointer{}, `{"count":null,"note":null}`},
		{"结构体指针成员非 nil", CoverageWithPointer{Count: &count, Note: &note}, `{"count":3,"note":"n"}`},
		// 切片元素里的 nil 接口：必须写 null 占位。跳过元素会改变下标语义，
		// 让 [1,null,3] 与 [1,3] 编成同一串字节。
		{"切片里的 nil 接口", []any{nil, 1, nil}, "[null,1,null]"},
		{"切片里的 nil 指针", []any{(*int)(nil)}, "[null]"},
		{"映射里的 nil 值", map[string]any{"a": nil}, `{"a":null}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JSON(tc.in)
			if err != nil {
				t.Fatalf("编码失败：%v", err)
			}
			if string(got) != tc.want {
				t.Errorf("编码 = %s，期望 %s", got, tc.want)
			}
		})
	}
}

func TestJSONRejectsUnsupportedKinds(t *testing.T) {
	// 无法精确表示的类型必须整次拒绝。悄悄跳过（写成 null 或省略）等于
	// 改变了被哈希的内容，而摘要照常给出——这类静默降级最难排查。
	ch := make(chan int)
	defer close(ch)

	cases := []struct {
		name string
		in   any
	}{
		{"通道", ch},
		{"nil 通道", (chan int)(nil)},
		{"函数", func() {}},
		{"uintptr", uintptr(1)},
		{"复数", complex(1, 2)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JSON(tc.in)
			if !errors.Is(err, ErrUnhashable) {
				t.Fatalf("应返回 ErrUnhashable，实际 err = %v，got = %s", err, got)
			}
			if got != nil {
				t.Errorf("编码失败时不应返回字节，实际 %s", got)
			}
			if err.Error() == "" {
				t.Error("错误信息不应为空")
			}
		})
	}
}

func TestJSONPropagatesNestedErrors(t *testing.T) {
	// 嵌套在里面的值出错，外层必须把错误原样抛上来：
	// 若只写出 [1] 就返回成功，一半的 JSON 会被当成合法内容参与哈希。
	weight := 1.5

	cases := []struct {
		name string
		in   any
	}{
		{"切片元素是浮点", []any{1, 2.5}},
		{"切片元素是通道", []any{make(chan int)}},
		{"嵌套切片里的浮点", []any{[]any{[]any{1.5}}}},
		{"映射值是浮点", map[string]any{"w": 1.5}},
		{"映射值嵌套浮点", map[string]any{"w": map[string]any{"v": 1.5}}},
		{"结构体成员是浮点", CoverageWithFloat{Name: "x", Weight: 1.5}},
		{"结构体成员是通道", CoverageWithChan{Ch: make(chan int)}},
		{"结构体指针成员指向浮点", struct {
			W *float64 `json:"w"`
		}{W: &weight}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JSON(tc.in)
			if !errors.Is(err, ErrUnhashable) {
				t.Fatalf("应返回 ErrUnhashable，实际 err = %v，got = %s", err, got)
			}
			if got != nil {
				t.Errorf("编码失败时不应返回字节，实际 %s", got)
			}
		})
	}
}

func TestJSONEncodesNilMapAsNull(t *testing.T) {
	// nil 映射与 nil 切片一样写 null，而空映射写 {}：
	// 二者不能混同，否则「没有这个对象」和「对象是空的」会得到同一摘要。
	var nilMap map[string]int
	got, err := JSON(nilMap)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	if string(got) != "null" {
		t.Errorf("nil 映射编码 = %s，期望 null", got)
	}

	got, err = JSON(map[string]any{"m": nilMap})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	if want := `{"m":null}`; string(got) != want {
		t.Errorf("编码 = %s，期望 %s", got, want)
	}

	got, err = JSON(map[string]int{})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	if string(got) != "{}" {
		t.Errorf("空映射编码 = %s，期望 {}", got)
	}
}

func TestJSONSortsManyMapKeys(t *testing.T) {
	// 插入顺序与字典序故意不一致：Go 的映射迭代顺序是随机的，
	// 只有显式排序才能让「同一份内容」每次编出同一串字节。
	in := map[string]int{
		"z": 26, "a": 1, "m": 13, "k10": 10, "k2": 2, "k1": 1, "B": 2, "b": 2,
	}
	want := `{"B":2,"a":1,"b":2,"k1":1,"k10":10,"k2":2,"m":13,"z":26}`

	for i := 0; i < 20; i++ {
		got, err := JSON(in)
		if err != nil {
			t.Fatalf("第 %d 次编码失败：%v", i, err)
		}
		if string(got) != want {
			t.Fatalf("第 %d 次编码 = %s，期望 %s", i, got, want)
		}
	}
}

func TestJSONNameResolutionFollowsJSONTag(t *testing.T) {
	// json 标签的选项（omitempty）本包不实现，但名字必须取逗号前的部分；
	// 只有选项没有名字时回退到字段名；没有标签时也用字段名。
	// 名字算错，等于换了一套成员名，摘要与其它实现就对不上了。
	got, err := JSON(CoverageTagged{Named: "n", Bare: "b", Untagged: "u", Skipped: "s", private: "p"})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	want := `{"Bare":"b","Untagged":"u","renamed":"n"}`
	if string(got) != want {
		t.Errorf("编码 = %s，期望 %s", got, want)
	}
	if strings.Contains(string(got), "Skipped") || strings.Contains(string(got), "private") {
		t.Errorf("被忽略的成员不应出现：%s", got)
	}
}

func TestJSONEncodesEmbeddedAndEmptyStructs(t *testing.T) {
	// 匿名成员的 JSON 名是类型名（带标签时取标签名）；没有可导出成员的结构体
	// 编成 {}。空对象必须是合法的规范输出，否则纯标记类型无法参与哈希。
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"没有可导出成员", CoverageAllPrivate{hidden: 1, note: "x"}, "{}"},
		{"空结构体", struct{}{}, "{}"},
		{"指针指向空结构体", &struct{}{}, "{}"},
		{"无标签匿名成员取类型名", CoverageEmbedding{CoverageEmbedded: CoverageEmbedded{B: "x"}, C: "y"}, `{"CoverageEmbedded":{"b":"x"},"c":"y"}`},
		{"带标签匿名成员取标签名", CoverageEmbeddingTagged{CoverageEmbedded: CoverageEmbedded{B: "x"}, D: "y"}, `{"d":"y","inner":{"b":"x"}}`},
		{"时间成员", CoverageWithTime{At: time.Date(2026, 9, 30, 1, 2, 3, 0, time.UTC)}, `{"at":"2026-09-30T01:02:03+00:00"}`},
		{"零值时间成员", CoverageWithTime{}, `{"at":"0001-01-01T00:00:00+00:00"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JSON(tc.in)
			if err != nil {
				t.Fatalf("编码失败：%v", err)
			}
			if string(got) != tc.want {
				t.Errorf("编码 = %s，期望 %s", got, tc.want)
			}
		})
	}
}

func TestJSONEscapesExactly(t *testing.T) {
	// 转义表必须逐字节钉死：少转义一个控制字符会写出非法 JSON，
	// 多转义一个普通字符会让同一份内容在两处得到不同字节。
	// 边界取 0x1f（转义）与 0x20（原样），防止把 < 写成 <=。
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"引号", `"`, `"\""`},
		{"反斜杠", `\`, `"\\"`},
		{"换行", "\n", `"\n"`},
		{"回车", "\r", `"\r"`},
		{"制表", "\t", `"\t"`},
		{"退格", "\b", `"\b"`},
		{"换页", "\f", `"\f"`},
		{"NUL", "\x00", `"\u0000"`},
		{"0x01", "\x01", `"\u0001"`},
		{"0x1f 仍转义", "\x1f", `"\u001f"`},
		{"0x20 不再转义", " ", `" "`},
		{"DEL 不转义", "\x7f", "\"\u007f\""},
		{"非 ASCII 不转义", "背包（大号）", `"背包（大号）"`},
		{"空字符串", "", `""`},
		{"混合", "a\"b\\c\nd\re\tf\bg\fh\x01i\x1f", `"a\"b\\c\nd\re\tf\bg\fh\u0001i\u001f"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JSON(tc.in)
			if err != nil {
				t.Fatalf("编码 %q 失败：%v", tc.in, err)
			}
			if string(got) != tc.want {
				t.Errorf("编码 %q = %s，期望 %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestJSONEscapesInsideMapKeys(t *testing.T) {
	// 映射的键是与值走同一套转义的另一条入口：键里带引号或换行时，
	// 若漏转义就会写出结构被破坏的 JSON。
	got, err := JSON(map[string]int{"a\"b": 1, "c\nd": 2, "e\\f": 3})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	want := `{"a\"b":1,"c\nd":2,"e\\f":3}`
	if string(got) != want {
		t.Errorf("编码 = %s，期望 %s", got, want)
	}
}

func TestTimeZeroValueAndFractionPadding(t *testing.T) {
	// 微秒部分不足六位要补零（.000010 不能写成 .10），
	// 亚微秒的纳秒截断到零后不能再输出小数点；零值时间与负偏移
	// （分钟非零）都必须稳定输出，否则同一条记录在两次采样间会换摘要。
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"零值时间", time.Time{}, "0001-01-01T00:00:00+00:00"},
		{"1 微秒补零", time.Date(2026, 9, 30, 1, 2, 3, 1234, time.UTC), "2026-09-30T01:02:03.000001+00:00"},
		{"10 微秒补零", time.Date(2026, 9, 30, 1, 2, 3, 10*1000, time.UTC), "2026-09-30T01:02:03.000010+00:00"},
		{"100 毫秒", time.Date(2026, 9, 30, 1, 2, 3, 100*1000*1000, time.UTC), "2026-09-30T01:02:03.100000+00:00"},
		{"亚微秒截断到零", time.Date(2026, 9, 30, 1, 2, 3, 100, time.UTC), "2026-09-30T01:02:03+00:00"},
		{"负偏移且分钟非零", time.Date(2026, 9, 30, 1, 2, 3, 0, time.FixedZone("NPT-neg", -(5*3600+1800))), "2026-09-30T01:02:03-05:30"},
		{"正偏移且分钟非零", time.Date(2026, 9, 30, 1, 2, 3, 0, time.FixedZone("NPT", 5*3600+2700)), "2026-09-30T01:02:03+05:45"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Time(tc.in); got != tc.want {
				t.Errorf("时间编码 = %s，期望 %s", got, tc.want)
			}
		})
	}

	// 截断到零微秒时输出里不能有小数点，否则同一时刻会出现两种写法
	got := Time(time.Date(2026, 9, 30, 1, 2, 3, 100, time.UTC))
	if strings.Contains(got, ".") {
		t.Errorf("微秒为零时不应有小数点：%s", got)
	}
}

func TestJSONNumberIntegersAnywhere(t *testing.T) {
	// json.Number 的底层类型是字符串，从映射、切片、结构体成员进来时
	// 都必须按整数字面量原样输出，而不是加上引号变成字符串——
	// 数据库读回来的数量与内存里构造的数量必须编出同一串字节。
	ok := []struct {
		name string
		in   any
		want string
	}{
		{"顶层整数", json.Number("2"), "2"},
		{"顶层负整数", json.Number("-15"), "-15"},
		{"顶层零", json.Number("0"), "0"},
		{"映射里的整数", map[string]any{"q": json.Number("2")}, `{"q":2}`},
		{"切片里的整数", []any{json.Number("0"), json.Number("12")}, "[0,12]"},
		{"结构体成员", CoverageWithNumber{Q: json.Number("3")}, `{"q":3}`},
		{"普通字符串仍是字符串", "2", `"2"`},
	}

	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			got, err := JSON(tc.in)
			if err != nil {
				t.Fatalf("编码失败：%v", err)
			}
			if string(got) != tc.want {
				t.Errorf("编码 = %s，期望 %s", got, tc.want)
			}
		})
	}

	// 小数与指数没有精确的整数表示，一律拒绝：接受它们等于把浮点放进了哈希
	for _, bad := range []string{"2.5", "1e3", "-1.5", "0.0", "abc", "", "0x10", "2 "} {
		t.Run("拒绝 "+bad, func(t *testing.T) {
			got, err := JSON(json.Number(bad))
			if !errors.Is(err, ErrUnhashable) {
				t.Fatalf("json.Number(%q) 应被拒绝，实际 err = %v，got = %s", bad, err, got)
			}
			if got != nil {
				t.Errorf("编码失败时不应返回字节，实际 %s", got)
			}
		})
	}

	// 通过结构体成员、映射、切片进来时同样要拒绝
	nested := []struct {
		name string
		in   any
	}{
		{"结构体成员", CoverageWithNumber{Q: json.Number("2.5")}},
		{"映射值", map[string]any{"q": json.Number("2.5")}},
		{"切片元素", []any{json.Number("2.5")}},
	}
	for _, tc := range nested {
		t.Run("嵌套拒绝 "+tc.name, func(t *testing.T) {
			if _, err := JSON(tc.in); !errors.Is(err, ErrUnhashable) {
				t.Fatalf("应返回 ErrUnhashable，实际 err = %v", err)
			}
		})
	}
}

func TestJSONRejectsNestingBeyondMaxDepth(t *testing.T) {
	// 自引用结构若没有深度上限，编码会一直递归到栈溢出。
	// 这里同时钉住边界：正好 64 层必须成功，第 65 层必须失败——
	// 只测「超深会失败」的话，把上限误设成 32 也不会被发现。
	const okDepth = 64

	var deep any = 1
	for i := 0; i < okDepth; i++ {
		deep = []any{deep}
	}
	got, err := JSON(deep)
	if err != nil {
		t.Fatalf("嵌套 %d 层应当成功，实际 err = %v", okDepth, err)
	}
	want := strings.Repeat("[", okDepth) + "1" + strings.Repeat("]", okDepth)
	if string(got) != want {
		t.Errorf("%d 层嵌套编码不正确（长度 %d，期望 %d）", okDepth, len(got), len(want))
	}

	deep = []any{deep} // 第 65 层
	got, err = JSON(deep)
	if !errors.Is(err, ErrUnhashable) {
		t.Fatalf("嵌套 %d 层应返回 ErrUnhashable，实际 err = %v，got = %s", okDepth+1, err, got)
	}
	if !strings.Contains(err.Error(), "64") {
		t.Errorf("错误信息应说明深度上限 64：%v", err)
	}
	if got != nil {
		t.Errorf("编码失败时不应返回字节，实际 %s", got)
	}
}

func TestJSONRejectsSelfReferentialStructChain(t *testing.T) {
	// 自引用结构体走的是另一条递归路径（结构体成员而不是切片元素），
	// 短链必须能正常编码，长链必须在上限处被拒绝而不是耗尽栈。
	short := &CoverageNode{Next: &CoverageNode{Next: &CoverageNode{}}}
	got, err := JSON(short)
	if err != nil {
		t.Fatalf("三层链编码失败：%v", err)
	}
	if want := `{"next":{"next":{"next":null}}}`; string(got) != want {
		t.Errorf("三层链编码 = %s，期望 %s", got, want)
	}

	const chainLength = 70
	nodes := make([]*CoverageNode, chainLength)
	for i := range nodes {
		nodes[i] = &CoverageNode{}
	}
	for i := 0; i < chainLength-1; i++ {
		nodes[i].Next = nodes[i+1]
	}

	if _, err := JSON(nodes[0]); !errors.Is(err, ErrUnhashable) {
		t.Fatalf("%d 层自引用链应返回 ErrUnhashable，实际 err = %v", chainLength, err)
	}
}

func TestHashPropagatesUnhashableError(t *testing.T) {
	// 值不可编码时 Hash 必须返回空摘要加错误。返回「空串 + nil 错误」会让
	// 调用方把空串当成合法摘要写进账本，幂等键随之全部相同。
	bad := []struct {
		name string
		in   any
	}{
		{"浮点", 1.5},
		{"通道", make(chan int)},
		{"函数", func() {}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			sum, err := Hash(tc.in)
			if !errors.Is(err, ErrUnhashable) {
				t.Fatalf("应返回 ErrUnhashable，实际 err = %v", err)
			}
			if sum != "" {
				t.Errorf("出错时摘要应为空串，实际 %q", sum)
			}
		})
	}

	// 可编码的值必须给出 64 位十六进制摘要，且与规范字节完全对应
	sum, err := Hash(json.Number("2"))
	if err != nil {
		t.Fatalf("哈希失败：%v", err)
	}
	if want := HashBytes([]byte("2")); sum != want {
		t.Errorf("Hash(json.Number(\"2\")) = %s，期望 %s", sum, want)
	}
}

func TestJSONReusesCachedFieldLayout(t *testing.T) {
	// 成员布局缓存命中时必须给出与首次完全相同的字节与顺序：
	// 若缓存里存的是未排序的切片，第二次编码就会换一个成员顺序。
	payload := coverageCachePayload{OperationID: "op-1", BuyerID: "b-1", Quantity: 2}
	want := `{"buyer_id":"b-1","operation_id":"op-1","quantity":2}`

	first, err := JSON(payload)
	if err != nil {
		t.Fatalf("第一次编码失败：%v", err)
	}
	if string(first) != want {
		t.Fatalf("第一次编码 = %s，期望 %s", first, want)
	}

	for i := 0; i < 5; i++ {
		next, err := JSON(payload)
		if err != nil {
			t.Fatalf("第 %d 次编码失败：%v", i, err)
		}
		if string(next) != want {
			t.Fatalf("第 %d 次编码 = %s，期望 %s", i, next, want)
		}
	}

	// 直接问一次缓存：命中时返回的仍是同一个已排序的成员切片
	cached := fieldsOf(reflect.TypeOf(payload))
	if len(cached) != 3 {
		t.Fatalf("缓存的成员数应为 3，实际 %d", len(cached))
	}
	if names := cached[0].name + "," + cached[1].name + "," + cached[2].name; names != "buyer_id,operation_id,quantity" {
		t.Errorf("缓存的成员顺序 = %s，期望 buyer_id,operation_id,quantity", names)
	}
}

func TestJSONFieldCacheIsConcurrencySafe(t *testing.T) {
	// 交易账本的哈希发生在并发事务里，成员布局缓存会被多个 goroutine 同时读写。
	// 这个用例配合 -race 运行：既验证并发下结果一致，也验证缓存加锁没有遗漏。
	// 类型只在函数内声明，保证这次编码一定从「缓存未命中」开始。
	type concurrentPayload struct {
		OperationID string `json:"operation_id"`
		BuyerID     string `json:"buyer_id"`
		Quantity    int    `json:"quantity"`
	}

	payload := concurrentPayload{OperationID: "op-1", BuyerID: "b-1", Quantity: 2}
	want := `{"buyer_id":"b-1","operation_id":"op-1","quantity":2}`

	const goroutines = 16
	const rounds = 25

	var wg sync.WaitGroup
	failures := make(chan string, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				got, err := JSON(payload)
				if err != nil {
					failures <- "编码失败：" + err.Error()
					return
				}
				if string(got) != want {
					failures <- "编码 = " + string(got) + "，期望 " + want
					return
				}
			}
		}()
	}

	wg.Wait()
	close(failures)
	for msg := range failures {
		t.Error(msg)
	}
}
