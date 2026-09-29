package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/config"
)

// testConfig 指向一个不会有服务监听的地址：构造必须成功，探测必须失败。
func testConfig() config.PostgresConfig {
	return config.PostgresConfig{
		Host:     "127.0.0.1",
		Port:     1,
		User:     "crosspilot",
		Password: "s3cr3t",
		Database: "crosspilot",
		SSLMode:  "disable",
		MaxConns: 4,
	}
}

// TestNewPoolDoesNotConnect 是本包最关键的一条：连接池是惰性建的，
// 数据库没起来也必须能构造成功，否则服务启动会被数据库可用性绑架。
func TestNewPoolDoesNotConnect(t *testing.T) {
	pool, err := NewPool(context.Background(), testConfig())
	if err != nil {
		t.Fatalf("构造连接池不应依赖数据库可用：%v", err)
	}
	t.Cleanup(pool.Close)

	if pool.Config().MaxConns != 4 {
		t.Errorf("MaxConns = %d，期望 4", pool.Config().MaxConns)
	}
	if pool.Config().MinConns != 0 {
		t.Errorf("MinConns = %d，期望 0", pool.Config().MinConns)
	}
	if got := pool.Config().ConnConfig.ConnectTimeout; got != pingTimeout {
		t.Errorf("ConnectTimeout = %v，期望 %v", got, pingTimeout)
	}
}

func TestNewPoolRejectsBadDSN(t *testing.T) {
	cfg := testConfig()
	cfg.SSLMode = "no-such-sslmode"

	if _, err := NewPool(context.Background(), cfg); err == nil {
		t.Fatal("无法解析的连接串应当报错")
	}
}

func TestCheckReportsUnreachable(t *testing.T) {
	pool, err := NewPool(context.Background(), testConfig())
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	t.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = Check(ctx, pool)
	if err == nil {
		t.Fatal("没有数据库在跑时探测应当失败")
	}
	if !strings.Contains(err.Error(), "探测失败") {
		t.Errorf("错误信息应当说明是探测失败：%v", err)
	}
}

func TestCheckNilPool(t *testing.T) {
	if err := Check(context.Background(), nil); err == nil {
		t.Fatal("nil 连接池应当报错而不是 panic")
	}
}
