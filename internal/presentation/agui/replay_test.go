package agui_test

import (
	"errors"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/presentation/agui"
)

func TestParseCursor_Valid(t *testing.T) {
	c, err := agui.ParseCursor("run-1:5")
	if err != nil {
		t.Fatalf("应解析成功：%v", err)
	}
	if c.RunID != "run-1" {
		t.Errorf("RunID = %q", c.RunID)
	}
	if c.Seq != 5 {
		t.Errorf("Seq = %d", c.Seq)
	}
}

func TestParseCursor_Errors(t *testing.T) {
	cases := []string{
		"",          // 空
		"run-1",     // 缺 seq
		"run-1:",    // 空 seq
		"run-1:abc", // 非数字 seq
		":5",        // 空 run_id
		"run-1:-3",  // 负 seq
	}
	for _, raw := range cases {
		_, err := agui.ParseCursor(raw)
		if err == nil {
			t.Errorf("%q 应报错", raw)
			continue
		}
		if !errors.Is(err, agui.ErrCursorInvalid) {
			t.Errorf("%q 应返回 ErrCursorInvalid，实际 %v", raw, err)
		}
	}
}

// TestBindToRun_RunIDMismatch 验收 E3：跨 run 游标拒绝。
func TestBindToRun_RunIDMismatch(t *testing.T) {
	c := agui.Cursor{RunID: "run-A", Seq: 5}
	_, err := agui.BindToRun(c, "run-B")
	if err == nil {
		t.Fatal("跨 run 应拒绝")
	}
	if !errors.Is(err, agui.ErrCursorMismatch) {
		t.Errorf("应返回 ErrCursorMismatch，实际 %v", err)
	}
}

func TestBindToRun_Match(t *testing.T) {
	c := agui.Cursor{RunID: "run-A", Seq: 5}
	since, err := agui.BindToRun(c, "run-A")
	if err != nil {
		t.Fatalf("匹配应成功：%v", err)
	}
	if since != 6 {
		t.Errorf("since 应为 6（cursor.Seq+1），实际 %d", since)
	}
}

func TestBindToRun_EmptyCursor(t *testing.T) {
	since, err := agui.BindToRun(agui.Cursor{}, "run-A")
	if err != nil {
		t.Fatalf("空 cursor 应返回 0：%v", err)
	}
	if since != 0 {
		t.Errorf("since 应为 0，实际 %d", since)
	}
}

// TestCheckGap_AcceptsFreshSub 验收 E2：cursor.Seq+1 == LastSeq 时通过。
func TestCheckGap_AcceptsFreshSub(t *testing.T) {
	if err := agui.CheckGap(5, 6); err != nil {
		t.Errorf("连续续传应通过：%v", err)
	}
	if err := agui.CheckGap(0, 0); err != nil {
		t.Errorf("首次订阅应通过：%v", err)
	}
}

// TestCheckGap_RejectsGap 验收 E2：cursor.Seq+1 != LastSeq 时报错。
func TestCheckGap_RejectsGap(t *testing.T) {
	err := agui.CheckGap(5, 7) // 跳过了 6
	if err == nil {
		t.Fatal("缺口应报错")
	}
	var ge *agui.GapError
	if !errors.As(err, &ge) {
		t.Errorf("应返回 *GapError，实际 %v", err)
	} else if ge.Expected != 6 || ge.Actual != 7 {
		t.Errorf("GapError 字段不对：%+v", ge)
	}
}

// TestCheckGap_StaleCursorResends 验证「过期 cursor（since < lastSeq+1）合法」：
// 客户端可能断电重启后失去内存中的最后序号；服务端只需从 since 起重发即可。
func TestCheckGap_StaleCursorResends(t *testing.T) {
	if err := agui.CheckGap(10, 5); err != nil {
		t.Errorf("过期 cursor 应被允许（重发），实际 %v", err)
	}
}
