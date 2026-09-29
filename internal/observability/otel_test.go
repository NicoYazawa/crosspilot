package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/NicoYazawa/crosspilot/internal/config"
)

func TestParseEndpoint(t *testing.T) {
	cases := []struct {
		name         string
		raw          string
		wantHostPort string
		wantInsecure bool
		wantErr      string
	}{
		{"裸地址默认明文", "localhost:4318", "localhost:4318", true, ""},
		{"http 为明文", "http://collector:4318", "collector:4318", true, ""},
		{"https 为加密", "https://collector.example.com:4318", "collector.example.com:4318", false, ""},
		{"空端点", "", "", false, "端点为空"},
		{"不支持的协议", "grpc://collector:4317", "", false, "应为 http 或 https"},
		{"缺少主机", "https://", "", false, "缺少主机"},
		{"无法解析", "http://[::1", "", false, "解析"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hostPort, insecure, err := parseEndpoint(tc.raw)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("期望报错含 %q，实际无错误", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("错误 = %v，期望含 %q", err, tc.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("未预期的错误：%v", err)
			}
			if hostPort != tc.wantHostPort {
				t.Errorf("host:port = %q，期望 %q", hostPort, tc.wantHostPort)
			}
			if insecure != tc.wantInsecure {
				t.Errorf("insecure = %v，期望 %v", insecure, tc.wantInsecure)
			}
		})
	}
}

// TestSetupWithoutEndpointIsNoop 覆盖默认部署形态：没配 collector 也要能启动。
func TestSetupWithoutEndpointIsNoop(t *testing.T) {
	shutdown, err := Setup(context.Background(), config.OTelConfig{ServiceName: "crosspilot-api"})
	if err != nil {
		t.Fatalf("未预期的错误：%v", err)
	}
	if shutdown == nil {
		t.Fatal("关闭函数不应为 nil，调用方需要能无条件 defer 它")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("空操作关闭返回错误：%v", err)
	}
}

func TestSetupRejectsMalformedEndpoint(t *testing.T) {
	_, err := Setup(context.Background(), config.OTelConfig{
		ServiceName: "crosspilot-api",
		Endpoint:    "kafka://collector:9092",
	})
	if err == nil {
		t.Fatal("非法端点应当报错")
	}
}

// TestSetupAndShutdownFlushesToCollector 起一个假的 OTLP 接收端，验证
// Setup 建立的管道真的能把数据送出去——不只是构造成功而已。
//
// 接收端回一个空的 protobuf 响应体，空消息是合法的 ExportXxxResponse，
// 导出器会当作成功。
func TestSetupAndShutdownFlushesToCollector(t *testing.T) {
	received := make(chan string, 2)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case received <- r.URL.Path:
		default:
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	ctx := context.Background()
	shutdown, err := Setup(ctx, config.OTelConfig{
		Endpoint:    collector.URL,
		ServiceName: "crosspilot-api",
		SampleRatio: 1,
	})
	if err != nil {
		t.Fatalf("未预期的错误：%v", err)
	}

	_, span := otel.Tracer("test").Start(ctx, "span")
	span.End()
	if _, err := otel.Meter("test").Int64Counter("test.counter"); err != nil {
		t.Fatalf("创建计数器失败：%v", err)
	}

	if err := shutdown(ctx); err != nil {
		t.Errorf("关闭时出错：%v", err)
	}

	// 收到指标推送即说明管道是通的；trace 是否落在同一次采样内不影响这条断言
	select {
	case path := <-received:
		if path != "/v1/metrics" && path != "/v1/traces" {
			t.Errorf("收到意外的路径 %q", path)
		}
	default:
		t.Error("关闭后没有向接收端推送任何数据")
	}
}

func TestServiceResource(t *testing.T) {
	res := serviceResource("crosspilot-api")
	if res == nil {
		t.Fatal("resource 不应为 nil")
	}

	var found bool
	for _, attr := range res.Attributes() {
		if attr.Key == "service.name" {
			found = true
			if attr.Value.AsString() != "crosspilot-api" {
				t.Errorf("service.name = %q", attr.Value.AsString())
			}
		}
	}
	if !found {
		t.Error("resource 缺少 service.name")
	}
}
