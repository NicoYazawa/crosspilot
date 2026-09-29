package postgres

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NicoYazawa/crosspilot/migrations"
)

// migrationsTable 记录已应用的迁移版本。
const migrationsTable = "schema_migrations"

// Direction 是迁移方向。
type Direction string

const (
	// Up 应用所有尚未执行的迁移。
	Up Direction = "up"

	// Down 回滚最近一次已应用的迁移，每次只回滚一步。
	Down Direction = "down"
)

// ErrUnknownDirection 表示迁移方向无法识别。
var ErrUnknownDirection = fmt.Errorf("postgres: 未知的迁移方向")

// migration 是一个已读入内存的迁移脚本。
type migration struct {
	version string // 形如 0001
	name    string // 形如 0001_init
	body    string
}

// Migrate 按方向执行迁移，返回实际执行的步数。
//
// 每一步都在独立事务里执行：脚本与版本记录要么一起生效，要么一起不生效。
// 中途失败不会留下「脚本跑了但没记账」的中间态。
func Migrate(ctx context.Context, pool *pgxpool.Pool, dir Direction) (int, error) {
	if pool == nil {
		return 0, fmt.Errorf("postgres: 连接池未初始化")
	}
	// 先校验方向：参数写错了就不该去动数据库
	if dir != Up && dir != Down {
		return 0, fmt.Errorf("%w: %q", ErrUnknownDirection, string(dir))
	}

	if err := ensureMigrationsTable(ctx, pool); err != nil {
		return 0, err
	}

	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return 0, err
	}

	switch dir {
	case Up:
		return migrateUp(ctx, pool, applied)
	default:
		return migrateDown(ctx, pool, applied)
	}
}

// ensureMigrationsTable 建好版本记录表，幂等。
func ensureMigrationsTable(ctx context.Context, pool *pgxpool.Pool) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS ` + migrationsTable + ` (
    version    text        PRIMARY KEY,
    name       text        NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
)`
	if _, err := pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("postgres: 创建 %s 失败: %w", migrationsTable, err)
	}
	return nil
}

// appliedVersions 返回已应用的版本集合。
func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM `+migrationsTable)
	if err != nil {
		return nil, fmt.Errorf("postgres: 读取已应用迁移失败: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("postgres: 扫描迁移版本失败: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: 遍历迁移版本失败: %w", err)
	}
	return applied, nil
}

// loadMigrations 从嵌入文件系统读出指定方向的全部脚本，按版本号升序排列。
func loadMigrations(dir Direction) ([]migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("postgres: 读取迁移目录失败: %w", err)
	}

	suffix := "." + string(dir) + ".sql"
	var out []migration
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, suffix) {
			continue
		}

		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return nil, fmt.Errorf("postgres: 读取 %s 失败: %w", name, err)
		}

		base := strings.TrimSuffix(name, suffix)
		version, _, found := strings.Cut(base, "_")
		if !found || version == "" {
			return nil, fmt.Errorf("postgres: 迁移文件名 %q 缺少 <版本>_<主题> 前缀", name)
		}

		out = append(out, migration{version: version, name: base, body: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func migrateUp(ctx context.Context, pool *pgxpool.Pool, applied map[string]bool) (int, error) {
	pending, err := loadMigrations(Up)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, m := range pending {
		if applied[m.version] {
			continue
		}
		if err := applyOne(ctx, pool, m,
			`INSERT INTO `+migrationsTable+` (version, name) VALUES ($1, $2)`, m.version, m.name); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func migrateDown(ctx context.Context, pool *pgxpool.Pool, applied map[string]bool) (int, error) {
	all, err := loadMigrations(Down)
	if err != nil {
		return 0, err
	}

	// 取已应用中版本号最大的那一个，每次只回滚一步
	var target *migration
	for i := range all {
		if !applied[all[i].version] {
			continue
		}
		if target == nil || all[i].version > target.version {
			target = &all[i]
		}
	}
	if target == nil {
		return 0, nil
	}

	if err := applyOne(ctx, pool, *target,
		`DELETE FROM `+migrationsTable+` WHERE version = $1`, target.version); err != nil {
		return 0, err
	}
	return 1, nil
}

// applyOne 在一个事务里执行脚本并记账。
func applyOne(ctx context.Context, pool *pgxpool.Pool, m migration, record string, args ...any) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // 已提交时回滚是空操作

	if err := execScript(ctx, tx, m.body); err != nil {
		return fmt.Errorf("postgres: 执行迁移 %s 失败: %w", m.name, err)
	}
	if _, err := tx.Exec(ctx, record, args...); err != nil {
		return fmt.Errorf("postgres: 记录迁移 %s 失败: %w", m.name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: 提交迁移 %s 失败: %w", m.name, err)
	}
	return nil
}

// execScript 执行可能包含多条语句的迁移脚本。
//
// 走简单查询协议：pgx 默认的扩展协议会把整段脚本当成一条预处理语句，
// 遇到分号分隔的多条语句会直接报错。
func execScript(ctx context.Context, tx pgx.Tx, script string) error {
	_, err := tx.Conn().PgConn().Exec(ctx, script).ReadAll()
	return err
}
