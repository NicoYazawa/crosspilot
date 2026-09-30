package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/NicoYazawa/crosspilot/internal/domain/session"
)

// 编译期确认本类型实现了领域端口。
var _ session.Store = (*Store)(nil)

// sessionIdentifierMax 是会话与所有者标识的长度上限。
const sessionIdentifierMax = 256

// Load 读取会话快照。不存在返回 ("", false, nil)。
func (s *Store) Load(ctx context.Context, sessionID string) (string, bool, error) {
	if _, err := session.ValidateIdentifier(sessionID, "session_id", sessionIdentifierMax); err != nil {
		return "", false, err
	}

	var state string
	err := s.pool.QueryRow(ctx,
		`SELECT state_json FROM agent_session_states WHERE session_id = $1`, sessionID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("pg: 读取会话快照失败: %w", err)
	}
	return state, true, nil
}

// Claim 取得执行权并返回最新快照。
//
// 每次调用都把 fence 加一，这是「执行权」的核心：上一个执行者手里的票据
// 立即失效，即使它还没意识到自己已经被取代。
//
// 这里刻意不做「先 SELECT 判断存在、再 INSERT」：两个并发 claim 会同时
// 判定「不存在」然后抢着插入。改为直接 UPDATE，rowcount 为 0 时才插入，
// 并让唯一约束冲突回到重试路径——把判断交给数据库，而不是应用层的时序假设。
func (s *Store) Claim(ctx context.Context, sessionID, buyerID string, create, enforceOwner bool) (session.Claim, error) {
	if _, err := session.ValidateIdentifier(sessionID, "session_id", sessionIdentifierMax); err != nil {
		return session.Claim{}, err
	}
	if _, err := session.ValidateIdentifier(buyerID, "buyer_id", sessionIdentifierMax); err != nil {
		return session.Claim{}, err
	}

	var claim session.Claim
	err := s.retry(ctx, "claim_session", func(ctx context.Context) error {
		return s.inTx(ctx, "claim_session", func(tx pgx.Tx) error {
			owner, err := s.resolveOwner(ctx, tx, sessionID, buyerID, create, enforceOwner)
			if err != nil {
				return err
			}

			var (
				revision int64
				fence    int64
			)
			err = tx.QueryRow(ctx, `
UPDATE session_write_claims
SET fence = fence + 1
WHERE session_id = $1
RETURNING revision, fence`, sessionID).Scan(&revision, &fence)
			switch {
			case err == nil:
			case errors.Is(err, pgx.ErrNoRows):
				if !create {
					return session.ErrNotFound
				}
				if err := tx.QueryRow(ctx, `
INSERT INTO session_write_claims (session_id, owner_id, revision, fence)
VALUES ($1, $2, 0, 1)
RETURNING revision, fence`, sessionID, owner).Scan(&revision, &fence); err != nil {
					if isUniqueViolation(err) {
						// 另一个 claim 刚刚建好这一行，重放整轮即可走到 UPDATE 分支
						return &retryableUniqueError{err: err}
					}
					return fmt.Errorf("pg: 建立会话执行权失败: %w", err)
				}
			default:
				return fmt.Errorf("pg: 推进会话执行权失败: %w", err)
			}

			state, hasState, err := loadStateTx(ctx, tx, sessionID)
			if err != nil {
				return err
			}
			claim = session.Claim{
				SessionID: sessionID,
				OwnerID:   owner,
				Revision:  revision,
				Fence:     fence,
				State:     state,
				HasState:  hasState,
			}
			return nil
		})
	})
	if err != nil {
		return session.Claim{}, err
	}
	return claim, nil
}

// resolveOwner 决定这次 claim 归属谁，并返回最终的 owner。
func (s *Store) resolveOwner(
	ctx context.Context,
	tx pgx.Tx,
	sessionID, buyerID string,
	create, enforceOwner bool,
) (string, error) {
	var owner *string
	err := tx.QueryRow(ctx,
		`SELECT owner_id FROM session_write_claims WHERE session_id = $1 FOR UPDATE`, sessionID).Scan(&owner)
	switch {
	case err == nil:
		if owner == nil {
			// 已有执行权但归属未绑定：只有确实没有历史快照时才允许认领，
			// 否则就是把一份来历不明的会话交给当前买家。
			hasState, stateErr := stateExists(ctx, tx, sessionID)
			if stateErr != nil {
				return "", stateErr
			}
			if hasState && enforceOwner {
				return "", session.ErrOwnerUnbound
			}
			if _, err := tx.Exec(ctx,
				`UPDATE session_write_claims SET owner_id = $2 WHERE session_id = $1`, sessionID, buyerID); err != nil {
				return "", fmt.Errorf("pg: 绑定会话归属失败: %w", err)
			}
			return buyerID, nil
		}
		if enforceOwner && *owner != buyerID {
			return "", session.ErrOwnerMismatch
		}
		return *owner, nil

	case errors.Is(err, pgx.ErrNoRows):
		hasState, stateErr := stateExists(ctx, tx, sessionID)
		if stateErr != nil {
			return "", stateErr
		}
		if !create {
			if hasState {
				return "", session.ErrOwnerUnbound
			}
			return "", session.ErrNotFound
		}
		if hasState && enforceOwner {
			return "", session.ErrOwnerUnbound
		}
		return buyerID, nil

	default:
		return "", fmt.Errorf("pg: 读取会话归属失败: %w", err)
	}
}

func stateExists(ctx context.Context, tx pgx.Tx, sessionID string) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM agent_session_states WHERE session_id = $1)`, sessionID).Scan(&exists); err != nil {
		return false, fmt.Errorf("pg: 探测会话快照失败: %w", err)
	}
	return exists, nil
}

func loadStateTx(ctx context.Context, tx pgx.Tx, sessionID string) (string, bool, error) {
	var state string
	err := tx.QueryRow(ctx,
		`SELECT state_json FROM agent_session_states WHERE session_id = $1`, sessionID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("pg: 读取会话快照失败: %w", err)
	}
	return state, true, nil
}

// Save 以条件更新写入快照，并在成功时返回版本已前进的票据。
//
// 四元组必须全部匹配：只有 session_id 与 owner_id 是所有权判断，
// revision 与 fence 是「你说的还是不是最新的那一份」判断。
// 少了任何一个，延迟写回都能悄悄覆盖掉更新的状态。
func (s *Store) Save(ctx context.Context, claim session.Claim, state string) (session.Claim, error) {
	if _, err := session.ValidateIdentifier(claim.SessionID, "session_id", sessionIdentifierMax); err != nil {
		return session.Claim{}, err
	}
	if err := session.ValidateState(state); err != nil {
		return session.Claim{}, err
	}

	var updated session.Claim
	err := s.retry(ctx, "save_session", func(ctx context.Context) error {
		return s.inTx(ctx, "save_session", func(tx pgx.Tx) error {
			var revision int64
			err := tx.QueryRow(ctx, `
UPDATE session_write_claims
SET revision = revision + 1
WHERE session_id = $1 AND owner_id = $2 AND revision = $3 AND fence = $4
RETURNING revision`, claim.SessionID, claim.OwnerID, claim.Revision, claim.Fence).Scan(&revision)
			if errors.Is(err, pgx.ErrNoRows) {
				return session.ErrStaleWrite
			}
			if err != nil {
				return fmt.Errorf("pg: 保存会话快照失败: %w", err)
			}

			if _, err := tx.Exec(ctx, `
INSERT INTO agent_session_states (session_id, state_json, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (session_id) DO UPDATE SET state_json = EXCLUDED.state_json, updated_at = now()`,
				claim.SessionID, state); err != nil {
				return fmt.Errorf("pg: 写入会话快照失败: %w", err)
			}

			updated = session.Claim{
				SessionID: claim.SessionID,
				OwnerID:   claim.OwnerID,
				Revision:  revision,
				Fence:     claim.Fence,
				State:     state,
				HasState:  true,
			}
			return nil
		})
	})
	if err != nil {
		return session.Claim{}, err
	}
	return updated, nil
}

// AssertOwner 校验会话归属，不改变执行权。
//
// 与 Claim 的区别是它不推进 fence：读取会话（例如列表、概览）不应该
// 把正在执行的写者作废。
func (s *Store) AssertOwner(ctx context.Context, sessionID, buyerID string, create, enforceOwner bool) error {
	if _, err := session.ValidateIdentifier(sessionID, "session_id", sessionIdentifierMax); err != nil {
		return err
	}
	if _, err := session.ValidateIdentifier(buyerID, "buyer_id", sessionIdentifierMax); err != nil {
		return err
	}

	return s.retry(ctx, "assert_session_owner", func(ctx context.Context) error {
		return s.inTx(ctx, "assert_session_owner", func(tx pgx.Tx) error {
			owner, err := s.resolveOwner(ctx, tx, sessionID, buyerID, create, enforceOwner)
			if err != nil {
				return err
			}
			if enforceOwner && owner != buyerID {
				return session.ErrOwnerMismatch
			}
			if !create {
				return nil
			}

			if _, err := tx.Exec(ctx, `
INSERT INTO session_write_claims (session_id, owner_id, revision, fence)
VALUES ($1, $2, 0, 0)
ON CONFLICT (session_id) DO NOTHING`, sessionID, owner); err != nil {
				return fmt.Errorf("pg: 建立会话归属失败: %w", err)
			}
			return nil
		})
	})
}
