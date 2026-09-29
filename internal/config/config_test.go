package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fakeEnv 构造一个注入用的查找函数。
func fakeEnv(pairs map[string]string) Lookup {
	return func(key string) (string, bool) {
		value, ok := pairs[key]
		return value, ok
	}
}

// minimalEnv 是能通过校验的最小环境。
func minimalEnv() map[string]string {
	return map[string]string{
		"POSTGRES_PASSWORD": "s3cr3t",
	}
}

func TestLoadFromAppliesDefaults(t *testing.T) {
	cfg, err := LoadFrom(fakeEnv(minimalEnv()))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"运行环境", cfg.Env, EnvDevelopment},
		{"监听地址", cfg.HTTP.Addr, ":8000"},
		{"读超时", cfg.HTTP.ReadTimeout, 15 * time.Second},
		{"优雅关闭超时", cfg.HTTP.ShutdownTimeout, 10 * time.Second},
		{"日志级别", cfg.Log.Level, "info"},
		{"日志格式", cfg.Log.Format, "json"},
		{"数据库主机", cfg.Postgres.Host, "localhost"},
		{"数据库端口", cfg.Postgres.Port, 5432},
		{"数据库名", cfg.Postgres.Database, "crosspilot"},
		{"连接池上限", cfg.Postgres.MaxConns, int32(20)},
		{"Redis 地址", cfg.Redis.Addr, "localhost:6379"},
		{"Qdrant 商品集合", cfg.Qdrant.ProductCollection, "crosspilot_products"},
		{"Qdrant 品类集合", cfg.Qdrant.CategoryCollection, "crosspilot_category_kb"},
		{"默认供应商", cfg.LLM.DefaultProvider, "qwen"},
		{"令牌类型", cfg.Auth.JWTTyp, "crosspilot-access+jwt"},
		{"令牌有效期", cfg.Auth.TokenTTL, 24 * time.Hour},
		{"采样比例", cfg.OTel.SampleRatio, 1.0},
		{"跨域来源默认不允许", len(cfg.HTTP.CORSOrigins), 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("%s = %v，期望 %v", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestLoadFromRequiresPostgresPassword(t *testing.T) {
	_, err := LoadFrom(fakeEnv(map[string]string{}))
	if err == nil {
		t.Fatal("缺少 POSTGRES_PASSWORD 时应报错")
	}
	if !strings.Contains(err.Error(), "POSTGRES_PASSWORD") {
		t.Errorf("错误信息应指出缺失的变量名，得到 %v", err)
	}
}

func TestLoadFromReportsAllProblemsAtOnce(t *testing.T) {
	env := minimalEnv()
	env["POSTGRES_PORT"] = "abc"
	env["CROSSPILOT_LOG_LEVEL"] = "verbose"
	env["LLM_REQUEST_TIMEOUT"] = "永远"

	_, err := LoadFrom(fakeEnv(env))
	if err == nil {
		t.Fatal("多项配置错误时应报错")
	}

	msg := err.Error()
	for _, want := range []string{"POSTGRES_PORT", "CROSSPILOT_LOG_LEVEL", "LLM_REQUEST_TIMEOUT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应包含 %s，实际得到 %v", want, err)
		}
	}
}

func TestLoadFromCORSOrigins(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"单个来源", "https://shop.example.com", []string{"https://shop.example.com"}},
		{"多个来源", "https://a.example.com,https://b.example.com",
			[]string{"https://a.example.com", "https://b.example.com"}},
		{"忽略空白项", " https://a.example.com , ,https://b.example.com ",
			[]string{"https://a.example.com", "https://b.example.com"}},
		{"全是空白等于未配置", " , , ", []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := minimalEnv()
			env["CROSSPILOT_HTTP_CORS_ORIGINS"] = tc.raw

			cfg, err := LoadFrom(fakeEnv(env))
			if err != nil {
				t.Fatalf("加载失败: %v", err)
			}
			if len(cfg.HTTP.CORSOrigins) != len(tc.want) {
				t.Fatalf("CORSOrigins = %v，期望 %v", cfg.HTTP.CORSOrigins, tc.want)
			}
			for i, want := range tc.want {
				if cfg.HTTP.CORSOrigins[i] != want {
					t.Errorf("CORSOrigins[%d] = %q，期望 %q", i, cfg.HTTP.CORSOrigins[i], want)
				}
			}
		})
	}
}

// TestLoadFromRejectsOutOfRangePoolSize 确认连接池上限不会被静默截断：
// 一个超出 int32 的配置值必须在加载期报错，而不是变成另一个数。
func TestLoadFromRejectsOutOfRangePoolSize(t *testing.T) {
	for _, raw := range []string{"0", "-1", "99999999999999999999", "2147483648"} {
		t.Run(raw, func(t *testing.T) {
			env := minimalEnv()
			env["POSTGRES_MAX_CONNS"] = raw

			_, err := LoadFrom(fakeEnv(env))
			if err == nil {
				t.Fatalf("POSTGRES_MAX_CONNS=%s 应导致加载失败", raw)
			}
			if !strings.Contains(err.Error(), "POSTGRES_MAX_CONNS") {
				t.Errorf("错误信息应指出变量名，得到 %v", err)
			}
		})
	}
}

func TestLoadFromRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"端口非整数", "POSTGRES_PORT", "not-a-number"},
		{"采样比例非小数", "OTEL_TRACES_SAMPLER_ARG", "abc"},
		{"超时格式非法", "LLM_REQUEST_TIMEOUT", "30"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := minimalEnv()
			env[tc.key] = tc.val
			if _, err := LoadFrom(fakeEnv(env)); err == nil {
				t.Errorf("%s=%q 应导致加载失败", tc.key, tc.val)
			}
		})
	}
}

func TestLoadFromCrossFieldValidation(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"未知运行环境", map[string]string{"CROSSPILOT_ENV": "prod"}, "CROSSPILOT_ENV"},
		{"未知日志格式", map[string]string{"CROSSPILOT_LOG_FORMAT": "xml"}, "CROSSPILOT_LOG_FORMAT"},
		{"未知日志级别", map[string]string{"CROSSPILOT_LOG_LEVEL": "trace"}, "CROSSPILOT_LOG_LEVEL"},
		{"连接池上限为零", map[string]string{"POSTGRES_MAX_CONNS": "0"}, "POSTGRES_MAX_CONNS"},
		{"采样比例越界", map[string]string{"OTEL_TRACES_SAMPLER_ARG": "1.5"}, "OTEL_TRACES_SAMPLER_ARG"},
		{"默认供应商不存在", map[string]string{"LLM_DEFAULT_PROVIDER": "openai"}, "LLM_DEFAULT_PROVIDER"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := minimalEnv()
			for k, v := range tc.env {
				env[k] = v
			}
			_, err := LoadFrom(fakeEnv(env))
			if err == nil {
				t.Fatalf("应加载失败")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息应包含 %s，得到 %v", tc.want, err)
			}
		})
	}
}

func TestLoadFromProviderOverrides(t *testing.T) {
	env := minimalEnv()
	env["LLM_ANTHROPIC_API_KEY"] = "sk-ant-xxx"
	env["LLM_ANTHROPIC_MODEL"] = "claude-opus-5-5"
	env["LLM_DEFAULT_PROVIDER"] = "anthropic"

	cfg, err := LoadFrom(fakeEnv(env))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	p, ok := cfg.LLM.Provider("anthropic")
	if !ok {
		t.Fatal("应存在 anthropic 供应商")
	}
	if p.Model != "claude-opus-5-5" {
		t.Errorf("模型 = %q，期望 claude-opus-5-5", p.Model)
	}
	if p.APIKey.Reveal() != "sk-ant-xxx" {
		t.Errorf("密钥明文不符：%q", p.APIKey.Reveal())
	}
	if p.BaseURL != "https://api.anthropic.com" {
		t.Errorf("BaseURL 应保留默认值，得到 %q", p.BaseURL)
	}

	if _, ok := cfg.LLM.Provider("openai"); ok {
		t.Error("未配置的供应商不应存在")
	}
}

func TestPostgresDSN(t *testing.T) {
	p := PostgresConfig{
		Host:     "db.internal",
		Port:     5433,
		User:     "app",
		Password: "s3cr3t",
		Database: "crosspilot",
		SSLMode:  "require",
	}

	dsn := p.DSN()
	for _, want := range []string{"app:s3cr3t", "db.internal:5433", "crosspilot", "sslmode=require"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("DSN 应包含 %s，实际 %q", want, dsn)
		}
	}

	if got := p.Address(); got != "db.internal:5433" {
		t.Errorf("Address() = %q，期望 db.internal:5433", got)
	}
	if got := (QdrantConfig{Host: "vec", Port: 6334}).Address(); got != "vec:6334" {
		t.Errorf("Qdrant Address() = %q，期望 vec:6334", got)
	}
}

func TestSecretRedactsEverywhere(t *testing.T) {
	const plaintext = "super-secret-value"
	s := Secret(plaintext)

	if got := s.String(); got != redacted {
		t.Errorf("String() = %q，期望 %q", got, redacted)
	}
	if got := s.GoString(); got != redacted {
		t.Errorf("GoString() = %q，期望 %q", got, redacted)
	}

	// 各种格式化动词都不得泄漏
	for _, verb := range []string{"%v", "%s", "%#v", "%+v", "%q"} {
		if out := fmt.Sprintf(verb, s); strings.Contains(out, plaintext) {
			t.Errorf("格式化 %s 泄漏了明文: %s", verb, out)
		}
	}

	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), plaintext) {
		t.Errorf("JSON 序列化泄漏了明文: %s", raw)
	}

	if got := s.LogValue().String(); strings.Contains(got, plaintext) {
		t.Errorf("slog 取值泄漏了明文: %s", got)
	}

	// 只有 Reveal 能拿到明文
	if got := s.Reveal(); got != plaintext {
		t.Errorf("Reveal() = %q，期望 %q", got, plaintext)
	}

	if !Secret("").IsZero() {
		t.Error("空密钥应被判为零值")
	}
	if s.IsZero() {
		t.Error("非空密钥不应被判为零值")
	}
}

// TestSecretDoesNotLeakThroughStructLogging 确认把整个配置结构丢进日志也不会泄漏。
func TestSecretDoesNotLeakThroughStructLogging(t *testing.T) {
	const plaintext = "p@ssw0rd-must-not-appear"
	cfg, err := LoadFrom(fakeEnv(map[string]string{
		"POSTGRES_PASSWORD":     plaintext,
		"AUTH_JWT_SECRET":       plaintext,
		"LLM_ANTHROPIC_API_KEY": plaintext,
	}))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("启动", "config", cfg.Postgres, "auth", cfg.Auth, "llm", cfg.LLM)

	if strings.Contains(buf.String(), plaintext) {
		t.Errorf("结构化日志泄漏了明文: %s", buf.String())
	}
	// 打码后仍应能看出字段位置
	if !strings.Contains(buf.String(), redacted) {
		t.Errorf("日志中应出现打码占位符，实际: %s", buf.String())
	}
}

func TestLoadFromNilLookupReportsMissingRequired(t *testing.T) {
	// nil 查找函数等价于「环境里什么都没有」
	_, err := LoadFrom(nil)
	if err == nil {
		t.Fatal("无环境变量时应因缺少必填项而报错")
	}
	if !strings.Contains(err.Error(), "POSTGRES_PASSWORD") {
		t.Errorf("错误应指出缺少 POSTGRES_PASSWORD，得到 %v", err)
	}
}
