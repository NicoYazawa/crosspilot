package agui_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/protocol"
	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
	"github.com/NicoYazawa/crosspilot/internal/presentation/agui"
	presauth "github.com/NicoYazawa/crosspilot/internal/presentation/auth"
)

// fakeSubmitter 记录每次 Submit 进来的请求；用于 E4 验证「重连不重计费」。
type fakeSubmitter struct {
	mu      sync.Mutex
	calls   []agui.SubmitRequest
	resp    []runevent.Event
	respErr error
}

func (f *fakeSubmitter) Submit(_ context.Context, req agui.SubmitRequest) ([]runevent.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	return f.resp, f.respErr
}

func (f *fakeSubmitter) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// LastCall 返回最后一次收到的提交请求；没有调用时 ok=false。
func (f *fakeSubmitter) LastCall() (agui.SubmitRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return agui.SubmitRequest{}, false
	}
	return f.calls[len(f.calls)-1], true
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// makeDeps 组装一组可工作的 Deps；submitter / journal 可由调用方覆盖。
func makeDeps(j agui.JournalStore, sub agui.RunSubmitter) agui.Deps {
	return agui.Deps{
		Journal:   j,
		Submitter: sub,
		Logger:    discardLog(),
	}
}

// serveWithIdentity 把 AG-UI 路由套上身份中间件再执行，模拟装配层的真实链路。
//
// Routes 自己不带鉴权（见其文档：身份由挂载层提供），所以直接调 Routes 等于
// 在测一条生产环境不存在的链路——submit 会因为拿不到买家身份而 401。
// 这里复刻 router.go 的挂载方式：先读会话头，再补 demo 身份。
func serveWithIdentity(deps agui.Deps, req *http.Request) *httptest.ResponseRecorder {
	h := presauth.SessionHeader()(
		presauth.DemoIdentity(demoBuyer, demoSession)(agui.Routes(deps)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 测试用的固定身份，与 router.go 的 demoBuyerID / demoSessionID 同构。
const (
	demoBuyer   = "test-buyer"
	demoSession = "test-session"
)

// submitEventsJSON 构造 submitter 的响应：n 条 RunEvent。
func submitEventsJSON(runID string, n int) []runevent.Event {
	out := make([]runevent.Event, n)
	seqr := runevent.NewSequencer(runID)
	for i := 0; i < n; i++ {
		kind := runevent.KindModelTurn
		if i == 0 {
			kind = runevent.KindRunStart
		}
		if i == n-1 {
			kind = runevent.KindRunFinished
		}
		body, _ := json.Marshal(map[string]any{"i": i})
		ev, err := seqr.Attach(seqr.Next(), kind, "main", body, time.Unix(1735000000+int64(i), 0).UTC())
		if err != nil {
			panic(err)
		}
		out[i] = ev
	}
	return out
}

// seedJournal 把 n 条事件塞进 journal。
func seedJournal(t *testing.T, j agui.JournalStore, runID string, n int) []runevent.Event {
	t.Helper()
	evs := submitEventsJSON(runID, n)
	for _, ev := range evs {
		if _, err := j.Append(context.Background(), ev); err != nil {
			t.Fatalf("seed Append 失败：%v", err)
		}
	}
	return evs
}

// sseFrame 是单条 SSE 帧。
type sseFrame struct {
	ID    string
	Event string
	Data  string
}

// parseSSE 从响应体里逐行解析 SSE 帧。
func parseSSE(t *testing.T, body io.Reader) []sseFrame {
	t.Helper()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	var frames []sseFrame
	var cur sseFrame
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if cur.ID != "" || cur.Event != "" || cur.Data != "" {
				frames = append(frames, cur)
			}
			cur = sseFrame{}
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		}
	}
	if cur.ID != "" || cur.Event != "" || cur.Data != "" {
		frames = append(frames, cur)
	}
	return frames
}

// TestE1_ResumeContinuity 验收 E1：客户端在 seq=N 断开，重连后从 N+1 续。
func TestE1_ResumeContinuity(t *testing.T) {
	j := agui.NewMemoryJournal()
	runID := "run-e1"
	seedJournal(t, j, runID, 5) // seq 0..4

	req := httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/events", nil)
	req.Header.Set("Last-Event-ID", runID+":2")

	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q", got)
	}

	frames := parseSSE(t, rec.Body)
	if len(frames) == 0 {
		t.Fatal("未读到任何 SSE 帧")
	}

	// cursor=2 意为「客户端已处理到 seq=2」，重连从 seq=3 起
	wantSeq := int64(3)
	for _, f := range frames {
		if f.ID == "" {
			continue
		}
		var got int64
		// 解析失败必须让测试失败：忽略错误时 got 保持零值，只要期望值不是 0
		// 就会「恰好」报错，而期望值一旦真是 0 就会把畸形 id 放过。
		if _, err := fmt.Sscanf(f.ID, runID+":%d:", &got); err != nil {
			t.Fatalf("id %q 不是 %s:<seq>: 形态：%v", f.ID, runID, err)
		}
		if got != wantSeq {
			t.Errorf("期望 id 含 seq=%d，实际 %q", wantSeq, f.ID)
		}
		wantSeq++
	}
	if wantSeq != 5 {
		t.Errorf("末 seq+1=%d 期望 5", wantSeq)
	}
}

// TestSSEDataCarriesWholeEvent 钉住 data 行的形态：它是完整的 runevent.Event，
// 不是只有 payload。
//
// 这条契约曾经两边都没测：后端这套用例只解析 id 行，从不看 data 里有什么；前端
// 用例喂的是自己拼好的完整事件。于是真链路上前端按 data.seq 做缺口检测（附录 C
// 第 4 条要求游标连续），读到 undefined，把每一条事件都当畸形帧丢掉——页面一条
// 消息、一张卡片都渲染不出来，而两端测试全绿。见 writeSSE 的说明。
func TestSSEDataCarriesWholeEvent(t *testing.T) {
	j := agui.NewMemoryJournal()
	runID := "run-data-shape"
	seeded := seedJournal(t, j, runID, 4)

	req := httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/events", nil)
	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	frames := parseSSE(t, rec.Body)
	if len(frames) != len(seeded) {
		t.Fatalf("帧数 = %d，期望 %d", len(frames), len(seeded))
	}

	for i, f := range frames {
		want := seeded[i]
		var got runevent.Event
		if err := json.Unmarshal([]byte(f.Data), &got); err != nil {
			t.Fatalf("第 %d 帧的 data 无法解析成 runevent.Event：%v（data=%s）", i, err, f.Data)
		}
		// 客户端实际依赖的四项：seq 判缺口、kind 分桶、run_id 认归属、
		// event_id 与 id 行对齐（断线续传要用它算游标）。
		if got.Seq != want.Seq {
			t.Errorf("第 %d 帧 data.seq = %d，期望 %d", i, got.Seq, want.Seq)
		}
		if got.Kind != want.Kind {
			t.Errorf("第 %d 帧 data.kind = %q，期望 %q", i, got.Kind, want.Kind)
		}
		if got.RunID != runID {
			t.Errorf("第 %d 帧 data.run_id = %q，期望 %q", i, got.RunID, runID)
		}
		if got.EventID != f.ID {
			t.Errorf("第 %d 帧 data.event_id = %q，而 id 行 = %q，两者必须一致", i, got.EventID, f.ID)
		}
		if string(got.Payload) != string(want.Payload) {
			t.Errorf("第 %d 帧 data.payload = %s，期望 %s", i, got.Payload, want.Payload)
		}
	}
}

// TestE2_GapRejected 验收 E2：cursor.Seq+1 != LastSeq 时拒绝重连。
func TestE2_GapRejected(t *testing.T) {
	j := agui.NewMemoryJournal()
	runID := "run-e2"
	seedJournal(t, j, runID, 5) // lastSeq=4

	req := httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/events", nil)
	req.Header.Set("Last-Event-ID", runID+":7") // 跳过 5,6

	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺口应被拒绝，状态码 = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "seq_gap") {
		t.Errorf("响应应包含 seq_gap，实际 %s", rec.Body.String())
	}
}

// TestE3_CrossRunRejected 验收 E3：cursor.RunID != target 时拒绝。
func TestE3_CrossRunRejected(t *testing.T) {
	j := agui.NewMemoryJournal()
	runID := "run-e3"
	seedJournal(t, j, runID, 3)

	req := httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/events", nil)
	req.Header.Set("Last-Event-ID", "run-other:1")

	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("跨 run 应被拒绝，状态码 = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cursor_mismatch") {
		t.Errorf("响应应包含 cursor_mismatch，实际 %s", rec.Body.String())
	}
}

// TestE4_ReconnectDoesNotCallModel 验收 E4：重连只 GET journal，绝不 POST 模型。
func TestE4_ReconnectDoesNotCallModel(t *testing.T) {
	j := agui.NewMemoryJournal()
	runID := "run-e4"
	seedJournal(t, j, runID, 5)

	sub := &fakeSubmitter{}

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/events", nil)
		req.Header.Set("Last-Event-ID", fmt.Sprintf("%s:%d", runID, 2+i))
		rec := httptest.NewRecorder()
		agui.Routes(makeDeps(j, sub)).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次重连状态码 %d", i, rec.Code)
		}
	}

	if sub.CallCount() != 0 {
		t.Errorf("Submitter 被调用 %d 次，重连不应触发模型", sub.CallCount())
	}
}

// TestE5_ServerRestartSentinel 验收 E5：journal 启动时为「未关闭 run」补 server_restart。
func TestE5_ServerRestartSentinel(t *testing.T) {
	j := agui.NewMemoryJournal()
	ctx := context.Background()

	// run-A：run_finished，不应被追加哨兵
	seedJournal(t, j, "run-A", 1)
	lastA, _ := j.LastSeq(ctx, "run-A")

	// run-B：未结束（没有 run_finished）
	if _, err := j.Append(ctx, runevent.Event{
		EventID: "manual:0:0", RunID: "run-B", Seq: 0, Kind: runevent.KindRunStart, Agent: "main",
		CreatedAt: time.Unix(1735000100, 0).UTC(),
		Payload:   json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(ctx, runevent.Event{
		EventID: "manual:1:0", RunID: "run-B", Seq: 1, Kind: runevent.KindModelTurn, Agent: "main",
		CreatedAt: time.Unix(1735000101, 0).UTC(),
		Payload:   json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	// 模拟 E5 启动恢复流程
	runs, err := j.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1735000200, 0).UTC()
	for _, r := range runs {
		if r.LastKind == runevent.KindRunFinished || r.LastKind == runevent.KindRunError || r.LastKind == runevent.KindServerRestart {
			continue
		}
		seqr := runevent.NewSequencer(r.RunID)
		for i := int64(0); i <= r.LastSeq; i++ {
			seqr.Next()
		}
		ev, _ := seqr.Attach(seqr.Next(), runevent.KindServerRestart, "system", []byte(`{"reason":"startup"}`), now)
		if _, err := j.Append(ctx, ev); err != nil {
			t.Fatalf("append server_restart 失败：%v", err)
		}
	}

	// run-A 末事件仍为 run_finished
	runA, _ := j.Since(ctx, "run-A", lastA, 1)
	if runA[0].Kind != runevent.KindRunFinished {
		t.Errorf("run-A 不应被追加哨兵，末事件 = %v", runA[0].Kind)
	}

	// run-B 应追加 server_restart
	bs, _ := j.LastSeq(ctx, "run-B")
	runBtail, _ := j.Since(ctx, "run-B", bs, 1)
	if runBtail[0].Kind != runevent.KindServerRestart {
		t.Errorf("run-B 应有 server_restart，末事件 = %v", runBtail[0].Kind)
	}
}

// TestE1_EmptyJournalResume 验证：客户端带 cursor 但 journal 为空时也能 200（since=1 > lastSeq+1=0）。
func TestE1_EmptyJournalResume(t *testing.T) {
	j := agui.NewMemoryJournal()
	runID := "run-empty"

	req := httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/events", nil)
	req.Header.Set("Last-Event-ID", runID+":0") // since=1, lastSeq=-1 → 1<=0 错误

	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec, req)

	// CheckGap(-1, 1)：since != 0 && since(=1) > lastSeq+1(=0) → 缺口
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空 journal + cursor:0 应拒绝，状态码=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "seq_gap") {
		t.Errorf("响应应包含 seq_gap，实际 %s", rec.Body.String())
	}
}

// TestE1_EmptyJournalFirstSub 验证：journal 为空且无 cursor 时正常返回（since=0）。
func TestE1_EmptyJournalFirstSub(t *testing.T) {
	j := agui.NewMemoryJournal()
	runID := "run-empty"

	req := httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/events", nil)

	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("空 journal + 首次订阅应成功，状态码=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q", got)
	}
}

// TestSubmitEndpoint_CallsSubmitter 验证 POST /runs 透传请求到 Submitter。
func TestSubmitEndpoint_CallsSubmitter(t *testing.T) {
	j := agui.NewMemoryJournal()
	sub := &fakeSubmitter{resp: submitEventsJSON("submit-run", 2)}

	body, _ := json.Marshal(map[string]any{
		"run_id":     "submit-run",
		"buyer_id":   "b1",
		"session_id": "s1",
		"query":      "test",
		"agent":      "main",
	})
	req := httptest.NewRequest(http.MethodPost, "/run", nil)
	req.Body = io.NopCloser(strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")

	rec := serveWithIdentity(makeDeps(j, sub), req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码=%d body=%s", rec.Code, rec.Body.String())
	}

	// 身份必须来自中间件，而不是请求体：body 里写的是 b1/s1，
	// submitter 收到的必须是中间件算出的 test-buyer/test-session。
	got, ok := sub.LastCall()
	if !ok {
		t.Fatal("submitter 未被调用")
	}
	if got.BuyerID != demoBuyer {
		t.Errorf("BuyerID = %q，期望来自上下文的 %q（请求体不得覆盖身份）", got.BuyerID, demoBuyer)
	}
	if got.SessionID != demoSession {
		t.Errorf("SessionID = %q，期望来自请求头的 %q", got.SessionID, demoSession)
	}
	if sub.CallCount() != 1 {
		t.Errorf("Submitter 应被调用 1 次，实际 %d", sub.CallCount())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["run_id"] != "submit-run" {
		t.Errorf("响应 run_id = %v", resp["run_id"])
	}
	evs, ok := resp["events"].([]any)
	if !ok || len(evs) != 2 {
		t.Errorf("响应 events 长度 = %d", len(evs))
	}
}

// TestE6_A2UI_ThreeMessagesAreValid 验收 E6：A2UI v0.9 三报文 schema 校验。
func TestE6_A2UI_ThreeMessagesAreValid(t *testing.T) {
	create := map[string]any{
		"action":    runevent.A2UIActionCreateSurface,
		"catalogId": runevent.A2UICatalogID,
		"version":   runevent.A2UIVersion,
		"components": []any{
			map[string]any{"id": "root", "type": "Column", "path": runevent.ShoppingRequirementsPath},
			map[string]any{"id": "search-input", "type": "TextInput"},
		},
	}
	if err := runevent.ValidateCreateSurface(create); err != nil {
		t.Errorf("createSurface 失败：%v", err)
	}

	updateComp := map[string]any{
		"action":    runevent.A2UIActionUpdateComponents,
		"catalogId": runevent.A2UICatalogID,
		"components": []any{
			map[string]any{"id": "card-1", "type": "ProductCard", "props": map[string]any{"title": "防水登山包"}},
		},
	}
	if err := runevent.ValidateUpdateComponents(updateComp); err != nil {
		t.Errorf("updateComponents 失败：%v", err)
	}

	updateData := map[string]any{
		"action":    runevent.A2UIActionUpdateDataModel,
		"catalogId": runevent.A2UICatalogID,
		"value": map[string]any{
			"path": runevent.ShoppingRequirementsPath,
			"data": []any{map[string]any{"key": "items"}},
		},
	}
	if err := runevent.ValidateUpdateDataModel(updateData); err != nil {
		t.Errorf("updateDataModel 失败：%v", err)
	}

	bad := map[string]any{
		"action":     runevent.A2UIActionCreateSurface,
		"catalogId":  "wrong.catalog",
		"components": []any{map[string]any{"id": "r", "type": "Column", "path": runevent.ShoppingRequirementsPath}},
	}
	if err := runevent.ValidateCreateSurface(bad); err == nil {
		t.Error("catalogId 不合规应被拒")
	}
}

// --- additional error-branch tests ---

// TestSubmitHandler_InvalidBody 验证非法 JSON 返回 400。
func TestSubmitHandler_InvalidBody(t *testing.T) {
	j := agui.NewMemoryJournal()
	req := httptest.NewRequest(http.MethodPost, "/run", nil)
	req.Body = io.NopCloser(strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON 应返回 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_body") {
		t.Errorf("响应应含 invalid_body，实际 %s", rec.Body.String())
	}
}

// TestSubmitHandler_MissingQuery 验证 query 为空返回 400。
func TestSubmitHandler_MissingQuery(t *testing.T) {
	j := agui.NewMemoryJournal()
	body, _ := json.Marshal(map[string]any{
		"buyer_id":   "b1",
		"session_id": "s1",
		"query":      "", // empty
	})
	req := httptest.NewRequest(http.MethodPost, "/run", nil)
	req.Body = io.NopCloser(strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("空 query 应返回 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "missing_query") {
		t.Errorf("响应应含 missing_query，实际 %s", rec.Body.String())
	}
}

// TestSubmitHandler_SubmitFails 验证 Submitter 报错时返回 500。
func TestSubmitHandler_SubmitFails(t *testing.T) {
	j := agui.NewMemoryJournal()
	body, _ := json.Marshal(map[string]any{
		"buyer_id":   "b1",
		"session_id": "s1",
		"query":      "test",
	})
	req := httptest.NewRequest(http.MethodPost, "/run", nil)
	req.Body = io.NopCloser(strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")

	failingSubmitter := &fakeSubmitter{respErr: errors.New("submit exploded")}
	rec := serveWithIdentity(makeDeps(j, failingSubmitter), req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("submit 失败应返回 500，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "submit_failed") {
		t.Errorf("响应应含 submit_failed，实际 %s", rec.Body.String())
	}
}

// TestSubmitHandler_模型未接入返回503 验证「能力未接入」与「调用失败」被分开。
//
// 这两者的处置完全相反：503 表示去开配置（重试无用），500 表示稍后重试。
// 曾经的实现把 submit 的任何失败都压成 500，等于让运维拿着一条 500 去排查一个
// 根本不存在的故障——正是这条用例守住的回归。
func TestSubmitHandler_模型未接入返回503(t *testing.T) {
	j := agui.NewMemoryJournal()
	body, _ := json.Marshal(map[string]any{
		"buyer_id":   "b1",
		"session_id": "s1",
		"query":      "登山包",
	})
	req := httptest.NewRequest(http.MethodPost, "/run", nil)
	req.Body = io.NopCloser(strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")

	// 用 %w 包装，确保分类靠的是 errors.Is 而不是字符串比对。
	failingSubmitter := &fakeSubmitter{
		respErr: fmt.Errorf("run 失败: %w", protocol.ErrModelUnavailable),
	}
	rec := serveWithIdentity(makeDeps(j, failingSubmitter), req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("模型未接入应返回 503，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Errorf("响应应含 model_unavailable，实际 %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "submit_failed") {
		t.Errorf("模型未接入不应落到 submit_failed，实际 %s", rec.Body.String())
	}
}

// TestMetaHandler_RunNotFound 验证 GET /runs/{id} 对未知 run 返回 404。
func TestMetaHandler_RunNotFound(t *testing.T) {
	j := agui.NewMemoryJournal()
	req := httptest.NewRequest(http.MethodGet, "/runs/nonexistent-run/events", nil)

	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec, req)

	// 先验证 metaHandler 本身（GET /runs/{runID} 无 events）
	req2 := httptest.NewRequest(http.MethodGet, "/runs/nonexistent-run", nil)
	rec2 := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusNotFound {
		t.Errorf("未知 run 应返回 404，实际 %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "run_not_found") {
		t.Errorf("响应应含 run_not_found，实际 %s", rec2.Body.String())
	}
}

// TestMetaHandler_JournalError 验证 metaHandler journal 报错时返回 500。
func TestMetaHandler_JournalError(t *testing.T) {
	j := &delegatingJournal{MemoryJournal: agui.NewMemoryJournal(), lastSeqErr: errors.New("journal exploded")}
	req := httptest.NewRequest(http.MethodGet, "/runs/somerun", nil)

	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("journal 报错应返回 500，实际 %d", rec.Code)
	}
}

// delegatingJournal seeds from an embedded MemoryJournal but can inject errors on specific methods.
type delegatingJournal struct {
	*agui.MemoryJournal
	sinceErr    error
	appendErr   error
	lastSeqErr  error
	listRunsErr error
}

func (d *delegatingJournal) Since(ctx context.Context, runID string, since int64, limit int) ([]runevent.Event, error) {
	if d.sinceErr != nil {
		return nil, d.sinceErr
	}
	return d.MemoryJournal.Since(ctx, runID, since, limit)
}

func (d *delegatingJournal) Append(ctx context.Context, ev runevent.Event) (int64, error) {
	if d.appendErr != nil {
		return 0, d.appendErr
	}
	return d.MemoryJournal.Append(ctx, ev)
}

func (d *delegatingJournal) LastSeq(ctx context.Context, runID string) (int64, error) {
	if d.lastSeqErr != nil {
		return -1, d.lastSeqErr
	}
	return d.MemoryJournal.LastSeq(ctx, runID)
}

func (d *delegatingJournal) ListRuns(ctx context.Context) ([]agui.RunMeta, error) {
	if d.listRunsErr != nil {
		return nil, d.listRunsErr
	}
	return d.MemoryJournal.ListRuns(ctx)
}

// TestStreamHandler_CursorInvalid 验证无效 cursor 返回 400。
func TestStreamHandler_CursorInvalid(t *testing.T) {
	j := agui.NewMemoryJournal()
	seedJournal(t, j, "run-cursor", 3)

	req := httptest.NewRequest(http.MethodGet, "/runs/run-cursor/events", nil)
	req.Header.Set("Last-Event-ID", "not-a-valid-cursor")

	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("无效 cursor 应返回 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cursor_invalid") {
		t.Errorf("响应应含 cursor_invalid，实际 %s", rec.Body.String())
	}
}

// TestStreamHandler_CrossRunCursor 验证跨 run cursor 返回 400。
func TestStreamHandler_CrossRunCursor(t *testing.T) {
	j := agui.NewMemoryJournal()
	seedJournal(t, j, "run-a", 3)
	seedJournal(t, j, "run-b", 3)

	req := httptest.NewRequest(http.MethodGet, "/runs/run-a/events", nil)
	req.Header.Set("Last-Event-ID", "run-b:1")

	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("跨 run cursor 应返回 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cursor_mismatch") {
		t.Errorf("响应应含 cursor_mismatch，实际 %s", rec.Body.String())
	}
}

// TestStreamHandler_JournalError 验证 journal.Since 报错时返回 500。
func TestStreamHandler_JournalError(t *testing.T) {
	j := &delegatingJournal{MemoryJournal: agui.NewMemoryJournal(), sinceErr: errors.New("journal io error")}
	seedJournal(t, j.MemoryJournal, "run-err", 3)

	req := httptest.NewRequest(http.MethodGet, "/runs/run-err/events", nil)

	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("journal 报错应返回 500，实际 %d", rec.Code)
	}
}

// TestStreamHandler_EmptyJournalWithCursor 验证 journal 为空但有 cursor 时返回 400。
func TestStreamHandler_EmptyJournalWithCursor(t *testing.T) {
	j := agui.NewMemoryJournal()
	runID := "run-empty"

	req := httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/events", nil)
	req.Header.Set("Last-Event-ID", runID+":0") // since=1, lastSeq=-1 → 1 > 0 缺口

	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空 journal + cursor 应拒绝，状态码=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "seq_gap") {
		t.Errorf("响应应含 seq_gap，实际 %s", rec.Body.String())
	}
}

// fakeCanceller 记录被取消的 run，并按预设结果应答。
type fakeCanceller struct {
	mu      sync.Mutex
	cancels []string
	result  bool
}

func (f *fakeCanceller) Cancel(runID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, runID)
	return f.result
}

// TestCancelEndpoint_Unsupported 验证未接入运行控制时返回 503 而不是假装成功。
//
// 用 200 应答一个没有真正中断推理的取消，会让调用方以为 token 已经省下，
// 而循环还在跑——这种「成功」比明确报错贵得多。
func TestCancelEndpoint_Unsupported(t *testing.T) {
	j := agui.NewMemoryJournal()
	deps := makeDeps(j, &fakeSubmitter{})
	// Canceller 故意留空

	req := httptest.NewRequest(http.MethodPost, "/runs/run-x/cancel", nil)
	rec := httptest.NewRecorder()
	agui.Routes(deps).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未接入运行控制应返回 503，实际 %d", rec.Code)
	}
}

// TestCancelEndpoint_NotActive 验证取消一个不在跑的 run 返回 404。
func TestCancelEndpoint_NotActive(t *testing.T) {
	j := agui.NewMemoryJournal()
	deps := makeDeps(j, &fakeSubmitter{})
	deps.Canceller = &fakeCanceller{result: false}

	req := httptest.NewRequest(http.MethodPost, "/runs/run-x/cancel", nil)
	rec := httptest.NewRecorder()
	agui.Routes(deps).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("取消不在跑的 run 应返回 404，实际 %d", rec.Code)
	}
}

// TestCancelEndpoint_CancelsRun 验证取消会真正交给 Canceller，且 runID 传对。
func TestCancelEndpoint_CancelsRun(t *testing.T) {
	j := agui.NewMemoryJournal()
	canceller := &fakeCanceller{result: true}
	deps := makeDeps(j, &fakeSubmitter{})
	deps.Canceller = canceller

	req := httptest.NewRequest(http.MethodPost, "/runs/run-42/cancel", nil)
	rec := httptest.NewRecorder()
	agui.Routes(deps).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("取消应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if len(canceller.cancels) != 1 || canceller.cancels[0] != "run-42" {
		t.Fatalf("Canceller 收到的 runID 不对：%v", canceller.cancels)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if body["cancelled"] != true || body["run_id"] != "run-42" {
		t.Fatalf("响应体不符合契约：%v", body)
	}
}

// TestSubmitHandler_RejectsWithoutIdentity 验证没有买家身份时 submit 被拒。
//
// 这条守的是「身份只能来自中间件」这个不变量：请求体里写满 buyer_id 也没用，
// 中间件没给出身份就是 401。
func TestSubmitHandler_RejectsWithoutIdentity(t *testing.T) {
	j := agui.NewMemoryJournal()
	sub := &fakeSubmitter{resp: submitEventsJSON("run-x", 1)}

	body, _ := json.Marshal(map[string]any{
		"buyer_id":   "attacker",
		"session_id": "stolen",
		"query":      "test",
	})
	req := httptest.NewRequest(http.MethodPost, "/run", nil)
	req.Body = io.NopCloser(strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")

	// 故意不套身份中间件：模拟一个漏挂鉴权的部署。
	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, sub)).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无身份应返回 401，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if sub.CallCount() != 0 {
		t.Errorf("无身份的请求不应触达 submitter，实际调用 %d 次", sub.CallCount())
	}
}

// TestSubmitHandler_SessionFromHeader 验证会话走 X-Session-ID 而非令牌。
func TestSubmitHandler_SessionFromHeader(t *testing.T) {
	j := agui.NewMemoryJournal()
	sub := &fakeSubmitter{resp: submitEventsJSON("run-x", 1)}

	body, _ := json.Marshal(map[string]any{"query": "test"})
	req := httptest.NewRequest(http.MethodPost, "/run", nil)
	req.Body = io.NopCloser(strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-ID", "sess-from-header")

	rec := serveWithIdentity(makeDeps(j, sub), req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码=%d body=%s", rec.Code, rec.Body.String())
	}
	got, ok := sub.LastCall()
	if !ok {
		t.Fatal("submitter 未被调用")
	}
	if got.SessionID != "sess-from-header" {
		t.Errorf("SessionID = %q，期望 sess-from-header", got.SessionID)
	}
}
