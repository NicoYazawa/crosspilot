package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ptr 返回一个 float64 指针，用来构造可选的 weighted_min 字段。
func ptr(v float64) *float64 { return &v }

// approx 判断两个百分数是否足够接近（浮点误差范围内）。
func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestWeightedPercent(t *testing.T) {
	tests := []struct {
		name       string
		packages   []packageCoverage
		exclusions []exclusion
		wantPct    float64
		wantStmts  int64
	}{
		{
			name: "按语句数加权而不是对包取平均",
			// a 有 300 条语句全中，b 有 100 条全不中：加权 = 300/400 = 75%。
			// 若错误地对包覆盖率取平均则是 50%，这一格就是用来区分口径的。
			packages: []packageCoverage{
				{name: "m/internal/a", statements: 300, covered: 300},
				{name: "m/internal/b", statements: 100, covered: 0},
			},
			wantPct:   75,
			wantStmts: 400,
		},
		{
			name: "排除包不计入分子也不计入分母",
			packages: []packageCoverage{
				{name: "m/internal/domain/a", statements: 100, covered: 90},
				{name: "m/internal/domain/b", statements: 100, covered: 70},
				// 这个包 0% 且语句数很大，若没被排除会把加权拉到 80% 以下
				{name: "m/internal/infra/persistence/pg", statements: 1000, covered: 0},
			},
			exclusions: []exclusion{{Prefix: "m/internal/infra/persistence/", Reason: "集成测试覆盖"}},
			wantPct:    80,
			wantStmts:  200,
		},
		{
			name: "全部被排除时为零",
			packages: []packageCoverage{
				{name: "m/internal/infra/persistence/pg", statements: 500, covered: 100},
			},
			exclusions: []exclusion{{Prefix: "m/internal/infra/persistence/", Reason: "集成测试覆盖"}},
			wantPct:    0,
			wantStmts:  0,
		},
		{
			name:      "空报告为零",
			packages:  nil,
			wantPct:   0,
			wantStmts: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config{WeightedMin: ptr(90), Exclusions: tt.exclusions}
			gotPct, gotStmts := weightedPercent(tt.packages, cfg)
			if !approx(gotPct, tt.wantPct) {
				t.Errorf("加权覆盖率 = %v，期望 %v", gotPct, tt.wantPct)
			}
			if gotStmts != tt.wantStmts {
				t.Errorf("参与统计的语句数 = %d，期望 %d", gotStmts, tt.wantStmts)
			}
		})
	}
}

func TestMatchPrecedence(t *testing.T) {
	// 规则按书写顺序取先命中者：更宽的规则写在前面时，后面的窄规则够不着。
	cfg := &config{
		Rules: []rule{
			{Prefix: "m/internal/", Min: 50},
			{Prefix: "m/internal/domain", Min: 95},
		},
		Exclusions: []exclusion{{Prefix: "m/internal/infra/persistence/", Reason: "集成测试"}},
	}

	t.Run("规则取先命中者", func(t *testing.T) {
		got, ok := matchRule(cfg, "m/internal/domain/order")
		if !ok {
			t.Fatal("应当命中规则")
		}
		if got.Min != 50 {
			t.Errorf("命中的门槛 = %v，期望 50（前面的宽规则先命中）", got.Min)
		}
	})

	t.Run("豁免与规则可同时命中但豁免优先于规则的执行", func(t *testing.T) {
		name := "m/internal/infra/persistence/pg"
		if _, ok := matchExclusion(cfg, name); !ok {
			t.Fatal("应当命中豁免")
		}
		// 再加一条会失败、但被豁免覆盖的规则：check 必须跳过它而不是报错。
		cfgWithRule := &config{
			WeightedMin: ptr(0),
			Rules:       []rule{{Prefix: name, Min: 100}},
			Exclusions:  cfg.Exclusions,
		}
		err := check([]packageCoverage{{name: name, statements: 100, covered: 10}}, cfgWithRule)
		if err != nil {
			t.Errorf("被豁免的包不应导致失败，实际：%v", err)
		}
	})
}

func TestMatches(t *testing.T) {
	tests := []struct {
		prefix, name string
		want         bool
	}{
		{"m/internal/config", "m/internal/config", true},
		{"m/internal/config", "m/internal/config/sub", true},
		{"m/internal/config/", "m/internal/config", true}, // 结尾斜杠不影响匹配自身
		{"m/internal/config/", "m/internal/config/sub", true},
		{"m/internal/config", "m/internal/configx", false}, // 不能误配同前缀的兄弟
		{"m/internal/config", "m/internal/other", false},
	}
	for _, tt := range tests {
		if got := matches(tt.prefix, tt.name); got != tt.want {
			t.Errorf("matches(%q, %q) = %v，期望 %v", tt.prefix, tt.name, got, tt.want)
		}
	}
}

func TestShadowedRules(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *config
		wantCount int
		wantHas   string // 任一说明中出现即可
	}{
		{
			name: "规则被豁免完全覆盖",
			cfg: &config{
				Rules:      []rule{{Prefix: "m/internal/infra/persistence/pg", Min: 70}},
				Exclusions: []exclusion{{Prefix: "m/internal/infra/persistence/", Reason: "集成测试"}},
			},
			wantCount: 1,
			wantHas:   "persistence/pg",
		},
		{
			name: "规则被前面更宽的规则覆盖",
			cfg: &config{
				Rules: []rule{
					{Prefix: "m/internal/", Min: 50},
					{Prefix: "m/internal/domain", Min: 95},
				},
			},
			wantCount: 1,
			wantHas:   "先命中者",
		},
		{
			name: "窄规则写在宽规则之前是可达的",
			cfg: &config{
				Rules: []rule{
					{Prefix: "m/internal/domain/shipping", Min: 81},
					{Prefix: "m/internal/domain", Min: 95},
				},
			},
			wantCount: 0,
		},
		{
			name: "被豁免部分覆盖的规则仍然可达",
			cfg: &config{
				Rules:      []rule{{Prefix: "m/internal/infra", Min: 80}},
				Exclusions: []exclusion{{Prefix: "m/internal/infra/persistence/", Reason: "集成测试"}},
			},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shadowedRules(tt.cfg)
			if len(got) != tt.wantCount {
				t.Fatalf("死规则数量 = %d，期望 %d：%v", len(got), tt.wantCount, got)
			}
			if tt.wantHas != "" && !strings.Contains(strings.Join(got, "\n"), tt.wantHas) {
				t.Errorf("死规则说明里未找到 %q：%v", tt.wantHas, got)
			}
		})
	}
}

func TestCheckFailurePaths(t *testing.T) {
	tests := []struct {
		name        string
		packages    []packageCoverage
		cfg         *config
		wantErr     bool
		wantContain string
	}{
		{
			name:     "包低于分档阈值",
			packages: []packageCoverage{{name: "m/internal/domain/a", statements: 100, covered: 80}},
			cfg: &config{
				WeightedMin: ptr(50),
				Rules:       []rule{{Prefix: "m/internal/domain", Min: 95}},
			},
			wantErr:     true,
			wantContain: "低于门槛",
		},
		{
			name: "各包达标但加权低于门槛",
			packages: []packageCoverage{
				{name: "m/internal/a", statements: 100, covered: 95},
				{name: "m/internal/b", statements: 100, covered: 85},
			},
			cfg: &config{
				WeightedMin: ptr(95), // 加权 = 180/200 = 90% < 95%
				Rules: []rule{
					{Prefix: "m/internal/a", Min: 95},
					{Prefix: "m/internal/b", Min: 85},
				},
			},
			wantErr:     true,
			wantContain: "加权覆盖率",
		},
		{
			name:     "包未命中任何规则与豁免",
			packages: []packageCoverage{{name: "m/internal/newpkg", statements: 10, covered: 10}},
			cfg: &config{
				WeightedMin: ptr(0),
				Rules:       []rule{{Prefix: "m/internal/domain", Min: 95}},
			},
			wantErr:     true,
			wantContain: "既未命中规则也未豁免",
		},
		{
			name: "被豁免的包即使低于规则门槛也不失败",
			packages: []packageCoverage{
				{name: "m/internal/infra/persistence/pg", statements: 1000, covered: 100},
			},
			cfg: &config{
				WeightedMin: ptr(0),
				Rules:       []rule{{Prefix: "m/internal/infra/persistence/pg", Min: 70}},
				Exclusions:  []exclusion{{Prefix: "m/internal/infra/persistence/", Reason: "集成测试"}},
			},
			wantErr: false,
		},
		{
			name:     "全部达标时不报错",
			packages: []packageCoverage{{name: "m/internal/domain/a", statements: 100, covered: 96}},
			cfg: &config{
				WeightedMin: ptr(90),
				Rules:       []rule{{Prefix: "m/internal/domain", Min: 95}},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := check(tt.packages, tt.cfg)
			if tt.wantErr && err == nil {
				t.Fatal("期望失败，但没有报错")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("期望通过，但报错：%v", err)
			}
			if tt.wantContain != "" && !strings.Contains(err.Error(), tt.wantContain) {
				t.Errorf("错误信息里未找到 %q：%v", tt.wantContain, err)
			}
		})
	}
}

func TestLoadConfigValidation(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantErr     bool
		wantContain string
	}{
		{
			name: "合法配置",
			yaml: "version: 1\nweighted_min: 90\nrules:\n  - prefix: m/internal/\n    min: 80\n",
		},
		{
			name:        "缺少 weighted_min",
			yaml:        "version: 1\nrules:\n  - prefix: m/internal/\n    min: 80\n",
			wantErr:     true,
			wantContain: "weighted_min",
		},
		{
			name:        "weighted_min 越界",
			yaml:        "version: 1\nweighted_min: 150\nrules:\n  - prefix: m/internal/\n    min: 80\n",
			wantErr:     true,
			wantContain: "weighted_min",
		},
		{
			name:        "版本不为 1",
			yaml:        "version: 2\nweighted_min: 90\nrules:\n  - prefix: m/internal/\n    min: 80\n",
			wantErr:     true,
			wantContain: "版本",
		},
		{
			name:        "豁免没有理由",
			yaml:        "version: 1\nweighted_min: 90\nrules:\n  - prefix: m/internal/\n    min: 80\nexclusions:\n  - prefix: m/cmd/\n",
			wantErr:     true,
			wantContain: "理由",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "thresholds.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadConfig(path)
			if tt.wantErr && err == nil {
				t.Fatal("期望报错，但没有")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("期望通过，但报错：%v", err)
			}
			if tt.wantContain != "" && !strings.Contains(err.Error(), tt.wantContain) {
				t.Errorf("错误信息里未找到 %q：%v", tt.wantContain, err)
			}
		})
	}
}

// TestRealThresholdsHaveNoDeadRules 守住仓库里真实的门槛文件：
// 一旦有人再写出被豁免架空的规则，这里会先于 CI 报错。
func TestRealThresholdsHaveNoDeadRules(t *testing.T) {
	cfg, err := loadConfig(filepath.Join("..", "..", "coverage-thresholds.yaml"))
	if err != nil {
		t.Fatalf("读取真实门槛配置失败：%v", err)
	}
	if dead := shadowedRules(cfg); len(dead) > 0 {
		t.Fatalf("真实门槛配置里存在死规则：\n%s", strings.Join(dead, "\n"))
	}
}
