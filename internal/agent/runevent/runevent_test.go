package runevent_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
)

func TestSequencerStartsFromZero(t *testing.T) {
	s := runevent.NewSequencer("run-1")

	if s.Last() != -1 {
		t.Fatalf("初始 Last 应为 -1，实际 %d", s.Last())
	}
	if seq := s.Next(); seq != 0 {
		t.Fatalf("首次 Next 应返回 0，实际 %d", seq)
	}
	if seq := s.Next(); seq != 1 {
		t.Fatalf("二次 Next 应返回 1，实际 %d", seq)
	}
	if s.Last() != 1 {
		t.Fatalf("Last 应为 1，实际 %d", s.Last())
	}
}

func TestAttachProducesWellFormedEvent(t *testing.T) {
	s := runevent.NewSequencer("run-1")
	now := time.Unix(1735000000, 0).UTC()
	ev, err := s.Attach(s.Next(), runevent.KindRunStart, "main",
		json.RawMessage(`{"query":"hi"}`), now)
	if err != nil {
		t.Fatalf("Attach 失败：%v", err)
	}
	if ev.RunID != "run-1" {
		t.Errorf("RunID = %q", ev.RunID)
	}
	if ev.Seq != 0 {
		t.Errorf("Seq = %d", ev.Seq)
	}
	if ev.Kind != runevent.KindRunStart {
		t.Errorf("Kind = %q", ev.Kind)
	}
	if !strings.Contains(ev.EventID, "run-1:0:") {
		t.Errorf("EventID 形态不符合：%q", ev.EventID)
	}
	if !ev.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt 不一致：%v vs %v", ev.CreatedAt, now)
	}
}

func TestAttachRejectsInvalidJSON(t *testing.T) {
	s := runevent.NewSequencer("run-1")
	_, err := s.Attach(s.Next(), runevent.KindModelTurn, "main",
		[]byte("{not json"), time.Now())
	if err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

func TestAttachNilPayloadBecomesEmptyObject(t *testing.T) {
	s := runevent.NewSequencer("run-1")
	ev, err := s.Attach(s.Next(), runevent.KindHeartbeat, "", nil, time.Now())
	if err != nil {
		t.Fatalf("Attach 失败：%v", err)
	}
	if string(ev.Payload) != "{}" {
		t.Errorf("nil payload 应被替换为 {}，实际 %q", string(ev.Payload))
	}
}

func TestVerifyGapRejectsNonContiguous(t *testing.T) {
	if err := runevent.VerifyGap(3, 4); err != nil {
		t.Errorf("3→4 应通过，实际 %v", err)
	}
	err := runevent.VerifyGap(3, 5)
	if err == nil {
		t.Fatal("3→5 应报缺口")
	}
	if !errors.Is(err, runevent.ErrGap) {
		t.Errorf("错误应为 ErrGap，实际 %v", err)
	}
}
