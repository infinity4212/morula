package work

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"morula/internal/chat"
)

// 助理自己做的小事（打开应用、去掉隔离标记、建一个目录）：助理在只读沙箱里，用 run_command 提出命令，
// 由 morula 执行，结果交回助理核对。要不要先等用户批准看审批档位和命令本身（risk.go）。改代码、分几步的活仍然 dispatch。

// commandTimeout 是一条命令最多跑多久。
const commandTimeout = 2 * time.Minute

type commandArgs struct {
	Command string `json:"command" jsonschema:"要执行的 shell 命令，morula 用 /bin/zsh -lc 执行。一两条命令能做完的小事才用这个"`
	Dir     string `json:"dir,omitempty" jsonschema:"在哪个目录执行；空串时在用户主目录执行"`
	Reason  string `json:"reason" jsonschema:"为什么执行、执行后会改变什么，一句话，显示在审批卡片上"`
}

// askCommand 按审批档位决定直接执行还是先发审批卡片（见 risk.go），返回给助理的结果和是否出错。
func (e *Engine) askCommand(ctx context.Context, in commandArgs) (string, bool) {
	cmd := strings.TrimSpace(in.Command)
	if cmd == "" || strings.TrimSpace(in.Reason) == "" {
		return "command 和 reason 都不能为空，什么都没执行", true
	}
	dir := strings.TrimSpace(in.Dir)
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "目录 " + dir + " 不存在，什么都没执行", true
	}
	risk, tier := riskOf(cmd), e.Tier()
	if risk == "" && (tier == TierAll || tier == TierList && readOnly(cmd)) {
		code, out := runShell(ctx, dir, cmd)
		return fmt.Sprintf("按用户的审批档位直接执行了，没有问用户（目录 %s）。退出码 %d。输出：\n%s\n退出码 0 不等于做成了：用只读命令核对结果（例如进程在不在、文件改没改），"+
			"核对不了的写未验证。", dir, code, orDefault(out, "（没有输出）")), false
	}
	input := map[string]string{"命令": cmd, "目录": dir}
	display := "助理想执行命令"
	if risk != "" {
		input["为什么要问"] = risk + "，任何审批档位都要你批准"
		display = "助理想执行高危命令"
	}
	ans, err := e.Chat.Wait(ctx, chat.Event{
		"type": "permission", "display": display, "description": in.Reason,
		"input": input, "free": true,
	})
	switch {
	case err != nil:
		return "卡片已失效，用户没有回答，什么都没执行", true
	case strings.TrimSpace(ans.Text) != "":
		return "用户没有批准，写了：" + strings.TrimSpace(ans.Text) + "。什么都没执行，按用户的意思办", false
	case !ans.Allow:
		return "用户拒绝了，什么都没执行。不要换个写法执行同样的操作；要做别的先问用户", false
	}
	code, out := runShell(ctx, dir, cmd)
	return fmt.Sprintf("已执行（目录 %s）。退出码 %d。输出：\n%s\n退出码 0 不等于做成了：用只读命令核对结果（例如进程在不在、文件改没改），"+
		"核对不了的写未验证。", dir, code, orDefault(out, "（没有输出）")), false
}

// runShell 执行一条命令，返回退出码和合并后的输出（只留最后 4000 个字）。超时或取消时杀掉整个进程组。
func runShell(ctx context.Context, dir, command string) (int, string) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, "/bin/zsh", "-lc", command)
	c.Dir = dir
	c.Stdin = nil
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 3 * time.Second
	err := c.Run()
	code := 0
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		code = -1
		out.WriteString("\n" + err.Error())
	}
	if ctx.Err() == context.DeadlineExceeded {
		out.WriteString(fmt.Sprintf("\n（超过 %s 没跑完，已停掉）", commandTimeout))
	}
	s := []rune(strings.TrimSpace(out.String()))
	if len(s) > 4000 {
		s = append([]rune("…（前面已省略）\n"), s[len(s)-4000:]...)
	}
	return code, string(s)
}
