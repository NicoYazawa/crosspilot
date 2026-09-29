// Package observability 提供链路追踪、指标的初始化与记录前的脱敏工具。
package observability

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"

	"github.com/NicoYazawa/crosspilot/internal/config"
)

// Version 是服务的版本号，由构建时注入，未注入时为 dev。
var Version = "dev"

// Setup 初始化链路追踪与指标管道，返回关闭函数。
//
// Endpoint 为空表示不导出：此时返回一个空操作的关闭函数，服务照常运行，
// 本地开发与 CI 不需要 collector 也能启动。
//
// 导出走 OTLP/HTTP 直连，不经由 collector 中转。
func Setup(ctx context.Context, cfg config.OTelConfig) (func(context.Context) error, error) {
	if cfg.Endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	hostPort, insecure, err := parseEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}

	traceOptions := []otlptracehttp.Option{otlptracehttp.WithEndpoint(hostPort)}
	metricOptions := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(hostPort)}
	if insecure {
		traceOptions = append(traceOptions, otlptracehttp.WithInsecure())
		metricOptions = append(metricOptions, otlpmetrichttp.WithInsecure())
	}

	traceExporter, err := otlptracehttp.New(ctx, traceOptions...)
	if err != nil {
		return nil, fmt.Errorf("observability: 构造链路导出器失败: %w", err)
	}
	metricExporter, err := otlpmetrichttp.New(ctx, metricOptions...)
	if err != nil {
		return nil, fmt.Errorf("observability: 构造指标导出器失败: %w", err)
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(serviceResource(cfg.ServiceName)),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
		sdkmetric.WithResource(serviceResource(cfg.ServiceName)),
	)

	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func(ctx context.Context) error {
		return errors.Join(
			tracerProvider.Shutdown(ctx),
			meterProvider.Shutdown(ctx),
		)
	}, nil
}

// serviceResource 描述本服务。不用带探测器的 resource.New，避免不同 semconv
// 版本之间的 schema URL 冲突导致启动失败。
func serviceResource(serviceName string) *resource.Resource {
	return resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(Version),
	)
}

// parseEndpoint 把 OTLP 端点拆成 dial 用的 host:port 与是否需要明文传输。
// 接受 "http://host:4318"、"https://host:4318" 或裸的 "host:4318"。
func parseEndpoint(raw string) (hostPort string, insecure bool, err error) {
	if !strings.Contains(raw, "://") {
		if raw == "" {
			return "", false, fmt.Errorf("observability: OTLP 端点为空")
		}
		return raw, true, nil
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false, fmt.Errorf("observability: 解析 OTLP 端点 %q 失败: %w", raw, err)
	}
	switch parsed.Scheme {
	case "http":
		insecure = true
	case "https":
		insecure = false
	default:
		return "", false, fmt.Errorf("observability: OTLP 端点协议应为 http 或 https，实际 %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", false, fmt.Errorf("observability: OTLP 端点 %q 缺少主机", raw)
	}
	return parsed.Host, insecure, nil
}
