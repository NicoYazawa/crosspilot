package canonical

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestJSONSortsObjectMembers(t *testing.T) {
	// 哈希的对象成员顺序不能取决于声明顺序，否则换个结构体就换一个摘要
	type payload struct {
		Quantity int    `json:"quantity"`
		Action   string `json:"action"`
		Title    string `json:"title"`
	}

	got, err := JSON(payload{Action: "create", Title: "背包", Quantity: 2})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	want := `{"action":"create","quantity":2,"title":"背包"}`
	if string(got) != want {
		t.Errorf("规范编码 = %s，期望 %s", got, want)
	}
}

func TestJSONHasNoWhitespace(t *testing.T) {
	got, err := JSON(map[string]any{"a": []int{1, 2, 3}, "b": map[string]any{"c": true}})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	want := `{"a":[1,2,3],"b":{"c":true}}`
	if string(got) != want {
		t.Errorf("规范编码 = %s，期望 %s", got, want)
	}
	if strings.ContainsAny(string(got), " \n\t") {
		t.Errorf("规范编码不应含空白：%s", got)
	}
}

func TestJSONKeepsNonASCIIUnescaped(t *testing.T) {
	// 转义成 \uXXXX 会改变字节序列，非 ASCII 标题必须原样输出
	got, err := JSON(map[string]string{"title": "防水登山包（大号）"})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	want := `{"title":"防水登山包（大号）"}`
	if string(got) != want {
		t.Errorf("规范编码 = %s，期望 %s", got, want)
	}
	if strings.Contains(string(got), `\u`) {
		t.Errorf("非 ASCII 字符被转义了：%s", got)
	}
}

func TestJSONEscapesControlCharacters(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`quote " backslash \`, `"quote \" backslash \\"`},
		{"line\nbreak", `"line\nbreak"`},
		{"tab\there", `"tab\there"`},
		{"bell\x07", `"bell\u0007"`},
	}

	for _, tc := range cases {
		got, err := JSON(tc.in)
		if err != nil {
			t.Fatalf("编码 %q 失败：%v", tc.in, err)
		}
		if string(got) != tc.want {
			t.Errorf("编码 %q = %s，期望 %s", tc.in, got, tc.want)
		}
	}
}

func TestJSONIsStableAcrossRuns(t *testing.T) {
	// 同一份内容两次编码必须逐字节一致；映射的迭代顺序在 Go 里是随机的，
	// 这条断言正是用来发现「忘了排序」的
	value := map[string]any{
		"operation_id": "op-1", "buyer_id": "buyer-1", "session_id": "s-1",
		"items": []any{
			map[string]any{"sku_id": "s-b", "quantity": 1},
			map[string]any{"sku_id": "s-a", "quantity": 2},
		},
	}

	first, err := JSON(value)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	for i := 0; i < 50; i++ {
		next, err := JSON(value)
		if err != nil {
			t.Fatalf("第 %d 次编码失败：%v", i, err)
		}
		if !bytes.Equal(next, first) {
			t.Fatalf("第 %d 次编码不一致：\n%s\n%s", i, first, next)
		}
	}
}

func TestJSONRejectsFloats(t *testing.T) {
	// 浮点没有精确十进制表示，进哈希就等于让摘要随二进制舍入漂移
	if _, err := JSON(map[string]any{"weight": 1.5}); err == nil {
		t.Fatal("浮点应当被拒绝，实际编码成功")
	}
	if _, err := JSON(3.14); err == nil {
		t.Fatal("浮点应当被拒绝，实际编码成功")
	}
}

func TestJSONRejectsNonStringMapKeys(t *testing.T) {
	if _, err := JSON(map[int]string{1: "a"}); err == nil {
		t.Fatal("非字符串键应当被拒绝，实际编码成功")
	}
}

func TestJSONHandlesNilAndEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil 接口", nil, "null"},
		{"nil 切片", []string(nil), "null"},
		{"空切片", []string{}, "[]"},
		{"空映射", map[string]string{}, "{}"},
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

func TestJSONSkipsUntaggedPrivateAndIgnoredFields(t *testing.T) {
	type mixed struct {
		Public  string `json:"public"`
		ignored string //nolint:unused // 私有成员由反射跳过，这里只为验证行为
		Skipped string `json:"-"`
	}

	got, err := JSON(mixed{Public: "x", ignored: "y", Skipped: "z"})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	if string(got) != `{"public":"x"}` {
		t.Errorf("编码 = %s，期望 {\"public\":\"x\"}", got)
	}
}

func TestJSONAcceptsJSONNumberIntegers(t *testing.T) {
	// 从数据库读回来的 JSON 用 json.Number 保留整数字面量，
	// 若退化成 float64，哈希就会在两处对同一笔交易算出不同摘要
	got, err := JSON(map[string]any{"quantity": json.Number("2")})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	if string(got) != `{"quantity":2}` {
		t.Errorf("编码 = %s，期望 {\"quantity\":2}", got)
	}

	if _, err := JSON(map[string]any{"quantity": json.Number("2.5")}); err == nil {
		t.Error("小数形式的 json.Number 应当被拒绝")
	}
}

func TestHashIsHexSHA256(t *testing.T) {
	// 已知向量：空字符串的 SHA-256 是公开常量，用它确认输出格式
	if got := HashBytes(nil); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("空输入哈希 = %s", got)
	}

	got, err := Hash(map[string]string{"action": "create"})
	if err != nil {
		t.Fatalf("哈希失败：%v", err)
	}
	want, err := Hash(map[string]string{"action": "create"})
	if err != nil {
		t.Fatalf("哈希失败：%v", err)
	}
	if got != want {
		t.Errorf("同一内容两次哈希不一致：%s / %s", got, want)
	}
	if len(got) != 64 {
		t.Errorf("哈希长度 = %d，期望 64", len(got))
	}

	other, err := Hash(map[string]string{"action": "cancel"})
	if err != nil {
		t.Fatalf("哈希失败：%v", err)
	}
	if got == other {
		t.Error("不同内容不应得到相同哈希")
	}
}

func TestTimeTruncatesToMicrosecond(t *testing.T) {
	moment := time.Date(2026, 9, 30, 1, 2, 3, 123456789, time.UTC)
	if got, want := Time(moment), "2026-09-30T01:02:03.123456+00:00"; got != want {
		t.Errorf("时间编码 = %s，期望 %s", got, want)
	}
}

func TestTimeOmitsZeroMicroseconds(t *testing.T) {
	moment := time.Date(2026, 9, 30, 1, 2, 3, 0, time.UTC)
	if got, want := Time(moment), "2026-09-30T01:02:03+00:00"; got != want {
		t.Errorf("时间编码 = %s，期望 %s", got, want)
	}
}

func TestTimeKeepsOffset(t *testing.T) {
	shanghai := time.FixedZone("CST", 8*3600)
	moment := time.Date(2026, 9, 30, 9, 2, 3, 0, shanghai)
	if got, want := Time(moment), "2026-09-30T09:02:03+08:00"; got != want {
		t.Errorf("时间编码 = %s，期望 %s", got, want)
	}

	west := time.FixedZone("PST", -8*3600)
	got := Time(time.Date(2026, 9, 30, 9, 2, 3, 0, west))
	if want := "2026-09-30T09:02:03-08:00"; got != want {
		t.Errorf("时间编码 = %s，期望 %s", got, want)
	}
}

func TestTimeHalfHourOffset(t *testing.T) {
	kolkata := time.FixedZone("IST", 5*3600+1800)
	got := Time(time.Date(2026, 9, 30, 9, 2, 3, 0, kolkata))
	if want := "2026-09-30T09:02:03+05:30"; got != want {
		t.Errorf("时间编码 = %s，期望 %s", got, want)
	}
}

func TestJSONEncodesTimeLikeCanonicalTime(t *testing.T) {
	moment := time.Date(2026, 9, 30, 1, 2, 3, 0, time.UTC)
	got, err := JSON(map[string]any{"expires_at": moment})
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	if want := `{"expires_at":"2026-09-30T01:02:03+00:00"}`; string(got) != want {
		t.Errorf("编码 = %s，期望 %s", got, want)
	}
}
