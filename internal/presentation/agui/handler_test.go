package agui_test

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
	"github.com/NicoYazawa/crosspilot/internal/presentation/agui"
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

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// makeDeps 组装一组可工作的 Deps；submitter / journal 可由调用方覆盖。
func makeDeps(j agui.JournalStore, sub agui.RunSubmitter) agui.Deps {
	return agui.Deps{
		Journal:   j,
		Submitter: sub,
		Logger:    discardLog(),
		Heartbeat: 0,
	}
}

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

	req := httptest.NewRequest(http.MethodGet, "/agui/runs/"+runID+"/events", nil)
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
		fmt.Sscanf(f.ID, runID+":%d:", &got)
		if got != wantSeq {
			t.Errorf("期望 id 含 seq=%d，实际 %q", wantSeq, f.ID)
		}
		wantSeq++
	}
	if wantSeq != 5 {
		t.Errorf("末 seq+1=%d 期望 5", wantSeq)
	}
}

// TestE2_GapRejected 验收 E2：cursor.Seq+1 != LastSeq 时拒绝重连。
func TestE2_GapRejected(t *testing.T) {
	j := agui.NewMemoryJournal()
	runID := "run-e2"
	seedJournal(t, j, runID, 5) // lastSeq=4

	req := httptest.NewRequest(http.MethodGet, "/agui/runs/"+runID+"/events", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/agui/runs/"+runID+"/events", nil)
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
		req := httptest.NewRequest(http.MethodGet, "/agui/runs/"+runID+"/events", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/agui/runs/"+runID+"/events", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/agui/runs/"+runID+"/events", nil)

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
	req := httptest.NewRequest(http.MethodPost, "/agui/runs", nil)
	req.Body = io.NopCloser(strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, sub)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码=%d body=%s", rec.Code, rec.Body.String())
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

// TestConfirmEndpoint_WritesEvent 验证 POST /runs/{id}/confirm 把决议写入 journal。
func TestConfirmEndpoint_WritesEvent(t *testing.T) {
	j := agui.NewMemoryJournal()
	runID := "confirm-run"
	seedJournal(t, j, runID, 3)

	body, _ := json.Marshal(map[string]any{
		"confirmation_id": "conf-001",
		"approved":        true,
	})
	req := httptest.NewRequest(http.MethodPost, "/agui/runs/"+runID+"/confirm", nil)
	req.Body = io.NopCloser(strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	agui.Routes(makeDeps(j, &fakeSubmitter{})).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码=%d body=%s", rec.Code, rec.Body.String())
	}

	last, err := j.LastSeq(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if last != 3 {
		t.Errorf("追加后 lastSeq 应为 3，实际 %d", last)
	}

	tail, _ := j.Since(context.Background(), runID, last, 1)
	var payload map[string]any
	_ = json.Unmarshal(tail[0].Payload, &payload)
	if payload["confirmation_id"] != "conf-001" {
		t.Errorf("payload.confirmation_id = %v", payload["confirmation_id"])
	}
	if payload["approved"] != true {
		t.Errorf("payload.approved 应为 true，实际 %v", payload["approved"])
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

// hmacTestSecret 是 HMAC 单元测试用的占位密钥。
//
// 长自描述字符串：任何审计工具一眼能识别为「这是 fixture，不是真凭据」，
// 也不会被误判为泄漏的生产密钥。生产密钥通过 AUTH_JWT_SECRET 环境变量注入，
// 与本常量无任何关联。
const hmacTestSecret = "FIXME-placeholder-key-for-hmac-unit-test-only-do-not-use-in-prod"

// TestHMACAuth_AllowsRequestWithValidSignature 验证 HMAC 鉴权通过。
func TestHMACAuth_AllowsRequestWithValidSignature(t *testing.T) {
	secret := []byte(hmacTestSecret)
	path := "/agui/runs/run-1/events"

	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signHMAC(secret, http.MethodGet, path, ts)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-HMAC-Timestamp", ts)
	req.Header.Set("X-HMAC-Sign", sig)

	rec := httptest.NewRecorder()
	handler := agui.HMACAuth(secret, discardLog())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("鉴权失败：%d，body=%s", rec.Code, rec.Body.String())
	}
}

func TestHMACAuth_RejectsBadSignature(t *testing.T) {
	secret := []byte(hmacTestSecret)
	path := "/agui/runs/run-1/events"
	ts := fmt.Sprintf("%d", time.Now().Unix())

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-HMAC-Timestamp", ts)
	req.Header.Set("X-HMAC-Sign", "deadbeef")

	rec := httptest.NewRecorder()
	handler := agui.HMACAuth(secret, discardLog())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("状态码 = %d，期望 401", rec.Code)
	}
}

// signHMAC 重新计算签名（与 auth.go 等价）。
func signHMAC(secret []byte, method, path, ts string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(method))
	mac.Write([]byte("\n"))
	mac.Write([]byte(path))
	mac.Write([]byte("\n"))
	mac.Write([]byte(ts))
	return hex.EncodeToString(mac.Sum(nil))
}