package http

import "runtime/debug"

// BuildInfo 是当前二进制的构建标识。
//
// 为什么从 build info 读而不是注入 ldflags：ldflags 需要在 Dockerfile、Taskfile
// 与本地构建三处各维护一次，任何一处漏掉就会得到「未知版本」的镜像，而那种
// 漏掉不会让构建失败，只会让线上排查时手上少一个关键事实。Go 1.18 起构建信息
// 里本来就带着 vcs.revision，直接读它等于零配置拿到同一个答案。
type BuildInfo struct {
	// Version 是模块版本；未使用模块代理构建时为 "(devel)" 或空。
	Version string `json:"version"`
	// Commit 是 vcs.revision（完整哈希）。
	Commit string `json:"commit"`
	// BuiltAt 是 vcs.time，RFC3339。
	BuiltAt string `json:"built_at,omitempty"`
	// Modified 表示构建时工作区有未提交改动。
	//
	// 它必须暴露出来：一个 commit 号相同的二进制，在「干净构建」与「带着
	// 未提交改动构建」两种情况下行为可能完全不同，只看 commit 会让人误以为
	// 线上跑的就是仓库里那份代码。
	Modified bool `json:"modified"`
}

// unknownVersion 表示构建信息不可用。
const unknownVersion = "unknown"

// ReadBuildInfo 读取当前二进制的构建信息。
//
// 读不到 vcs 信息是正常情况（例如用 `go build` 在非 git 目录构建、或
// 上游用 `-buildvcs=false`），此时返回 unknown 而不是报错：一个健康检查
// 端点的职责是报告状态，不应该因为拿不到版本号而失败。
func ReadBuildInfo() BuildInfo {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return BuildInfo{Version: unknownVersion, Commit: unknownVersion}
	}

	out := BuildInfo{Version: info.Main.Version}
	if out.Version == "" || out.Version == "(devel)" {
		out.Version = unknownVersion
	}
	out.Commit = unknownVersion

	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			out.Commit = s.Value
			// 完整 40 位哈希在日志里读起来太费劲，短哈希足够定位。
			if len(out.Commit) > shortCommitLen {
				out.Commit = out.Commit[:shortCommitLen]
			}
		case "vcs.time":
			out.BuiltAt = s.Value
		case "vcs.modified":
			out.Modified = s.Value == "true"
		}
	}
	return out
}

// shortCommitLen 是暴露给外部的提交哈希长度。
const shortCommitLen = 12
