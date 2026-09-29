// Package migrations 以嵌入文件的形式提供数据库迁移脚本。
//
// 迁移脚本随二进制一起编译进去，部署时无需额外拷贝 SQL 文件，
// 也避免出现「二进制与脚本版本不一致」这种难以排查的问题。
package migrations

import "embed"

// FS 是全部迁移脚本。文件名形如 0001_init.up.sql / 0001_init.down.sql，
// 版本号决定执行顺序。
//
//go:embed *.sql
var FS embed.FS
