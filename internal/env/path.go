// Package env 补齐从 Finder 启动时缺失的 PATH。
// Finder 打开的 app 只有 /usr/bin:/bin:/usr/sbin:/sbin，找不到 codex、opencode、pi。
package env

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FixPath 用交互式登录 shell 的 PATH 替换当前 PATH，再补上常见目录。
// 必须带 -i：很多人（包括 ~/.local/bin）是在 .zshrc 里改 PATH 的，只有交互式 shell 才会读 .zshrc。
func FixPath() {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// 用标记包住结果，避开 .zshrc 里其他输出。
	cmd := exec.CommandContext(ctx, shell, "-i", "-l", "-c", `printf '__MORULA_PATH__%s__MORULA_PATH__' "$PATH"`)
	cmd.Stdin = nil
	out, _ := cmd.Output()
	var parts []string
	if s := string(out); strings.Count(s, "__MORULA_PATH__") >= 2 {
		parts = strings.Split(strings.Split(s, "__MORULA_PATH__")[1], ":")
	}
	parts = append(parts, strings.Split(os.Getenv("PATH"), ":")...)

	home, _ := os.UserHomeDir()
	extra := []string{
		filepath.Join(home, ".local/bin"),
		filepath.Join(home, ".bun/bin"), // 插件用 Bun 运行
		"/opt/homebrew/bin",
		"/usr/local/bin",
	}
	// nvm 装的命令（例如 codex）在 ~/.nvm/versions/node/<版本>/bin
	if dirs, _ := filepath.Glob(filepath.Join(home, ".nvm/versions/node/*/bin")); len(dirs) > 0 {
		extra = append(extra, dirs...)
	}
	parts = append(parts, extra...)

	seen, list := map[string]bool{}, []string{}
	for _, p := range parts {
		if p != "" && !seen[p] {
			seen[p] = true
			list = append(list, p)
		}
	}
	os.Setenv("PATH", strings.Join(list, ":"))
}
