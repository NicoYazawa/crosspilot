package pg_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// TestDigestSurvivesDatabaseRoundTrip 钉住一条必要性质：
// 确认单从数据库读回后算出的快照摘要，必须与写入前算出的摘要相同。
//
// 摘要不一致意味着决议永远无法通过快照校验，交易再也完不成。
func TestDigestSurvivesDatabaseRoundTrip(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)

	confirmation := env.prepare(t, createRequest("operation-1", defaultItem(2)))

	stored, err := env.store.Confirmation(context.Background(),
		confirmation.ConfirmationID, "buyer-1", "session-1")
	if err != nil {
		t.Fatalf("读回确认单失败：%v", err)
	}

	before, err := confirmation.ComputeSnapshotHash()
	if err != nil {
		t.Fatalf("计算内存摘要失败：%v", err)
	}
	after, err := stored.ComputeSnapshotHash()
	if err != nil {
		t.Fatalf("计算读回摘要失败：%v", err)
	}

	left, _ := json.Marshal(confirmation.Payload)
	right, _ := json.Marshal(stored.Payload)
	t.Logf("内存载荷：%s", left)
	t.Logf("读回载荷：%s", right)
	t.Logf("内存有效期：%s（%s）", confirmation.ExpiresAt.Format(time.RFC3339Nano), confirmation.ExpiresAt.UTC())
	t.Logf("读回有效期：%s", stored.ExpiresAt.UTC())
	t.Logf("内存摘要：%s", before)
	t.Logf("读回摘要：%s", after)

	if before != after {
		t.Errorf("摘要不一致：写入前 %s / 读回后 %s", before, after)
	}
	if stored.SnapshotHash != confirmation.SnapshotHash {
		t.Errorf("存储摘要 = %s，期望 %s", stored.SnapshotHash, confirmation.SnapshotHash)
	}
}
