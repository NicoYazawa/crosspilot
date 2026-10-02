// Command coverage-gate 按包检查测试覆盖率，低于门槛即以非零码退出。
//
// 门槛按包分档而不是全局一刀切：领域层是纯逻辑，要求接近全覆盖才有意义；
// 依赖真实数据库的执行器则做不到，硬套同一个数字只会逼人写空测试凑数。
//
// 每个出现在覆盖率报告里的包都必须命中一条规则或一条豁免。命中不了就报错，
// 这样新加的包不会因为「默认门槛」而悄悄放松要求。
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// config 是门槛文件的结构。
type config struct {
	Version int `yaml:"version"`
	// WeightedMin 是加权覆盖率门槛（百分数）。用指针而不是 float64，
	// 是为了区分「写了 0」和「没写」：漏写时应当报错，而不是静默地
	// 把加权门禁关掉。指针为 nil 即表示配置里没有这个键。
	WeightedMin *float64    `yaml:"weighted_min"`
	Rules       []rule      `yaml:"rules"`
	Exclusions  []exclusion `yaml:"exclusions"`
}

// rule 是一条覆盖率要求。
type rule struct {
	Prefix string  `yaml:"prefix"`
	Min    float64 `yaml:"min"`
	Reason string  `yaml:"reason"`
}

// exclusion 是一条豁免，必须写明理由。
type exclusion struct {
	Prefix string `yaml:"prefix"`
	Reason string `yaml:"reason"`
}

// packageCoverage 是一个包的聚合覆盖率。
type packageCoverage struct {
	name       string
	statements int64
	covered    int64
}

// percent 返回语句加权覆盖率。
func (p packageCoverage) percent() float64 {
	if p.statements == 0 {
		return 0
	}
	return float64(p.covered) / float64(p.statements) * 100
}

func main() {
	profile := flag.String("profile", "coverage.out", "覆盖率报告路径")
	thresholds := flag.String("thresholds", "coverage-thresholds.yaml", "门槛配置路径")
	flag.Parse()

	if err := run(*profile, *thresholds); err != nil {
		fmt.Fprintf(os.Stderr, "coverage-gate: %v\n", err)
		os.Exit(1)
	}
}

func run(profilePath, thresholdsPath string) error {
	cfg, err := loadConfig(thresholdsPath)
	if err != nil {
		return err
	}

	// 死规则只依赖配置本身，先于覆盖率报告检查：
	// 一条永远不会生效的规则等同于没有这条规则，必须在它被人当成
	// 「已经守住了某个包」之前就暴露出来。
	if dead := shadowedRules(cfg); len(dead) > 0 {
		return fmt.Errorf("门槛配置存在永远不会生效的规则：\n%s\n请删除该死规则，或调整前缀/顺序使其可达",
			strings.Join(dead, "\n"))
	}

	packages, err := loadProfile(profilePath)
	if err != nil {
		return err
	}
	if len(packages) == 0 {
		return fmt.Errorf("覆盖率报告 %s 里没有任何包，请确认测试确实跑过", profilePath)
	}

	report(packages, cfg)
	return check(packages, cfg)
}

func loadConfig(path string) (*config, error) {
	// 路径来自命令行参数，读取本地文件正是这个工具的职责
	raw, err := os.ReadFile(path) //nolint:gosec // G304：路径由调用方显式给出
	if err != nil {
		return nil, fmt.Errorf("读取门槛配置失败: %w", err)
	}

	var cfg config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("解析门槛配置失败: %w", err)
	}
	if cfg.Version != 1 {
		return nil, fmt.Errorf("门槛配置版本应为 1，实际 %d", cfg.Version)
	}
	if cfg.WeightedMin == nil {
		return nil, errors.New("门槛配置缺少 weighted_min（加权覆盖率门槛）")
	}
	if *cfg.WeightedMin < 0 || *cfg.WeightedMin > 100 {
		return nil, fmt.Errorf("weighted_min 的门槛 %v 不在 [0,100]", *cfg.WeightedMin)
	}
	if len(cfg.Rules) == 0 {
		return nil, errors.New("门槛配置里没有任何规则")
	}

	for i, r := range cfg.Rules {
		if r.Prefix == "" {
			return nil, fmt.Errorf("第 %d 条规则缺少 prefix", i+1)
		}
		if r.Min < 0 || r.Min > 100 {
			return nil, fmt.Errorf("规则 %s 的门槛 %v 不在 [0,100]", r.Prefix, r.Min)
		}
	}
	for i, e := range cfg.Exclusions {
		if e.Prefix == "" {
			return nil, fmt.Errorf("第 %d 条豁免缺少 prefix", i+1)
		}
		if strings.TrimSpace(e.Reason) == "" {
			return nil, fmt.Errorf("豁免 %s 必须写明理由", e.Prefix)
		}
	}
	return &cfg, nil
}

// loadProfile 把 coverage.out 按包聚合。
//
// 报告格式为固定的一组行：mode 行，随后每行
// <导入路径>/<文件>:<起>.<列>,<止>.<列> <语句数> <命中次数>。
func loadProfile(path string) ([]packageCoverage, error) {
	// 同 loadConfig：路径由调用方给出
	file, err := os.Open(path) //nolint:gosec // G304：路径由调用方显式给出
	if err != nil {
		return nil, fmt.Errorf("打开覆盖率报告失败: %w", err)
	}
	defer func() { _ = file.Close() }()

	type tally struct{ statements, covered int64 }
	tallies := make(map[string]*tally)

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}

		block, rest, found := strings.Cut(line, " ")
		if !found {
			return nil, fmt.Errorf("无法解析覆盖率行: %q", line)
		}
		name := packageOf(block)

		fields := strings.Fields(rest)
		if len(fields) != 2 {
			return nil, fmt.Errorf("覆盖率行 %q 的计数字段应为 2 个，实际 %d 个", line, len(fields))
		}
		statements, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("解析语句数失败（%q）: %w", line, err)
		}
		hits, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("解析命中次数失败（%q）: %w", line, err)
		}

		entry, ok := tallies[name]
		if !ok {
			entry = &tally{}
			tallies[name] = entry
		}
		entry.statements += statements
		if hits > 0 {
			entry.covered += statements
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取覆盖率报告失败: %w", err)
	}

	out := make([]packageCoverage, 0, len(tallies))
	for name, t := range tallies {
		out = append(out, packageCoverage{name: name, statements: t.statements, covered: t.covered})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// packageOf 从 `<导入路径>/<文件>:<位置>` 里取出导入路径。
//
// 导入路径最后一段可能含点（如 v2 后缀的模块），因此从冒号处往回找第一个
// 斜杠，而不是从末尾数点。
func packageOf(block string) string {
	colon := strings.Index(block, ":")
	if colon < 0 {
		return block
	}
	slash := strings.LastIndex(block[:colon], "/")
	if slash < 0 {
		return block
	}
	return block[:slash]
}

func report(packages []packageCoverage, cfg *config) {
	weighted, statements := weightedPercent(packages, cfg)
	fmt.Printf("加权覆盖率（非排除包，按语句数加权）：%.1f%%（门槛 %.1f%%，统计 %d 条语句）\n",
		weighted, *cfg.WeightedMin, statements)

	fmt.Println("包覆盖率：")
	for _, p := range packages {
		status := "??"
		if _, ok := matchExclusion(cfg, p.name); ok {
			status = "--"
		} else if r, ok := matchRule(cfg, p.name); ok {
			status = "  "
			if p.percent() < r.Min {
				status = "!!"
			}
		}
		fmt.Printf("%s %6.1f%%  %s\n", status, p.percent(), shortName(p.name))
	}
	fmt.Println("（!! 低于门槛，-- 已豁免，?? 未纳入管理）")
}

func check(packages []packageCoverage, cfg *config) error {
	var failures, unmanaged []string

	for _, p := range packages {
		// 豁免优先于规则：豁免是更明确的取舍，不该被前置的宽泛规则盖掉
		if _, excluded := matchExclusion(cfg, p.name); excluded {
			continue
		}

		rule, ok := matchRule(cfg, p.name)
		if !ok {
			unmanaged = append(unmanaged, p.name)
			continue
		}
		if got := p.percent(); got < rule.Min {
			failures = append(failures, fmt.Sprintf("  %s：%.1f%%，低于门槛 %.1f%%",
				shortName(p.name), got, rule.Min))
		}
	}

	var errs []error
	if got, statements := weightedPercent(packages, cfg); got < *cfg.WeightedMin {
		errs = append(errs, fmt.Errorf(
			"加权覆盖率 %.1f%%，低于门槛 %.1f%%（仅统计非排除包，共 %d 条语句）",
			got, *cfg.WeightedMin, statements))
	}
	if len(unmanaged) > 0 {
		errs = append(errs, fmt.Errorf(
			"以下包既未命中规则也未豁免，请在 coverage-thresholds.yaml 里明确其归属：\n  %s",
			strings.Join(unmanaged, "\n  ")))
	}
	if len(failures) > 0 {
		errs = append(errs, fmt.Errorf("以下包未达门槛：\n%s", strings.Join(failures, "\n")))
	}
	return errors.Join(errs...)
}

// weightedPercent 计算非排除包的加权覆盖率，并返回参与统计的语句总数。
//
// 公式（与开发计划 §4.2 一致）：Σ(包语句数 × 包覆盖率) / Σ(包语句数)。
//
// 口径说明：计划里说的「行数」在本工具里取覆盖率报告给出的「语句数」——
// go test -coverprofile 的每条记录就是「<起> <止> <语句数> <命中次数>」，
// 语句数即该覆盖块的语句条数，也是 go tool cover 报告的计数单位。同一份
// profile 同时给出总语句数与命中语句数，无需另行数源码行，且天然与
// percent() 的口径一致。因此上式等价于 Σ命中语句数 / Σ语句数。
//
// 仅统计非排除包：被 exclusions 命中的包不计入分子，也不计入分母。
func weightedPercent(packages []packageCoverage, cfg *config) (float64, int64) {
	var statements, covered int64
	for _, p := range packages {
		if _, excluded := matchExclusion(cfg, p.name); excluded {
			continue
		}
		statements += p.statements
		covered += p.covered
	}
	if statements == 0 {
		return 0, 0
	}
	return float64(covered) / float64(statements) * 100, statements
}

// shadowedRules 找出永远不可能命中的规则，返回可读的中文说明。
//
// 有死规则和没写这条规则是一回事，但前者会让人误以为某个包已经被守住，
// 因此必须显式报出来。有两类：
//
//  1. 被某条豁免完全覆盖。exclusions 优先于 rules，落在豁免前缀之下的
//     规则永远不会被 check() 用到——persistence/pg 的 min:70 就是这样
//     被更宽的 persistence/ 豁免架空的。
//  2. 被排在自己前面的、范围更大的规则完全覆盖。规则按书写顺序取先命中者，
//     后写的窄规则如果整体落在前面的宽规则之下，也永远命不中。
//
// 判定只看前缀包含关系（matches），与覆盖率报告里实际出现哪些包无关，
// 这样即使某个包这次没被测到，死规则也不会因此「看起来是活的」。
func shadowedRules(cfg *config) []string {
	var out []string
	for i, r := range cfg.Rules {
		if e, covered := coveredByExclusion(cfg, r.Prefix); covered {
			out = append(out, fmt.Sprintf(
				"  规则 %s 被豁免 %s 完全覆盖：豁免优先，该规则永远不会生效", r.Prefix, e))
			continue
		}
		for j := 0; j < i; j++ {
			if matches(cfg.Rules[j].Prefix, r.Prefix) {
				out = append(out, fmt.Sprintf(
					"  规则 %s 被前面的规则 %s 完全覆盖：先命中者生效，该规则永远不会生效",
					r.Prefix, cfg.Rules[j].Prefix))
				break
			}
		}
	}
	return out
}

// coveredByExclusion 判断某个前缀是否整体落在一条豁免之下。
func coveredByExclusion(cfg *config, prefix string) (string, bool) {
	for _, e := range cfg.Exclusions {
		if matches(e.Prefix, prefix) {
			return e.Prefix, true
		}
	}
	return "", false
}

func matchRule(cfg *config, name string) (rule, bool) {
	for _, r := range cfg.Rules {
		if matches(r.Prefix, name) {
			return r, true
		}
	}
	return rule{}, false
}

func matchExclusion(cfg *config, name string) (exclusion, bool) {
	for _, e := range cfg.Exclusions {
		if matches(e.Prefix, name) {
			return e, true
		}
	}
	return exclusion{}, false
}

// matches 判断包名是否落在某个前缀下。
//
// 前缀同时锚定「这个包本身」和「它的子树」，因此结尾写不写斜杠都行：
// internal/infra 既匹配 internal/infra，也匹配 internal/infra/redis。
// 少了这条，写 internal/config 会漏掉它自己，写 internal/config/ 又会漏掉它自己。
func matches(prefix, name string) bool {
	prefix = strings.TrimSuffix(strings.TrimSpace(prefix), "/")
	return name == prefix || strings.HasPrefix(name, prefix+"/")
}

// shortName 去掉模块前缀，让报告里的包名保持可读。
func shortName(name string) string {
	const marker = "/crosspilot/"
	if idx := strings.Index(name, marker); idx >= 0 {
		return name[idx+len(marker):]
	}
	return name
}
