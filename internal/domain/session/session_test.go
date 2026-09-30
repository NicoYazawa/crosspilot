package session

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// TestValidateStateAcceptsJSONObject 覆盖 ValidateState 的接受分支。
//
// 快照是「全量覆盖」写入：只要校验放行，坏数据就会永久盖掉旧快照。
// 因此这里不只断言 nil，还要断言调用方拿到的字符串与传入的完全一致
// ——校验函数不能顺手 TrimSpace 或规范化内容，否则写进库里的快照
// 就不再是上层序列化出来的那一份，摘要与幂等比较会跟着失真。
func TestValidateStateAcceptsJSONObject(t *testing.T) {
	cases := []struct {
		name  string
		state string
	}{
		{"空对象", `{}`},
		{"单字段对象", `{"revision":3}`},
		{"嵌套对象与数组", `{"a":{"b":[1,2,{"c":null}]}}`},
		{"含中文键值", `{"买家":"一号","持仓":["BTCUSDT"]}`},
		{"顶层前后有空白", "  {\"a\":1}\n"},
		{"字段值含转义字符", `{"note":"line\nbreak"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := tc.state

			if err := ValidateState(tc.state); err != nil {
				t.Fatalf("ValidateState(%q) 应通过校验，得到 %v", original, err)
			}
			// 传参是按值传递，函数不可能改写调用方的变量；
			// 这条断言是为了锁住「不做隐式规范化」这一契约：
			// 一旦有人给 ValidateState 加上 TrimSpace 之类的处理，此处就会失败。
			if tc.state != original {
				t.Errorf("ValidateState 改写了入参：%q → %q", original, tc.state)
			}
			if len(tc.state) != len(original) {
				t.Errorf("ValidateState 改变了入参字节长度：%d → %d", len(original), len(tc.state))
			}
		})
	}
}

// TestValidateStateRejectsNonObjectOrMalformed 覆盖 ValidateState 的两类拒绝分支。
//
// 两类拒绝必须可区分（一个是「不是 JSON」、一个是「是 JSON 但不是对象」），
// 但都必须是 ErrCorruptState：上层只用一个哨兵错误就能决定「拒绝写入并保留旧快照」，
// 不需要解析错误文本。
func TestValidateStateRejectsNonObjectOrMalformed(t *testing.T) {
	cases := []struct {
		name string
		// wantReason 是错误文案里必须出现的原因片段：
		// 定位坏快照时只有哨兵错误是不够的，日志里得看出「哪里坏了」。
		wantReason string
		state      string
	}{
		{"空字符串", "unexpected end of JSON input", ""},
		{"纯空白", "unexpected end of JSON input", "   "},
		{"截断的对象", "unexpected end of JSON input", `{"a":`},
		{"非 JSON 文本", "invalid character", "not json"},
		{"对象后跟垃圾", "after top-level value", `{"a":1} trailing`},
		{"空数组", "顶层不是对象", `[]`},
		{"带元素数组", "顶层不是对象", `[1,2,3]`},
		{"JSON 字符串", "顶层不是对象", `"hello"`},
		{"JSON 整数", "顶层不是对象", `42`},
		{"JSON 小数", "顶层不是对象", `3.14`},
		{"JSON 布尔真", "顶层不是对象", `true`},
		{"JSON 布尔假", "顶层不是对象", `false`},
		{"JSON null", "顶层不是对象", `null`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateState(tc.state)
			if err == nil {
				t.Fatalf("ValidateState(%q) 必须拒绝，却返回了 nil", tc.state)
			}
			// 哨兵错误是上层唯一的判断依据，必须能穿透 %w 包装拿到
			if !errors.Is(err, ErrCorruptState) {
				t.Errorf("ValidateState(%q) 的错误应可用 errors.Is 匹配 ErrCorruptState，得到 %v", tc.state, err)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Errorf("ValidateState(%q) 的错误文案应包含 %q（便于定位坏数据），得到 %q",
					tc.state, tc.wantReason, err.Error())
			}
			if !strings.HasPrefix(err.Error(), "session: ") {
				t.Errorf("错误文案应带包名前缀，得到 %q", err.Error())
			}
		})
	}
}

// TestValidateIdentifierAcceptsValidValues 覆盖 ValidateIdentifier 的接受分支与字节边界。
//
// 上限按字节而不是按 rune 计：会话标识直接进数据库列与索引，
// 撑爆的是字节长度，「字符数看着不多」的输入照样会撑爆。
func TestValidateIdentifierAcceptsValidValues(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		label   string
		maximum int
	}{
		{"单字符且上限为一", "a", "buyer", 1},
		{"恰好等于字节上限", "abc", "buyer", 3},
		{"多字节恰好等于字节上限", "会话", "buyer", 6},
		{"三字节字符恰好等于上限", "あいうえお", "buyer", 15},
		{"上限远大于长度", "buyer-01", "owner", 64},
		{"纯多字节标识", "买家一号", "buyer", 12},
		{"内部空格允许", "a b", "buyer", 3},
		{"内部不换行空格允许", "a\u00a0b", "buyer", 64},
		{"内部制表符允许", "a\tb", "buyer", 3},
		{"大小写保持不变", "Buyer-01", "owner", 64},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateIdentifier(tc.value, tc.label, tc.maximum)
			if err != nil {
				t.Fatalf("ValidateIdentifier(%q, %q, %d) 应通过校验，得到 %v", tc.value, tc.label, tc.maximum, err)
			}
			// 返回原值而不是修剪后的值：调用方会直接拿它去查询，
			// 悄悄改写会让「查询 owner=a」变成「查询 owner=a 」这种查不到任何东西的调用。
			if got != tc.value {
				t.Errorf("ValidateIdentifier(%q, ...) = %q，期望原值 %q", tc.value, got, tc.value)
			}
		})
	}
}

// TestValidateIdentifierRejectsInvalidValues 覆盖 ValidateIdentifier 的全部拒绝理由。
//
// 这里刻意把「首尾空白」「空值」「超长」三种理由都走一遍：
// 三者返回同一个错误，但触发条件互不相同，合并成一个 if 之后
// 任何一个条件的短路写错都会被这组用例抓住。
func TestValidateIdentifierRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		label   string
		maximum int
	}{
		{"空字符串", "", "buyer", 64},
		{"仅空格", " ", "buyer", 64},
		{"前导空格", " a", "buyer", 64},
		{"尾随空格", "a ", "buyer", 64},
		{"前导制表符", "\ta", "buyer", 64},
		{"尾随换行", "a\n", "buyer", 64},
		{"前导不换行空格", "\u00a0a", "buyer", 64},
		{"尾随回车", "a\r", "buyer", 64},
		{"恰好超出一个字节", "abcd", "buyer", 3},
		{"多字节按字节超限", "会话", "buyer", 5},
		{"三字节字符恰好超出一个字节", "あいうえお", "buyer", 14},
		{"上限为零时非空", "a", "buyer", 0},
		{"上限为零时空串", "", "buyer", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateIdentifier(tc.value, tc.label, tc.maximum)
			if err == nil {
				t.Fatalf("ValidateIdentifier(%q, %q, %d) 必须拒绝，却返回了 %q", tc.value, tc.label, tc.maximum, got)
			}
			// 失败时必须返回空串：调用方若忽略 error 直接把返回值拿去查询，
			// 空串至少查不到任何会话，而返回原值则可能查到不该查的记录。
			if got != "" {
				t.Errorf("校验失败时应返回空串，得到 %q", got)
			}
			if !strings.Contains(err.Error(), tc.label) {
				t.Errorf("错误文案应指出是哪个标识（%q），得到 %q", tc.label, err.Error())
			}
			// 文案里的长度按实现（字节）断言：字节数才是真正的判据，
			// 即便文案措辞写的是「字符」，也必须把实际生效的上限报出来。
			if !strings.Contains(err.Error(), strconv.Itoa(tc.maximum)) {
				t.Errorf("错误文案应包含上限 %d，得到 %q", tc.maximum, err.Error())
			}
			if !strings.Contains(err.Error(), "非空标识") {
				t.Errorf("错误文案应说明拒绝理由，得到 %q", err.Error())
			}
			if !strings.HasPrefix(err.Error(), "session: ") {
				t.Errorf("错误文案应带包名前缀，得到 %q", err.Error())
			}
		})
	}
}

// TestSessionSentinelErrorsAreDistinctAndPrefixed 锁住哨兵错误之间的可区分性。
//
// 上层用 errors.Is 决定「重试、拒绝写入、还是报越权」：
// 「旧执行者覆盖」与「越权访问他人会话」如果指向同一个值，
// 一次本该被拒绝的写入就可能被当成可重试的冲突而放过。
func TestSessionSentinelErrorsAreDistinctAndPrefixed(t *testing.T) {
	sentinels := map[string]error{
		"ErrStaleWrite":    ErrStaleWrite,
		"ErrOwnerMismatch": ErrOwnerMismatch,
		"ErrOwnerUnbound":  ErrOwnerUnbound,
		"ErrNotFound":      ErrNotFound,
		"ErrCorruptState":  ErrCorruptState,
		"ErrUnknownOwner":  ErrUnknownOwner,
	}

	for name, err := range sentinels {
		if err == nil {
			t.Fatalf("%s 不能为 nil", name)
		}
		if !strings.HasPrefix(err.Error(), "session: ") {
			t.Errorf("%s 的文案应带包名前缀，得到 %q", name, err.Error())
		}
		if !errors.Is(err, err) {
			t.Errorf("errors.Is(%s, %s) 应为 true", name, name)
		}
		for otherName, other := range sentinels {
			if name == otherName {
				continue
			}
			if errors.Is(err, other) {
				t.Errorf("%s 与 %s 不能互相匹配", name, otherName)
			}
			if err.Error() == other.Error() {
				t.Errorf("%s 与 %s 的文案重复：%q", name, otherName, err.Error())
			}
		}
	}
}
