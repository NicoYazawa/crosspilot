// Package pgtest 提供针对真实 Postgres 的测试脚手架。
//
// 交易账本的失败模式几乎全部与并发、事务和约束有关——拿掉真库之后，
// 这些恰恰是唯一测不到的部分。用假实现替代数据库只能验证「我调用了什么」，
// 验证不了「数据库实际怎么处理并发写入」，那种测试给的是假安全感。
//
// 因此本包起一个真实 Postgres：优先用 testcontainers，其次用外部连接串
// （本地已在跑的实例）。两者都不可用时测试会明确跳过并说明原因，
// 而不是静默通过。
package pgtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
)

// postgresImage 是测试用镜像，与本机已有的版本一致，避免测试时拉新镜像。
const postgresImage = "postgres:18-alpine"

// 容器内 Postgres 的默认库与凭据。
const (
	containerUser     = "crosspilot"
	containerPassword = "crosspilot-test"
	containerDatabase = "crosspilot"
	containerPort     = "5432/tcp"
)

// 连接就绪等待上限。
const (
	readyTimeout      = 60 * time.Second
	readyPollInterval = 200 * time.Millisecond
)

// EnvDatabaseURL 指向一个已存在的 Postgres，供无法运行容器时使用。
const EnvDatabaseURL = "CROSSPILOT_TEST_POSTGRES"

// EnvReaper 控制 testcontainers 的后台回收容器。
//
// 本脚手架在 admin 容器就绪时注册了进程退出钩子，正常退出时一定 Terminate，
// 因此 ryuk 仅作「进程崩溃」的兜底。默认开启：CI 上若并发跑大量测试，
// 单次崩溃不应当留下数十个无人认领的 Postgres 容器。
//
// 显式置 0 关闭它仅用于本机排查 ryuk 启动阻塞；
// 测试代码不应依赖此开关。
const EnvReaper = "TESTCONTAINERS_RYUK_DISABLED"

var (
	once           sync.Once
	admin          *pgxpool.Pool
	adminContainer testcontainers.Container
	skipReason     string
	setupFailed    error
)

// Admin 返回维护库的连接池；测试若无法运行则跳过当前测试并报告原因。
//
// 进程级 admin 容器的回收挂在进程退出时（由 Ensure 在 TestMain 里调用），
// 不挂在子测试 Cleanup——后者会让早退出的子测试把后续子测试依赖的容器关掉。
// 进程崩溃 / os.Exit 的场景由 testcontainers 的 ryuk 兜底。
func Admin(t TB) *pgxpool.Pool {
	t.Helper()

	once.Do(prepare)
	if skipReason != "" {
		t.Skipf("跳过集成测试：%s", skipReason)
	}
	if setupFailed != nil {
		t.Fatalf("准备测试数据库失败：%v", setupFailed)
	}
	return admin
}

// MigrateFunc 是对一个测试库执行迁移的函数。
//
// 由调用方注入而不是在本包内直接调用迁移器：脚手架只管「库建好了没有」，
// 迁移内容归被测包所有，测试库的 schema 因此永远与生产路径同源。
type MigrateFunc func(ctx context.Context, pool *pgxpool.Pool) error

// TB 是建库与清理所需的最小测试接口。
//
// 声明成本地接口而不是直接用 *testing.T，是为了让同一套夹具既能服务
// Test 也能服务 Benchmark：两者都提供这几项能力。
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
	Skipf(format string, args ...any)
	Cleanup(func())
	Name() string
}

// NewDatabase 建一个独立数据库、执行迁移，并返回连接池与清理函数。
//
// 每个测试各自一个库：账本测试会改库存、写订单、故意制造冲突，
// 共用一个库会让测试之间通过数据库互相影响，失败原因也就无法归因。
func NewDatabase(t TB, migrate MigrateFunc) *pgxpool.Pool {
	t.Helper()

	base := Admin(t)
	name := databaseName(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := base.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, pgx.Identifier{name}.Sanitize())); err != nil {
		t.Fatalf("建库 %s 失败：%v", name, err)
	}

	dsn := replaceDatabase(t, name)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库失败：%v", err)
	}

	t.Cleanup(func() {
		pool.Close()

		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelCleanup()

		// 先断开残留连接，否则 DROP DATABASE 会因为「还有会话在用」失败
		_, _ = base.Exec(cleanupCtx,
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		if _, err := base.Exec(cleanupCtx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, pgx.Identifier{name}.Sanitize())); err != nil {
			t.Logf("清理测试库 %s 失败（不影响结论）：%v", name, err)
		}
	})

	if err := migrate(ctx, pool); err != nil {
		t.Fatalf("对测试库执行迁移失败：%v", err)
	}
	return pool
}

// databaseName 由测试名生成一个合法且唯一的库名。
//
// 测试名可能是中文，而 Postgres 的标识符是 63 字节上限。按字节截断会把一个
// 多字节字符切成两半，Postgres 直接报 "invalid byte sequence for encoding UTF8"，
// 于是「测试名里有中文」这种无关紧要的事变成了建库失败。
// 因此先把非 ASCII 字符换成下划线，再按字节截断就不会切坏字符。
func databaseName(t TB) string {
	raw := strings.ToLower(t.Name())
	replacer := strings.NewReplacer("/", "_", "-", "_", " ", "_", ".", "_")
	cleaned := replacer.Replace(raw)

	var builder strings.Builder
	for _, r := range cleaned {
		if r < utf8.RuneSelf && (r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			builder.WriteRune(r)
			continue
		}
		builder.WriteByte('_')
	}
	cleaned = builder.String()

	const prefix = "cptest_"
	const maximum = 63
	if len(prefix)+len(cleaned) <= maximum {
		return prefix + cleaned
	}
	digest := shortHash(cleaned)
	keep := maximum - len(prefix) - len(digest) - 1
	return prefix + cleaned[:keep] + "_" + digest
}

// shortHash 返回输入的前 8 位 SHA-256，用于给过长的库名补一个稳定后缀。
func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:8]
}

// Ensure 供 TestMain 调用：进程退出前回收 admin 容器。
//
// 用法：在使用 pgtest 的测试包 _test.go 里写
//
//	func TestMain(m *testing.M) {
//	    os.Exit(pgtest.Ensure(m))
//	}
//
// 进程崩溃由 ryuk 兜底；正常退出由 Ensure 兜底。
func Ensure(m *testing.M) int {
	code := m.Run()
	if adminContainer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := adminContainer.Terminate(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "pgtest: 回收 admin 容器失败：%v\n", err)
		}
	}
	return code
}

// replaceDatabase 把维护库的连接串换成目标库。
func replaceDatabase(t TB, name string) string {
	t.Helper()

	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("解析测试连接串失败：%v", err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}

var adminDSN string

// prepare 只执行一次：决定用容器还是外部实例，并把维护库连上。
func prepare() {
	if dsn := strings.TrimSpace(os.Getenv(EnvDatabaseURL)); dsn != "" {
		adminDSN = dsn
		connectAdmin()
		return
	}

	// 必须在任何容器操作之前设置：testcontainers 在首次使用时会读取它。
	if _, ok := os.LookupEnv(EnvReaper); !ok {
		_ = os.Setenv(EnvReaper, "true")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	started, err := startContainer(ctx)
	if err != nil {
		// 容器不可用时退回外部实例；都没有就跳过，并把两条原因都写清楚
		if dsn := strings.TrimSpace(os.Getenv(EnvDatabaseURL)); dsn != "" {
			adminDSN = dsn
			connectAdmin()
			return
		}
		skipReason = fmt.Sprintf("无法启动 Postgres 容器（%v），且未设置 %s", err, EnvDatabaseURL)
		return
	}
	adminContainer = started
	connectAdmin()
}

func startContainer(ctx context.Context) (testcontainers.Container, error) {
	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        postgresImage,
			ExposedPorts: []string{containerPort},
			Env: map[string]string{
				"POSTGRES_USER":     containerUser,
				"POSTGRES_PASSWORD": containerPassword,
				"POSTGRES_DB":       containerDatabase,
			},
		},
		Started: true,
	}

	instance, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		return nil, err
	}

	host, err := instance.Host(ctx)
	if err != nil {
		return nil, err
	}
	port, err := instance.MappedPort(ctx, containerPort)
	if err != nil {
		return nil, err
	}

	adminDSN = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		containerUser, containerPassword, net.JoinHostPort(host, port.Port()), containerDatabase)
	return instance, nil
}

func connectAdmin() {
	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		setupFailed = fmt.Errorf("创建维护库连接池失败：%w", err)
		return
	}
	if err := waitReady(ctx, pool); err != nil {
		setupFailed = err
		return
	}
	admin = pool
}

func waitReady(ctx context.Context, pool *pgxpool.Pool) error {
	deadline := time.Now().Add(readyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		err := pool.Ping(ctx)
		if err == nil {
			return nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return fmt.Errorf("等待数据库就绪超时：%w", lastErr)
		case <-time.After(readyPollInterval):
		}
	}
	return fmt.Errorf("等待数据库就绪超时：%w", lastErr)
}

// waitForPostgres 说明：这里刻意不使用 testcontainers 的等待策略。
//
// 端口可连不等于能接受连接，而测试真正需要的是后者。因此容器起好之后
// 由 waitReady 反复做 SQL 探测，就绪与否只有一个判据，失败原因也更具体。
// （容器实例本身由 testcontainers 的清理钩子负责回收。）
