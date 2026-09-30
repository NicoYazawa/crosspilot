// Package arch 用测试守住包之间的依赖方向。
//
// 规矩只有一条：依赖只能由外向内。领域层不知道数据库、HTTP 与模型供应商的
// 存在，因此它可以被完整单测，也不会因为换了基础设施而被牵动。
//
// 为什么是测试而不是外部工具：这条约束必须在开发机上和 CI 上给出同样的结论。
// 用标准库直接读导入声明，任何平台都得到一致结果，也不需要额外的配置文件与
// 供应链。
package arch

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// modulePrefix 是本模块的导入路径前缀。
const modulePrefix = "github.com/NicoYazawa/crosspilot/"

// rootPrefix 是「本项目自己的包」在导入路径上的特征。
const rootPrefix = "github.com/NicoYazawa/crosspilot"

// component 是一个架构组件：一组目录，以及它们被允许依赖什么。
type component struct {
	// name 出现在违规信息里。
	name string
	// dirs 是相对仓库根目录的目录前缀，命中即归属该组件。
	dirs []string
	// projectDeps 是允许依赖的本项目包前缀（相对模块根，不带模块前缀）。
	projectDeps []string
	// vendorDeps 是允许依赖的第三方包前缀。标准库始终允许，无需列出。
	vendorDeps []string
	// anyVendor 表示这个组件可以使用任意第三方包。
	anyVendor bool
}

// components 是全部组件。顺序无关紧要，但目录不能重叠。
//
// 没有出现在这里的 internal/ 下的包会导致测试失败：新包必须显式声明归属，
// 不能因为「没写规则」而默认获得全部自由。
var components = []component{
	{
		name:        "domain",
		dirs:        []string{"internal/domain"},
		projectDeps: []string{"internal/domain"},
		vendorDeps:  []string{"github.com/govalues/decimal"},
	},
	{
		name: "config",
		dirs: []string{"internal/config"},
	},
	{
		name:        "observability",
		dirs:        []string{"internal/observability"},
		projectDeps: []string{"internal/config"},
		vendorDeps:  []string{"go.opentelemetry.io/otel"},
	},
	{
		name: "infra",
		dirs: []string{"internal/infra"},
		projectDeps: []string{
			"internal/domain", "internal/config", "migrations",
			// pgtest 是 Postgres 的测试脚手架（起容器、建库、跑迁移），
			// 只被同层的测试文件导入。它是这一个适配器自己的测试设施，
			// 因此就近放在它服务的包下面，而不是提成一个上层公共包——
			// 提上去反而会让 application/presentation 也能依赖它。
			"internal/infra/persistence/pg/pgtest",
		},
		vendorDeps: []string{
			"github.com/jackc/pgx",
			"github.com/redis/go-redis",
			"go.opentelemetry.io/otel",
			// 集成测试要起真实 Postgres。这条依赖只出现在测试文件里，
			// 但测试文件同样计入依赖方向检查——测试里绕开分层去依赖内层
			// 一样是耦合，第三方依赖同理。
			"github.com/testcontainers/testcontainers-go",
		},
	},
	{
		// 应用层是用例编排：它认识领域模型，但不认识数据库与 HTTP。
		// 持久化细节通过领域端口注入，因此这里只允许依赖 internal/domain。
		name:        "application",
		dirs:        []string{"internal/application"},
		projectDeps: []string{"internal/domain"},
	},
	{
		name:        "presentation",
		dirs:        []string{"internal/presentation"},
		projectDeps: []string{"internal/domain", "internal/config", "internal/infra"},
		vendorDeps:  []string{"github.com/go-chi/chi"},
	},
	{
		// 装配根是唯一的例外：它的职责就是把所有人接起来
		name:      "container",
		dirs:      []string{"internal/container"},
		anyVendor: true,
		projectDeps: []string{
			"internal",
		},
	},
}

// importRef 是一条导入声明及其出处。
type importRef struct {
	pkg  string // 被导入的包路径
	file string // 声明它的文件（仓库相对路径）
	line int
}

func TestDependencyDirection(t *testing.T) {
	root := repoRoot(t)
	refs, scanned := collectImports(t, root)

	// 扫描本身也要被验证：走不到文件的话，下面的断言会全部落空
	if scanned < minExpectedFiles {
		t.Fatalf("只扫描到 %d 个文件，少于预期的 %d 个；扫描逻辑可能已经失效",
			scanned, minExpectedFiles)
	}

	var violations []string
	seenComponents := make(map[string]bool)

	for _, ref := range refs {
		owner, ok := componentOf(ref.file)
		if !ok {
			// 只有 internal/ 下的包必须归属某个组件，其余（cmd、tools、migrations）不在约束内
			if strings.HasPrefix(ref.file, "internal/") {
				violations = append(violations,
					fmt.Sprintf("%s:%d 属于 internal/ 但没有归属任何组件，请在 components 里声明", ref.file, ref.line))
			}
			continue
		}
		seenComponents[owner.name] = true

		// 同目录的外部测试包（package foo_test）会导入它正在测试的那个包。
		// 这不是跨层依赖——它就是同一个包，自己没有「依赖自己」这回事。
		// 不放行这一条，任何带集成测试的包都会被判违规，规则就沦为噪音，
		// 而一条总在报错的规则最终的命运是被关掉。
		if isOwnPackage(ref.file, ref.pkg) {
			continue
		}

		if !allowed(owner, ref.pkg) {
			violations = append(violations, fmt.Sprintf(
				"%s:%d %s 组件不允许依赖 %s", ref.file, ref.line, owner.name, ref.pkg))
		}
	}

	// 每个组件都要真的有文件，否则规则会随着目录改名而静默失效
	for _, want := range components {
		if !seenComponents[want.name] {
			violations = append(violations, fmt.Sprintf(
				"组件 %s 没有匹配到任何文件，目录可能已经改名：%v", want.name, want.dirs))
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("依赖方向违规 %d 处：\n  %s", len(violations), strings.Join(violations, "\n  "))
	}
}

// minExpectedFiles 是扫描文件数的下限，用来发现「扫描悄悄失败」。
const minExpectedFiles = 15

// isOwnPackage 报告一条导入是否为「文件所在目录的那个包」。
func isOwnPackage(file, pkg string) bool {
	own := strings.TrimSuffix(filepath.ToSlash(filepath.Dir(file)), "/")
	return pkg == modulePrefix+own || pkg == rootPrefix+"/"+own
}

func allowed(c component, pkg string) bool {
	if isStdlib(pkg) {
		return true
	}

	if !strings.HasPrefix(pkg, rootPrefix) {
		return c.anyVendor || underAny(pkg, c.vendorDeps)
	}

	// 本项目内的包：去掉模块前缀后按目录前缀比对
	internal := strings.TrimPrefix(pkg, modulePrefix)
	if internal == pkg { // 恰为模块根包本身
		internal = ""
	}
	return underAny(internal, c.projectDeps)
}

func underAny(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if under(path, prefix) {
			return true
		}
	}
	return false
}

// under 判断 path 是否落在 prefix 之下。
//
// 前缀同时锚定「这个包本身」和「它的子树」，因此结尾写不写斜杠都行：
// internal/domain 既匹配 internal/domain，也匹配 internal/domain/order。
func under(path, prefix string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// isStdlib 用「导入路径首段是否含点」来区分标准库与外部模块。
//
// 这是 Go 生态里的通行判据：标准库路径的第一段（fmt、net、go）从不含点，
// 而模块路径的第一段一定是域名。
func isStdlib(pkg string) bool {
	if pkg == "" {
		return false
	}
	first, _, _ := strings.Cut(pkg, "/")
	return !strings.Contains(first, ".")
}

func componentOf(file string) (component, bool) {
	for _, c := range components {
		for _, dir := range c.dirs {
			if under(file, dir) {
				return c, true
			}
		}
	}
	return component{}, false
}

// collectImports 读取仓库内所有 Go 文件（含测试文件）的导入声明。
//
// 测试文件同样计入：测试里绕开分层去直接依赖内层，一样是耦合。
func collectImports(t *testing.T, root string) ([]importRef, int) {
	t.Helper()

	var refs []importRef
	scanned := 0
	fileSet := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipDir(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		scanned++

		file, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("解析 %s 失败: %w", rel, err)
		}
		for _, spec := range file.Imports {
			pkg, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return fmt.Errorf("解析 %s 的导入路径失败: %w", rel, err)
			}
			refs = append(refs, importRef{
				pkg:  pkg,
				file: rel,
				line: fileSet.Position(spec.Pos()).Line,
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	return refs, scanned
}

// skipDir 跳过不该纳入扫描的目录。
func skipDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "testdata", ".git", "bin":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// repoRoot 回到仓库根目录。
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取工作目录失败: %v", err)
	}
	// 本文件位于 tools/arch
	return filepath.Dir(filepath.Dir(dir))
}

// --- 规则引擎自身的测试 ---
//
// 一个「永远返回 true」的检查器和没有检查器是一样的。下面这些用例保证规则
// 本身是有效的，而不是因为写错而放行了一切。

func TestUnder(t *testing.T) {
	cases := []struct {
		path   string
		prefix string
		want   bool
	}{
		{"internal/domain", "internal/domain", true},
		{"internal/domain/order", "internal/domain", true},
		{"internal/domain", "internal/domain/", true},
		{"internal/domainx", "internal/domain", false},
		{"internal/infra", "internal/domain", false},
		{"internal", "internal", true},
	}

	for _, tc := range cases {
		if got := under(tc.path, tc.prefix); got != tc.want {
			t.Errorf("under(%q, %q) = %v，期望 %v", tc.path, tc.prefix, got, tc.want)
		}
	}
}

func TestIsStdlib(t *testing.T) {
	for _, pkg := range []string{"fmt", "net/http", "go/parser", "encoding/json", "log/slog"} {
		if !isStdlib(pkg) {
			t.Errorf("%q 应判定为标准库", pkg)
		}
	}
	for _, pkg := range []string{"github.com/go-chi/chi/v5", "go.opentelemetry.io/otel", "gopkg.in/yaml.v3"} {
		if isStdlib(pkg) {
			t.Errorf("%q 不应判定为标准库", pkg)
		}
	}
	// 标准库与第三方都必须被识别，空串按非标准库处理
	if isStdlib("") {
		t.Error("空导入路径不应判定为标准库")
	}
}

func TestAllowedRejectsCrossLayerImports(t *testing.T) {
	domain := mustComponent(t, "domain")
	config := mustComponent(t, "config")
	container := mustComponent(t, "container")

	cases := []struct {
		name string
		c    component
		pkg  string
		want bool
	}{
		{"领域层可用标准库", domain, "fmt", true},
		{"领域层可用金额库", domain, "github.com/govalues/decimal", true},
		{"领域层内部互相依赖", domain, "github.com/NicoYazawa/crosspilot/internal/domain/catalog", true},
		{"领域层不得依赖基础设施", domain, "github.com/NicoYazawa/crosspilot/internal/infra/postgres", false},
		{"领域层不得依赖配置", domain, "github.com/NicoYazawa/crosspilot/internal/config", false},
		{"领域层不得依赖任意第三方", domain, "github.com/go-chi/chi/v5", false},
		{"配置层只有标准库", config, "fmt", true},
		{"配置层不得依赖观测层", config, "github.com/NicoYazawa/crosspilot/internal/observability", false},
		{"装配根可用任意第三方", container, "github.com/go-chi/chi/v5", true},
		{"装配根可依赖接口层", container, "github.com/NicoYazawa/crosspilot/internal/presentation/http", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allowed(tc.c, tc.pkg); got != tc.want {
				t.Errorf("allowed(%s, %q) = %v，期望 %v", tc.c.name, tc.pkg, got, tc.want)
			}
		})
	}
}

func TestComponentOf(t *testing.T) {
	cases := []struct {
		file string
		want string
	}{
		{"internal/domain/order/order.go", "domain"},
		{"internal/infra/postgres/pool.go", "infra"},
		{"internal/container/container.go", "container"},
		{"internal/presentation/http/router.go", "presentation"},
	}

	for _, tc := range cases {
		got, ok := componentOf(tc.file)
		if !ok {
			t.Errorf("%s 应当归属组件 %s，实际没有归属", tc.file, tc.want)
			continue
		}
		if got.name != tc.want {
			t.Errorf("%s 归属 %s，期望 %s", tc.file, got.name, tc.want)
		}
	}

	if _, ok := componentOf("cmd/server/main.go"); ok {
		t.Error("cmd/ 下的文件不在组件约束范围内")
	}
}

func mustComponent(t *testing.T, name string) component {
	t.Helper()
	for _, c := range components {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("组件 %s 未定义", name)
	return component{}
}
