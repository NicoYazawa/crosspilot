# testcontainers 端点探测在本机间歇性失败

## 概述

**现象**：`task check` 偶尔以 `--- FAIL: TestPgCatalogFind_返回全部规格` 收场，报的是

```
panic: rootless Docker is not supported on Windows
  ...internal/core.extractDockerSocketFromClient (docker_host.go:220)
```

而这台机器跑的是 Docker Desktop 的 Linux 引擎，跟 rootless 毫无关系。单独跑该用例则必然通过。

**结论**：这条报错信息与真实原因完全无关。真实原因是 testcontainers 在 Windows 上判断
Docker 端点时，落到了一次 `os.Stat("//./pipe/docker_engine")` 上；Go 的 `os.Stat` 对命名管道
会真的 `CreateFile` **打开**它，而 Docker Desktop 的管道实例数有限，占满时报
`All pipe instances are busy`。这个 stat 失败之后，六条候选渠道全数落空，
`isHostNotSet` 又把其中五条「未设置」类错误滤掉，只剩最后一条 rootless 的报错——
于是 panic 信息成了那句与 Windows 无关的话。

**它有两个症状，而且更常见的那个不是 panic，是静默跳过**：

| 症状 | 触发点 | 表现 |
|---|---|---|
| **集成测试静默跳过** | `NewClient` 失败（`docker_host.go:166`） | 容器起不来 → 脚手架 `Skipf` → 包级结果仍是 `ok` |
| **测试进程 panic** | socket 解析落空（`docker_host.go:220`） | 整个包 FAIL，报 rootless 错 |

**范围**：只改测试脚手架与其用例，不动任何生产代码路径，不动任何阈值。

---

## 一、诊断过程

### 1.1 先确认报错信息是否可信

panic 说的是「Windows 不支持 rootless docker」，但：

```
$ docker info --format '{{.OperatingSystem}}'
Docker Desktop (containerized)
```

这台机器根本没有 rootless 引擎。先在 `docker_host.go` 里找到这句话的来源：

```
166	cli, err := NewClient(ctx)
168	if err != nil { panic(err) }          ← panic 之一
...
204	info, err := cli.Info(ctx, client.InfoOptions{})
210	if info.Info.OperatingSystem == "Docker Desktop" { ... }   ← 硬编码相等
218	dockerHost, err := extractDockerHost(ctx)
220	if err != nil { panic(err) }          ← panic 之二
```

`ErrRootlessDockerNotSupportedWindows` 是 `extractDockerHost` 六条候选里**最后一条**
（`rootlessDockerSocketPath`）的错误。它之所以成为唯一可见的错误，是因为
`isHostNotSet` 会把前五条「未设置」类错误全部滤掉，剩下这一条没被滤。

**也就是说：panic 文案是一条被过滤剩下的错误，不是诊断。** 只要 `extractDockerHost`
失败，看到的就是它。

### 1.2 为什么第 210 行的相等判断永远不成立

`OperatingSystem` 在容器化的 Docker Desktop 上是 `"Docker Desktop (containerized)"`，
与代码里硬编码的 `"Docker Desktop"` 永不相等。于是「命中就早点返回」那条捷径永不生效，
解析一路退到 `extractDockerHost`。

用探针确认了这一点：本机的 socket 解析返回 `//./pipe/docker_engine`，
而这条值只可能来自 `extractDockerHost` 的第 4 条候选 `dockerSocketPath`
（因为第 210 行那条捷径本该返回的是 `WindowsDockerSocketPath = "//var/run/docker.sock"`）。

### 1.3 那条候选靠什么判断

```
func dockerSocketPath(_ context.Context) (string, error) {
	if fileExists(DockerSocketPath) { return DockerSocketPathWithSchema, nil }
	return "", ErrSocketNotFoundInPath
}

func fileExists(f string) bool { _, err := os.Stat(f); return err == nil }
```

`DockerSocketPath` 由 `docker_socket.go` 的 `init()` 从 Docker 的默认 host 推导，
在本机就是 `//./pipe/docker_engine`。

**关键点**：Windows 上 `os.Stat` 一个命名管道不是「查一下在不在」——它会 `CreateFile`
真的把管道打开。管道实例被占满时它就失败。实测（`go run` 直接量）：

| 调用节奏 | 失败率 |
|---|---|
| 紧凑循环 | **15157 / 20000（75.8%）** |
| 每次间隔 5ms | **113 / 2000（5.7%）** |
| 每次间隔 50ms | 0 / 400（0.0%） |

紧凑循环那一栏主要是自己跟自己抢，但 5ms 一栏说明**在真实交错下失败率并不为零**。
全量测试有几十个包并发跑容器，正好是会把管道实例占满的场景。

### 1.4 两个症状，同一个根因

这个 stat 有**两个**调用点，都要堵，只堵一个只是把 panic 从一行挪到另一行：

| # | 调用点 | 失败后果 |
|---|---|---|
| 1 | `docker_host.go:166` `NewClient` → `ExtractDockerHost` | `panic:168`；**也是容器启动失败、测试被静默跳过的根源** |
| 2 | `docker_host.go:204` `cli.Info` → `extractDockerHost` | `panic:220` |

症状 1 比症状 2 更值得警惕：`NewClient` 失败会让 `GenericContainer` 直接报错，
脚手架把它归为「环境不可用」而 `Skipf`，**包级结果依然是 `ok`**。
实测对照见 §3.2——同一批集成测试，5 个用例 0 通过 5 跳过，而 `go test` 报的是 `ok`。

这与本目录另一份记录（[测试容器泄漏修复.md](测试容器泄漏修复.md)）里 ryuk 那条属于同一类缺陷：
**失败被伪装成通过**。

---

## 二、修复

### 2.1 把端点与 socket 路径钉死

新增 [harness.go](../../../internal/infra/persistence/pg/pgtest/harness.go) 的
`pinDockerEndpoint(goos)`，在 `prepare()` 里、任何容器操作之前调用：

```go
func pinDockerEndpoint(goos string) {
	if goos != "windows" {
		return
	}
	if _, ok := os.LookupEnv(EnvDockerHost); !ok {
		_ = os.Setenv(EnvDockerHost, windowsDockerHost)
	}
	if _, ok := os.LookupEnv(EnvDockerSocketOverride); !ok {
		_ = os.Setenv(EnvDockerSocketOverride, windowsDockerSocketPath)
	}
}
```

两个变量分别命中两条调用点之前的更早分支：

- `DOCKER_HOST` 命中 `extractDockerHost` 的**第 2 条**候选（在 stat 之前）；
- `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` 命中 socket 解析的**第 2 步**（同样在 stat 之前）。

钉的值就是 Windows 上 Docker Desktop 的**默认值本身**，不是换一个端点：
testcontainers 推导 `DockerSocketPath` 时看的就是这个默认值，它压根不读 `docker context`。
所以这不是行为变更，而是把库已经假定的那个值写明，去掉中间那次探测。

`goos` 由调用方传入而不是直接读 `runtime.GOOS`：这样「非 Windows 上什么都不做」
这条分支在任何平台上都能被测试执行到，而不是只在 Linux CI 上才有机会暴露——
而 CI 上生效的恰恰只有这一条。

只在 Windows 上设、且不覆盖调用方已设好的值：npipe 在 Linux 上不是合法端点，
而一个已经指明了远端 daemon 的环境恰恰最不该被覆盖。

### 2.2 只用 `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` 是不够的

先只加了这一个，实测 panic 从 220 挪到了 168（见 §3.1 的 B 组），
因为 `NewClient` 走的是另一条路径。两个都加才归零。

---

## 三、验证

### 3.1 先让 panic 按需复现

间歇性问题最难办的是「修完没再见到」并不能证明修好了。所以先把管道占满
（8 个进程持续 `os.Stat` 同一条管道），让失败率变成 100%，再对照：

| 组 | `DOCKER_HOST` | socket override | 12 次结果 |
|---|---|---|---|
| A | 不设 | 不设 | **12/12 panic** |
| B | 不设 | 设 | **10/12 panic**（栈从 220 挪到 168） |
| C | 设 | 设 | **0/12 panic** |

A 组复现出的完整栈与线上那次一致：

```
panic: rootless Docker is not supported on Windows [recovered, repanicked]
  ...internal/core.extractDockerSocketFromClient (docker_host.go:220)
  ...internal/core.extractDockerSocket (docker_host.go:172)
  ...internal/core.MustExtractDockerSocket.func1 (docker_host.go:118)
```

对照组不保留在仓库里（它要求修改 harness 才能复现），数值记在这里。

### 3.2 真实集成测试的对照：症状 1 才是主要的

同样的 8 路管道占用下，跑真实的 pg 集成测试：

```
$ go test ./internal/infra/persistence/pg/ -run 'TestPgCatalogFind' -count=1 -v
```

| | RUN | PASS | SKIP | panic | 包级结果 |
|---|---|---|---|---|---|
| **关掉 `pinDockerEndpoint`**（对照） | 5 | **0** | **5** | 0 | `ok` ← **假绿** |
| **开着** | 5 | **5** | 0 | 0 | `ok` |

对照组 3 次全是 5 跳过 0 通过，跳过原因里就写着那句 rootless 报错。

**这条比 panic 更严重**：panic 至少会让包变红、有人来看；静默跳过是包级 `ok`，
而集成测试其实一个都没跑。修复后同一条件下 3 次全是 5 通过。

### 3.3 全量套件不再有跳过

```
$ go test ./... -count=1 -skip 'P95Under' -v
退出码 0；--- SKIP 计数：0
```

### 3.4 新用例不是摆设（变异）

新增 [harness_internal_test.go](../../../internal/infra/persistence/pg/pgtest/harness_internal_test.go)
的 `TestPinDockerEndpointOnlyOnWindows`（goos 逐一代入 windows/linux/darwin/js）与
`TestPinDockerEndpointKeepsCallerValues`。三项变异，逐项确认变红：

| 变异 | 结果 |
|---|---|
| 删掉 `goos != "windows"` 判断 | `linux` / `darwin` / `js` 三个子用例 FAIL |
| 去掉 `LookupEnv` 守卫、无条件写入 | `TestPinDockerEndpointKeepsCallerValues` FAIL |
| socket 值带上 schema（`unix:///...`） | `windows` 子用例 FAIL |

第三项针对的是一个真陷阱：testcontainers 的 `checkDockerSocketFn` 见到 `tcp://` 会
**静默改写**成 `/var/run/docker.sock`，见到 `unix://` / `npipe://` 会剥前缀——
两种都会让钉的值和实际生效的值不一致，而且看不出来。

还原用备份副本，未使用 `git checkout`。

### 3.5 门禁

| 门禁 | 结果 |
|---|---|
| `task check` | **退出码 0** |
| `task lint` | **0 issues** |
| `task cover:gate` | **加权 94.2%**（门槛 90%，3896 条语句） |
| `go test ./...`（含全量 -v 普查） | 0 SKIP |

---

## 四、遗留

### 4.1 `task perf` 仍在预算线上（预先存在，与本次改动无关）

```
P50=14.711ms  P95=18.4615ms  预算 18ms   → FAIL
P50=3.2757ms  P95=3.9584ms   预算 18ms   → PASS（幂等重放）
并发写入                                     → PASS
```

本次改动**没有任何一行碰到写入路径**，P50 仍稳定在本机延迟底座上。
按既有约定**未调整阈值**。详见 [测试容器泄漏修复.md](测试容器泄漏修复.md) 的 4.2。

### 4.2 「容器起不来就跳过」这条语义本身值得再讨论

本次把**会导致误跳过**的那条不稳定的探测去掉了，但脚手架的分支方向没动：
`startContainer` 失败一律 `Skipf`。它的本意是「本机没装 Docker 也能跑」，
但代价是——**任何**容器启动失败都会长成绿色的 `ok`。

区分「没有 Docker」与「Docker 坏了」需要一个可靠的判据，而这正是本次刚被证明
不可靠的那一个领域。因此这里只记录、不改：改它会影响所有依赖 Docker 的测试包，
应当单独讨论并单独留证。
