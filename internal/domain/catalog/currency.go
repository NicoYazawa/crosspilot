package catalog

import (
	"fmt"
	"strings"
)

// Currency 是 ISO 4217 的三位大写字母币种代码。
type Currency string

// 两位小数位的币种，也是 ISO 4217 的默认小数位。
const (
	USD Currency = "USD"
	EUR Currency = "EUR"
	CNY Currency = "CNY"
	GBP Currency = "GBP"
)

// 零小数位的币种，金额不带小数点。
const (
	JPY Currency = "JPY"
	KRW Currency = "KRW"
	VND Currency = "VND"
)

// 三位小数位的币种。
const (
	BHD Currency = "BHD"
	IQD Currency = "IQD"
	JOD Currency = "JOD"
	KWD Currency = "KWD"
	LYD Currency = "LYD"
	OMR Currency = "OMR"
	TND Currency = "TND"
)

// defaultScale 是未在 scaleOverrides 中列出的币种的小数位数。
const defaultScale = 2

// scaleOverrides 列出标准小数位不是两位的币种。
var scaleOverrides = map[Currency]int{
	JPY: 0,
	KRW: 0,
	VND: 0,

	BHD: 3,
	IQD: 3,
	JOD: 3,
	KWD: 3,
	LYD: 3,
	OMR: 3,
	TND: 3,
}

// ParseCurrency 解析币种代码。大小写不敏感，两侧空白会被裁掉。
// 代码形状不合法时返回 ErrInvalidCurrency。
func ParseCurrency(s string) (Currency, error) {
	c := Currency(strings.ToUpper(strings.TrimSpace(s)))
	if !c.Valid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidCurrency, s)
	}
	return c, nil
}

// Valid 报告该币种是否为合法的三位大写字母代码。
//
// 只校验形状，不比对 ISO 4217 全表：收录新币种不应导致既有数据解析失败。
func (c Currency) Valid() bool {
	if len(c) != 3 {
		return false
	}
	for i := 0; i < len(c); i++ {
		if c[i] < 'A' || c[i] > 'Z' {
			return false
		}
	}
	return true
}

// Scale 返回该币种金额应保留的小数位数，可直接用作 decimal 的舍入位数。
func (c Currency) Scale() int {
	if n, ok := scaleOverrides[c]; ok {
		return n
	}
	return defaultScale
}

// String 实现 fmt.Stringer。
func (c Currency) String() string { return string(c) }
