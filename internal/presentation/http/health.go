package http

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// 探测项在响应里的取值。
const (
	stateUp   = "up"
	stateDown = "down"

	statusOK   = "ok"
	statusDown = "down"
)

// probeBudget 是全部探测项共享的时间上限。
//
// 健康检查自己也是被探测对象：一个在依赖失联时要花两秒才回话的 /health，
// 会被编排系统判成不健康——哪怕它想问的那件事本来是「数据库是不是挂了」。
// 因此这里给整轮探测一个硬上限，超时按该项不可用处理。
const probeBudget = time.Second

// Check 是一项就绪探测。
type Check struct {
	// Name 是该项在响应里的键名，如 db、redis。
	Name string
	// Probe 返回 nil 表示该项正常。
	Probe func(ctx context.Context) error
}

// healthResponse 是健康检查的响应体。
//
// 带版本与提交号：编排系统只关心 status，但排查线上问题时第一个要问的是
// 「这个实例跑的是哪一版」。把它挂在健康端点上是唯一不需要额外通道、
// 也不需要数据库就能拿到的事实。
type healthResponse struct {
	Status  string            `json:"status"`
	Version string            `json:"version"`
	Commit  string            `json:"commit"`
	Checks  map[string]string `json:"checks"`
}

// HealthHandler 汇总所有探测项，全部正常返回 200，任何一项失败返回 503。
//
// 响应里逐项标明状态，编排系统据此判断能否接流量，人也能一眼看出是哪一层
// 掉了。探测本身不重试也不缓存：健康检查应当反映此刻的真实状态。
//
// 各项并发执行，整体耗时取决于最慢的一项而不是它们的总和；再叠加 probeBudget
// 兜底，无论依赖如何表现，响应时间都有上限。
func HealthHandler(logger *slog.Logger, checks ...Check) http.HandlerFunc {
	build := ReadBuildInfo()
	return healthHandler(logger, build, checks...)
}

func healthHandler(logger *slog.Logger, build BuildInfo, checks ...Check) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), probeBudget)
		defer cancel()

		results := make([]string, len(checks))
		states := make(map[string]string, len(checks))
		var wg sync.WaitGroup

		for i, check := range checks {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := runProbe(ctx, check); err != nil {
					results[i] = stateDown
					if logger != nil {
						logger.WarnContext(ctx, "健康探测失败",
							slog.String("check", check.Name),
							slog.Any("error", err))
					}
					return
				}
				results[i] = stateUp
			}()
		}
		wg.Wait()

		overall := statusOK
		status := http.StatusOK
		for i, check := range checks {
			states[check.Name] = results[i]
			if results[i] == stateDown {
				overall = statusDown
				status = http.StatusServiceUnavailable
			}
		}

		writeJSON(w, status, healthResponse{
			Status:  overall,
			Version: build.Version,
			Commit:  build.Commit,
			Checks:  states,
		}, logger)
	}
}

// runProbe 执行单项探测，并把「超出预算」也算作不可用。
//
// 探测被放进独立协程，是为了在探测函数自己不遵守超时约定时仍能按时返回；
// 通道带缓冲，迟到的结果不会把协程卡住。
func runProbe(ctx context.Context, check Check) error {
	done := make(chan error, 1)
	go func() { done <- check.Probe(ctx) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("探测超时（上限 %s）: %w", probeBudget, ctx.Err())
	}
}

// LiveHandler 只报告进程本身还活着，不触碰任何外部依赖。
//
// 与就绪探测分开：依赖不可用时应当停止接流量，但不应当被反复重启。
func LiveHandler(logger *slog.Logger) http.HandlerFunc {
	build := ReadBuildInfo()
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, healthResponse{
			Status:  statusOK,
			Version: build.Version,
			Commit:  build.Commit,
			Checks:  map[string]string{},
		}, logger)
	}
}
