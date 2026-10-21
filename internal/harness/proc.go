package harness

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// proc 是一次 harness 进程：按行读它的 JSON 输出，停止时杀整个进程组（理由见 codex.Run）。
type proc struct {
	cmd    *exec.Cmd
	out    io.ReadCloser
	stderr bytes.Buffer
	stop   func() bool
}

// launch 启动 bin。env 非空时追加到 morula 自己的环境变量后面。
// PWD 总设成 dir：env 为空时 exec 自己会设，非空时不会，子进程拿到的是 morula 的 PWD。opencode 按 PWD 定会话的目录，
// 续跑时 PWD 和会话的目录对不上，进程答完不输出也不退出（2026-10-20 实测 opencode 1.19.3）。
func launch(ctx context.Context, bin string, args []string, dir, stdin string, env []string) (*proc, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 3 * time.Second
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
		if dir != "" {
			cmd.Env = append(cmd.Env, "PWD="+dir)
		}
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	p := &proc{cmd: cmd}
	cmd.Stderr = &p.stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p.out = out
	p.stop = context.AfterFunc(ctx, func() { time.AfterFunc(3*time.Second, func() { out.Close() }) })
	return p, nil
}

// lines 把每一行交给 f，直到输出关闭。
func (p *proc) lines(f func([]byte)) {
	sc := bufio.NewScanner(p.out)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		f(sc.Bytes())
	}
}

// wait 等进程退出，把退出码、自己报的失败和 stderr 末尾拼成错误。
func (p *proc) wait(ctx context.Context, name, failure string) error {
	defer p.stop()
	err := p.cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil && failure == "" {
		return nil
	}
	msg := failure
	if msg == "" {
		msg = strings.TrimSpace(tailLines(p.stderr.String(), 6))
	}
	if err != nil {
		msg = err.Error() + "：" + msg
	}
	return &runError{name + " 运行失败：" + msg}
}

type runError struct{ s string }

func (e *runError) Error() string { return e.s }

// schemaNote 给不支持按 JSON Schema 约束回复的 harness，把要求写进提示词末尾。
func schemaNote(schema string) string {
	if schema == "" {
		return ""
	}
	return "\n\n# 回复格式\n\n最后一条回复只输出一个 JSON 对象，不加代码围栏，不加别的文字。它要符合这个 JSON Schema：\n\n" + schema + "\n"
}

// noClaude 判断模型是不是 Claude 的，是则排除。
func noClaude(id string) bool {
	l := strings.ToLower(id)
	return !strings.Contains(l, "claude") && !strings.Contains(l, "anthropic")
}

// effortOrder 是各 harness 思考强度的通用顺序，用来给档位排序。
var effortOrder = []string{"none", "off", "minimal", "low", "medium", "high", "xhigh", "max"}
