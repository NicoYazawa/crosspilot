// Package container 是唯一的装配根：把配置、基础设施与接口层接到一起。
//
// 依赖只在这里创建并向下注入，其他包一律通过构造函数接收所需依赖，不自行
// 读取全局状态、不自行连接外部服务。依赖方向因此是单向的，测试里也能按需
// 替换任意一层。
package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/NicoYazawa/crosspilot/internal/agent/llm"
	agentobs "github.com/NicoYazawa/crosspilot/internal/agent/observability"
	"github.com/NicoYazawa/crosspilot/internal/agent/orchestrator"
	"github.com/NicoYazawa/crosspilot/internal/agent/prompts"
	"github.com/NicoYazawa/crosspilot/internal/agent/protocol"
	"github.com/NicoYazawa/crosspilot/internal/agent/tools"
	"github.com/NicoYazawa/crosspilot/internal/application/catalogsearch"
	appobs "github.com/NicoYazawa/crosspilot/internal/application/observability"
	"github.com/NicoYazawa/crosspilot/internal/application/orderflow"
	tradesvc "github.com/NicoYazawa/crosspilot/internal/application/trade"
	"github.com/NicoYazawa/crosspilot/internal/config"
	"github.com/NicoYazawa/crosspilot/internal/infra/logging"
	infraobs "github.com/NicoYazawa/crosspilot/internal/infra/observability"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg"
	"github.com/NicoYazawa/crosspilot/internal/infra/postgres"
	infraredis "github.com/NicoYazawa/crosspilot/internal/infra/redis"
	otelobs "github.com/NicoYazawa/crosspilot/internal/observability"
	"github.com/NicoYazawa/crosspilot/internal/presentation/agui"
	presauth "github.com/NicoYazawa/crosspilot/internal/presentation/auth"
	"github.com/NicoYazawa/crosspilot/internal/presentation/commerce"
	presentation "github.com/NicoYazawa/crosspilot/internal/presentation/http"
	preobs "github.com/NicoYazawa/crosspilot/internal/presentation/observability"
	"github.com/NicoYazawa/crosspilot/internal/pricing"
)

// 探测项在 /health 响应里的键名。
const (
	checkDatabase = "db"
	checkRedis    = "redis"
)

// 默认 agent 名。与 AG-UI 的 SubmitRequest.Agent 缺省值保持一致。
const defaultAgent = "main"

// Container 持有进程运行期间的长生命周期依赖。
type Container struct {
	Config  *config.Config
	Logger  *slog.Logger
	Pool    *pgxpool.Pool
	Redis   *goredis.Client
	Handler http.Handler

	emitter       *agentobs.Emitter
	cancelEmitter context.CancelFunc
	shutdownOTel  func(context.Context) error
}

// Build 按配置装配整个进程。
//
// 装配过程不建立真实连接：数据库或缓存暂时不可用不会让启动失败，就绪性由
// /health 单独探测。这样依赖恢复之前服务就能先起来，编排系统也能看到明确的
// 「已启动但未就绪」，而不是反复重启一个起不来的进程。
func Build(ctx context.Context, cfg *config.Config, stderr io.Writer) (*Container, error) {
	logger, err := logging.New(cfg.Log, stderr)
	if err != nil {
		return nil, err
	}

	shutdownOTel, err := otelobs.Setup(ctx, cfg.OTel)
	if err != nil {
		return nil, err
	}

	pool, err := postgres.NewPool(ctx, cfg.Postgres)
	if err != nil {
		// 装配失败时已经建立的资源要还回去，否则导出器的后台协程会留在原地
		_ = shutdownOTel(ctx)
		return nil, err
	}

	redisClient := infraredis.NewClient(cfg.Redis, logger)

	app, err := buildApplication(ctx, cfg, logger, pool)
	if err != nil {
		_ = shutdownOTel(ctx)
		pool.Close()
		return nil, err
	}

	return &Container{
		Config:        cfg,
		Logger:        logger,
		Pool:          pool,
		Redis:         redisClient,
		Handler:       buildRouter(cfg, logger, pool, redisClient, app),
		emitter:       app.emitter,
		cancelEmitter: app.cancelEmitter,
		shutdownOTel:  shutdownOTel,
	}, nil
}

// application 是一组已经接好线的上层依赖。
//
// 单独抽出来是为了让 Build 的错误处理保持一条直线：任何一步装配失败，
// 已经建立的资源由 Build 统一归还，不必在每个 return 上重复一遍。
type application struct {
	journal *agui.MemoryJournal
	emitter *agentobs.Emitter
	// cancelEmitter 取消喂给 Emitter 的那个 ctx。Close 时在 emitter.Close()
	// **之后**调用：那之前取消会让最后一批以 context.Canceled 落不了库。
	cancelEmitter context.CancelFunc
	metrics       *agentobs.Metrics
	agui          *agui.Deps
	commerce      *commerce.Handler
	observability *preobs.Handler
}

// systemClock 是全部层共用的时钟实现。
//
// 各层各自声明了 Clock 接口（结构相同、名字相同），这里用一个类型同时满足它们：
// 时钟是同一件事，为每一层各包一个适配器只会让「现在几点」出现多个来源。
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// buildApplication 装配应用层与接口层的依赖。
//
// 接收 config：模型网关的接入参数（选哪家、base url、密钥、超时）只能从配置来，
// 而 agent 层不允许依赖 internal/config，所以由这里把 cfg.LLM 拍平后注入。
func buildApplication(
	_ context.Context,
	cfg *config.Config,
	logger *slog.Logger,
	pool *pgxpool.Pool,
) (*application, error) {
	clock := systemClock{}

	// --- 交易账本 ---
	store, err := pg.New(pg.Config{Pool: pool, Clock: clock, Logger: logger})
	if err != nil {
		return nil, err
	}
	tradeSvc, err := tradesvc.New(tradesvc.Config{
		Store:   store,
		Catalog: pg.NewCatalog(pool),
		Clock:   clock,
	})
	if err != nil {
		return nil, err
	}

	// --- 观测通道 ---
	//
	// journal 与 sink 是两条独立的下游：journal 是事实源（SSE 回放依据），
	// sink 是观测副本（成本与差分）。观测可以丢，journal 不能丢。
	journal := agui.NewMemoryJournal()

	// --- 模型网关 ---
	//
	// 先于观测通道构造：成本归因要按 (provider, model) 查价格表，而「配置里选了
	// 哪家、最后是不是真接上了」只有 buildDecisionProvider 知道（密钥缺失或
	// anthropic 未接入时会退回 unavailableModel）。在别处重推一遍身份，迟早会
	// 与它分叉。
	model, identity := buildDecisionProvider(cfg, logger)

	metrics := agentobs.NewMetrics("crosspilot")
	redactor, err := infraobs.NewRedactor()
	if err != nil {
		return nil, err
	}
	// 价格表：默认用内嵌表，PRICING_PRICEBOOK_PATH 非空时覆盖。
	// 解析失败直接让启动失败——带着一张坏表上线等于全量 unpriced，
	// 而那种降级在 /health 上看不出来。
	book, err := loadPriceBook(cfg.Pricing.PriceBookPath)
	if err != nil {
		return nil, err
	}
	logger.Info("价格表已加载",
		slog.String("version", book.Version),
		slog.String("source", book.Source),
		slog.Int("entries", book.Size()))

	// 当前生效的模型不在价格表里：现在就说，而不是等看板上出现 unpriced_count
	// 再回头找是哪一家没覆盖。这是运维提示，不是错误——未定价是显式记录的状态，
	// 服务应当照常跑。
	if identity.Model != "" && !book.HasModel(identity.Provider, identity.Model) {
		logger.Warn("当前模型不在价格表中：其调用将以 unpriced 落库（不会被当成 0 元）",
			slog.String("provider", identity.Provider),
			slog.String("model", identity.Model),
			slog.String("price_book_version", book.Version))
	}

	emitter := agentobs.NewEmitter(
		// 成本装饰器包住 pg sink：只有装配根可以引用 internal/pricing。
		newPricingSink(pg.NewObsSink(pool, logger), book, identity, logger),
		agentobs.EmitterConfig{
			Logger:   logger,
			Redactor: redactor,
			Metrics:  metrics,
		})

	// 启动后台 flush 协程。这一步此前是缺的——Emitter 造出来了，但 Run 从没有
	// 调用点，于是 Emit 只是往一个 1024 缓冲的 channel 里塞记录，没有消费者，
	// 全部在进程退出时静默消失（观测四张表恒为 0 行）。
	//
	// ctx 必须独立于 Build 收到的信号 ctx：Emitter 退出时用「Run 收到的那个 ctx」
	// 去 flush 最后一批，而信号 ctx 在关闭时已经被取消，那批会以 context.Canceled
	// 全部丢掉——且错误被 errors.Is(err, context.Canceled) 静默跳过，日志里什么
	// 都看不到。「优雅关闭了，但最后一批没落库」正是这么来的。
	//nolint:gosec // G118：cancel 不是被丢弃，而是存进 Container.cancelEmitter，由 Close 在 emitter.Close() 之后调用
	emitterCtx, cancelEmitter := context.WithCancel(context.Background())
	emitter.Start(emitterCtx)

	// --- 商品检索 ---
	//
	// embedder / vectorIndex / reranker 传 nil：Qdrant 集合当前为空，用例会自动
	// 退回关键词召回（见 catalogsearch.UseCase.executeSearch）。tariffSchedule 传
	// nil 表示没有汇率表，到手价会如实标成「不可用」而不是算一个编造的运费。
	searchUC := catalogsearch.New(persistence.NewCatalogRepo(pool), nil, nil, nil, nil)

	// --- Agent 工具 ---
	//
	// 只注册已实现的工具。声明里不出现没 handler 的工具，模型就不会去调它们
	// 然后每步都拿到 unknown tool（见 toolDefs 的过滤）。
	executor := tools.NewExecutor()
	executor.Register(tools.ToolProductSearch, newProductSearchHandler(searchUC))

	// --- Agent 编排 ---
	orchestratorInstance := orchestrator.New(model, executor)
	orchestratorInstance.SystemPrompt = prompts.MainAgent
	orchestratorInstance.Tools = toolDefs(executor)

	bridge := &orderflow.Bridge{
		Orch:         orchestratorInstance,
		J:            journal,
		C:            clock,
		DefaultAgent: defaultAgent,
		Emitter:      emitter,
	}

	// --- 可观测用例 ---
	//
	// 回放与差分读 journal；成本与实验读 Postgres 的 observability schema。
	// 两者都不经过交易账本的事务——观测写入拖慢下单是不可接受的。
	useCases := appobs.New(journal, pg.NewCostStore(pool), pg.NewExperimentStore(pool), clock)

	return &application{
		journal:       journal,
		emitter:       emitter,
		cancelEmitter: cancelEmitter,
		metrics:       metrics,
		agui: &agui.Deps{
			Journal:   journal,
			Submitter: agui.NewSubmitter(bridge, defaultAgent),
			Canceller: bridge,
			Logger:    logger,
		},
		commerce:      commerce.NewHandler(tradeSvc, logger),
		observability: preobs.NewHandler(useCases, metrics),
	}, nil
}

// anthropicProviderName 是配置里 Anthropic 那家的名字。
// 它走 Messages 协议而不是 OpenAI 兼容协议，本阶段没有对应适配器。
const anthropicProviderName = "anthropic"

// buildDecisionProvider 按配置挑一个模型 provider。
//
// 任何一步不成立都**退回 unavailableModel 而不是让启动失败**：没配密钥是运维
// 状态，不是装配错误。服务照常起来、/health 照常绿，只有这一条链路明确回 503——
// 比一个起不来的容器容易排障得多。
// 除 provider 本身外还返回它的身份 (provider, model)，供成本归因查价格表。
// 身份随返回值一起出来，而不是让调用方从 cfg 里再推一遍：退回 unavailableModel
// 的那些分支同样要给出一个诚实的身份（此时 model 为空，成本一律标 unpriced）。
func buildDecisionProvider(
	cfg *config.Config,
	logger *slog.Logger,
) (orchestrator.DecisionProvider, modelIdentity) {
	pc, ok := cfg.LLM.Provider(cfg.LLM.DefaultProvider)
	if !ok {
		logger.Warn("配置里没有选中的模型 provider，AG-UI run 将返回 503",
			slog.String("provider", cfg.LLM.DefaultProvider))
		return unavailableModel{logger: logger}, modelIdentity{}
	}
	// 密钥缺失 / 协议未接时仍然报出配置声明的身份：这两种情况下真的会有调用
	// 尝试（都会立刻失败），把 (provider, model) 记下来比记空值更有助于排障。
	identity := modelIdentity{Provider: pc.Name, Model: pc.Model}

	if pc.APIKey.IsZero() {
		logger.Warn("未配置模型 API Key，AG-UI run 将返回 503",
			slog.String("provider", pc.Name))
		return unavailableModel{logger: logger}, identity
	}
	if pc.Name == anthropicProviderName {
		// 拿一个必然 400 的 OpenAI 格式请求去打 Messages 端点，只会得到一条
		// 语义模糊的上游报错；不如在这里就说清是协议没接。
		logger.Warn("anthropic 走 Messages 协议，适配器尚未实现，AG-UI run 将返回 503")
		return unavailableModel{logger: logger}, identity
	}

	client, err := llm.New(llm.Config{
		BaseURL: pc.BaseURL,
		// Reveal 是 Secret 取明文的唯一出口。这是它在本进程的第二个调用点
		// （第一个是 buildRouter 里的 JWT 密钥），明文只在这一次传递中出现。
		APIKey: pc.APIKey.Reveal(),
		// Provider 不参与请求构造，只被回填到响应上供成本归因查价格表——
		// 价格表按 (provider, model) 定键，而只有适配器知道自己在替哪家说话。
		Provider:   pc.Name,
		Model:      pc.Model,
		Timeout:    cfg.LLM.RequestTimeout,
		MaxRetries: cfg.LLM.MaxRetries,
		Logger:     logger,
	})
	if err != nil {
		logger.Error("模型适配器构造失败，AG-UI run 将返回 503", slog.Any("error", err))
		return unavailableModel{logger: logger}, identity
	}
	return client, identity
}

// loadPriceBook 按配置加载价格表：path 为空时用内嵌的默认表。
//
// 内嵌表解析失败 = 构建错误，必须让启动失败；外挂表解析失败同理——一张坏表
// 会让全量调用变成 unpriced，而在 /health 与错误率上看不出来。
func loadPriceBook(path string) (*pricing.PriceBook, error) {
	if path == "" {
		return pricing.Default()
	}
	book, err := pricing.LoadFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("装配: 加载价格表 %q 失败: %w", path, err)
	}
	if book.Size() == 0 {
		// LoadFromFile 对「文件不存在」返回空表 + nil（dev 友好）。但这里是
		// 运维显式配了路径的场景：路径写错却静默变成「全量 unpriced」，
		// 是把一个配置错误伪装成了业务状态。
		return nil, fmt.Errorf("装配: 价格表 %q 不存在或为空", path)
	}
	return book, nil
}

// toolDefs 把已注册 handler 的工具转成模型可见的声明。
//
// 为什么按 executor.Has 过滤：工具声明是模型对「自己能做什么」的唯一依据。
// 把没有实现的工具写进声明，模型会照着去调，然后每一步都拿到 unknown tool——
// 一轮意图被无意义的失败耗尽，而日志里看起来只是「模型不听话」。
//
// 排序是为了让发给模型的声明顺序确定：不确定的顺序会让同一份 prompt 在不同
// 进程得到不同的字节，prompt 缓存与 A/B 对比都会因此失效。
func toolDefs(executor *tools.Executor) []protocol.ToolDef {
	registry := tools.NewToolRegistry()

	out := make([]protocol.ToolDef, 0, len(registry.All()))
	for _, def := range registry.All() {
		if !executor.Has(def.Name) {
			continue
		}
		schema, err := registry.ToJSONSchema(def.Name)
		if err != nil {
			continue
		}
		params, err := json.Marshal(schema["parameters"])
		if err != nil {
			continue
		}
		out = append(out, protocol.ToolDef{
			Name:        def.Name,
			Description: def.Description,
			Parameters:  params,
		})
	}

	slices.SortFunc(out, func(a, b protocol.ToolDef) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// unavailableModel 是没有可用模型 provider 时的 DecisionProvider。
//
// 它存在的意义不是「占位」，而是把「能力未接入」与「这次调用失败了」分开：
// 前者回 503（去开配置，重试无用），后者回 500。混成一个会让运维拿着一条
// 500 去排查一个根本不存在的故障。
type unavailableModel struct {
	logger *slog.Logger
}

// Next 实现 orchestrator.DecisionProvider。
func (m unavailableModel) Next(context.Context, protocol.Request) (protocol.Response, error) {
	if m.logger != nil {
		m.logger.Warn("收到 agent run 请求，但模型网关尚未接入")
	}
	return protocol.Response{}, fmt.Errorf(
		"%w：请在环境变量中配置 LLM_<PROVIDER>_API_KEY（当前 provider 见 LLM_DEFAULT_PROVIDER）",
		protocol.ErrModelUnavailable)
}

func buildRouter(
	cfg *config.Config,
	logger *slog.Logger,
	pool *pgxpool.Pool,
	redisClient *goredis.Client,
	app *application,
) http.Handler {
	if len(cfg.Auth.JWTSecret) == 0 {
		// 只在这一个地方告警，且只说一次：漏配密钥的后果是「所有人都是同一个
		// demo 买家」，这不是崩溃而是一种静默降级，必须在启动日志里留下痕迹。
		logger.Warn("未配置 AUTH_JWT_SECRET：身份校验已停用，全部请求将使用 demo 买家身份")
	}

	return presentation.NewRouter(presentation.Deps{
		Config: cfg,
		Logger: logger,
		Checks: []presentation.Check{
			{
				Name:  checkDatabase,
				Probe: func(ctx context.Context) error { return postgres.Check(ctx, pool) },
			},
			{
				Name:  checkRedis,
				Probe: func(ctx context.Context) error { return infraredis.Check(ctx, redisClient) },
			},
		},
		Auth: presauth.Config{
			// Reveal 是 Secret 唯一允许取出明文的出口；它出现在装配根，
			// 因为这里是密钥从配置流向使用者的那一步，再往下就只是字节。
			Secret: []byte(cfg.Auth.JWTSecret.Reveal()),
			Typ:    cfg.Auth.JWTTyp,
			Logger: logger,
		},
		AGUI:          app.agui,
		Commerce:      app.commerce,
		Observability: app.observability,
	})
}

// Close 释放全部资源，并汇总关闭过程中的错误。
//
// 即使前一项关闭失败也继续关剩下的：漏关资源比多一条错误信息严重得多。
func (c *Container) Close(ctx context.Context) error {
	var errs []error

	if c.emitter != nil {
		// 先 Close 再 cancel，顺序不能反：Close 会让 Run 走 closed 分支、用当时
		// 仍存活的 emitterCtx flush 最后一批；反过来则那批会以 Canceled 全部丢掉，
		// 且因为错误被静默跳过，日志里连一行都看不到。
		if err := c.emitter.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if c.cancelEmitter != nil {
		c.cancelEmitter()
	}
	if c.Redis != nil {
		if err := c.Redis.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if c.Pool != nil {
		c.Pool.Close()
	}
	if c.shutdownOTel != nil {
		if err := c.shutdownOTel(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}
