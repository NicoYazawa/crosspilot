package persistence

import (
	"os"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg/pgtest"
)

// TestMain 在进程退出时回收 admin Postgres 容器。
//
// 这个包用了 pgtest.NewDatabase（见 catalog_repo_test.go），但此前没有
// TestMain——于是每跑一次 `go test ./internal/infra/persistence/` 就留下一个
// 永远不再被回收的 Postgres 容器。本机上攒了十几个，全是从这里漏出去的。
//
// pgtest 的 ryuk 兜底在本机不可用（Docker Desktop 不把 docker socket 暴露进
// 容器，ryuk 起不来就会让整个容器启动失败），所以这行不是可选项：
// 少了它没有第二道防线。该要求由 tools/arch 的结构测试强制。
func TestMain(m *testing.M) {
	os.Exit(pgtest.Ensure(m))
}
