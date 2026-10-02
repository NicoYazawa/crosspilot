// mutation-tester 是 H2 验收标准的自研实现。
//
// 原理：把包复制到临时目录，对源码做单点操作符变异，运行测试；测试失败→杀死，测试通过→存活。
// 杀死率 = 杀死数 / 总数。
//
// 变异操作符：
//
//	<=  →  <     >=  →  >     ==  →  !=     !=  →  ==
//	&&  →  ||    ||  →  &&    +   →  -      -   →  +
//
// 用法：go run ./scripts/mutation --pkg ./internal/domain/catalog --limit 100
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	pkgFlag      = flag.String("pkg", "", "要测试的包路径（如 ./internal/domain/catalog）")
	limitFlag    = flag.Int("limit", 0, "最多变异次数（0=不限）")
	workersFlag  = flag.Int("workers", 4, "并发 worker 数")
	verboseFlag  = flag.Bool("v", false, "详细输出")
	globalThresh = 0.70
	domainThresh = 0.80
)

type mutPoint struct {
	file    string
	line    int
	origTok string
	newTok  string
	desc    string
}

type result struct {
	pt     mutPoint
	killed bool
	output string
}

// 操作符变异映射
var opMutations = [][2]string{
	{"<=", "<"},
	{">=", ">"},
	{"==", "!="},
	{"!=", "=="},
	{"&&", "||"},
	{"||", "&&"},
	{"+", "-"},
	{"-", "+"},
}

func main() {
	os.Exit(run())
}

// run 承载全部逻辑并返回进程退出码。
//
// 不直接在 main 里 os.Exit 是为了让 ctx 的 defer cancel() 能真正执行：
// os.Exit 会立即终止进程，绕过所有 defer，超时 context 永远不会被释放。
// 返回退出码由 main 统一 os.Exit，语义不变而 defer 得以正常收尾。
func run() int {
	flag.Parse()
	if *pkgFlag == "" {
		fmt.Println("必须指定 --pkg")
		flag.Usage()
		return 1
	}

	threshold := globalThresh
	if strings.Contains(*pkgFlag, "/domain/") {
		threshold = domainThresh
	}

	pkgAbs, err := filepath.Abs(*pkgFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "filepath.Abs: %v\n", err)
		return 1
	}
	fmt.Printf("mutation tester: pkg=%s threshold=%.0f%% workers=%d\n", pkgAbs, threshold*100, *workersFlag)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// 找出所有 .go 源文件（排除 _test.go）
	files, err := findGoFiles(pkgAbs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "find files: %v\n", err)
		return 1
	}
	if len(files) == 0 {
		fmt.Println("没有找到源文件")
		return 1
	}
	fmt.Printf("找到 %d 个源文件\n", len(files))

	// 收集所有变异点
	points := collectMutationPoints(files)
	if len(points) == 0 {
		fmt.Println("没有找到变异点")
		return 0
	}
	if *limitFlag > 0 && len(points) > *limitFlag {
		points = points[:*limitFlag]
	}
	fmt.Printf("找到 %d 个变异点\n", len(points))

	// 并发执行
	var total, killed int64
	var mu sync.Mutex
	var results []result
	var wg sync.WaitGroup
	sem := make(chan struct{}, *workersFlag)

	for i, pt := range points {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, pt mutPoint) {
			defer wg.Done()
			defer func() { <-sem }()

			r := runMutation(ctx, pt, pkgAbs)
			atomic.AddInt64(&total, 1)
			if r.killed {
				atomic.AddInt64(&killed, 1)
			}

			mu.Lock()
			results = append(results, r)
			if *verboseFlag || r.killed {
				status := "KILLED"
				if !r.killed {
					status = "SURVIVED"
				}
				fmt.Printf("[%3d/%d] %s  %s:%d  %s\n", idx+1, len(points), status, filepath.Base(pt.file), pt.line, r.output)
			}
			mu.Unlock()
		}(i, pt)
	}
	wg.Wait()

	killRate := float64(killed) / float64(total)
	pass := killRate >= threshold

	fmt.Println()
	fmt.Println("=== 结果 ===")
	fmt.Printf("总数:   %d\n", total)
	fmt.Printf("杀死:   %d\n", killed)
	fmt.Printf("存活:   %d\n", total-killed)
	fmt.Printf("杀死率: %.1f%%\n", killRate*100)
	fmt.Printf("阈值:   %.0f%%\n", threshold*100)
	fmt.Printf("结果:   %s\n", map[bool]string{true: "PASS", false: "FAIL"}[pass])

	if !pass {
		fmt.Println()
		fmt.Println("=== 存活的变异（需人工审查）===")
		for _, r := range results {
			if !r.killed {
				fmt.Printf("  %s:%d  [%s]  %s\n", filepath.Base(r.pt.file), r.pt.line, r.pt.desc, r.output)
			}
		}
		return 1
	}
	return 0
}

func findGoFiles(pkgPath string) ([]string, error) {
	var files []string
	err := filepath.Walk(pkgPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// 遍历出错意味着有文件/目录读不到，此时 info 为 nil。
			// 不能吞掉错误返回 nil 假装成功：那会让上层把「没找到源文件」
			// 误报成路径选错，而真正的原因（权限、IO）被掩盖。
			return err
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".muttmp") || strings.HasSuffix(path, ".mutbak") {
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

func collectMutationPoints(files []string) []mutPoint {
	var points []mutPoint
	for _, f := range files {
		pts := findInFile(f)
		points = append(points, pts...)
	}
	return points
}

func findInFile(file string) []mutPoint {
	var out []mutPoint

	if strings.Contains(file, "vendor") || strings.Contains(file, "generated") {
		return out
	}

	// 路径来自本脚本 --pkg 参数枚举出的源码文件，读取它们正是变异脚本的职责
	data, err := os.ReadFile(file) //nolint:gosec // G304：file 来自 --pkg 目录下枚举的 .go 源文件
	if err != nil {
		return out
	}
	lines := bytes.Split(data, []byte{'\n'})

	for lineIdx, line := range lines {
		strLine := string(line)

		// 跳过注释行
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 && (trimmed[0] == '/' || trimmed[0] == '*') {
			continue
		}

		// 跳过 import/package 行
		if bytes.HasPrefix(trimmed, []byte("import")) || bytes.HasPrefix(trimmed, []byte("package")) {
			continue
		}

		// 跳过包含在字符串字面量中的情况（简单启发式）
		// 如果行中有 `"` 说明可能有字符串字面量，需要更精确判断
		hasString := bytes.Contains(line, []byte{'"'})

		for _, m := range opMutations {
			orig := m[0]
			if strings.Contains(strLine, orig) {
				// 简单检查：如果 orig 在注释后，跳过
				if ci := strings.Index(strLine, "//"); ci >= 0 && strings.Index(strLine, orig) > ci {
					continue
				}
				// 如果有字符串字面量且 orig 在字符串内，跳过
				if hasString && isInsideStringLiteral(strLine, orig) {
					continue
				}
				out = append(out, mutPoint{
					file:    file,
					line:    lineIdx + 1,
					origTok: orig,
					newTok:  m[1],
					desc:    orig + " → " + m[1],
				})
			}
		}
	}

	return out
}

// isInsideStringLiteral 简单判断 opTok 是否在字符串字面量内部
func isInsideStringLiteral(line, op string) bool {
	// 找到 op 的位置，看它是否被匹配的单引号或双引号包围
	idx := strings.Index(line, op)
	if idx < 0 {
		return false
	}
	// 简单策略：如果 op 前面有奇数个未转义的引号，认为在字符串内
	// 这是一个很粗略的启发式
	before := line[:idx]
	inString := false
	for i := 0; i < len(before); i++ {
		if before[i] == '"' && (i == 0 || before[i-1] != '\\') {
			inString = !inString
		}
	}
	return inString
}

// runMutation 把包复制到临时目录，做一次变异，运行测试
func runMutation(ctx context.Context, pt mutPoint, pkgAbs string) result {
	r := result{pt: pt}

	// 1. 复制整个包到临时目录
	tmpDir, err := copyPkgToTempDir(pkgAbs)
	if err != nil {
		r.output = "copy to temp: " + err.Error()
		return r
	}
	// 临时目录清理失败不影响本次变异结论（测试已在其中跑完），
	// 且此刻无补救手段，故显式忽略返回值。
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// 2. 在临时文件上做变异
	mutFile := filepath.Join(tmpDir, filepath.Base(filepath.Dir(pt.file)), filepath.Base(pt.file))
	relFile := pt.file[len(pkgAbs)+1:]

	// 找到正确的临时文件路径
	tmpFiles, err := listFiles(tmpDir)
	if err != nil {
		// 遍历临时目录失败会导致定位错文件，从而变异错位置，
		// 因此这里不能继续，直接判定本次变异无效。
		r.output = "列出临时文件失败: " + err.Error()
		return r
	}
	for _, f := range tmpFiles {
		rel, _ := filepath.Rel(tmpDir, f)
		if filepath.ToSlash(rel) == relFile || strings.HasSuffix(f, filepath.Base(pt.file)) {
			// 尝试匹配
			origAbs, _ := filepath.Rel(tmpDir, pt.file)
			if filepath.ToSlash(rel) == filepath.ToSlash(origAbs) {
				mutFile = f
				break
			}
			if filepath.Base(f) == filepath.Base(pt.file) {
				mutFile = f
			}
		}
	}

	if err := applyMutation(mutFile, pt.line, pt.origTok, pt.newTok); err != nil {
		r.output = "apply mutation: " + err.Error()
		return r
	}

	// 3. 运行 go test
	// 参数 tmpDir 是本脚本用 os.MkdirTemp 新建的临时目录，不是外部可控输入，
	// 也无法收敛成常量（每次跑都要换目录）；命令本身固定为 "go test"。
	cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-timeout=30s", tmpDir) //nolint:gosec // G204：参数为脚本自建的临时目录
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Dir = tmpDir

	err = cmd.Run()
	if err != nil {
		r.killed = true
		r.output = "test FAILED"
	} else {
		r.killed = false
		r.output = "test PASSED"
	}
	_ = stderr

	return r
}

func copyPkgToTempDir(pkgAbs string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "mut-*")
	if err != nil {
		return "", err
	}

	return tmpDir, copyDir(pkgAbs, tmpDir)
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		dstPath := filepath.Join(dst, rel)

		if info.IsDir() {
			// 临时目录仅供本机变异测试读写，无需对同组/其他用户开放
			return os.MkdirAll(dstPath, 0o750)
		}

		if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".muttmp") || strings.HasSuffix(path, ".mutbak") {
			return nil
		}

		// 源文件来自 --pkg 指定的包目录，逐文件复制是变异流程的第一步
		data, err := os.ReadFile(path) //nolint:gosec // G304：path 来自 --pkg 目录下的源码
		if err != nil {
			return err
		}
		// 临时副本只用于跑测试，不需要他人可读
		// G703 是误报：dstPath 由 filepath.Join(临时目录, 相对路径) 拼出，仍落在临时目录内
		return os.WriteFile(dstPath, data, 0o600) //nolint:gosec // G703：目标在自建临时目录内，无路径穿越
	})
}

func listFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	// 遍历错误必须上抛：拿不到完整的文件列表就可能把变异打到错误的位置
	return files, err
}

func applyMutation(file string, line int, origTok, newTok string) error {
	// file 是上一步在临时目录里定位到的源码副本路径，非外部输入
	data, err := os.ReadFile(file) //nolint:gosec // G304：file 为临时目录内的源码副本
	if err != nil {
		return err
	}
	lines := bytes.Split(data, []byte{'\n'})
	if line < 1 || line > len(lines) {
		return fmt.Errorf("行号 %d 越界", line)
	}

	origLine := string(lines[line-1])
	mutLine := strings.Replace(origLine, origTok, newTok, 1)
	if mutLine == origLine {
		return fmt.Errorf("替换无变化")
	}

	lines[line-1] = []byte(mutLine)
	// 写的是临时副本，仅本机测试使用
	// G703 是误报：file 是上一步在临时目录内定位到的副本路径，写回同一路径
	return os.WriteFile(file, bytes.Join(lines, []byte{'\n'}), 0o600) //nolint:gosec // G703：回写临时目录内的副本，无路径穿越
}

// 引用未使用的 runtime 以避免编译错误
var _ = runtime.NumCPU
