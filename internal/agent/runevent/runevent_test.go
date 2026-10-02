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

func TestA2UIError_Error(t *testing.T) {
	err := &runevent.A2UIError{Field: "catalogId", Want: "globex.local/shopping-v2", Got: "wrong.catalog"}
	got := err.Error()
	want := "a2ui: 字段 catalogId 应为 globex.local/shopping-v2，实际 wrong.catalog"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestAsString(t *testing.T) {
	// nil value
	bad := map[string]any{
		"action":    nil,
		"catalogId": runevent.A2UICatalogID,
		"components": []any{
			map[string]any{"id": "root", "type": "Column", "path": runevent.ShoppingRequirementsPath},
		},
	}
	err := runevent.ValidateCreateSurface(bad)
	if err == nil {
		t.Fatal("nil action 应报错")
	}
	if !strings.Contains(err.Error(), "null") {
		t.Errorf("错误信息 = %q，期望含 'null'", err.Error())
	}

	// non-string value (int)
	bad2 := map[string]any{
		"action":    123,
		"catalogId": runevent.A2UICatalogID,
		"components": []any{
			map[string]any{"id": "root", "type": "Column", "path": runevent.ShoppingRequirementsPath},
		},
	}
	err2 := runevent.ValidateCreateSurface(bad2)
	if err2 == nil {
		t.Fatal("int action 应报错")
	}
	if !strings.Contains(err2.Error(), "non-string") {
		t.Errorf("错误信息 = %q，期望含 'non-string'", err2.Error())
	}
}

func TestItoa(t *testing.T) {
	// Trigger itoa via component index error path at index 0.
	bad := map[string]any{
		"action":    runevent.A2UIActionCreateSurface,
		"catalogId": runevent.A2UICatalogID,
		"components": []any{
			"not-an-object",
		},
	}
	err := runevent.ValidateCreateSurface(bad)
	if err == nil {
		t.Fatal("components[0] 非对象应报错")
	}
	if !strings.Contains(err.Error(), "components[0]") {
		t.Errorf("错误信息 = %q，期望含 'components[0]'", err.Error())
	}
}

func TestSequencer_RunID(t *testing.T) {
	s := runevent.NewSequencer("run-42")
	if got := s.RunID(); got != "run-42" {
		t.Errorf("RunID() = %q, want run-42", got)
	}
}

func TestSequencer_AttachWithID(t *testing.T) {
	s := runevent.NewSequencer("run-1")
	now := time.Unix(1735000000, 0).UTC()

	t.Run("空EventID错误", func(t *testing.T) {
		_, err := s.AttachWithID("", 0, runevent.KindRunStart, "main", nil, now)
		if err == nil {
			t.Fatal("空 EventID 应报错")
		}
		if !strings.Contains(err.Error(), "EventID 不能为空") {
			t.Errorf("错误信息 = %q，期望含 'EventID 不能为空'", err.Error())
		}
	})

	t.Run("Attach错误传播", func(t *testing.T) {
		_, err := s.AttachWithID("my-event-id", -1, runevent.KindRunStart, "main", nil, now)
		if err == nil {
			t.Fatal("seq < 0 应报错")
		}
		if !strings.Contains(err.Error(), "非法序号") {
			t.Errorf("错误信息 = %q，期望含 '非法序号'", err.Error())
		}
	})

	t.Run("成功", func(t *testing.T) {
		ev, err := s.AttachWithID("my-event-id", 0, runevent.KindRunStart, "main", []byte(`{}`), now)
		if err != nil {
			t.Fatalf("AttachWithID 失败: %v", err)
		}
		if ev.EventID != "my-event-id" {
			t.Errorf("EventID = %q, want my-event-id", ev.EventID)
		}
		if ev.RunID != "run-1" {
			t.Errorf("RunID = %q, want run-1", ev.RunID)
		}
		if ev.Seq != 0 {
			t.Errorf("Seq = %d, want 0", ev.Seq)
		}
	})
}

func TestSequencer_Attach_NegativeSeq(t *testing.T) {
	s := runevent.NewSequencer("run-1")
	_, err := s.Attach(-1, runevent.KindRunStart, "main", nil, time.Now())
	if err == nil {
		t.Fatal("seq < 0 应报错")
	}
	if !strings.Contains(err.Error(), "非法序号") {
		t.Errorf("错误信息 = %q，期望含 '非法序号'", err.Error())
	}
}

func TestSequencer_Attach_InvalidJSONPayload(t *testing.T) {
	s := runevent.NewSequencer("run-1")
	_, err := s.Attach(s.Next(), runevent.KindRunStart, "main", []byte("not-json"), time.Now())
	if err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	if !strings.Contains(err.Error(), "不是合法 JSON") {
		t.Errorf("错误信息 = %q，期望含 '不是合法 JSON'", err.Error())
	}
}

func TestValidateUpdateComponents_WrongAction(t *testing.T) {
	bad := map[string]any{
		"action":    "wrong_action",
		"catalogId": runevent.A2UICatalogID,
		"components": []any{
			map[string]any{"id": "c1", "type": "Column"},
		},
	}
	err := runevent.ValidateUpdateComponents(bad)
	if err == nil {
		t.Fatal("wrong action 应报错")
	}
	if !strings.Contains(err.Error(), "action") {
		t.Errorf("错误信息 = %q，期望含 'action'", err.Error())
	}
}

func TestValidateUpdateComponents_CatalogIdNotString(t *testing.T) {
	bad := map[string]any{
		"action":    runevent.A2UIActionUpdateComponents,
		"catalogId": 12345,
		"components": []any{
			map[string]any{"id": "c1", "type": "Column"},
		},
	}
	err := runevent.ValidateUpdateComponents(bad)
	if err == nil {
		t.Fatal("catalogId 非 string 应报错")
	}
	if !strings.Contains(err.Error(), "catalogId") {
		t.Errorf("错误信息 = %q，期望含 'catalogId'", err.Error())
	}
}

func TestValidateUpdateComponents_ComponentNotObject(t *testing.T) {
	bad := map[string]any{
		"action":    runevent.A2UIActionUpdateComponents,
		"catalogId": runevent.A2UICatalogID,
		"components": []any{
			"i am not an object",
		},
	}
	err := runevent.ValidateUpdateComponents(bad)
	if err == nil {
		t.Fatal("component 非对象应报错")
	}
	if !strings.Contains(err.Error(), "non-object") {
		t.Errorf("错误信息 = %q，期望含 'non-object'", err.Error())
	}
}

func TestValidateUpdateComponents_MissingComponents(t *testing.T) {
	bad := map[string]any{
		"action":    runevent.A2UIActionUpdateComponents,
		"catalogId": runevent.A2UICatalogID,
	}
	err := runevent.ValidateUpdateComponents(bad)
	if err == nil {
		t.Fatal("components 缺失应报错")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("错误信息 = %q，期望含 'missing'", err.Error())
	}
}

func TestValidateUpdateComponents_ComponentTypeEmpty(t *testing.T) {
	bad := map[string]any{
		"action":    runevent.A2UIActionUpdateComponents,
		"catalogId": runevent.A2UICatalogID,
		"components": []any{
			map[string]any{"id": "c1", "type": ""},
		},
	}
	err := runevent.ValidateUpdateComponents(bad)
	if err == nil {
		t.Fatal("type 为空应报错")
	}
	if !strings.Contains(err.Error(), "type") {
		t.Errorf("错误信息 = %q，期望含 'type'", err.Error())
	}
}

func TestValidateUpdateDataModel_ValueMissing(t *testing.T) {
	bad := map[string]any{
		"action":    runevent.A2UIActionUpdateDataModel,
		"catalogId": runevent.A2UICatalogID,
	}
	err := runevent.ValidateUpdateDataModel(bad)
	if err == nil {
		t.Fatal("value 缺失应报错")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("错误信息 = %q，期望含 'missing'", err.Error())
	}
}

func TestValidateUpdateDataModel_DataMissing(t *testing.T) {
	bad := map[string]any{
		"action":    runevent.A2UIActionUpdateDataModel,
		"catalogId": runevent.A2UICatalogID,
		"value":     map[string]any{"path": "/requirements"},
	}
	err := runevent.ValidateUpdateDataModel(bad)
	if err == nil {
		t.Fatal("data 缺失应报错")
	}
	if !strings.Contains(err.Error(), "data") {
		t.Errorf("错误信息 = %q，期望含 'data'", err.Error())
	}
}

func TestValidateUpdateDataModel_WrongAction(t *testing.T) {
	bad := map[string]any{
		"action":    "wrong_action",
		"catalogId": runevent.A2UICatalogID,
		"value": map[string]any{
			"path": "/requirements",
			"data": []any{},
		},
	}
	err := runevent.ValidateUpdateDataModel(bad)
	if err == nil {
		t.Fatal("wrong action 应报错")
	}
	if !strings.Contains(err.Error(), "action") {
		t.Errorf("错误信息 = %q，期望含 'action'", err.Error())
	}
}

func TestValidateUpdateDataModel_CatalogIdNotString(t *testing.T) {
	bad := map[string]any{
		"action":    runevent.A2UIActionUpdateDataModel,
		"catalogId": 12345,
		"value": map[string]any{
			"path": "/requirements",
			"data": []any{},
		},
	}
	err := runevent.ValidateUpdateDataModel(bad)
	if err == nil {
		t.Fatal("catalogId 非 string 应报错")
	}
	if !strings.Contains(err.Error(), "catalogId") {
		t.Errorf("错误信息 = %q，期望含 'catalogId'", err.Error())
	}
}

func TestValidateUpdateDataModel_ValueNotMap(t *testing.T) {
	bad := map[string]any{
		"action":    runevent.A2UIActionUpdateDataModel,
		"catalogId": runevent.A2UICatalogID,
		"value":     "not a map",
	}
	err := runevent.ValidateUpdateDataModel(bad)
	if err == nil {
		t.Fatal("value 非 map 应报错")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("错误信息 = %q，期望含 'missing'", err.Error())
	}
}

func TestValidateUpdateDataModel_PathNotString(t *testing.T) {
	bad := map[string]any{
		"action":    runevent.A2UIActionUpdateDataModel,
		"catalogId": runevent.A2UICatalogID,
		"value": map[string]any{
			"path": 123,
			"data": []any{},
		},
	}
	err := runevent.ValidateUpdateDataModel(bad)
	if err == nil {
		t.Fatal("path 非 string 应报错")
	}
	if !strings.Contains(err.Error(), "non-empty string") {
		t.Errorf("错误信息 = %q，期望含 'non-empty string'", err.Error())
	}
}

func TestValidateUpdateDataModel_DataMissingOnly(t *testing.T) {
	bad := map[string]any{
		"action":    runevent.A2UIActionUpdateDataModel,
		"catalogId": runevent.A2UICatalogID,
		"value": map[string]any{
			"path": "/requirements",
		},
	}
	err := runevent.ValidateUpdateDataModel(bad)
	if err == nil {
		t.Fatal("data 缺失应报错")
	}
	if !strings.Contains(err.Error(), "data") {
		t.Errorf("错误信息 = %q，期望含 'data'", err.Error())
	}
}

func TestValidateCreateSurface_ComponentTypeEmpty(t *testing.T) {
	bad := map[string]any{
		"action":    runevent.A2UIActionCreateSurface,
		"catalogId": runevent.A2UICatalogID,
		"components": []any{
			map[string]any{"id": "root", "type": "", "path": runevent.ShoppingRequirementsPath},
		},
	}
	err := runevent.ValidateCreateSurface(bad)
	if err == nil {
		t.Fatal("type 为空应报错")
	}
	if !strings.Contains(err.Error(), "type") {
		t.Errorf("错误信息 = %q，期望含 'type'", err.Error())
	}
}

func TestValidateUpdateDataModel_VersionMismatch(t *testing.T) {
	bad := map[string]any{
		"action":    runevent.A2UIActionCreateSurface,
		"catalogId": runevent.A2UICatalogID,
		"version":   "99.9",
		"components": []any{
			map[string]any{"id": "root", "type": "Column", "path": runevent.ShoppingRequirementsPath},
		},
	}
	err := runevent.ValidateCreateSurface(bad)
	if err == nil {
		t.Fatal("version 不匹配应报错")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("错误信息 = %q，期望含 'version'", err.Error())
	}
}
