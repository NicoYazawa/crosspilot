package observability_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	agentobs "github.com/NicoYazawa/crosspilot/internal/agent/observability"
	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
	appobs "github.com/NicoYazawa/crosspilot/internal/application/observability"
	preobs "github.com/NicoYazawa/crosspilot/internal/presentation/observability"
)

// fakeJournal 是 appobs.JournalStore 的本地替身。
type fakeJournal struct {
	mu      sync.Mutex
	events  map[string][]runevent.Event
	lastSeq map[string]int64
}

func newFakeJournal() *fakeJournal {
	return &fakeJournal{
		events:  map[string][]runevent.Event{},
		lastSeq: map[string]int64{},
	}
}

func (j *fakeJournal) Append(_ context.Context, ev runevent.Event) (int64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	last, seen := j.lastSeq[ev.RunID]
	if !seen && ev.Seq != 0 {
		return 0, errors.New("seq gap")
	}
	if seen && ev.Seq != last+1 {
		return last, errors.New("seq gap")
	}
	j.events[ev.RunID] = append(j.events[ev.RunID], ev)
	j.lastSeq[ev.RunID] = ev.Seq
	return ev.Seq, nil
}

func (j *fakeJournal) Since(_ context.Context, runID string, since int64, limit int) ([]runevent.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	src := j.events[runID]
	out := make([]runevent.Event, 0, len(src))
	for _, ev := range src {
		if ev.Seq >= since {
			out = append(out, ev)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (j *fakeJournal) LastSeq(_ context.Context, runID string) (int64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	last, ok := j.lastSeq[runID]
	if !ok {
		return -1, errors.New("unknown run")
	}
	return last, nil
}

// fakeCostStore 是 appobs.CostStore 的本地替身。
type fakeCostStore struct {
	mu     sync.Mutex
	events map[string][]appobs.CostEvent
}

func (c *fakeCostStore) CostOfRun(_ context.Context, runID string) ([]appobs.CostEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.events[runID], nil
}

// fakeExpStore 是 appobs.ExperimentStore 的本地替身。
type fakeExpStore struct{}

func (fakeExpStore) ArmFor(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

func (fakeExpStore) ArmsSummary(_ context.Context, _ string) ([]appobs.ArmSummary, error) {
	return []appobs.ArmSummary{
		{Arm: "A", Calls: 10, LatencyP95Ms: 1000},
	}, nil
}

// makeSeqEvent 构造一条事件。
func makeSeqEvent(runID string, seq int64, payload string) runevent.Event {
	seqr := runevent.NewSequencer(runID)
	for i := int64(0); i < seq; i++ {
		seqr.Next()
	}
	now := time.Unix(1700000000, 0).UTC()
	ev, err := seqr.Attach(seqr.Next(), runevent.KindModelTurn, "main", []byte(payload), now)
	if err != nil {
		panic(err)
	}
	return ev
}

// newRouter 装 handler 到 chi router 上。
func newRouter(j *fakeJournal, c *fakeCostStore) *chi.Mux {
	uc := appobs.New(j, c, fakeExpStore{}, nil)
	// expvar 名字是全局的；用原子计数器生成唯一后缀避免测试间冲突
	metrics := agentobs.NewMetrics("handler_test_" + nextMetricID())
	h := preobs.NewHandler(uc, metrics)
	inner := h.Routes()
	r := chi.NewRouter()
	r.Mount("/", inner)
	return r
}

var metricCounter atomic.Int64

// nextMetricID 返回单调递增的唯一数字，expvar 的 var 名字必须全局唯一。
func nextMetricID() string {
	return strconv.FormatInt(metricCounter.Add(1), 10)
}

func TestHandler_ReplayEvents_ReturnsAllEvents(t *testing.T) {
	t.Parallel()
	j := newFakeJournal()
	for i := int64(0); i < 3; i++ {
		_, _ = j.Append(context.Background(), makeSeqEvent("r1", i, `{"k":"v"}`))
	}
	r := newRouter(j, &fakeCostStore{events: map[string][]appobs.CostEvent{}})

	req := httptest.NewRequest(http.MethodGet, "/observability/runs/r1/events", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	var resp struct {
		RunID  string           `json:"run_id"`
		Events []runevent.Event `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if resp.RunID != "r1" {
		t.Errorf("run_id 应为 r1，实际 %q", resp.RunID)
	}
	if len(resp.Events) != 3 {
		t.Errorf("应有 3 条事件，实际 %d", len(resp.Events))
	}
}

func TestHandler_ReplayEvents_EmptyRunIDReturns400(t *testing.T) {
	t.Parallel()
	r := newRouter(newFakeJournal(), &fakeCostStore{events: map[string][]appobs.CostEvent{}})
	req := httptest.NewRequest(http.MethodGet, "/observability/runs//events", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound && w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400/404，实际 %d", w.Code)
	}
}

func TestHandler_RunCost_AggregatesProvider(t *testing.T) {
	t.Parallel()
	c := &fakeCostStore{events: map[string][]appobs.CostEvent{}}
	c.events["r1"] = []appobs.CostEvent{
		{EventID: "e1", RunID: "r1", Provider: "qwen", Model: "qwen3-max",
			TokensIn: 100, CostMinor: 1, Currency: "CNY"},
		{EventID: "e2", RunID: "r1", Provider: "qwen", Model: "qwen3-max",
			TokensIn: 200, Unpriced: true, Currency: "CNY"},
	}
	r := newRouter(newFakeJournal(), c)

	req := httptest.NewRequest(http.MethodGet, "/observability/runs/r1/cost", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var resp appobs.CostSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if resp.UnpricedCount != 1 {
		t.Errorf("F4 闸门：UnpricedCount 应为 1，实际 %d", resp.UnpricedCount)
	}
	if resp.TotalCalls != 2 {
		t.Errorf("TotalCalls 应为 2，实际 %d", resp.TotalCalls)
	}
}

func TestHandler_RunDiff_RequiresAgainstParam(t *testing.T) {
	t.Parallel()
	r := newRouter(newFakeJournal(), &fakeCostStore{events: map[string][]appobs.CostEvent{}})
	req := httptest.NewRequest(http.MethodGet, "/observability/runs/r1/diff", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", w.Code)
	}
}

func TestHandler_RunDiff_ReturnsComparison(t *testing.T) {
	t.Parallel()
	j := newFakeJournal()
	for _, seq := range []int64{0, 1, 2} {
		_, _ = j.Append(context.Background(), makeSeqEvent("base", seq, `{"k":"v"}`))
	}
	for _, seq := range []int64{0, 1, 2} {
		_, _ = j.Append(context.Background(), makeSeqEvent("agn", seq, `{"k":"DIFFERENT"}`))
	}
	r := newRouter(j, &fakeCostStore{events: map[string][]appobs.CostEvent{}})

	req := httptest.NewRequest(http.MethodGet, "/observability/runs/base/diff?against=agn", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	var resp appobs.DiffResult
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if resp.Changed != 3 {
		t.Errorf("三条 payload 都应报 changed，实际 %d", resp.Changed)
	}
}

func TestHandler_ExperimentArms_ReturnsArms(t *testing.T) {
	t.Parallel()
	r := newRouter(newFakeJournal(), &fakeCostStore{events: map[string][]appobs.CostEvent{}})
	req := httptest.NewRequest(http.MethodGet, "/observability/experiments/exp1/arms", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if resp["experiment"] != "exp1" {
		t.Errorf("experiment 应为 exp1，实际 %v", resp["experiment"])
	}
}

func TestHandler_ExperimentArms_EmptyKeyReturns400(t *testing.T) {
	t.Parallel()
	r := newRouter(newFakeJournal(), &fakeCostStore{events: map[string][]appobs.CostEvent{}})
	req := httptest.NewRequest(http.MethodGet, "/observability/experiments//arms", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound && w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400/404，实际 %d", w.Code)
	}
}

func TestHandler_Metrics_ReturnsSnapshot(t *testing.T) {
	t.Parallel()
	r := newRouter(newFakeJournal(), &fakeCostStore{events: map[string][]appobs.CostEvent{}})
	req := httptest.NewRequest(http.MethodGet, "/observability/metrics", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	var snap agentobs.MetricsSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// 默认 metrics（NewMetrics 后没 Emit 过）：全 0
	if snap.EmitTotal != 0 || snap.DroppedTotal != 0 {
		t.Errorf("新 metrics 应全 0，实际 %+v", snap)
	}
}
