// SSE 处理器：POST /commerce/ag-ui/run 与 GET /commerce/ag-ui/runs/{id}/events。
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
//
// 空行把它与 package 子句隔开：本包的包注释在 journal.go，Go 只认紧贴
// package 的那一段；不隔开会让两个文件都变成「包注释」，godoc 与 revive
// 都会各说各话。

package agui

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

	"github.com/go-chi/chi/v5"

	"github.com/NicoYazawa/crosspilot/internal/agent/protocol"
	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
	presauth "github.com/NicoYazawa/crosspilot/internal/presentation/auth"
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
//
// BuyerID / SessionID 标记为 json:"-"：它们只能来自中间件解析出的身份
// （JWT 的 sub 与 X-Session-ID），不能来自请求体。若允许请求体提供，
// 任何持有效令牌的调用方都能把 buyer_id 写成别人，从而以他人身份下单——
// 鉴权中间件算出来的身份会被这一行 JSON 直接推翻。
type SubmitRequest struct {
	RunID     string `json:"run_id,omitempty"`
	BuyerID   string `json:"-"`
	SessionID string `json:"-"`
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

// RunCanceller 是「中断一次在跑的 run」的端口。
//
// 单独声明而不是并进 RunSubmitter：取消与提交是两条独立的生命周期，
// 把它们塞进同一个接口会迫使每个测试替身都实现一个它并不关心的方法。
type RunCanceller interface {
	// Cancel 中断指定的 run；返回 false 表示该 run 不在跑。
	Cancel(runID string) bool
}

// Deps 是 SSE 处理器装配依赖。
type Deps struct {
	Journal   JournalStore
	Submitter RunSubmitter
	Logger    *slog.Logger

	// Canceller 是可选的运行控制端口。为 nil 时 cancel 端点返回 503——
	// 明确告知「这个部署没有接运行控制」，而不是回一个 200 假装取消成功。
	Canceller RunCanceller
}

// Routes 返回 AG-UI 子路由，路径相对于挂载点。
//
// 附录 B 第 8–11 条的路由前缀 /commerce/ag-ui 由装配层用 Mount 决定；
// 前缀不写在这里，是因为 chi 对同一个挂载路径只允许挂一次。
//
// 期望调用方在挂载点之外再加 RequestID / Recoverer / CORS 与鉴权中间件。
func Routes(deps Deps) http.Handler {
	r := chi.NewRouter()
	r.Post("/run", submitHandler(deps))
	r.Get("/runs/{runID}", metaHandler(deps))
	r.Get("/runs/{runID}/events", streamHandler(deps))
	r.Post("/runs/{runID}/cancel", cancelHandler(deps))
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

		// 身份只认中间件算出来的那一份。
		req.BuyerID = presauth.BuyerFrom(r.Context())
		req.SessionID = presauth.SessionFrom(r.Context())
		if req.BuyerID == "" {
			writeError(w, deps.Logger, http.StatusUnauthorized, "missing_buyer",
				errors.New("请求未携带买家身份"))
			return
		}

		events, err := deps.Submitter.Submit(r.Context(), req)
		if err != nil {
			// 分类依据是「调用方重试同一个请求会怎样」，而不是错误出在哪一层：
			// 没接模型是部署状态（重试无用，该去开配置），回 503 让调用方一眼看懂；
			// 其余 submit 失败保持 500，避免把真正的服务端故障伪装成「稍后重试」。
			// 这与 commerce.writeServiceError 的分类口径一致。
			if errors.Is(err, protocol.ErrModelUnavailable) {
				writeError(w, deps.Logger, http.StatusServiceUnavailable, "model_unavailable", err)
				return
			}
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
//   - data: <json> 行 → 完整的 runevent.Event（含 seq；客户端靠它做缺口检测）
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
		var since int64
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
			if err := writeSSE(w, flusher, ev); err != nil {
				// 订阅端已断开：后面的事件没有读者，继续写只是空转；而且响应
				// 已经写坏了，重试也补不回来。记一条日志收工。
				if deps.Logger != nil {
					deps.Logger.WarnContext(r.Context(), "agui: SSE 写出失败，中止回放",
						slog.String("run_id", runID),
						slog.Any("error", err))
				}
				return
			}
		}
		flusher.Flush()

		// 5. 实时尾巴：当且仅当 Publisher 接入时才挂起等待。
		//
		// 当前没有 publisher（E9 在 P5 阶段补），回放完成即结束。
		// 生产环境接入 publisher 后，本函数在这里改为阻塞 select。
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

// cancelHandler 处理 POST /commerce/ag-ui/runs/{runID}/cancel。
//
// 它调用 Canceller 中断底层推理，而不是往 journal 里写一条「已取消」事件了事。
// 只改状态位的取消会让模型继续跑到结束——调用方以为省下了 token，账单上
// 一个都没少。
//
// 决议（买家批准/拒绝某张确认单）不在这里：那是 /commerce/confirmations/
// {id}/resolve 的职责，走交易账本，与「停掉一次推理」是两件事。
func cancelHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := chi.URLParam(r, "runID")
		if runID == "" {
			writeError(w, deps.Logger, http.StatusBadRequest, "missing_run_id", errors.New("run_id 缺失"))
			return
		}
		if deps.Canceller == nil {
			writeError(w, deps.Logger, http.StatusServiceUnavailable, "cancel_unsupported",
				errors.New("本部署未接入运行控制"))
			return
		}
		if !deps.Canceller.Cancel(runID) {
			writeError(w, deps.Logger, http.StatusNotFound, "run_not_active",
				errors.New("该 run 不在运行中"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "cancelled": true}, deps.Logger)
	}
}

// --- internal helpers ----------------------------------------------------

func setSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

// writeSSE 写出一条完整的 SSE 帧。
//
// 先在内存里拼好再一次性 Write：一帧由 id/event/data 三行加空行组成，分几次
// 写意味着客户端可能读到「有 id 没 data」的半截帧；一次写至少保证帧内不撕裂。
//
// data 行放的是**整个 runevent.Event**，不是它的 payload。
//
// 曾经这里只写 ev.Payload，理由是「kind 已经在 event 行、event_id 已经在 id 行，
// data 里再放一遍是冗余」。这个理由漏掉了 seq：客户端做缺口检测（附录 C 第 4 条
// 「id: {runId}:{seq}，必须连续」）需要每条事件的序号，而 event_id 是
// `{runId}:{seq}:{nanos}` 这种内部形态，不该让客户端去拆。于是前端按它自己的契约
// （types.ts 声明「Event JSON tag 与 runevent.go 对齐」）读 data.seq，读到 undefined，
// 把**每一条事件**都当成畸形帧丢掉——真链路上页面一条消息、一张卡片都渲染不出来。
//
// 两端各自的测试都用自己的格式，谁都没发现：后端这套用例只解析 id 行，从不看 data
// 里有什么；前端用例喂的是拼好的完整事件。这个缝现在由 handler_test 的
// TestSSEDataCarriesWholeEvent 钉住。
//
// 返回错误而不是吞掉：写失败说明订阅端已经走了，或者 journal 里存了无法序列化的
// 载荷。调用方据此停止回放——继续为不存在的读者生成事件，只会让一个已经断掉的
// 连接继续占着推理和网络。
func writeSSE(w io.Writer, flusher http.Flusher, ev runevent.Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("agui: 序列化事件 %s 失败：%w", ev.EventID, err)
	}

	var buf bytes.Buffer
	buf.WriteString("id: ")
	buf.WriteString(ev.EventID)
	buf.WriteString("\nevent: ")
	buf.WriteString(string(ev.Kind))
	buf.WriteString("\ndata: ")
	buf.Write(body)
	buf.WriteString("\n\n")

	if _, err := w.Write(buf.Bytes()); err != nil {
		return err
	}
	flusher.Flush()
	return nil
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

// newRunID 生成一个 run 标识。
//
// 形式：`run_<unixNanos>`——以时间戳开头便于在日志里按时间排序；
// 不使用 ULID / UUID 是因为本阶段不需要跨节点去重（journal 单进程）。
func newRunID() string {
	return fmt.Sprintf("run_%d", time.Now().UnixNano())
}
