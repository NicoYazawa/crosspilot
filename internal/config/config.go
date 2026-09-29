// Package config 从环境变量加载并校验服务配置。
//
// 加载是显式的：包被 import 时不读取任何环境变量、不产生副作用，
// 只有调用 Load 才会读取。缺失必填项直接返回错误让进程启动失败，
// 不会静默回退到默认值——配置错误应当在启动时暴露，而不是在半夜的交易里。
package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// 运行环境取值。
const (
	EnvDevelopment = "development"
	EnvStaging     = "staging"
	EnvProduction  = "production"
)

// maxPoolConns 是连接池上限的硬上限。
//
// pgxpool 的 MaxConns 是 int32，超过就可能被截断成一个荒唐的小数字。
// 与其让 int32 的窄化悄悄发生，不如在这里就挡住。
const maxPoolConns = math.MaxInt32

// Lookup 抽象环境变量读取，测试可注入固定表。
type Lookup func(key string) (value string, ok bool)

// Config 是服务的全部配置。字段按关注点分组，组内每个字段都有默认值，
// 除非它被标记为必填。
type Config struct {
	Env      string
	HTTP     HTTPConfig
	Log      LogConfig
	Postgres PostgresConfig
	Redis    RedisConfig
	Qdrant   QdrantConfig
	LLM      LLMConfig
	Auth     AuthConfig
	OTel     OTelConfig
}

// HTTPConfig 是 HTTP 服务端配置。
type HTTPConfig struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	// CORSOrigins 是允许跨域访问的来源列表，空表示不允许任何跨域来源。
	CORSOrigins []string
}

// LogConfig 是日志配置。
type LogConfig struct {
	Level  string // debug | info | warn | error
	Format string // json | text
}

// PostgresConfig 是主数据库配置。
type PostgresConfig struct {
	Host     string
	Port     int
	User     string
	Password Secret
	Database string
	SSLMode  string
	MaxConns int32
}

// Address 返回 host:port。
func (c PostgresConfig) Address() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// DSN 返回连接串。密钥以明文拼入，调用方不得记录其返回值。
func (c PostgresConfig) DSN() string {
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=%s",
		c.User, c.Password.Reveal(), c.Address(), c.Database, c.SSLMode)
}

// RedisConfig 是缓存与协调存储配置。
type RedisConfig struct {
	Addr     string
	Password Secret
	DB       int
}

// QdrantConfig 是向量库配置。
type QdrantConfig struct {
	Host               string
	Port               int
	APIKey             Secret
	ProductCollection  string
	CategoryCollection string
}

// Address 返回 host:port。
func (c QdrantConfig) Address() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// ProviderConfig 是一个模型供应商的接入参数。
type ProviderConfig struct {
	Name    string
	BaseURL string
	APIKey  Secret
	Model   string
}

// LLMConfig 是模型网关配置。
//
// 多家供应商并存，跨 OpenAI 兼容协议与 Anthropic Messages 协议两种协议族，
// 因此每家单独持有一套接入参数。
type LLMConfig struct {
	DefaultProvider string
	Providers       map[string]ProviderConfig
	RequestTimeout  time.Duration
	MaxRetries      int
}

// Provider 返回指定名称的供应商配置。
func (c LLMConfig) Provider(name string) (ProviderConfig, bool) {
	p, ok := c.Providers[name]
	return p, ok
}

// AuthConfig 是身份与令牌配置。
type AuthConfig struct {
	JWTSecret Secret
	JWTTyp    string
	TokenTTL  time.Duration
}

// OTelConfig 是可观测配置。Endpoint 为空表示关闭导出。
type OTelConfig struct {
	Endpoint    string
	ServiceName string
	SampleRatio float64
}

// Load 从进程环境变量加载配置。
func Load() (*Config, error) { return LoadFrom(os.LookupEnv) }

// LoadFrom 从给定的查找函数加载配置。所有缺失与格式错误会被一次性汇总返回，
// 而不是遇到第一个就中断——一次启动就能看到全部问题。
func LoadFrom(lookup Lookup) (*Config, error) {
	l := &loader{lookup: lookup}

	cfg := &Config{
		Env: l.str("CROSSPILOT_ENV", EnvDevelopment),
		HTTP: HTTPConfig{
			Addr:            l.str("CROSSPILOT_HTTP_ADDR", ":8000"),
			ReadTimeout:     l.duration("CROSSPILOT_HTTP_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:    l.duration("CROSSPILOT_HTTP_WRITE_TIMEOUT", 0),
			IdleTimeout:     l.duration("CROSSPILOT_HTTP_IDLE_TIMEOUT", 60*time.Second),
			ShutdownTimeout: l.duration("CROSSPILOT_SHUTDOWN_TIMEOUT", 10*time.Second),
			CORSOrigins:     l.list("CROSSPILOT_HTTP_CORS_ORIGINS", nil),
		},
		Log: LogConfig{
			Level:  l.str("CROSSPILOT_LOG_LEVEL", "info"),
			Format: l.str("CROSSPILOT_LOG_FORMAT", "json"),
		},
		Postgres: PostgresConfig{
			Host:     l.str("POSTGRES_HOST", "localhost"),
			Port:     l.int("POSTGRES_PORT", 5432),
			User:     l.str("POSTGRES_USER", "crosspilot"),
			Password: l.requiredSecret("POSTGRES_PASSWORD"),
			Database: l.str("POSTGRES_DB", "crosspilot"),
			SSLMode:  l.str("POSTGRES_SSLMODE", "disable"),
			MaxConns: l.boundedInt32("POSTGRES_MAX_CONNS", 20, 1, maxPoolConns),
		},
		Redis: RedisConfig{
			Addr:     l.str("REDIS_ADDR", "localhost:6379"),
			Password: l.secret("REDIS_PASSWORD"),
			DB:       l.int("REDIS_DB", 0),
		},
		Qdrant: QdrantConfig{
			Host:               l.str("QDRANT_HOST", "localhost"),
			Port:               l.int("QDRANT_PORT", 6334),
			APIKey:             l.secret("QDRANT_API_KEY"),
			ProductCollection:  l.str("QDRANT_PRODUCT_COLLECTION", "crosspilot_products"),
			CategoryCollection: l.str("QDRANT_CATEGORY_KB_COLLECTION", "crosspilot_category_kb"),
		},
		LLM: LLMConfig{
			DefaultProvider: l.str("LLM_DEFAULT_PROVIDER", "qwen"),
			Providers: map[string]ProviderConfig{
				"qwen": l.provider("QWEN", "https://dashscope.aliyuncs.com/compatible-mode/v1", "qwen3-max"),
				"minimax": l.provider(
					"MINIMAX", "https://api.minimax.chat/v1", "MiniMax-M2"),
				"deepseek": l.provider("DEEPSEEK", "https://api.deepseek.com/v1", "deepseek-chat"),
				"anthropic": l.provider(
					"ANTHROPIC", "https://api.anthropic.com", "claude-sonnet-5-5"),
			},
			RequestTimeout: l.duration("LLM_REQUEST_TIMEOUT", 60*time.Second),
			MaxRetries:     l.int("LLM_MAX_RETRIES", 2),
		},
		Auth: AuthConfig{
			JWTSecret: l.secret("AUTH_JWT_SECRET"),
			JWTTyp:    l.str("AUTH_JWT_TYP", "crosspilot-access+jwt"),
			TokenTTL:  l.duration("AUTH_ACCESS_TOKEN_TTL", 24*time.Hour),
		},
		OTel: OTelConfig{
			Endpoint:    l.str("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
			ServiceName: l.str("OTEL_SERVICE_NAME", "crosspilot-api"),
			SampleRatio: l.float("OTEL_TRACES_SAMPLER_ARG", 1.0),
		},
	}

	l.validate(cfg)

	if err := l.err(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loader 累积读取过程中的错误与已取得的量。
type loader struct {
	lookup Lookup
	errs   []error
}

func (l *loader) err() error {
	if len(l.errs) == 0 {
		return nil
	}
	return errors.Join(l.errs...)
}

func (l *loader) raw(key string) (string, bool) {
	if l.lookup == nil {
		return "", false
	}
	value, ok := l.lookup(key)
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	return value, true
}

func (l *loader) str(key, fallback string) string {
	if value, ok := l.raw(key); ok {
		return value
	}
	return fallback
}

// boundedInt32 读取一个落在 [min, max] 内的整数并转成 int32。
//
// 越界时记录错误并回退到默认值：把范围检查摆在窄化转换之前，
// 避免一个过大的配置值被静默截断成另一个数。
func (l *loader) boundedInt32(key string, fallback, low, high int) int32 {
	value := l.int(key, fallback)
	if value < low || value > high {
		l.errs = append(l.errs, fmt.Errorf("config: %s 应在 [%d, %d] 内，实际 %d", key, low, high, value))
		return int32(fallback) //nolint:gosec // G115：fallback 是包内常量，且上面已确保它在 [low, high] 内
	}
	return int32(value) //nolint:gosec // G115：上面的范围检查已保证 value 落在 int32 可表示范围内
}

// list 读取逗号分隔的多值配置，空白项被丢弃。
func (l *loader) list(key string, fallback []string) []string {
	raw, ok := l.raw(key)
	if !ok {
		return fallback
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (l *loader) secret(key string) Secret {
	value, _ := l.raw(key)
	return Secret(value)
}

func (l *loader) requiredSecret(key string) Secret {
	value, ok := l.raw(key)
	if !ok {
		l.errs = append(l.errs, fmt.Errorf("config: 必填环境变量 %s 未设置", key))
	}
	return Secret(value)
}

func (l *loader) int(key string, fallback int) int {
	raw, ok := l.raw(key)
	if !ok {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("config: %s 应为整数，实际 %q", key, raw))
		return fallback
	}
	return value
}

func (l *loader) float(key string, fallback float64) float64 {
	raw, ok := l.raw(key)
	if !ok {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("config: %s 应为小数，实际 %q", key, raw))
		return fallback
	}
	return value
}

func (l *loader) duration(key string, fallback time.Duration) time.Duration {
	raw, ok := l.raw(key)
	if !ok {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("config: %s 应为时长（如 30s、5m），实际 %q", key, raw))
		return fallback
	}
	return value
}

// provider 读取一家供应商的参数，键名形如 LLM_QWEN_API_KEY。
func (l *loader) provider(suffix, baseURL, model string) ProviderConfig {
	name := strings.ToLower(suffix)
	return ProviderConfig{
		Name:    name,
		BaseURL: l.str("LLM_"+suffix+"_BASE_URL", baseURL),
		APIKey:  l.secret("LLM_" + suffix + "_API_KEY"),
		Model:   l.str("LLM_"+suffix+"_MODEL", model),
	}
}

// validate 做跨字段校验。
func (l *loader) validate(cfg *Config) {
	switch cfg.Env {
	case EnvDevelopment, EnvStaging, EnvProduction:
	default:
		l.errs = append(l.errs, fmt.Errorf("config: CROSSPILOT_ENV 应为 %s/%s/%s 之一，实际 %q",
			EnvDevelopment, EnvStaging, EnvProduction, cfg.Env))
	}

	switch cfg.Log.Format {
	case "json", "text":
	default:
		l.errs = append(l.errs, fmt.Errorf("config: CROSSPILOT_LOG_FORMAT 应为 json 或 text，实际 %q", cfg.Log.Format))
	}

	switch cfg.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		l.errs = append(l.errs, fmt.Errorf("config: CROSSPILOT_LOG_LEVEL 应为 debug/info/warn/error 之一，实际 %q", cfg.Log.Level))
	}

	// POSTGRES_MAX_CONNS 的范围由 boundedInt32 在读取时就把住，这里不再重复

	if cfg.OTel.SampleRatio < 0 || cfg.OTel.SampleRatio > 1 {
		l.errs = append(l.errs, fmt.Errorf("config: OTEL_TRACES_SAMPLER_ARG 应在 [0,1]，实际 %v", cfg.OTel.SampleRatio))
	}
	if _, ok := cfg.LLM.Providers[cfg.LLM.DefaultProvider]; !ok {
		l.errs = append(l.errs, fmt.Errorf("config: LLM_DEFAULT_PROVIDER %q 不在已配置的供应商中",
			cfg.LLM.DefaultProvider))
	}
}
