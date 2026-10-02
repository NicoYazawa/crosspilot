// 本文件是 pgtest 的包内测试，刻意用 package pgtest 而不是 pgtest_test。
//
// 需要断言的契约都在包内：databaseName 的库名生成规则、replaceDatabase 对连接串的改写、
// 以及 Admin 在「拿不到数据库」时到底做了什么。这些都不是对外接口，
// 从包外观察只能看到「跳过」这一结果，看不到原因是否被如实报出。
//
// 容器相关的 startContainer / connectAdmin / waitReady / NewDatabase 需要 Docker
// 与真实 Postgres，本文件不覆盖它们——那部分由 pg 包的集成测试在真库上验证，
// 用假对象替代只会换来「我调用了什么」的假安全感。
package pgtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
)

// postgresIdentRe 是 Postgres 对未加引号标识符的字符集要求。
var postgresIdentRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// shortHashRe 是「8 位小写十六进制」的形状约束，用来把短哈希与随意截断区分开。
var shortHashRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// abortSignal 让假 TB 复现 testing.T 的终止语义。
//
// 真实的 Fatalf / Skipf 会调用 runtime.Goexit：被调用之后的语句一行都不会执行。
// 只做记录、不终止的假 TB 会让「跳过」之后的分支继续跑下去，
// 从而把一个本该死掉的执行路径演成「看起来正常返回」。
type abortSignal struct {
	kind string
	msg  string
}

func (s abortSignal) Error() string { return s.kind + ": " + s.msg }

// fakeTB 是可控的 TB 实现：Name 由测试指定，其余调用被记录下来。
//
// 需要它而不能直接用 *testing.T 的原因：库名生成要喂进中文名、超长名等
// 真实测试函数不可能叫出来的名字，而 fail-closed 行为要的是「没被调用」这一断言，
// 不是「让本次测试失败」。
type fakeTB struct {
	name    string
	abort   bool
	helpers int
	fatals  []string
	logs    []string
	skips   []string
	cleanup []func()
}

func (f *fakeTB) Helper() { f.helpers++ }

func (f *fakeTB) Fatalf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	f.fatals = append(f.fatals, msg)
	if f.abort {
		panic(abortSignal{kind: "Fatalf", msg: msg})
	}
}

func (f *fakeTB) Logf(format string, args ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}

func (f *fakeTB) Skipf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	f.skips = append(f.skips, msg)
	if f.abort {
		panic(abortSignal{kind: "Skipf", msg: msg})
	}
}

func (f *fakeTB) Cleanup(fn func()) { f.cleanup = append(f.cleanup, fn) }

func (f *fakeTB) Name() string { return f.name }

// assertPostgresIdentifier 断言生成结果能直接塞进未加引号的 SQL。
//
// 逐个字节地查，是因为 databaseName 的注释承诺「先换掉非 ASCII 再按字节截断」；
// 只断言长度会漏掉「截断把汉字切了一半」这种最要命的失败——
// 那种库名在 Postgres 上会以 "invalid byte sequence for encoding UTF8" 报错，
// 与测试真正想验证的账本逻辑毫无关系。
func assertPostgresIdentifier(t *testing.T, name string) {
	t.Helper()

	if name == "" {
		t.Fatal("库名不能为空")
	}
	if len(name) > 63 {
		t.Errorf("库名 %q 共 %d 字节，超过 Postgres 标识符的 63 字节上限", name, len(name))
	}
	if !postgresIdentRe.MatchString(name) {
		t.Errorf("库名 %q 含非法字符，未加引号的标识符只允许 [a-z0-9_]", name)
	}
	if !utf8.ValidString(name) {
		t.Errorf("库名 %q 不是合法 UTF-8（很可能在多字节字符中间被截断）", name)
	}
	head := name[0]
	if head != '_' && (head < 'a' || head > 'z') {
		t.Errorf("库名 %q 以 %q 开头，Postgres 标识符必须以字母或下划线开头", name, head)
	}
	for i := 0; i < len(name); i++ {
		if name[i] >= utf8.RuneSelf {
			t.Errorf("库名 %q 第 %d 字节为 0x%02x，仍含非 ASCII 字节", name, i, name[i])
			break
		}
	}
}

// TestDatabaseNameNormalizesASCIITestName 覆盖库名的常规路径与四种替换符。
//
// 断言完整结果而不是「含有 cptest_ 前缀」：库名是 CREATE DATABASE 的实参，
// 少替换一个字符（例如 `.`）就会让 SQL 语法出错，前缀断言放不过这种错。
func TestDatabaseNameNormalizesASCIITestName(t *testing.T) {
	cases := []struct {
		desc string
		name string
		want string
	}{
		{"大写转小写", "TestSession", "cptest_testsession"},
		{"斜杠换成下划线", "TestSession/Claim", "cptest_testsession_claim"},
		{"连字符换成下划线", "TestSession-Claim", "cptest_testsession_claim"},
		{"空格换成下划线", "TestSession Claim", "cptest_testsession_claim"},
		{"点换成下划线", "TestSession.Claim", "cptest_testsession_claim"},
		{"四种字符同时出现", "TestSession/Claim-Owner.Save State", "cptest_testsession_claim_owner_save_state"},
		{"其余 ASCII 一律换下划线", "Test:100%x'y", "cptest_test_100_x_y"},
		{"数字保留", "TestLedger2P1", "cptest_testledger2p1"},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := databaseName(&fakeTB{name: tc.name})
			if got != tc.want {
				t.Errorf("databaseName(%q) = %q，期望 %q", tc.name, got, tc.want)
			}
			assertPostgresIdentifier(t, got)
		})
	}
}

// TestDatabaseNameNormalizesAwayTestNameDifferences 断言「只差大小写与分隔符的名字」得到同一个库名。
//
// 这是规范化的意义所在：同一组用例换个写法（t.Run 名字里用 `.` 还是 `-`）
// 不应该凭空多出一个几乎同名的库，否则排查残留库时会分不清谁是谁。
// 反过来也必须记住：正因为它们会撞名，超长名的唯一性才必须靠哈希后缀兜住
// （见 TestDatabaseNameHashKeepsLongNamesUnique）。
func TestDatabaseNameNormalizesAwayTestNameDifferences(t *testing.T) {
	first := databaseName(&fakeTB{name: "TestLedger/Conflict.Save"})
	second := databaseName(&fakeTB{name: "testledger-conflict save"})

	if first != second {
		t.Errorf("仅分隔符与大小写不同的测试名应得到同一个库名，得到 %q 与 %q", first, second)
	}
	if first != "cptest_testledger_conflict_save" {
		t.Errorf("规范化结果 = %q，期望 %q", first, "cptest_testledger_conflict_save")
	}
}

// TestDatabaseNameReplacesNonASCIIWithUnderscore 覆盖中文名路径。
//
// 每个非 ASCII 字符换成一个下划线，是为了让后续的按字节截断永远切在
// 单字节字符上；这里同时断言「结果里没有任何 >= 0x80 的字节」，
// 因为只要漏掉一个多字节字符，Postgres 建库就会报 UTF-8 错误。
func TestDatabaseNameReplacesNonASCIIWithUnderscore(t *testing.T) {
	// 下划线个数 = 非 ASCII 字符数 + 被替换的分隔符数：
	// 每个汉字、全角标点各贡献一个下划线，`/`、`.` 之类的分隔符也各贡献一个。
	// 这个等式本身就是契约的一部分，所以期望值写成算式而不是数出来的常量。
	cases := []struct {
		desc string
		name string
		want string
	}{
		{"纯中文", "测试会话", "cptest_" + strings.Repeat("_", 4)},
		{"中文加斜杠", "测试会话/中文", "cptest_" + strings.Repeat("_", 4+1+2)},
		{"中文混 ASCII", "TestLedger/下单", "cptest_testledger" + strings.Repeat("_", 1+2)},
		{"全角标点", "测试。", "cptest_" + strings.Repeat("_", 2+1)},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := databaseName(&fakeTB{name: tc.name})
			if got != tc.want {
				t.Errorf("databaseName(%q) = %q，期望 %q", tc.name, got, tc.want)
			}
			assertPostgresIdentifier(t, got)
		})
	}
}

// TestDatabaseNameTruncatesToByteLimitWithHash 覆盖 63 字节边界两侧。
//
// 边界必须精确：56 字节的正文恰好凑满 63（7 字节前缀 + 56），
// 此时不能加哈希后缀，否则库名长度白白少 9 个字节；
// 57 字节就必须走截断并带哈希，否则 Postgres 直接拒绝建库。
func TestDatabaseNameTruncatesToByteLimitWithHash(t *testing.T) {
	const (
		prefix  = "cptest_"
		maximum = 63
	)

	t.Run("恰好等于上限不截断", func(t *testing.T) {
		name := strings.Repeat("a", 56)
		want := prefix + name
		if len(want) != maximum {
			t.Fatalf("用例自身有问题：期望构造 %d 字节的名字，实际 %d", maximum, len(want))
		}

		got := databaseName(&fakeTB{name: name})
		if got != want {
			t.Errorf("databaseName(56×\"a\") = %q，期望 %q", got, want)
		}
		// 没有哈希后缀：结果里除了正文不应出现别的字符
		if strings.TrimPrefix(got, prefix) != name {
			t.Errorf("未超限时不应加入哈希后缀，得到 %q", got)
		}
		assertPostgresIdentifier(t, got)
	})

	t.Run("超出一个字节即截断", func(t *testing.T) {
		cleaned := strings.Repeat("a", 57)
		want := prefix + cleaned[:47] + "_" + shortHash(cleaned)
		if len(want) != maximum {
			t.Fatalf("用例自身有问题：期望 %d 字节，实际 %d", maximum, len(want))
		}

		got := databaseName(&fakeTB{name: cleaned})
		if got != want {
			t.Errorf("databaseName(57×\"a\") = %q，期望 %q", got, want)
		}
		if !strings.HasPrefix(got, prefix) {
			t.Errorf("截断后仍必须保留前缀 %q，得到 %q", prefix, got)
		}
		if !shortHashRe.MatchString(got[len(got)-8:]) {
			t.Errorf("截断后应以 _ + 8 位小写十六进制结尾，得到 %q", got)
		}
		assertPostgresIdentifier(t, got)
	})

	t.Run("超长中文名截断后仍是合法标识符", func(t *testing.T) {
		// 100 个汉字：按字符截断会切坏 UTF-8，按字节截断前先换成下划线才安全
		got := databaseName(&fakeTB{name: strings.Repeat("测", 100)})

		if len(got) != maximum {
			t.Errorf("超长名应恰好截到 %d 字节，得到 %d（%q）", maximum, len(got), got)
		}
		want := prefix + strings.Repeat("_", 47) + "_" + shortHash(strings.Repeat("_", 100))
		if got != want {
			t.Errorf("超长中文名 = %q，期望 %q", got, want)
		}
		assertPostgresIdentifier(t, got)
	})

	t.Run("哈希针对规范化后的名字", func(t *testing.T) {
		// 大写与 `.` 必须先规范化再算哈希，否则同一个测试名换个大小写
		// 会得到不同库名，唯一性就不再由测试名本身决定
		cleaned := strings.Repeat("z", 60) + "_b"
		want := prefix + cleaned[:47] + "_" + shortHash(cleaned)
		if len(want) != maximum {
			t.Fatalf("用例自身有问题：期望 %d 字节，实际 %d", maximum, len(want))
		}

		got := databaseName(&fakeTB{name: strings.Repeat("Z", 60) + ".b"})
		if got != want {
			t.Errorf("databaseName(60×\"Z\"+\".b\") = %q，期望 %q（哈希应基于小写且已替换的正文）", got, want)
		}
	})
}

// TestDatabaseNameHashKeepsLongNamesUnique 覆盖哈希后缀存在的理由：唯一性。
//
// 两个长名共享同一个长前缀时，截断后的前 47 字节完全相同——
// 没有哈希后缀就会撞名，而 CREATE DATABASE 撞名会让两个测试互相污染。
func TestDatabaseNameHashKeepsLongNamesUnique(t *testing.T) {
	shared := strings.Repeat("x", 100)
	first := databaseName(&fakeTB{name: shared + "alpha"})
	second := databaseName(&fakeTB{name: shared + "beta"})

	if first == second {
		t.Fatalf("共享长前缀的两个测试名必须得到不同的库名，却都是 %q", first)
	}
	if len(first) != 63 || len(second) != 63 {
		t.Errorf("超长名应恰好 63 字节，得到 %d 与 %d", len(first), len(second))
	}
	// 前 54 字节（前缀 + 47 字节正文 + 分隔下划线）必然相同，
	// 差异只可能来自最后 8 位哈希——这正是「哈希保证唯一」的落点
	if first[:54] != second[:54] {
		t.Errorf("截断正文部分应相同：%q vs %q", first[:54], second[:54])
	}
	if first[54:] == second[54:] {
		t.Errorf("哈希后缀应不同才能保证唯一，两边都是 %q", first[54:])
	}
	assertPostgresIdentifier(t, first)
	assertPostgresIdentifier(t, second)
}

// TestShortHashIsStableEightLowercaseHex 用已知答案锁住 shortHash 的实现。
//
// 只断言「长度是 8」会被任何截断字符串的实现蒙混过关；
// 这里比对 SHA-256 的公开测试向量，确保换实现时库名不会有隐蔽变化
// （库名一变，本地遗留的测试库就会对不上号）。
func TestShortHashIsStableEightLowercaseHex(t *testing.T) {
	cases := []struct {
		value string
		want  string
	}{
		{"", "e3b0c442"},    // SHA-256("")   = e3b0c44298fc1c14...
		{"abc", "ba7816bf"}, // SHA-256("abc") = ba7816bf8f01cfea...
		{"会话", "a6328025"},  // 多字节输入按 UTF-8 字节喂给 SHA-256，不按 rune 处理
	}

	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			got := shortHash(tc.value)

			sum := sha256.Sum256([]byte(tc.value))
			want := hex.EncodeToString(sum[:])[:8]
			if got != want {
				t.Errorf("shortHash(%q) = %q，期望 SHA-256 前 8 位 %q", tc.value, got, want)
			}
			if tc.want != "" && got != tc.want {
				t.Errorf("shortHash(%q) = %q，与已知向量 %q 不符（实现可能被改动）", tc.value, got, tc.want)
			}
			if !shortHashRe.MatchString(got) {
				t.Errorf("shortHash(%q) = %q，应为 8 位小写十六进制", tc.value, got)
			}
			if again := shortHash(tc.value); again != got {
				t.Errorf("shortHash(%q) 两次调用结果不同：%q vs %q", tc.value, got, again)
			}
		})
	}
}

// TestShortHashDiffersForDifferentInputs 断言短哈希对相邻输入的区分度。
//
// 库名的唯一性完全押在这 8 位十六进制上，因此这里不只看两个样例：
// 用一组只有一字之差的输入统计去重后的个数，任何「取前缀」或常量返回都会被抓住。
func TestShortHashDiffersForDifferentInputs(t *testing.T) {
	inputs := make([]string, 0, 128)
	for i := 0; i < 128; i++ {
		inputs = append(inputs, fmt.Sprintf("TestLedger/conflict-%03d", i))
	}
	// 再加上几组「看起来很像」的输入
	inputs = append(inputs, "a", "A", "a ", " a", "aa", "a\u00a0b")

	seen := make(map[string]string, len(inputs))
	for _, in := range inputs {
		got := shortHash(in)
		if !shortHashRe.MatchString(got) {
			t.Fatalf("shortHash(%q) = %q，应为 8 位小写十六进制", in, got)
		}
		if prev, ok := seen[got]; ok {
			t.Errorf("shortHash 出现碰撞：%q 与 %q 都得到 %q", prev, in, got)
			continue
		}
		seen[got] = in
	}

	if len(seen) != len(inputs) {
		t.Errorf("去重后得到 %d 个哈希，输入共 %d 个，说明存在碰撞或实现未区分输入", len(seen), len(inputs))
	}
}

// unsetEnv 在本次测试期间清掉一个环境变量，结束后恢复原值（含「原本就不存在」）。
//
// 用 os.Unsetenv 而不是 t.Setenv("")：空串与「未设置」在本文件的逻辑里是两回事，
// LookupEnv 能区分它们，而 pinDockerEndpoint 正是靠这个区分决定要不要写入。
func unsetEnv(t *testing.T, key string) {
	t.Helper()

	previous, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("清除环境变量 %s 失败：%v", key, err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, previous)
			return
		}
		_ = os.Unsetenv(key)
	})
}

// TestPinDockerEndpointOnlyOnWindows 断言钉端点的作用范围。
//
// 这两个变量只在 Windows 上是对的：npipe 在 Linux/CI 上根本不是合法的 docker 端点，
// 一旦在没有 GOOS 判断的情况下设置，CI（Ubuntu）上所有集成测试都会连不上 docker——
// 而且是「连不上 → 跳过」的静默失败，比直接报错更难查。
//
// goos 逐一喂进来（而不是断言 runtime.GOOS 的结果），是为了让「非 Windows 上什么都
// 不做」这条分支在任何平台上都被真正执行到：只在本机跑，这条分支永远走不到，
// 而它恰恰是 CI 上唯一生效的那条。
func TestPinDockerEndpointOnlyOnWindows(t *testing.T) {
	cases := []struct {
		goos    string
		wantSet bool
	}{
		{"windows", true},
		{"linux", false},
		{"darwin", false},
		{"js", false},
	}

	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			unsetEnv(t, EnvDockerHost)
			unsetEnv(t, EnvDockerSocketOverride)

			pinDockerEndpoint(tc.goos)

			host, hostSet := os.LookupEnv(EnvDockerHost)
			socket, socketSet := os.LookupEnv(EnvDockerSocketOverride)

			if !tc.wantSet {
				if hostSet || socketSet {
					t.Fatalf("%s 上不应写入 docker 端点，却得到 %s=%q %s=%q",
						tc.goos, EnvDockerHost, host, EnvDockerSocketOverride, socket)
				}
				return
			}

			if !hostSet || !socketSet {
				t.Fatalf("%s 上应同时钉住两个变量，实际 %s set=%v、%s set=%v",
					tc.goos, EnvDockerHost, hostSet, EnvDockerSocketOverride, socketSet)
			}
			if host != windowsDockerHost {
				t.Errorf("%s = %q，期望 %q", EnvDockerHost, host, windowsDockerHost)
			}
			if socket != windowsDockerSocketPath {
				t.Errorf("%s = %q，期望 %q", EnvDockerSocketOverride, socket, windowsDockerSocketPath)
			}

			// socket 值不能带 schema：testcontainers 的 checkDockerSocketFn 见到
			// tcp:// 会**静默改写成** /var/run/docker.sock，见到 unix:// 或 npipe://
			// 会把前缀剥掉——两种都会让这里钉的值与实际生效的值不一致，且看不出来。
			if strings.Contains(socket, "://") {
				t.Errorf("socket override 应为裸路径（带 schema 会被 testcontainers 改写或剥离）：%q", socket)
			}
			if !strings.HasPrefix(host, "npipe://") {
				t.Errorf("Windows 上的 docker 端点应为 npipe 形式，得到 %q", host)
			}
		})
	}
}

// TestPinDockerEndpointKeepsCallerValues 断言调用方显式设好的值不会被覆盖。
//
// 需要一个不完整的环境（例如指向远程 daemon、或 CI 上注入了自己的 socket 路径）时，
// 覆盖掉它会让排查方向完全跑偏——而这类环境恰恰是最需要看清真实端点的地方。
func TestPinDockerEndpointKeepsCallerValues(t *testing.T) {
	const (
		customHost   = "tcp://192.0.2.10:2375"
		customSocket = "/run/custom/docker.sock"
	)
	t.Setenv(EnvDockerHost, customHost)
	t.Setenv(EnvDockerSocketOverride, customSocket)

	pinDockerEndpoint("windows")

	if got := os.Getenv(EnvDockerHost); got != customHost {
		t.Errorf("%s 被改写成 %q，调用方设好的 %q 必须原样保留", EnvDockerHost, got, customHost)
	}
	if got := os.Getenv(EnvDockerSocketOverride); got != customSocket {
		t.Errorf("%s 被改写成 %q，调用方设好的 %q 必须原样保留", EnvDockerSocketOverride, got, customSocket)
	}
}

// TestReplaceDatabaseSwapsOnlyDatabasePath 断言连接串改写只动库名。
//
// 维护库与测试库只差库名：主机、端口、凭据、参数（sslmode 之类）必须原样保留，
// 否则「连上了但连的是别处」会表现成权限错误或莫名其妙的空表，
// 排查成本远高于在这里断言一次。
func TestReplaceDatabaseSwapsOnlyDatabasePath(t *testing.T) {
	original := adminDSN
	t.Cleanup(func() { adminDSN = original })

	// 下面两条是断言用的夹具串，不是真实凭据；拆开拼接反而看不清「转义前后」的对照
	const dsn = "postgres://user:pa%40ss@db.example.com:6543/maintenance?sslmode=disable&application_name=cp" //nolint:gosec // G101: 测试夹具，非真实凭据
	adminDSN = dsn

	tb := &fakeTB{}
	got := replaceDatabase(tb, "cptest_session_store")

	// 精确断言整条串：用户信息里的 @ 会按 URL 规则重新转义，正好一并锁住
	want := "postgres://user:pa%40ss@db.example.com:6543/cptest_session_store?sslmode=disable&application_name=cp" //nolint:gosec // G101: 测试夹具，非真实凭据
	if got != want {
		t.Errorf("replaceDatabase = %q，期望 %q", got, want)
	}

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("replaceDatabase 的返回值必须自身可解析，得到错误 %v（%q）", err, got)
	}
	if parsed.Path != "/cptest_session_store" {
		t.Errorf("Path = %q，期望 %q", parsed.Path, "/cptest_session_store")
	}
	if parsed.Scheme != "postgres" {
		t.Errorf("Scheme = %q，期望 postgres", parsed.Scheme)
	}
	if parsed.Host != "db.example.com:6543" {
		t.Errorf("Host = %q，期望 db.example.com:6543", parsed.Host)
	}
	if parsed.User.Username() != "user" {
		t.Errorf("用户名 = %q，期望 user", parsed.User.Username())
	}
	if pw, _ := parsed.User.Password(); pw != "pa@ss" {
		t.Errorf("密码 = %q，期望 pa@ss（转义后应还原）", pw)
	}
	if parsed.RawQuery != "sslmode=disable&application_name=cp" {
		t.Errorf("查询参数 = %q，期望原样保留 sslmode=disable&application_name=cp", parsed.RawQuery)
	}

	// 包级变量是宿主的判断依据，函数只应读它，不能就地改写
	if adminDSN != dsn {
		t.Errorf("replaceDatabase 不应改写包级 adminDSN：%q", adminDSN)
	}
	if tb.helpers == 0 {
		t.Error("replaceDatabase 应调用 t.Helper()，否则失败会归因到脚手架而不是调用点")
	}
	if len(tb.fatals) != 0 {
		t.Errorf("连接串合法时不应报错，得到 %v", tb.fatals)
	}
}

// TestReplaceDatabaseWithoutQuery 断言没有查询参数时的结果也是干净的目标库串。
//
// 少了这一例，就可能在拼接时凭空多出 "?" 或残留维护库路径，
// 而带查询参数的用例看不出来。
func TestReplaceDatabaseWithoutQuery(t *testing.T) {
	original := adminDSN
	t.Cleanup(func() { adminDSN = original })

	adminDSN = "postgres://crosspilot:crosspilot-test@127.0.0.1:5432/postgres" //nolint:gosec // G101: 测试夹具，非真实凭据

	got := replaceDatabase(&fakeTB{}, "cptest_plain")
	if want := "postgres://crosspilot:crosspilot-test@127.0.0.1:5432/cptest_plain"; got != want { //nolint:gosec // G101: 测试夹具，非真实凭据
		t.Errorf("replaceDatabase = %q，期望 %q", got, want)
	}

	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("返回值必须可解析：%v", err)
	}
	if parsed.Path != "/cptest_plain" {
		t.Errorf("Path = %q，期望 %q", parsed.Path, "/cptest_plain")
	}
	if parsed.RawQuery != "" {
		t.Errorf("原本没有查询参数时不应出现 %q", parsed.RawQuery)
	}
}

// TestReplaceDatabaseFailsLoudlyOnUnparsableDSN 断言连接串解析失败不是静默的。
//
// 假 TB 的 Fatalf 一旦不终止，函数会继续对 nil 的 *url.URL 赋值而 panic，
// 因此这里用 abort 语义的假 TB 让 Fatalf 真的中断执行：
// 断言「报了错」并且「没有返回值」两件事，缺一件都说明失败被吞掉了。
func TestReplaceDatabaseFailsLoudlyOnUnparsableDSN(t *testing.T) {
	original := adminDSN
	t.Cleanup(func() { adminDSN = original })

	// 这一条要的就是「端口非法」的串，凭据部分同样是夹具（G101 见下）
	adminDSN = "postgres://user:secret@db.example.com:notaport/maintenance" //nolint:gosec // G101: 测试夹具，非真实凭据
	tb := &fakeTB{abort: true}

	var returned bool
	func() {
		defer func() {
			recovered := recover()
			sig, ok := recovered.(abortSignal)
			if !ok {
				t.Errorf("连接串无法解析时应通过 Fatalf 终止，实际 panic 值为 %v", recovered)
				return
			}
			if sig.kind != "Fatalf" {
				t.Errorf("应调用 Fatalf，实际调用 %s：%q", sig.kind, sig.msg)
			}
			if !strings.Contains(sig.msg, "解析测试连接串失败") {
				t.Errorf("错误文案应说明是解析连接串失败，得到 %q", sig.msg)
			}
			// 端口是最常见的写错之处，原因必须落在文案里才可定位
			if !strings.Contains(sig.msg, "notaport") {
				t.Errorf("错误文案应包含底层解析错误（含出错片段），得到 %q", sig.msg)
			}
		}()

		_ = replaceDatabase(tb, "cptest_broken")
		returned = true
	}()

	if returned {
		t.Error("连接串无法解析时 replaceDatabase 不应返回值（否则会拿着空串去连库）")
	}
}

// TestAdminFailsClosedWhenHarnessUnavailable 断言拿不到数据库时 Admin 的两个出口。
//
// Admin 的三条分支决定了「集成测试是跳过还是失败」，方向正好相反：
// 环境不可用应该跳过（Skipf），脚手架自身出错必须失败（Fatalf），
// 把两者搞反会让本机缺 Docker 时出现一堆假失败——或者更糟，
// 让真正坏掉的夹具被当成「环境问题」静默跳过，测试就永远不跑了。
func TestAdminFailsClosedWhenHarnessUnavailable(t *testing.T) {
	// Admin 内部第一步是 once.Do(prepare)，而 prepare 会尝试启动真实容器（最长 3 分钟）。
	// 先用一个空动作把 once 标记为已完成，本测试才能只观察 prepare 之后的分支：
	// 「没有可达数据库时 Admin 到底怎么表现」。
	// 代价是本二进制里不会再真正 prepare——pgtest 目录下只有这一个测试文件，
	// 需要真库的集成测试都在 pg 包（独立测试二进制）里，因此不会互相影响。
	once.Do(func() {})

	originalSkip, originalFailed, originalAdmin := skipReason, setupFailed, admin
	t.Cleanup(func() {
		skipReason, setupFailed, admin = originalSkip, originalFailed, originalAdmin
	})

	t.Run("有跳过原因时调用 Skipf 并写明原因", func(t *testing.T) {
		skipReason = "无法启动 Postgres 容器（docker 未运行），且未设置 CROSSPILOT_TEST_POSTGRES"
		setupFailed = errors.New("创建维护库连接池失败：dial tcp: connection refused")
		admin = nil
		tb := &fakeTB{name: "TestFake"}

		got := Admin(tb)

		if len(tb.skips) != 1 {
			t.Fatalf("应恰好调用一次 Skipf，实际 %d 次：%v", len(tb.skips), tb.skips)
		}
		if !strings.Contains(tb.skips[0], skipReason) {
			t.Errorf("Skipf 的文案必须原样报出原因 %q，得到 %q", skipReason, tb.skips[0])
		}
		if !strings.Contains(tb.skips[0], "跳过集成测试") {
			t.Errorf("Skipf 的文案应说明这是跳过而不是通过，得到 %q", tb.skips[0])
		}
		if tb.helpers == 0 {
			t.Error("Admin 应调用 t.Helper()，否则跳过信息会归因到脚手架而不是调用点")
		}
		// 记录型假 TB 的 Skipf 不会终止执行，所以这里必然拿到 nil（admin 被置空）。
		// 这正是「静默返回 nil 连接池」的危险形态；下一个子测试用终止语义证明
		// 真实 testing.T 下调用方根本走不到这一行。
		if got != nil {
			t.Errorf("跳过分支下不应返回可用连接池，得到 %v", got)
		}
	})

	t.Run("Skipf 终止语义下调用方拿不到空连接池", func(t *testing.T) {
		// 同时给出跳过原因与启动失败：环境不可用属于「本机跑不了集成测试」，
		// 应当跳过；若被 setupFailed 抢先生效，缺 Docker 的人就会看到一片红。
		skipReason = "无法连接外部测试实例"
		setupFailed = errors.New("创建维护库连接池失败：dial tcp: connection refused")
		admin = nil
		tb := &fakeTB{name: "TestFake", abort: true}

		var returned bool
		func() {
			defer func() {
				recovered := recover()
				sig, ok := recovered.(abortSignal)
				if !ok {
					t.Errorf("Skipf 应终止测试（真实 testing.T 会 runtime.Goexit），实际 panic 值为 %v", recovered)
					return
				}
				if sig.kind != "Skipf" {
					t.Errorf("有跳过原因时应调用 Skipf，实际调用 %s：%q", sig.kind, sig.msg)
				}
				if !strings.Contains(sig.msg, skipReason) {
					t.Errorf("Skipf 的文案应包含原因 %q，得到 %q", skipReason, sig.msg)
				}
				// Skipf 已经终止，说明 setupFailed 的检查根本没被执行到：
				// 这正是「跳过优先于失败」的证据。
				if len(tb.fatals) != 0 {
					t.Errorf("有跳过原因时不应调用 Fatalf，实际：%v", tb.fatals)
				}
			}()

			_ = Admin(tb)
			returned = true
		}()

		if returned {
			t.Error("Skipf 之后 Admin 仍返回了：真实 testing.T 会在此终止，出现返回值即说明没有 fail-closed")
		}
	})

	t.Run("脚手架出错时调用 Fatalf 并写明原因", func(t *testing.T) {
		skipReason = ""
		setupFailed = errors.New("等待数据库就绪超时：context deadline exceeded")
		admin = nil
		tb := &fakeTB{name: "TestFake"}

		got := Admin(tb)

		if len(tb.fatals) != 1 {
			t.Fatalf("应恰好调用一次 Fatalf，实际 %d 次：%v", len(tb.fatals), tb.fatals)
		}
		if !strings.Contains(tb.fatals[0], "准备测试数据库失败") {
			t.Errorf("Fatalf 的文案应说明准备数据库失败，得到 %q", tb.fatals[0])
		}
		if !strings.Contains(tb.fatals[0], setupFailed.Error()) {
			t.Errorf("Fatalf 的文案应包含底层错误 %q，得到 %q", setupFailed, tb.fatals[0])
		}
		// 夹具坏了必须失败而不是跳过，否则这类故障永远不会被人看到
		if len(tb.skips) != 0 {
			t.Errorf("脚手架出错时不应调用 Skipf，实际：%v", tb.skips)
		}
		if got != nil {
			t.Errorf("准备失败时不应返回可用连接池，得到 %v", got)
		}
	})

	t.Run("两者皆空时返回已就绪的维护库连接池", func(t *testing.T) {
		skipReason = ""
		setupFailed = nil

		// 端口 1 上不会有 Postgres；pgxpool.New 只解析配置、默认不建立连接
		// （MinConns 为 0），所以这是一个零成本且不会真正拨号的哨兵值。
		const lazyDSN = "postgres://crosspilot:crosspilot-test@127.0.0.1:1/crosspilot?sslmode=disable&connect_timeout=1" //nolint:gosec // G101: 测试夹具，非真实凭据
		pool, err := pgxpool.New(context.Background(), lazyDSN)
		if err != nil {
			t.Fatalf("构造哨兵连接池失败：%v", err)
		}
		t.Cleanup(pool.Close)
		admin = pool

		tb := &fakeTB{name: "TestFake"}
		got := Admin(tb)

		if got != pool {
			t.Errorf("Admin 应返回包级 admin 连接池 %p，得到 %p", pool, got)
		}
		if got == nil {
			t.Error("准备成功时不能返回 nil 连接池")
		}
		if len(tb.skips) != 0 || len(tb.fatals) != 0 {
			t.Errorf("准备成功时不应跳过或失败：skips=%v fatals=%v", tb.skips, tb.fatals)
		}
		if len(tb.logs) != 0 {
			t.Errorf("准备成功时不应有额外日志，得到 %v", tb.logs)
		}
		// Admin 只交出连接池，不注册清理：测试库的 DROP 与池的关闭都在 NewDatabase 里。
		// 这里断言它没有偷偷注册，免得调用方误以为关池是自动的。
		if len(tb.cleanup) != 0 {
			t.Errorf("Admin 不应注册清理函数，实际注册 %d 个", len(tb.cleanup))
		}
	})
}
