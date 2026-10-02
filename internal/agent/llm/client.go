// Package llm 是 OpenAI 兼容协议的模型适配器。
//
// 覆盖 qwen / deepseek / minimax 三家——它们的 /chat/completions 同一协议族。
// Anthropic 的 Messages API 是另一族，不在本包内（装配时按「未接入」处理）。
//
// 为什么在 agent 层而不是 infra：编排层需要 DecisionProvider 实现，而分层规则
// 禁止 infra 依赖 internal/agent。反过来 agent 层不能依赖 internal/config，
// 所以本包只收扁平参数，由装配根把 cfg.LLM 映射进来。
//
// 只用标准库：agent 组件的第三方白名单只有 uuid，引入 HTTP 客户端库要改架构规则。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/protocol"
)

// errNoChoices 表示上游返回了 200 但没有任何候选项。
var errNoChoices = errors.New("llm: 响应缺少 choices")

// maxErrorBody 是错误响应体进入日志前保留的字节数。
//
// 截断而不是丢弃：上游的错误信息是排障的唯一线索；但也不能无限留——
// 网关的 HTML 错误页动辄几十 KB，整段塞进日志既没用又淹掉上下文。
const maxErrorBody = 512

// Config 是适配器的全部输入。
//
// 刻意用扁平字段而不是 config.ProviderConfig：agent 层不允许依赖 internal/config，
// 「把配置翻译成参数」是装配根的职责。
type Config struct {
	BaseURL string
	// APIKey 是明文，只从装配根的 Secret.Reveal() 来。
	// 它是非导出字段的初值，不会出现在任何日志或错误信息里。
	APIKey string
	// Provider 是供应商标识（qwen / deepseek / …），可选。
	//
	// 它不参与任何请求构造，只是被回填到响应上供成本归因查价格表——价格表按
	// (provider, model) 定键，而只有适配器知道自己在替哪一家说话。
	Provider   string
	Model      string
	Timeout    time.Duration
	MaxRetries int
	// HTTPClient 供测试注入 httptest 的客户端；nil 时按 Timeout 构建。
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// Client 实现 orchestrator.DecisionProvider。
type Client struct {
	baseURL string
	// apiKey 非导出：slog.Any("client", c) 之类的意外打印不会泄漏它。
	apiKey   string
	provider string
	model    string
	client   *http.Client
	retries  int
	logger   *slog.Logger
}

// New 构造适配器。BaseURL 与 Model 为空是装配错误，直接报出来，
// 而不是等到第一次调用才在上游拿到一个语义模糊的 400。
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("llm: BaseURL 必填")
	}
	if cfg.Model == "" {
		return nil, errors.New("llm: Model 必填")
	}

	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Client{
		baseURL:  strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:   cfg.APIKey,
		provider: cfg.Provider,
		model:    cfg.Model,
		client:   hc,
		retries:  max(cfg.MaxRetries, 0),
		logger:   logger,
	}, nil
}

// Next 实现 protocol.Provider：完成一次模型调用，必要时重试。
//
// 只重试瞬时错误（网络故障、429、5xx）。4xx 是确定性的——重试只会把同一个
// 失败重复一遍，还拖长调用方的等待。
func (c *Client) Next(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	body, err := json.Marshal(toWireRequest(c.model, req))
	if err != nil {
		return protocol.Response{}, fmt.Errorf("llm: 请求序列化失败: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			// 退避期间也要能被取消：调用方按下停止后不该还等满退避时间。
			select {
			case <-ctx.Done():
				return protocol.Response{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * 400 * time.Millisecond):
			}
		}

		resp, err := c.doOnce(ctx, body)
		if err == nil {
			// 回填来源标识。放在这里而不是 doOnce 里：doOnce 是纯传输，
			// 「谁回答了这次请求」是适配器对自己身份的知识，重试成功后同样成立。
			resp.Provider = c.provider
			resp.Model = c.model
			return resp, nil
		}
		lastErr = err

		if !retriable(err) {
			return protocol.Response{}, err
		}
		c.logger.WarnContext(ctx, "llm: 调用失败，准备重试",
			slog.String("model", c.model),
			slog.Int("attempt", attempt),
			slog.Any("error", err),
		)
	}
	// 原样返回最后一次失败，不包装成 ErrModelUnavailable：那是「没配模型」的
	// sentinel，回 503 表示「去开配置」。上游一阵抖动是另一回事——调用方应当
	// 看到一次普通的调用失败（500），而不是被告知去改环境变量。
	return protocol.Response{}, lastErr
}

// doOnce 发一次请求。body 由调用方预先序列化，重试时复用同一份字节。
func (c *Client) doOnce(ctx context.Context, body []byte) (protocol.Response, error) {
	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return protocol.Response{}, fmt.Errorf("llm: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// 明文的唯一出口。这一行之上不打印 header、之下不返回值。
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	res, err := c.client.Do(httpReq)
	if err != nil {
		return protocol.Response{}, err
	}
	defer func() { _ = res.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return protocol.Response{}, fmt.Errorf("llm: 读取响应失败: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return protocol.Response{}, &StatusError{
			Status: res.StatusCode,
			Body:   truncate(string(raw), maxErrorBody),
		}
	}

	resp, err := parseWireResponse(raw)
	if err != nil {
		return protocol.Response{}, fmt.Errorf("llm: 解析响应失败: %w", err)
	}
	return resp, nil
}

// StatusError 是上游返回的非 2xx。
//
// 实现 Timeout() 而不是自造一套瞬时/确定性分类：net.Error 就是这个形状，
// 熔断器与重试逻辑都按它判「值不值得再来一次」。429 限流与 5xx 是瞬时的，
// 4xx 其余是确定性的。
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("llm: HTTP %d: %s", e.Status, e.Body)
}

// Timeout 报告这个状态码是否代表「稍后重试可能成功」。
func (e *StatusError) Timeout() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// retriable 判定一次失败是否值得重试。
//
// 网络层错误（连接被拒、DNS、超时）一律可重试；ctx 取消由上层识别，
// 这里返回 true 也不会有害——下一次尝试会立刻看到 ctx.Done。
func retriable(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Timeout()
	}
	return true
}

// truncate 把超长文本按字节截断，并标明发生过截断。
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…(已截断)"
}
