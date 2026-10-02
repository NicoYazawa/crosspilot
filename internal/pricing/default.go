package pricing

import (
	"bytes"
	_ "embed"
	"fmt"
)

// defaultTable 是随二进制发布的默认价格表。
//
// 用 go:embed 而不是「镜像里 COPY 一个 yaml」：外挂文件有一条很难发现的失败路径——
// 文件没打进镜像时 LoadFromFile 按「不存在」处理、返回空表，于是全量 unpriced，
// 而服务照常启动、/health 照常 200。内嵌让「表在不在」这件事退化为「代码在不在」，
// 编译期就定死了。运维仍可用 PRICING_PRICEBOOK_PATH 覆盖。
//
//go:embed default_pricing.yaml
var defaultTable []byte

// Default 返回内置价格表。
//
// 每次调用重新解析：价格表在启动期构造一次，解析成本（十几行 YAML）可以忽略，
// 而不缓存就没有「谁在什么时候改了包级变量」的并发问题。
//
// 内置表解析失败返回 error 而不是空表：表是我们自己编进二进制的，解析不了
// 说明代码与数据不同步，属于构建错误，必须让启动失败而不是静默降级成全量 unpriced。
func Default() (*PriceBook, error) {
	book, err := LoadFromReader(bytes.NewReader(defaultTable), "embedded:default_pricing.yaml")
	if err != nil {
		return nil, fmt.Errorf("pricing: 内置价格表解析失败（构建错误）: %w", err)
	}
	return book, nil
}
