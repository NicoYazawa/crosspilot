package lease

import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// 编译期确认 Redis 客户端实现了租约后端。
var _ Backend = (*RedisBackend)(nil)

// RedisBackend 用 Redis 实现租约存储。
//
// 三个操作都必须是原子的，因此都走 Lua 脚本而不是「读一次再写一次」：
// 后者在两个执行者同时操作时会双双认为自己成功，互斥就没了。
type RedisBackend struct {
	client goredis.UniversalClient
}

// NewRedisBackend 用给定的 Redis 客户端构造后端。
func NewRedisBackend(client goredis.UniversalClient) (*RedisBackend, error) {
	if client == nil {
		return nil, errors.New("lease: 缺少 Redis 客户端")
	}
	return &RedisBackend{client: client}, nil
}

// acquireScript 只在键不存在时建立租约，返回 1/0。
//
// SET NX PX 本身就是原子的，不必用脚本；这里保留脚本形式是为了让
// 三个操作的语义写在同一处——续约与释放必须比对持有者，只有它们需要脚本。
const acquireScript = `
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2]) then
    return 1
end
return 0
`

// renewScript 只在键的值仍是自己时重置过期时间，返回 1/0。
//
// 判断与延长必须在同一个脚本里：分两步做的话，中间可能被别人抢走键，
// 于是「我续约成功」与「键其实是别人的」会同时成立。
const renewScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
    return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0
`

// releaseScript 只在键的值仍是自己时删除它，返回 1/0。
//
// 这个比对不能省：租约过期后被别人接管，此时释放若直接 DEL，
// 删掉的是新持有者的键，互斥会彻底失效。
const releaseScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
    return redis.call('DEL', KEYS[1])
end
return 0
`

// Acquire 实现 Backend。
func (b *RedisBackend) Acquire(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	result, err := b.client.Eval(ctx, acquireScript, []string{key}, owner, ttl.Milliseconds()).Int64()
	if err != nil {
		return false, fmt.Errorf("lease: Redis 抢占失败: %w", err)
	}
	return result == 1, nil
}

// Renew 实现 Backend。
func (b *RedisBackend) Renew(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	result, err := b.client.Eval(ctx, renewScript, []string{key}, owner, ttl.Milliseconds()).Int64()
	if err != nil {
		return false, fmt.Errorf("lease: Redis 续约失败: %w", err)
	}
	return result == 1, nil
}

// Release 实现 Backend。
func (b *RedisBackend) Release(ctx context.Context, key, owner string) (bool, error) {
	result, err := b.client.Eval(ctx, releaseScript, []string{key}, owner).Int64()
	if err != nil {
		return false, fmt.Errorf("lease: Redis 释放失败: %w", err)
	}
	return result == 1, nil
}

// Get 实现 Backend。
func (b *RedisBackend) Get(ctx context.Context, key string) (string, bool, error) {
	value, err := b.client.Get(ctx, key).Result()
	if errors.Is(err, goredis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lease: Redis 读取租约失败: %w", err)
	}
	return value, true, nil
}
