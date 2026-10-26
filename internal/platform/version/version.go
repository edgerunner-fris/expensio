// Package version 暴露构建信息，由链接期 -ldflags 注入。
package version

var (
	// Version 由 Makefile 的 -X 注入，未注入时为 dev。
	Version = "dev"
	// Commit 是构建所用的 git revision。
	Commit = "none"
	// BuildTime 是构建时间（UTC，RFC3339）。
	BuildTime = "unknown"
)

// Info 返回可直接写入 /healthz 或启动日志的构建信息。
func Info() map[string]string {
	return map[string]string{
		"version":   Version,
		"commit":    Commit,
		"buildTime": BuildTime,
	}
}
