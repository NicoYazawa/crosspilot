// Package session 是会话执行权与快照写入的领域模型。
//
// 会话状态是全量快照，读改写之间必然存在窗口。这个窗口靠两样东西封住：
//
//   - 执行权（fence）：每次取得执行权都会让上一个执行者作废，
//     两个执行者不会同时认为自己有权写；
//   - 版本（revision）：每次成功保存都会让版本加一，
//     拿着旧版本回来的延迟写入会被拒绝。
//
// 二者一起构成条件更新：session_id + owner_id + revision + fence 四元组缺一不可。
// 只靠执行权无法拦住「先取到执行权、写完释放、旧执行者才回来写」这一情形；
// 只靠版本则无法阻止两个执行者同时以同一版本写入（其中一个会被覆盖而不自知）。
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Claim 是一次执行权的授予。
//
// 它是「票据」而不是「锁」：持有票据不代表当前一定有权写，
// 写入时还要用票据上的四元组去数据库里换。
type Claim struct {
	SessionID string
	OwnerID   string
	Revision  int64
	Fence     int64
	// State 是取得执行权时读到的快照，格式由上层决定（本轮是 JSON 文本）。
	State string
	// HasState 区分「快照为空」与「快照为空字符串」。
	HasState bool
}

// 会话写入相关的错误。
var (
	// ErrStaleWrite 表示票据已过期：执行权或版本已经被别人推进。
	ErrStaleWrite = errors.New("session: 会话执行权或版本已更新，拒绝旧执行者覆盖最新状态")

	// ErrOwnerMismatch 表示调用者不是该会话的所有者。
	ErrOwnerMismatch = errors.New("session: 无权访问其他买家的会话")

	// ErrOwnerUnbound 表示历史会话没有可信归属，拒绝在未迁移的情况下写入。
	ErrOwnerUnbound = errors.New("session: 历史会话归属未绑定，请先迁移 owner")

	// ErrNotFound 表示会话不存在。
	ErrNotFound = errors.New("session: 会话不存在")

	// ErrCorruptState 表示待写入的快照不是合法的 JSON 对象。
	//
	// 宁可拒绝写入也不能覆盖：一份坏快照会让会话再也读不回来。
	ErrCorruptState = errors.New("session: 会话快照不是有效 JSON 对象，未覆盖现有状态")

	// ErrUnknownOwner 表示调用者没有给出所有者标识。
	ErrUnknownOwner = errors.New("session: 会话所有者标识为空")
)

// ValidateState 校验待写入的快照是合法的 JSON 对象。
func ValidateState(state string) error {
	var probe any
	if err := json.Unmarshal([]byte(state), &probe); err != nil {
		return fmt.Errorf("%w: %w", ErrCorruptState, err)
	}
	if _, ok := probe.(map[string]any); !ok {
		return fmt.Errorf("%w: 顶层不是对象", ErrCorruptState)
	}
	return nil
}

// ValidateIdentifier 校验会话与所有者标识：非空、无首尾空白、不超长。
func ValidateIdentifier(value, label string, maximum int) (string, error) {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maximum {
		return "", fmt.Errorf("session: %s 必须为不带首尾空格的非空标识，最长 %d 字符", label, maximum)
	}
	return value, nil
}

// Store 是会话快照的持久化端口。
//
// 实现必须保证 Claim 与 Save 各自是原子的条件更新：
//
//   - Claim 在同一会话上每次调用都让 fence 前进，因此上一个执行者的票据立即失效；
//   - Save 只在四元组完全匹配时写入，否则返回 ErrStaleWrite。
type Store interface {
	// Load 读取快照。不存在返回 ("", false, nil)。
	Load(ctx context.Context, sessionID string) (string, bool, error)

	// Claim 取得执行权并返回最新快照。
	//
	// create 为真时，会话不存在就新建；enforceOwner 为真时，会话已有归属
	// 且与 buyerID 不符则返回 ErrOwnerMismatch。
	Claim(ctx context.Context, sessionID, buyerID string, create, enforceOwner bool) (Claim, error)

	// Save 以条件更新写入快照，并在成功时返回版本已前进的票据。
	Save(ctx context.Context, claim Claim, state string) (Claim, error)

	// AssertOwner 校验会话归属，不改变执行权。
	AssertOwner(ctx context.Context, sessionID, buyerID string, create, enforceOwner bool) error
}
