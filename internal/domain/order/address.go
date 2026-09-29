package order

import (
	"fmt"
	"strings"
)

// Address 是收货地址。
type Address struct {
	Recipient  string
	Phone      string
	Country    string // ISO 3166-1 alpha-2，两位大写字母
	Province   string
	City       string
	Line1      string
	Line2      string
	PostalCode string
}

// Validate 报告地址是否满足约束。
func (a Address) Validate() error {
	if strings.TrimSpace(a.Recipient) == "" {
		return fmt.Errorf("%w: 收货人为空", ErrInvalidOrder)
	}
	if strings.TrimSpace(a.Phone) == "" {
		return fmt.Errorf("%w: 联系电话为空", ErrInvalidOrder)
	}
	if !validCountryCode(a.Country) {
		return fmt.Errorf("%w: 国家代码 %q 非法的两位大写字母", ErrInvalidOrder, a.Country)
	}
	if strings.TrimSpace(a.City) == "" {
		return fmt.Errorf("%w: 城市为空", ErrInvalidOrder)
	}
	if strings.TrimSpace(a.Line1) == "" {
		return fmt.Errorf("%w: 详细地址为空", ErrInvalidOrder)
	}
	return nil
}

// validCountryCode 校验 ISO 3166-1 alpha-2 代码的形状。
// 只校验形状，不比对全表。
func validCountryCode(code string) bool {
	if len(code) != 2 {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return false
		}
	}
	return true
}
