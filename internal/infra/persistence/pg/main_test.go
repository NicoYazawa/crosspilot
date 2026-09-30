package pg_test

import (
	"os"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg/pgtest"
)

// TestMain 在进程退出时回收 admin Postgres 容器。
//
// 没有 TestMain 时每个测试进程结束后都会留下一个无人认领的容器，
// ryuk 兜底慢且会把测试链变脆。Ensure 走「m.Run 返回后 Terminate」路径，
// 是「进程级 admin 容器」语义在 Go 测试框架里唯一干净的兜底点。
func TestMain(m *testing.M) {
	os.Exit(pgtest.Ensure(m))
}
