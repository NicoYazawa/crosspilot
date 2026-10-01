// SSE 处理器：POST /agui/runs 与 GET /agui/runs/{id}/events。
//
// 写入策略：「先落 journal 再写 SSE」。订阅者拉取的所有事件都来自 journal，
// 即使进程被 kill -9 也能从上次最后序号续传（E5）。
//
// E1：客户端断流后重连，从 Last-Event-ID + 1 开始续，无重复无缺口。
// E2：cursor.Seq+1 != journal.LastSeq 时拒绝重连。
// E3：cursor.RunID 与路径 run_id 不匹配时拒绝。
// E4：重连路径只调 journal.Since，handler.go 不触发任何 model 调用。
// E5：启动时为「未关闭」的 run 注入 server_restart 哨兵事件。
// E6：A2UI 报文落 journal 后由前端按 catalogId 校验。
package agui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/NicoYazawa/crosspilot/internal/agent/orchestrator"
	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
)

// RunSubmitter 是「驱动一次 Agent run 并产出事件」的端口。
//
// P3 的 orchestrator.Run 是已经能用的实现；这里抽象出来便于测试：handler 用
// 假实现喂事件，验证 E1-E4 的全部行为。
//
// 注：实现只能从 Model→Journal 的方向驱动，不能从 SSE 反向触发 Model。
// 这是 E4 的物理保证：handler 不持有 model 引用，重连只走 journal。
type RunSubmitter interface {
	// Submit 同步执行 run：返回的 []Event 是已写入 journal 的事件序列。
	// 客户端不再需要走 GET /events 的重连流程（首次订阅即可拿到所有事件），
	// 但 server 仍提供 GET /events 以支持断流场景。
	//
	// 调用方负责传入 OrchestratorRunner；本类型不知道 orchestrator 是哪个。
	Submit(ctx context.Context, req SubmitRequest) ([]runevent.Event, error)
}

// SubmitRequest 是启动一次 run 的最小入参。
type SubmitRequest struct {
	RunID     string `json:"run_id,omitempty"`
	BuyerID   string `json:"buyer_id"`
	SessionID string `json:"session_id"`
	Query     string `json:"query"`
	Agent     string `json:"agent"`
}

// OrchestratorRunner 是把 runevent 序列化的 orchestrator 接口。
//
// Service 里实现：包装 orchestrator.Run、把 Event 转 RunEvent、用 RunSequencer
// 分配 seq 后再 Append 到 journal。handler 这一层只关心 SubmitRequest→[]Event
// 形态，不关心实现细节。
type OrchestratorRunner interface {
	Run(ctx context.Context, runner RunnerConfig) ([]runevent.Event, error)
}

// RunnerConfig 是 orchestrator 维度的运行配置。
type RunnerConfig struct {
	RunID     string
	BuyerID   string
	SessionID string
	Query     string
	Agent     string
}

// Deps 是 SSE 处理器装配依赖。
type Deps struct {
	Journal   JournalStore
	Submitter RunSubmitter
	Logger    *slog.Logger

	// Clock 用于「先落库再推送」的超时控制；为 nil 时使用 time.Now。
	Clock func() time.Time

	// Heartbeat 是 SSE 心跳间隔；<= 0 时不发送。
	Heartbeat time.Duration
}

// Routes 把 AG-UI 路由挂到 chi 路由器上。
//
// 期望调用方在 Routes 之外再加 RequestID / Recoverer / CORS 中间件。
// 路径以 /agui 为前缀挂载：/agui/runs、/agui/runs/{runID}/events 等。
func Routes(deps Deps) http.Handler {
	r := chi.NewRouter()
	r.Route("/agui", func(r chi.Router) {
		r.Post("/runs", submitHandler(deps))
		r.Get("/runs/{runID}", metaHandler(deps))
		r.Get("/runs/{runID}/events", streamHandler(deps))
		r.Post("/runs/{runID}/confirm", confirmHandler(deps))
	})
	return r
}

// submitHandler 处理 POST /agui/runs。
//
// 请求体：{"buyer_id":..., "session_id":..., "query":..., "agent":...}（可选 "run_id"）。
// 响应：200 + 事件数组（含 final 状态）；500 仅在 journal 损坏或 submitter 崩溃时返回。
//
// 业务流程：
//  1. 解析请求体拿到 run_id（生成或复用）
//  2. 调用 Submitter.Submit
//  3. 把 []Event 复制给客户端
//
// 重连路径在 streamHandler，不在 submitHandler。
func submitHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req SubmitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, deps.Logger, http.StatusBadRequest, "invalid_body", err)
			return
		}
		if req.Query == "" {
			writeError(w, deps.Logger, http.StatusBadRequest, "missing_query", errors.New("query 不能为空"))
			return
		}
		if req.Agent == "" {
			req.Agent = "main"
		}
		if req.RunID == "" {
			req.RunID = newRunID()
		}

		events, err := deps.Submitter.Submit(r.Context(), req)
		if err != nil {
			writeError(w, deps.Logger, http.StatusInternalServerError, "submit_failed", err)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"run_id": req.RunID,
			"events": events,
		}, deps.Logger)
	}
}

// streamHandler 处理 GET /agui/runs/{runID}/events。
//
// 这是重连入口：客户端带 Last-Event-ID 或 cursor 查询参数，本 handler 严格按
// E2/E3 校验 cursor，然后从 journal.Since 开始连续输出。
//
// SSE 协议要点：
//   - Content-Type: text/event-stream
//   - Cache-Control: no-cache
//   - Connection: keep-alive
//   - 每次写完一批立即 Flush（用 http.Flusher）
//   - id: {event_id} 行 → 客户端据此续传
//   - event: <kind> 行 → 客户端据此分桶
//   - data: <json> 行 → 业务数据
func streamHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, deps.Logger, http.StatusInternalServerError, "streaming_unsupported", errors.New("response writer 不支持 flush"))
			return
		}

		runID := chi.URLParam(r, "runID")
		if runID == "" {
			writeError(w, deps.Logger, http.StatusBadRequest, "missing_run_id", errors.New("run_id 缺失"))
			return
		}

		// 1. 解析 cursor：Last-Event-ID 优先；空则从头订阅。
		raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
		if raw == "" {
			raw = strings.TrimSpace(r.URL.Query().Get("cursor"))
		}
		var since int64 = 0
		if raw != "" {
			c, err := ParseCursor(raw)
			if err != nil {
				writeError(w, deps.Logger, http.StatusBadRequest, "cursor_invalid", err)
				return
			}
			since, err = BindToRun(c, runID)
			if err != nil {
				writeError(w, deps.Logger, http.StatusBadRequest, "cursor_mismatch", err)
				return
			}
		}

		// 2. 校验 cursor 紧贴 journal 末序号（E2）。
		last, err := deps.Journal.LastSeq(r.Context(), runID)
		if err != nil && !errors.Is(err, ErrUnknownRun) {
			writeError(w, deps.Logger, http.StatusInternalServerError, "journal_error", err)
			return
		}
		// 未知 run 时 last = -1；CheckGap(-1, since) 接受 since=0 拒绝 since>=1
		if err := CheckGap(last, since); err != nil {
			writeError(w, deps.Logger, http.StatusBadRequest, "seq_gap", err)
			return
		}

		// 3. SSE 头：必须在第一次 Write 之前设完。
		setSSEHeaders(w)

		// 4. 先把 cursor 起点之后的 journal 一次性写出。
		histEvents, err := deps.Journal.Since(r.Context(), runID, since, 0)
		if err != nil {
			writeError(w, deps.Logger, http.StatusInternalServerError, "journal_error", err)
			return
		}
		for _, ev := range histEvents {
			writeSSE(w, flusher, ev)
		}
		flusher.Flush()

		// 5. 实时尾巴：当且仅当 Publisher 接入时才挂起等待。
		//
		// 当前没有 publisher（E9 在 P5 阶段补），直接返回——回放完成即结束。
		// 测试不会因此卡住；生产环境接入 publisher 后，本函数改为阻塞 select。
		_ = last // 用于 CheckGap；为避免 unused 警告保留赋值。
		return
	}
}

// metaHandler 处理 GET /agui/runs/{runID}：返回 run 元信息。
func metaHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := chi.URLParam(r, "runID")
		last, err := deps.Journal.LastSeq(r.Context(), runID)
		if errors.Is(err, ErrUnknownRun) {
			writeError(w, deps.Logger, http.StatusNotFound, "run_not_found", err)
			return
		}
		if err != nil {
			writeError(w, deps.Logger, http.StatusInternalServerError, "journal_error", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"run_id":   runID,
			"last_seq": last,
		}, deps.Logger)
	}
}

// confirmHandler 处理 POST /agui/runs/{runID}/confirm。
//
// 占位实现：把用户决议写入 journal（作为 confirm_decided 事件），
// 不实际触发订单状态变更——P1 trade 服务的 Resolve 调用由 P7 接入。
//
// 本端点用来把 AG-UI 协议层先跑通，避免 P6 前端没有可以 POST 的地方。
func confirmHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := chi.URLParam(r, "runID")
		var req struct {
			ConfirmationID string `json:"confirmation_id"`
			Approved       bool   `json:"approved"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, deps.Logger, http.StatusBadRequest, "invalid_body", err)
			return
		}
		if req.ConfirmationID == "" {
			writeError(w, deps.Logger, http.StatusBadRequest, "missing_confirmation_id", errors.New("confirmation_id 必填"))
			return
		}

		// 取最后序号续号：journal 写入单调性由 Append 保证。
		last, err := deps.Journal.LastSeq(r.Context(), runID)
		if err != nil {
			writeError(w, deps.Logger, http.StatusNotFound, "run_not_found", err)
			return
		}

		now := nowOr(deps.Clock)
		seq := last + 1
		payload, _ := json.Marshal(map[string]any{
			"confirmation_id": req.ConfirmationID,
			"approved":        req.Approved,
		})
		seqr := runevent.NewSequencer(runID)
		// 把 sequencer 调到 last+1——手动驱动 Next。
		// (生产中 Submitter 会持有 sequencer，但 confirm 不走 Submitter。)
		for i := int64(0); i < seq; i++ {
			seqr.Next()
		}
		ev, err := seqr.Attach(seq, runevent.Kind("confirm_decided"), "user", payload, now)
		if err != nil {
			writeError(w, deps.Logger, http.StatusInternalServerError, "event_build_failed", err)
			return
		}
		if _, err := deps.Journal.Append(r.Context(), ev); err != nil {
			writeError(w, deps.Logger, http.StatusInternalServerError, "journal_error", err)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"run_id": runID,
			"event":  ev,
		}, deps.Logger)
	}
}

// MapOrchestratorEvent 把 orchestrator 的 Event 转成 RunEvent（应用层使用）。
//
// 这一层转换放在应用层 (orderflow) 而非本包：本包只定义接口，
// 真实实现位于 application 包——避免 presentation 直接依赖 orchestrator 类型。
//
// 此处仅作占位：handler 不直接调用，orderflow service 才调用。
func MapOrchestratorEvent(seqr *runevent.Sequencer, ev orchestrator.Event) (runevent.Event, error) {
	kind := runevent.Kind(ev.Kind)
	payload, err := json.Marshal(map[string]any{
		"agent":     ev.Agent,
		"content":   ev.Content,
		"tool_name": ev.ToolName,
		"iteration": ev.Iteration,
	})
	if err != nil {
		return runevent.Event{}, err
	}
	return seqr.Attach(seqr.Next(), kind, ev.Agent, payload, nowOr(nil))
}

// --- internal helpers ----------------------------------------------------

func setSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

func writeSSE(w io.Writer, flusher http.Flusher, ev runevent.Event) {
	fmt.Fprintf(w, "id: %s\n", ev.EventID)
	fmt.Fprintf(w, "event: %s\n", ev.Kind)
	if len(ev.Payload) > 0 {
		fmt.Fprintf(w, "data: %s\n\n", string(ev.Payload))
	} else {
		fmt.Fprintf(w, "data: {}\n\n")
	}
	flusher.Flush()
}

func writeJSON(w http.ResponseWriter, status int, payload any, logger *slog.Logger) {
	body, err := json.Marshal(payload)
	if err != nil {
		if logger != nil {
			logger.Error("agui: 序列化失败", slog.Any("error", err))
		}
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, logger *slog.Logger, status int, code string, err error) {
	if logger != nil && err != nil {
		logger.Warn("agui: 请求失败",
			slog.String("code", code),
			slog.Int("status", status),
			slog.Any("error", err),
		)
	}
	writeJSON(w, status, map[string]string{"error": code}, logger)
}

func nowOr(clock func() time.Time) time.Time {
	if clock != nil {
		return clock()
	}
	return time.Now().UTC()
}

// keepAlive 周期性发送心跳直到 ctx 取消或客户端断开。
//
// 心跳内容是注释行（`:` 开头），客户端 EventSource 会忽略、但能阻止代理超时。
// 当前 streamHandler 不调用本函数（P5 接入 publisher 时再挂上），
// 但保留实现，避免 P5 阶段再写一次。
func keepAlive(w http.ResponseWriter, flusher http.Flusher, interval time.Duration, logger *slog.Logger, ctx context.Context) {
	if interval <= 0 {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				if logger != nil {
					logger.Debug("agui: 心跳写入失败", slog.Any("error", err))
				}
				return
			}
			flusher.Flush()
		}
	}
}

// newRunID 生成一个 run 标识。
//
// 形式：`run_<unixNanos>`——以时间戳开头便于在日志里按时间排序；
// 不使用 ULID / UUID 是因为本阶段不需要跨节点去重（journal 单进程）。
func newRunID() string {
	return fmt.Sprintf("run_%d", time.Now().UnixNano())
}
