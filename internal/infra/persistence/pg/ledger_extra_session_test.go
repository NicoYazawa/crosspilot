package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/domain/session"
)

// 本文件补齐会话存储的三条薄路径：大快照往返、无归属的历史会话、
// 以及「补归属」与「取执行权」的分界。
//
// 会话快照是全量覆盖写入，一次截断或一次误认领都会让会话再也读不回来，
// 而且这类损坏在小数据下几乎不会暴露。

// TestLedgerExtraSessionLargeStateRoundTrip 验证 100 KB 级快照逐字节往返。
//
// 小快照的往返几乎总是对的，真正会出问题的是大状态：截断、编码转换、
// 隐式长度上限都只在数据变大之后才暴露。这里比的是完整字符串而不是长度，
// 因为「长度相同但内容被替换」正是编码类缺陷的典型形态。
func TestLedgerExtraSessionLargeStateRoundTrip(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	const payloadBytes = 100 * 1024
	// 用拼接而不是 fmt.Sprintf(`... "%s" ...`)：这两侧的引号属于 JSON 自身，
	// 若顺着 gocritic 改用 %q，载荷里就会多出转义引号，测到的就不是一条普通的大消息了。
	state := `{"messages":["` + strings.Repeat("a", payloadBytes) + `"]}`
	if len(state) <= payloadBytes {
		t.Fatalf("测试数据未达到预期规模：%d 字节", len(state))
	}

	claim, err := env.store.Claim(ctx, "session-large", "buyer-1", true, true)
	if err != nil {
		t.Fatalf("取得执行权失败：%v", err)
	}

	saved, err := env.store.Save(ctx, claim, state)
	if err != nil {
		t.Fatalf("保存大快照失败：%v", err)
	}
	// 保存成功必须同时推进版本：否则同一张票据可以无限次覆盖最新状态
	if saved.Revision != claim.Revision+1 {
		t.Errorf("保存后版本 = %d，期望 %d", saved.Revision, claim.Revision+1)
	}

	loaded, found, err := env.store.Load(ctx, "session-large")
	if err != nil {
		t.Fatalf("读取大快照失败：%v", err)
	}
	if !found {
		t.Fatal("保存过的会话应当能被读到")
	}
	if loaded != state {
		t.Errorf("读回的快照与写入不一致：长度 %d / %d", len(loaded), len(state))
	}
}

// TestLedgerExtraSessionClaimWithoutClaimRow 覆盖「有快照、没有归属行」的历史会话。
//
// 直接写 agent_session_states 是构造这种状态的唯一方式：账本的 Save 一定会先
// 条件更新 session_write_claims，因此正常路径写不出「有快照无归属」。
//
// 此时存储层报的是 ErrOwnerUnbound（先迁移归属）而不是 ErrNotFound，
// 这一点是刻意的、也是本用例要钉住的：报「不存在」会让调用方以 create=true
// 重新认领，等于把一份来历不明的历史会话直接交到当前买家手里。
func TestLedgerExtraSessionClaimWithoutClaimRow(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	if _, err := env.pool.Exec(ctx, `
INSERT INTO agent_session_states (session_id, state_json, updated_at)
VALUES ('session-legacy', '{"legacy":true}', now())`); err != nil {
		t.Fatalf("写入历史快照失败：%v", err)
	}

	_, err := env.store.Claim(ctx, "session-legacy", "buyer-1", false, true)
	if !errors.Is(err, session.ErrOwnerUnbound) {
		t.Fatalf("期望 ErrOwnerUnbound，实际：%v", err)
	}
	if errors.Is(err, session.ErrNotFound) {
		t.Error("有历史快照的会话不能报告为「不存在」：那会让调用方以 create=true 重新认领它")
	}

	// 拒绝的是「取得执行权」，不是「读取」：快照本身仍然可读
	state, found, err := env.store.Load(ctx, "session-legacy")
	if err != nil || !found {
		t.Fatalf("读取历史快照失败：found=%v err=%v", found, err)
	}
	if state != `{"legacy":true}` {
		t.Errorf("快照 = %q，期望保持原内容", state)
	}

	// 对照组：快照与归属行都不存在时才是真正的「未找到」
	if _, err := env.store.Claim(ctx, "session-missing", "buyer-1", false, true); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("无快照无归属时期望 ErrNotFound，实际：%v", err)
	}

	// AssertOwner 走同一条归属解析：不允许新建且要求强制归属时同样拒绝
	if err := env.store.AssertOwner(ctx, "session-legacy", "buyer-1", true, true); !errors.Is(err, session.ErrOwnerUnbound) {
		t.Fatalf("强制归属校验期望 ErrOwnerUnbound，实际：%v", err)
	}

	// 显式放弃强制归属（enforceOwner=false）才允许补上归属：
	// 这是迁移工具走的路径，它知道自己在给哪份历史会话补 owner
	if err := env.store.AssertOwner(ctx, "session-legacy", "buyer-1", true, false); err != nil {
		t.Fatalf("补建归属失败：%v", err)
	}

	var (
		owner    string
		revision int64
		fence    int64
	)
	if err := env.pool.QueryRow(ctx,
		`SELECT owner_id, revision, fence FROM session_write_claims WHERE session_id = $1`,
		"session-legacy").Scan(&owner, &revision, &fence); err != nil {
		t.Fatalf("读取执行权行失败：%v", err)
	}
	if owner != "buyer-1" {
		t.Errorf("归属 = %q，期望 buyer-1", owner)
	}
	// 「补归属」不是「取得执行权」，因此两个计数器都必须保持 0
	if fence != 0 || revision != 0 {
		t.Errorf("归属行 = (revision %d, fence %d)，期望 (0, 0)", revision, fence)
	}
}

// TestLedgerExtraSessionAssertOwnerEstablishesZeroFenceClaim 覆盖补归属后的首次取权。
//
// AssertOwner 在会话全新时只声明「这个会话属于谁」，不把执行权发给任何人，
// 因此它写入的 fence 必须是 0。紧接着的第一次 Claim 拿到 1——
// 若 AssertOwner 顺手把 fence 推到 1，票据的单调性就失去了参照点，
// 「第一个执行者」与「第二个执行者」也就无从区分。
func TestLedgerExtraSessionAssertOwnerEstablishesZeroFenceClaim(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	if err := env.store.AssertOwner(ctx, "session-fresh", "buyer-1", true, true); err != nil {
		t.Fatalf("新建会话归属失败：%v", err)
	}

	var (
		owner    string
		revision int64
		fence    int64
	)
	if err := env.pool.QueryRow(ctx,
		`SELECT owner_id, revision, fence FROM session_write_claims WHERE session_id = $1`,
		"session-fresh").Scan(&owner, &revision, &fence); err != nil {
		t.Fatalf("读取执行权行失败：%v", err)
	}
	if owner != "buyer-1" {
		t.Errorf("归属 = %q，期望 buyer-1", owner)
	}
	if fence != 0 {
		t.Errorf("新建归属的 fence = %d，期望 0", fence)
	}
	if revision != 0 {
		t.Errorf("新建归属的 revision = %d，期望 0", revision)
	}

	claim, err := env.store.Claim(ctx, "session-fresh", "buyer-1", false, true)
	if err != nil {
		t.Fatalf("取得执行权失败：%v", err)
	}
	if claim.Fence != 1 {
		t.Errorf("首次取得执行权的 fence = %d，期望 1", claim.Fence)
	}
	if claim.Revision != 0 {
		t.Errorf("尚未保存过，revision = %d，期望 0", claim.Revision)
	}
	if claim.HasState {
		t.Error("全新会话不应带快照")
	}
	if claim.OwnerID != "buyer-1" {
		t.Errorf("票据归属 = %q，期望 buyer-1", claim.OwnerID)
	}
}
