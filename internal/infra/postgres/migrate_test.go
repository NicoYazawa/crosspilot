package postgres

import (
	"context"
	"errors"
	"testing"
)

func TestLoadMigrationsUp(t *testing.T) {
	got, err := loadMigrations(Up)
	if err != nil {
		t.Fatalf("读取上迁移失败：%v", err)
	}
	if len(got) == 0 {
		t.Fatal("至少应当有一个上迁移脚本")
	}

	first := got[0]
	if first.version != "0001" {
		t.Errorf("首个版本号 = %q，期望 0001", first.version)
	}
	if first.name != "0001_init" {
		t.Errorf("首个迁移名 = %q，期望 0001_init", first.name)
	}
	if first.body == "" {
		t.Error("脚本内容为空，说明文件没有被真正读进来")
	}
}

func TestLoadMigrationsDown(t *testing.T) {
	got, err := loadMigrations(Down)
	if err != nil {
		t.Fatalf("读取下迁移失败：%v", err)
	}
	if len(got) == 0 {
		t.Fatal("至少应当有一个下迁移脚本")
	}
	if got[0].version != "0001" {
		t.Errorf("首个版本号 = %q，期望 0001", got[0].version)
	}
}

// TestLoadMigrationsExcludesOppositeDirection 确认两个方向互不串味：
// 上迁移里不能混进 .down.sql，反之亦然。
func TestLoadMigrationsExcludesOppositeDirection(t *testing.T) {
	up, err := loadMigrations(Up)
	if err != nil {
		t.Fatalf("读取上迁移失败：%v", err)
	}
	down, err := loadMigrations(Down)
	if err != nil {
		t.Fatalf("读取下迁移失败：%v", err)
	}

	// 同一个版本号在两个方向各出现一次，且内容不同（0001 的两份都是注释）
	if len(up) != len(down) {
		t.Errorf("上迁移 %d 个、下迁移 %d 个，应当一一对应", len(up), len(down))
	}
	for i := range up {
		if up[i].version != down[i].version {
			t.Errorf("第 %d 个版本号不对应：上 %q、下 %q", i, up[i].version, down[i].version)
		}
	}
}

// TestLoadMigrationsIsSorted 版本号必须升序，执行顺序依赖这一点。
func TestLoadMigrationsIsSorted(t *testing.T) {
	got, err := loadMigrations(Up)
	if err != nil {
		t.Fatalf("读取上迁移失败：%v", err)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].version >= got[i].version {
			t.Errorf("顺序错误：%q 排在 %q 之前", got[i-1].version, got[i].version)
		}
	}
}

func TestMigrateRejectsNilPool(t *testing.T) {
	if _, err := Migrate(context.Background(), nil, Up); err == nil {
		t.Fatal("nil 连接池应当报错")
	}
}

// TestMigrateRejectsUnknownDirectionBeforeTouchingDatabase 确认方向校验发生在
// 建表之前：参数写错就不该去动数据库。
func TestMigrateRejectsUnknownDirectionBeforeTouchingDatabase(t *testing.T) {
	pool, err := NewPool(context.Background(), testConfig())
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	t.Cleanup(pool.Close)

	_, err = Migrate(context.Background(), pool, Direction("sideways"))
	if !errors.Is(err, ErrUnknownDirection) {
		t.Fatalf("错误 = %v，期望 ErrUnknownDirection", err)
	}
}
