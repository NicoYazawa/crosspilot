package lease

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// 本文件覆盖 RedisBackend 的全部本地逻辑。
//
// 真实的 Lua 语义（SET NX、比对持有者再续期/删除）只能用真 Redis 验证，
// 但「脚本返回 1/0 如何翻译成 bool」「底层错误如何包装」「键不存在时
// Get 返回什么」这些都不需要服务器：前者用替身客户端精确摆出每种返回值，
// 后者用一个必然连不上的地址驱动真实客户端，确认失败被如实上报而不是 panic。

// stubRedisClient 是只为 Eval / Get 提供答案的替身客户端。
//
// RedisBackend 只用到这两个方法，其余方法由内嵌接口补足（永不被调用），
// 于是无需真实 Redis 就能把 redis.go 的分支走全：脚本返回 1、返回 0、
// 底层报错、GET 命中、GET 未命中（redis.Nil）、GET 报错。
type stubRedisClient struct {
	goredis.UniversalClient // 仅用于补足方法集，任何调用都会 panic，因此不能被执行

	eval func(ctx context.Context, script string, keys []string, args ...any) (int64, error)
	get  func(ctx context.Context, key string) (string, error)
}

// 编译期确认替身满足后端所需的客户端接口
var _ goredis.UniversalClient = (*stubRedisClient)(nil)

func (c *stubRedisClient) Eval(ctx context.Context, script string, keys []string, args ...any) *goredis.Cmd {
	if c.eval == nil {
		return goredis.NewCmdResult(int64(0), nil)
	}
	value, err := c.eval(ctx, script, keys, args...)
	return goredis.NewCmdResult(value, err)
}

func (c *stubRedisClient) Get(ctx context.Context, key string) *goredis.StringCmd {
	if c.get == nil {
		return goredis.NewStringResult("", goredis.Nil)
	}
	value, err := c.get(ctx, key)
	return goredis.NewStringResult(value, err)
}

// evalCall 是一次 Eval 调用的入参快照。
type evalCall struct {
	script string
	keys   []string
	args   []any
}

// mustRedisBackend 用替身客户端构造后端。
func mustRedisBackend(t *testing.T, client goredis.UniversalClient) *RedisBackend {
	t.Helper()

	backend, err := NewRedisBackend(client)
	if err != nil {
		t.Fatalf("构造 RedisBackend 失败：%v", err)
	}
	return backend
}

// closedAddr 返回一个刚刚关闭的本地端口，连过去必定被拒绝，且不需要等超时。
func closedAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占用端口失败：%v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("释放端口失败：%v", err)
	}
	return addr
}

// TestNewRedisBackendRejectsNilClient 守住空客户端检查。
//
// 装配期给不出客户端时必须立刻报错：否则要等到第一次抢占才 panic，
// 而那时已经在一个正在处理的会话里了。
func TestNewRedisBackendRejectsNilClient(t *testing.T) {
	backend, err := NewRedisBackend(nil)
	if err == nil {
		t.Fatal("客户端为 nil 时构造必须报错")
	}
	if backend != nil {
		t.Errorf("构造失败时不应返回后端，得到 %v", backend)
	}
	if !strings.Contains(err.Error(), "缺少 Redis 客户端") {
		t.Errorf("错误信息应说明缺的是 Redis 客户端：%q", err.Error())
	}
}

// TestRedisBackendMapsScriptResults 守住脚本返回值到 bool 的翻译。
//
// 三个操作都靠 Lua 返回 1/0 表示「成了/没成」。翻译写反的后果是
// 抢不到却以为抢到了（互斥形同虚设），或抢到了却以为没抢到（谁都无法开工）。
// 同时断言下发的是哪段脚本、键与参数是什么：脚本串错位会让语义悄悄变化，
// 而 TTL 的单位必须是毫秒（写成秒会让租期差三个数量级）。
func TestRedisBackendMapsScriptResults(t *testing.T) {
	ops := []struct {
		name       string
		script     string
		errorLabel string
		args       []any
		invoke     func(backend *RedisBackend) (bool, error)
	}{
		{
			name:       "抢占",
			script:     acquireScript,
			errorLabel: "Redis 抢占失败",
			args:       []any{"owner-1", int64(1500)},
			invoke: func(backend *RedisBackend) (bool, error) {
				return backend.Acquire(context.Background(), "lease:key", "owner-1", 1500*time.Millisecond)
			},
		},
		{
			name:       "续约",
			script:     renewScript,
			errorLabel: "Redis 续约失败",
			args:       []any{"owner-1", int64(1500)},
			invoke: func(backend *RedisBackend) (bool, error) {
				return backend.Renew(context.Background(), "lease:key", "owner-1", 1500*time.Millisecond)
			},
		},
		{
			name:       "释放",
			script:     releaseScript,
			errorLabel: "Redis 释放失败",
			args:       []any{"owner-1"},
			invoke: func(backend *RedisBackend) (bool, error) {
				return backend.Release(context.Background(), "lease:key", "owner-1")
			},
		},
	}

	for _, op := range ops {
		t.Run(op.name+"成功", func(t *testing.T) {
			var got evalCall
			client := &stubRedisClient{eval: func(_ context.Context, script string, keys []string, args ...any) (int64, error) {
				got = evalCall{script: script, keys: keys, args: args}
				return 1, nil
			}}
			ok, err := op.invoke(mustRedisBackend(t, client))
			if err != nil {
				t.Fatalf("脚本返回 1 时不应报错：%v", err)
			}
			if !ok {
				t.Error("脚本返回 1 应翻译成 true")
			}
			if got.script != op.script {
				t.Errorf("下发的脚本不是 %s 对应的那段，脚本错位会让语义悄悄变化", op.name)
			}
			if len(got.keys) != 1 || got.keys[0] != "lease:key" {
				t.Errorf("KEYS = %v，期望 [lease:key]", got.keys)
			}
			if len(got.args) != len(op.args) {
				t.Fatalf("ARGV = %v，期望 %v", got.args, op.args)
			}
			for i := range op.args {
				if got.args[i] != op.args[i] {
					t.Errorf("ARGV[%d] = %v(%T)，期望 %v(%T)", i, got.args[i], got.args[i], op.args[i], op.args[i])
				}
			}
		})

		t.Run(op.name+"未命中", func(t *testing.T) {
			client := &stubRedisClient{eval: func(context.Context, string, []string, ...any) (int64, error) {
				return 0, nil
			}}
			ok, err := op.invoke(mustRedisBackend(t, client))
			if err != nil {
				t.Fatalf("脚本返回 0 不该是错误：%v", err)
			}
			if ok {
				t.Error("脚本返回 0 应翻译成 false（键不属于自己或没抢到）")
			}
		})

		t.Run(op.name+"底层报错", func(t *testing.T) {
			client := &stubRedisClient{eval: func(context.Context, string, []string, ...any) (int64, error) {
				return 0, errBackendDown
			}}
			ok, err := op.invoke(mustRedisBackend(t, client))
			if ok {
				t.Error("底层报错时不能报告成功")
			}
			if !errors.Is(err, errBackendDown) {
				t.Fatalf("底层错误应被包装保留，得到 %v", err)
			}
			if !strings.Contains(err.Error(), op.errorLabel) {
				t.Errorf("错误信息应说明是%s：%q", op.name, err.Error())
			}
		})
	}
}

// TestRedisBackendGet 守住键读取的三种结果。
//
// 「键不存在」必须是 (\"\", false, nil) 而不是错误：租约过期是正常状态，
// 把它当故障会让调用方在每次空闲时都记一条错误日志。
func TestRedisBackendGet(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		err     error
		want    string
		wantOK  bool
		wantErr bool
	}{
		{name: "命中", value: "owner-1", err: nil, want: "owner-1", wantOK: true},
		{name: "键不存在", value: "", err: goredis.Nil, want: "", wantOK: false},
		{name: "底层报错", value: "", err: errBackendDown, want: "", wantOK: false, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotKey string
			client := &stubRedisClient{get: func(_ context.Context, key string) (string, error) {
				gotKey = key
				return tc.value, tc.err
			}}
			value, ok, err := mustRedisBackend(t, client).Get(context.Background(), "lease:key")

			if gotKey != "lease:key" {
				t.Errorf("查询的键 = %q，期望 lease:key", gotKey)
			}
			if ok != tc.wantOK {
				t.Errorf("存在标记 = %v，期望 %v", ok, tc.wantOK)
			}
			if value != tc.want {
				t.Errorf("值 = %q，期望 %q", value, tc.want)
			}
			if tc.wantErr {
				if !errors.Is(err, errBackendDown) {
					t.Fatalf("底层错误应被包装保留，得到 %v", err)
				}
				if !strings.Contains(err.Error(), "Redis 读取租约失败") {
					t.Errorf("错误信息应说明是读取失败：%q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错：%v", err)
			}
		})
	}
}

// TestRedisBackendSurfacesConnectionErrors 用真实客户端确认故障被如实上报。
//
// 指向一个刚关闭的端口（连接必定被拒绝），断言四个方法都返回错误而不是
// panic，也不会一直挂着。MaxRetries 设为 -1、DialerRetries 设为 1，
// 关掉 go-redis 自带的重试：这条用例验证的是「失败会被上报」，
// 而不是重试策略——不关掉就要为每次拨号等满退避，纯属浪费测试时间。
func TestRedisBackendSurfacesConnectionErrors(t *testing.T) {
	client := goredis.NewClient(&goredis.Options{
		Addr:          closedAddr(t),
		DialTimeout:   30 * time.Millisecond,
		ReadTimeout:   30 * time.Millisecond,
		WriteTimeout:  30 * time.Millisecond,
		DialerRetries: 1,
		MaxRetries:    -1,
	})
	t.Cleanup(func() { _ = client.Close() })

	backend := mustRedisBackend(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if ok, err := backend.Acquire(ctx, "lease:key", "owner-1", time.Second); err == nil || ok {
		t.Errorf("连不上时 Acquire 必须返回错误，得到 ok=%v err=%v", ok, err)
	} else if !strings.Contains(err.Error(), "Redis 抢占失败") {
		t.Errorf("错误信息应说明是抢占失败：%q", err.Error())
	}

	if ok, err := backend.Renew(ctx, "lease:key", "owner-1", time.Second); err == nil || ok {
		t.Errorf("连不上时 Renew 必须返回错误，得到 ok=%v err=%v", ok, err)
	} else if !strings.Contains(err.Error(), "Redis 续约失败") {
		t.Errorf("错误信息应说明是续约失败：%q", err.Error())
	}

	if ok, err := backend.Release(ctx, "lease:key", "owner-1"); err == nil || ok {
		t.Errorf("连不上时 Release 必须返回错误，得到 ok=%v err=%v", ok, err)
	} else if !strings.Contains(err.Error(), "Redis 释放失败") {
		t.Errorf("错误信息应说明是释放失败：%q", err.Error())
	}

	if value, ok, err := backend.Get(ctx, "lease:key"); err == nil || ok || value != "" {
		t.Errorf("连不上时 Get 必须返回错误，得到 value=%q ok=%v err=%v", value, ok, err)
	} else if !strings.Contains(err.Error(), "Redis 读取租约失败") {
		t.Errorf("错误信息应说明是读取失败：%q", err.Error())
	}
}
