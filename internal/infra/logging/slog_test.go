package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/NicoYazawa/crosspilot/internal/config"
)

func TestNewFormats(t *testing.T) {
	cases := []struct {
		format string
		want   string // JSON 与文本输出的首个字节特征
	}{
		{"json", "{"},
		{"text", "time="},
	}

	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			var buf bytes.Buffer
			logger, err := New(config.LogConfig{Level: "info", Format: tc.format}, &buf)
			if err != nil {
				t.Fatalf("未预期的错误：%v", err)
			}

			logger.Info("测试")
			if got := buf.String(); !strings.HasPrefix(got, tc.want) {
				t.Errorf("输出 = %q，期望以 %q 开头", got, tc.want)
			}
		})
	}
}

func TestNewRejectsUnknownFormatAndLevel(t *testing.T) {
	if _, err := New(config.LogConfig{Level: "info", Format: "yaml"}, nil); err == nil {
		t.Error("未知格式应当报错")
	}
	if _, err := New(config.LogConfig{Level: "verbose", Format: "json"}, nil); err == nil {
		t.Error("未知级别应当报错")
	}
}

func TestNewIsCaseInsensitive(t *testing.T) {
	if _, err := New(config.LogConfig{Level: "INFO", Format: "JSON"}, nil); err != nil {
		t.Errorf("大小写不应影响识别：%v", err)
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger, err := New(config.LogConfig{Level: "warn", Format: "json"}, &buf)
	if err != nil {
		t.Fatalf("未预期的错误：%v", err)
	}

	logger.Info("不该出现")
	logger.Warn("该出现")

	got := buf.String()
	if strings.Contains(got, "不该出现") {
		t.Errorf("info 级别的日志不应在 warn 阈值下输出：%s", got)
	}
	if !strings.Contains(got, "该出现") {
		t.Errorf("warn 级别日志缺失：%s", got)
	}
}

func TestContextFieldsAreInjected(t *testing.T) {
	var buf bytes.Buffer
	logger, err := New(config.LogConfig{Level: "info", Format: "json"}, &buf)
	if err != nil {
		t.Fatalf("未预期的错误：%v", err)
	}

	ctx := context.Background()
	ctx = WithRequestID(ctx, "req-1")
	ctx = WithSessionID(ctx, "sess-1")
	ctx = WithRunID(ctx, "run-1")
	logger.InfoContext(ctx, "带关联字段")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("解析日志失败：%v，原文 %s", err, buf.String())
	}

	want := map[string]string{"request_id": "req-1", "session_id": "sess-1", "run_id": "run-1"}
	for key, value := range want {
		if record[key] != value {
			t.Errorf("%s = %v，期望 %q", key, record[key], value)
		}
	}
}

func TestAbsentContextFieldsAreOmitted(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := New(config.LogConfig{Level: "info", Format: "json"}, &buf)

	logger.InfoContext(context.Background(), "无关联字段")

	got := buf.String()
	for _, key := range []string{"request_id", "session_id", "run_id", "trace_id"} {
		if strings.Contains(got, key) {
			t.Errorf("缺少关联字段时不应输出 %s：%s", key, got)
		}
	}
}

func TestTraceIDFromSpanContext(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := New(config.LogConfig{Level: "info", Format: "json"}, &buf)

	traceID := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	spanID := trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID}))

	logger.InfoContext(ctx, "带 trace")

	if got := buf.String(); !strings.Contains(got, traceID.String()) {
		t.Errorf("日志里没有 trace_id：%s", got)
	}
}

func TestAccessorsReturnEmptyWhenAbsent(t *testing.T) {
	ctx := context.Background()
	if RequestID(ctx) != "" || SessionID(ctx) != "" || RunID(ctx) != "" {
		t.Error("未写入时应当返回空串")
	}

	//nolint:staticcheck // 显式覆盖 nil context 这条防御分支
	if RequestID(nil) != "" || SessionID(nil) != "" || RunID(nil) != "" {
		t.Error("nil context 应当返回空串而不是 panic")
	}
}

func TestContextKeysDoNotCollide(t *testing.T) {
	ctx := WithRequestID(context.Background(), "req")
	if SessionID(ctx) != "" || RunID(ctx) != "" {
		t.Error("不同的关联字段使用了相互冲突的键")
	}
}

func TestHandlerWithAttrsAndGroup(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := New(config.LogConfig{Level: "info", Format: "json"}, &buf)

	logger.With("service", "catalog").WithGroup("order").Info("下单", slog.Int("lines", 2))

	got := buf.String()
	if !strings.Contains(got, "service") || !strings.Contains(got, "order") {
		t.Errorf("WithAttrs/WithGroup 未生效：%s", got)
	}

	// WithAttrs 之后仍然要带上 context 里的关联字段
	buf.Reset()
	logger.With("service", "catalog").InfoContext(WithRequestID(context.Background(), "req-2"), "带属性")
	if !strings.Contains(buf.String(), "req-2") {
		t.Errorf("WithAttrs 之后丢失了关联字段：%s", buf.String())
	}
}

func TestHandlerEnabledDelegatesToInner(t *testing.T) {
	handler := ContextHandler{inner: slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError})}

	if handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("应当沿用内层 handler 的阈值")
	}
	if !handler.Enabled(context.Background(), slog.LevelError) {
		t.Error("error 级别应当通过")
	}
}
