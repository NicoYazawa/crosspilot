// Package observability 是 P5 可观测三件套的 HTTP 适配器。
//
// 路由：
//
//	GET /observability/runs/{runID}/events?from=N&limit=M
//	  → 回放某 run 的一段事件（F1）
//	GET /observability/runs/{runID}/cost
//	  → 某 run 的成本摘要（F4 unpriced 显式）
//	GET /observability/runs/{runID}/diff?against=otherRunID
//	  → 两条 run 的事件序列对比（F2）
//	GET /observability/experiments/{key}/arms
//	  → 实验各臂聚合（F8 留 judge 缺口）
//	GET /observability/metrics
//	  → 通道运行时计数（F10 与 token 无关的指标）
//
// 这些路由不直接 export application/observability.UseCases 的方法——
// 通过 handler.go 包装，便于：
//   - 在 handler 层做参数解析与校验（不让 application 层知道 HTTP）
//   - 在中间件层做 auth/audit（日志里记录「谁查了哪个 run 的 cost」）
//   - 在容器层做依赖注入的统一生命周期
package observability

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	agentobs "github.com/NicoYazawa/crosspilot/internal/agent/observability"
	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
	"github.com/NicoYazawa/crosspilot/internal/application/observability"
)

// Handler 把 application/observability.UseCases 暴露成 HTTP。
type Handler struct {
	UC *observability.UseCases
	// Metrics 是观测通道的运行时计数；为 nil 时 /observability/metrics 返回 503。
	Metrics *agentobs.Metrics
}

// NewHandler 构造 handler。
func NewHandler(uc *observability.UseCases, metrics *agentobs.Metrics) *Handler {
	return &Handler{UC: uc, Metrics: metrics}
}

// Routes 返回可观测子路由，路径相对于挂载点。
//
// 前缀不写在这里，由装配层用 Mount("/observability", ...) 决定：chi 对同一个
// 挂载路径只允许挂一次，三个子应用若各自带着绝对前缀去挂 "/"，第二个就会
// panic——而这正是「子路由自己声明前缀」这种写法必然会走到的死胡同。
//
// 期望调用方在挂载点之外再加 RequestID / Recoverer / CORS 中间件。
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/runs/{runID}/events", h.replayEvents)
	r.Get("/runs/{runID}/cost", h.runCost)
	r.Get("/runs/{runID}/diff", h.runDiff)
	r.Get("/experiments/{key}/arms", h.experimentArms)
	r.Get("/metrics", h.metrics)
	return r
}

// metrics 返回当前观测通道的快照。
//
// 行为：Metrics 为 nil → 返回 503（说明容器装配阶段未挂指标收集器）。
// Metrics 非 nil → 返回 JSON 快照（与 token 无关的指标，F10 闸门）。
func (h *Handler) metrics(w http.ResponseWriter, _ *http.Request) {
	if h.Metrics == nil {
		writeError(w, http.StatusServiceUnavailable, "metrics 未挂载（容器未装配）")
		return
	}
	writeJSON(w, http.StatusOK, h.Metrics.Snapshot())
}

// ---- replay ----

type replayResponse struct {
	RunID    string           `json:"run_id"`
	Events   []runevent.Event `json:"events"`
	TotalSeq int64            `json:"total_seq"`
	HasMore  bool             `json:"has_more"`
}

func (h *Handler) replayEvents(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	if runID == "" {
		writeError(w, http.StatusBadRequest, "runID 不能为空")
		return
	}
	from, err := parseInt64Query(r, "from", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "from 不合法: "+err.Error())
		return
	}
	limit, err := parseIntQuery(r, "limit", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "limit 不合法: "+err.Error())
		return
	}

	res, err := h.UC.Replay(r.Context(), runID, from, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "回放失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, replayResponse{
		RunID:    res.RunID,
		Events:   res.Events,
		TotalSeq: res.TotalSeq,
		HasMore:  res.HasMore,
	})
}

// ---- cost ----

func (h *Handler) runCost(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	if runID == "" {
		writeError(w, http.StatusBadRequest, "runID 不能为空")
		return
	}
	res, err := h.UC.CostOfRun(r.Context(), runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "成本查询失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- diff ----

func (h *Handler) runDiff(w http.ResponseWriter, r *http.Request) {
	baseline := chi.URLParam(r, "runID")
	against := r.URL.Query().Get("against")
	if baseline == "" || against == "" {
		writeError(w, http.StatusBadRequest, "需要 ?against=otherRunID")
		return
	}
	if baseline == against {
		writeError(w, http.StatusBadRequest, "baseline 与 against 不能相同")
		return
	}
	res, err := h.UC.Diff(r.Context(), baseline, against)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "diff 失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- experiment arms ----

func (h *Handler) experimentArms(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "key 不能为空")
		return
	}
	res, err := h.UC.ExperimentArms(r.Context(), key)
	if err != nil {
		if errors.Is(err, observability.ErrExperimentNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"experiment": key,
		"arms":       res,
		"fetched_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// ---- helpers ----

func parseInt64Query(r *http.Request, name string, def int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}

func parseIntQuery(r *http.Request, name string, def int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%q 不是整数: %w", raw, err)
	}
	return v, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
