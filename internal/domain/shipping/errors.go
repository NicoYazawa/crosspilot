// Package shipping 是跨境电商的物流与关税领域模型。
package shipping

import "errors"

var (
	// ErrUnsupportedDestination 表示目的国/地区不在支持列表中。
	ErrUnsupportedDestination = errors.New("shipping: 不支持的目的地")
)
