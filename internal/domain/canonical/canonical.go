// Package canonical 把值编码成可用于哈希的规范形式。
//
// 交易账本的幂等键与确认单摘要都建立在一段字节串之上：同一份业务内容在不同
// 时间、不同进程、乃至不同语言的实现里都必须编码成同一串字节，否则「内容没变」
// 与「摘要没变」就会分叉，哈希比较随之失去意义。
//
// 本包因此不做通用序列化，只做哈希用的规范编码：
//
//   - 对象成员按键升序排列（不是按结构体声明顺序）；
//   - 不输出任何多余空白；
//   - 非 ASCII 字符原样输出，不转义成 \uXXXX；
//   - 只接受能精确表示的数据，浮点一律拒绝——哈希不能用近似值。
//
// 时间另有 CanonicalTime：纳秒会被截断到微秒，因为更细的精度一旦进入哈希，
// 就会让「同一时刻」在两次采样间产生不同摘要。
package canonical

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrUnhashable 表示值含有无法规范编码的成分（浮点、非字符串键的映射、
// 不支持的字段类型等）。这类值不能被悄悄跳过：漏掉一个字段就等于改变了
// 被哈希的内容，而摘要却照常给出。
var ErrUnhashable = fmt.Errorf("canonical: 值无法规范编码")

// JSON 返回 v 的规范 JSON 编码。
//
// 结构体按 json 标签取成员名（无标签则用字段名），并按成员名升序输出；
// 映射的键必须是字符串。
func JSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encode(&buf, reflect.ValueOf(v), 0); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Hash 返回 v 的规范编码的 SHA-256，以十六进制小写表示。
func Hash(v any) (string, error) {
	encoded, err := JSON(v)
	if err != nil {
		return "", err
	}
	return HashBytes(encoded), nil
}

// HashBytes 返回一段字节的 SHA-256，以十六进制小写表示。
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Time 把时间编码成规范化字符串。
//
// 规则与哈希场景的要求一致：保留到微秒（纳秒截断），偏移量写成 ±HH:MM，
// UTC 写作 +00:00。
//
// 注意它保留时间自身的时区，不做换算：2026-09-09T08:05:00Z 与
// 2026-09-09T16:05:00+08:00 是同一个时刻，却会得到两个不同字符串。
// 因此凡是进入哈希的时间**必须先归一为 UTC**——否则同一笔交易在
// 「内存里算一次、从数据库读回来再算一次」会得到两个摘要，
// 而数据库读回的时间携带的是数据库会话的时区。
func Time(t time.Time) string {
	t = t.Truncate(time.Microsecond)
	base := t.Format("2006-01-02T15:04:05")

	var buf strings.Builder
	buf.Grow(len(base) + len("+00:00") + 7)
	buf.WriteString(base)
	if micro := t.Nanosecond() / 1000; micro != 0 {
		buf.WriteByte('.')
		buf.WriteString(pad6(micro))
	}

	_, offset := t.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	buf.WriteString(sign)
	buf.WriteString(pad2(offset / 3600))
	buf.WriteByte(':')
	buf.WriteString(pad2(offset % 3600 / 60))
	return buf.String()
}

// maxDepth 限制递归深度，防止自引用结构把编码过程拖死。
const maxDepth = 64

func encode(buf *bytes.Buffer, v reflect.Value, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: 嵌套超过 %d 层", ErrUnhashable, maxDepth)
	}

	// 接口与指针要先展开：判断具体类型的逻辑只写一遍
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			buf.WriteString("null")
			return nil
		}
		v = v.Elem()
	}

	if v.Kind() == reflect.Invalid {
		buf.WriteString("null")
		return nil
	}

	if v.Kind() == reflect.Struct && v.CanInterface() {
		if moment, ok := v.Interface().(time.Time); ok {
			writeString(buf, Time(moment))
			return nil
		}
	}

	switch v.Kind() {
	case reflect.Bool:
		if v.Bool() {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil

	case reflect.String:
		// json.Number 的底层类型是字符串，但它承载的是数字字面量
		if v.CanInterface() {
			if number, ok := v.Interface().(json.Number); ok {
				if _, err := strconv.ParseInt(number.String(), 10, 64); err != nil {
					return fmt.Errorf("%w: json.Number %q 不是整数", ErrUnhashable, number.String())
				}
				buf.WriteString(number.String())
				return nil
			}
		}
		writeString(buf, v.String())
		return nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		buf.WriteString(strconv.FormatInt(v.Int(), 10))
		return nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		buf.WriteString(strconv.FormatUint(v.Uint(), 10))
		return nil

	case reflect.Float32, reflect.Float64:
		return fmt.Errorf("%w: 浮点 %v 不能进入哈希（十进制金额请用定点类型）", ErrUnhashable, v.Float())

	case reflect.Slice, reflect.Array:
		return encodeSlice(buf, v, depth)

	case reflect.Map:
		return encodeMap(buf, v, depth)

	case reflect.Struct:
		return encodeStruct(buf, v, depth)

	default:
		return fmt.Errorf("%w: 不支持的类型 %s", ErrUnhashable, v.Type())
	}
}

// encodeSlice 写出数组或切片。
func encodeSlice(buf *bytes.Buffer, v reflect.Value, depth int) error {
	if v.Kind() == reflect.Slice && v.IsNil() {
		buf.WriteString("null")
		return nil
	}
	buf.WriteByte('[')
	for i := 0; i < v.Len(); i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := encode(buf, v.Index(i), depth+1); err != nil {
			return err
		}
	}
	buf.WriteByte(']')
	return nil
}

func encodeMap(buf *bytes.Buffer, v reflect.Value, depth int) error {
	if v.IsNil() {
		buf.WriteString("null")
		return nil
	}
	if v.Type().Key().Kind() != reflect.String {
		return fmt.Errorf("%w: 映射键必须是字符串，实际 %s", ErrUnhashable, v.Type().Key())
	}

	keys := v.MapKeys()
	sorted := make([]string, len(keys))
	for i, key := range keys {
		sorted[i] = key.String()
	}
	sort.Strings(sorted)

	buf.WriteByte('{')
	for i, key := range sorted {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeString(buf, key)
		buf.WriteByte(':')
		if err := encode(buf, v.MapIndex(reflect.ValueOf(key).Convert(v.Type().Key())), depth+1); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

func encodeStruct(buf *bytes.Buffer, v reflect.Value, depth int) error {
	fields := fieldsOf(v.Type())

	buf.WriteByte('{')
	for i, field := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeString(buf, field.name)
		buf.WriteByte(':')
		if err := encode(buf, v.Field(field.index), depth+1); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

// field 是一个参与编码的结构体成员。
type field struct {
	name  string
	index int
}

// fieldsCache 缓存结构体的成员布局：同一类型在一次运行里会被编码很多次。
// 交易账本的哈希计算发生在并发事务里，因此读写都要加锁。
var (
	fieldsMu    sync.RWMutex
	fieldsCache = map[reflect.Type][]field{}
)

func fieldsOf(t reflect.Type) []field {
	fieldsMu.RLock()
	cached, ok := fieldsCache[t]
	fieldsMu.RUnlock()
	if ok {
		return cached
	}

	fields := make([]field, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		structural := t.Field(i)
		if !structural.IsExported() {
			continue
		}
		name, skip := jsonName(structural)
		if skip {
			continue
		}
		fields = append(fields, field{name: name, index: i})
	}
	sort.Slice(fields, func(a, b int) bool { return fields[a].name < fields[b].name })

	fieldsMu.Lock()
	fieldsCache[t] = fields
	fieldsMu.Unlock()
	return fields
}

// jsonName 取成员在 JSON 中的名字，遵循 encoding/json 的基本约定。
func jsonName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", true
	}
	if tag != "" {
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			return name, false
		}
	}
	// 无标签时与 encoding/json 一致：字段名原样使用
	return f.Name, false
}

// writeString 写出一个 JSON 字符串字面量。
//
// 转义集合与 encoding/json 的默认行为一致：控制字符、引号与反斜杠转义，
// 其余字符（含非 ASCII）原样输出。
func writeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			if r < 0x20 {
				// 其余控制字符用 \u00XX，与 JSON 规范一致
				buf.WriteString(`\u`)
				const hexDigits = "0123456789abcdef"
				buf.WriteByte(hexDigits[(r>>12)&0xf])
				buf.WriteByte(hexDigits[(r>>8)&0xf])
				buf.WriteByte(hexDigits[(r>>4)&0xf])
				buf.WriteByte(hexDigits[r&0xf])
				continue
			}
			// 非 ASCII 字符原样输出：转义成 \uXXXX 会让「同一份内容」
			// 在不同实现里产生不同字节，哈希随之分叉
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func pad6(n int) string {
	out := strconv.Itoa(n)
	for len(out) < 6 {
		out = "0" + out
	}
	return out
}
