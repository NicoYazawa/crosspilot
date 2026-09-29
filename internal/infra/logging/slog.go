// Package logging 提供结构化日志与请求关联字段。
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/NicoYazawa/crosspilot/internal/config"
)

// New 按配置构造 logger。w 为 nil 时写入标准错误。
func New(cfg config.LogConfig, w io.Writer) (*slog.Logger, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	options := &slog.HandlerOptions{Level: level}

	var base slog.Handler
	switch strings.ToLower(cfg.Format) {
	case "text":
		base = slog.NewTextHandler(w, options)
	case "json":
		base = slog.NewJSONHandler(w, options)
	default:
		return nil, fmt.Errorf("logging: 未知日志格式 %q", cfg.Format)
	}

	return slog.New(ContextHandler{inner: base}), nil
}

func parseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(name) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging: 未知日志级别 %q", name)
	}
}

// contextKey 是本包在 context 中使用的键类型，避免与其他包的键相撞。
type contextKey int

const (
	keyRequestID contextKey = iota
	keySessionID
	keyRunID
)

// WithRequestID 把请求标识写入 context。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

// WithSessionID 把会话标识写入 context。
func WithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keySessionID, id)
}

// WithRunID 把一次 Agent 运行的标识写入 context。
func WithRunID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRunID, id)
}

// RequestID 取出请求标识，不存在时返回空串。
func RequestID(ctx context.Context) string { return stringField(ctx, keyRequestID) }

// SessionID 取出会话标识，不存在时返回空串。
func SessionID(ctx context.Context) string { return stringField(ctx, keySessionID) }

// RunID 取出运行标识，不存在时返回空串。
func RunID(ctx context.Context) string { return stringField(ctx, keyRunID) }

func stringField(ctx context.Context, key contextKey) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(key).(string)
	return value
}

// ContextHandler 在每条日志上附加从 context 取到的关联字段。
//
// 这样调用点只需 log.InfoContext(ctx, ...)，不必手工传递 request_id、session_id
// 与 trace_id，也就不会因为漏传而丢失关联。
type ContextHandler struct {
	inner slog.Handler
}

// 编译期确认实现了 slog.Handler。
var _ slog.Handler = ContextHandler{}

// Enabled 实现 slog.Handler。
func (h ContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle 附加关联字段后转发给内层 handler。
func (h ContextHandler) Handle(ctx context.Context, record slog.Record) error {
	attrs := make([]slog.Attr, 0, 4)

	if id := RequestID(ctx); id != "" {
		attrs = append(attrs, slog.String("request_id", id))
	}
	if id := SessionID(ctx); id != "" {
		attrs = append(attrs, slog.String("session_id", id))
	}
	if id := RunID(ctx); id != "" {
		attrs = append(attrs, slog.String("run_id", id))
	}
	if span := trace.SpanContextFromContext(ctx); span.HasTraceID() {
		attrs = append(attrs, slog.String("trace_id", span.TraceID().String()))
	}

	record.AddAttrs(attrs...)
	return h.inner.Handle(ctx, record)
}

// WithAttrs 实现 slog.Handler。
func (h ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ContextHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup 实现 slog.Handler。
func (h ContextHandler) WithGroup(name string) slog.Handler {
	return ContextHandler{inner: h.inner.WithGroup(name)}
}
