package redis

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/config"
)

// closedPort 返回一个刚刚关闭的本地端口，连过去必定被拒绝，且不需要等待超时。
func closedPort(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占用端口失败：%v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("释放端口失败：%v", err)
	}
	return addr
}

// TestNewClientDoesNotConnect 覆盖惰性构造：Redis 没起来也要能构造出客户端。
func TestNewClientDoesNotConnect(t *testing.T) {
	client := NewClient(config.RedisConfig{
		Addr:     closedPort(t),
		Password: "s3cr3t",
		DB:       2,
	}, nil)
	t.Cleanup(func() { _ = client.Close() })

	options := client.Options()
	if options.DB != 2 {
		t.Errorf("DB = %d，期望 2", options.DB)
	}
	if options.DialTimeout != pingTimeout {
		t.Errorf("DialTimeout = %v，期望 %v", options.DialTimeout, pingTimeout)
	}
}

// TestCheckFailsFastOnRefusedConnection 确认依赖失联时探测立刻返回。
//
// 每次拨号都要等一个超时，再叠加内部重试与退避，会让失联场景下的 /health
// 慢上若干倍——而它要回答的恰恰是「是不是挂了」。这条用例是 P0 验收里
// 「数据库断开时健康检查仍要快速给出 503」的单元测试形态。
func TestCheckFailsFastOnRefusedConnection(t *testing.T) {
	client := NewClient(config.RedisConfig{Addr: closedPort(t)}, nil)
	t.Cleanup(func() { _ = client.Close() })

	if got := client.Options().DialerRetries; got != 1 {
		t.Errorf("DialerRetries = %d，期望 1（只拨一次）", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := Check(ctx, client)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("没有 Redis 在跑时探测应当失败")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("探测耗时 %v，对健康探测来说过慢", elapsed)
	}
}

// TestClientLoggerGoesToSlog 确认客户端自身的日志并入结构化日志。
func TestClientLoggerGoesToSlog(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// 客户端只提供全局日志入口，因此这里直接验证转接实现本身
	slogLogger{logger: logger}.Printf(context.Background(), "连接 %s 失败", "127.0.0.1:6379")

	out := buf.String()
	if !strings.Contains(out, "连接 127.0.0.1:6379 失败") {
		t.Errorf("日志内容不符：%s", out)
	}
	if !strings.Contains(out, `"level":"DEBUG"`) {
		t.Errorf("客户端日志应为 debug 级别：%s", out)
	}
}

func TestCheckReportsUnreachable(t *testing.T) {
	client := NewClient(config.RedisConfig{Addr: closedPort(t)}, nil)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := Check(ctx, client)
	if err == nil {
		t.Fatal("没有 Redis 在跑时探测应当失败")
	}
	if !strings.Contains(err.Error(), "探测失败") {
		t.Errorf("错误信息应当说明是探测失败：%v", err)
	}
}

func TestCheckNilClient(t *testing.T) {
	if err := Check(context.Background(), nil); err == nil {
		t.Fatal("nil 客户端应当报错而不是 panic")
	}
}

func TestCheckHonoursContextDeadline(t *testing.T) {
	// 指向一个不会响应的地址，确认超时由调用方的 ctx 兜住
	client := NewClient(config.RedisConfig{Addr: "192.0.2.1:6379"}, nil) // TEST-NET-1，不可路由
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := Check(ctx, client); err == nil {
		t.Fatal("不可达地址应当探测失败")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("探测耗时 %v，说明没有遵循 ctx 或内建超时", elapsed)
	}
}

func TestPingTimeoutIsShort(t *testing.T) {
	// /health 会被编排系统频繁调用，探测超时必须短
	if pingTimeout > 3*time.Second {
		t.Errorf("pingTimeout = %v，对健康探测来说过长", pingTimeout)
	}
}
