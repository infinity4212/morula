package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// codex 驱动本机的 Codex CLI（codex exec --json），用它自己登录的账号和模型。
//
// 不改 ~/.codex/config.toml：模型、推理强度、沙箱都用命令行参数只对这次调用生效。
// 本机全局配置可能是 danger-full-access，所以每次都显式给出沙箱，不沿用全局值。
type codex struct{}

func init() { register(codex{}) }

func (codex) Name() string { return "codex" }

func (codex) usesTools() bool { return true }

func (codex) resumes() bool { return true }

func (codex) compacts() bool { return true }

func (codex) profile() Profile {
	return Profile{Bin: "codex", Sessions: "~/.codex/sessions", Sandbox: "codex 自带隔离：只读时不能改文件；可写时只能改项目文件夹里的文件", Tools: true, Compact: true, Resume: true, Images: true}
}

// toolArgs 用 -c 只对这次调用挂上 MCP 服务器。工具设成免批准，否则 approval_policy=never 时调用会被拒；
// 要用户拍板的工具由 morula 自己发卡片。
func toolArgs(t *Tools) []string {
	if t == nil {
		return nil
	}
	q := func(v string) string { b, _ := json.Marshal(v); return string(b) } // TOML 基本字符串与 JSON 字符串转义相同
	list := func(vs []string) string {
		out := make([]string, len(vs))
		for i, v := range vs {
			out[i] = q(v)
		}
		return "[" + strings.Join(out, ",") + "]"
	}
	var env []string
	for k, v := range t.Env {
		env = append(env, k+"="+q(v))
	}
	sort.Strings(env)
	const k = "mcp_servers.morula_assistant."
	var out []string
	if t.URL != "" {
		out = []string{"-c", k + "url=" + q(t.URL)}
	} else {
		out = []string{
			"-c", k + "command=" + q(t.Command),
			"-c", k + "args=" + list(t.Args),
			"-c", k + "env={" + strings.Join(env, ",") + "}",
		}
	}
	out = append(out,
		"-c", k+"enabled_tools="+list(t.Names),
		"-c", k+`default_tools_approval_mode="approve"`,
		"-c", k+"startup_timeout_sec=20",
	)
	if t.Timeout > 0 {
		out = append(out, "-c", k+"tool_timeout_sec="+strconv.Itoa(t.Timeout))
	}
	return out
}

func codexHome() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// Detect 只要求装了 codex 命令。codex 没登录时它自己的模型不能用，插件模型和 API key 来源照常能用。
func (codex) Detect() error {
	if _, err := exec.LookPath("codex"); err != nil {
		return errors.New("没找到 codex 命令")
	}
	return nil
}

// CodexLoggedIn 看 codex 登录过没有（有 auth.json）。
func CodexLoggedIn() bool {
	_, err := os.Stat(filepath.Join(codexHome(), "auth.json"))
	return err == nil
}

// Models 读 codex 自己缓存的模型列表，只列它在选择器里显示的那些；后面接上插件模型。
// codex 没登录时只列插件模型。
func (codex) Models() []Model {
	var gw []Model
	for _, m := range pluginModels() {
		if m.Via != chatgptVia { // ChatGPT 订阅 codex 自己就能用，不绕网关
			gw = append(gw, m)
		}
	}
	if !CodexLoggedIn() {
		return gw
	}
	var own []Model
	for _, m := range codexModels() {
		if Plugins.Off == nil || !Plugins.Off("codex", m.ID) {
			own = append(own, m)
		}
	}
	return append(own, gw...)
}

// CodexModels 是 codex 订阅自己的全部模型，不管用户勾没勾选。「模型来源」页用它列 codex 一行。
func CodexModels() []Model { return codexModels() }

// codexModels 是 codex 自己的模型。
func codexModels() []Model {
	b, err := os.ReadFile(filepath.Join(codexHome(), "models_cache.json"))
	if err != nil {
		return []Model{}
	}
	var c struct {
		Models []struct {
			Slug       string `json:"slug"`
			Display    string `json:"display_name"`
			Visibility string `json:"visibility"`
			Default    string `json:"default_reasoning_level"`
			Window     int    `json:"context_window"`
			Levels     []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if json.Unmarshal(b, &c) != nil {
		return []Model{}
	}
	out := []Model{}
	for _, m := range c.Models {
		if m.Visibility != "list" {
			continue
		}
		md := Model{ID: m.Slug, Name: m.Display, Default: m.Default, Efforts: []string{}, Context: m.Window}
		for _, l := range m.Levels {
			md.Efforts = append(md.Efforts, l.Effort)
		}
		out = append(out, md)
	}
	return out
}

// CodexModel 判断 id 是不是 codex 自己的模型。
func CodexModel(id string) bool {
	for _, m := range codexModels() {
		if m.ID == id {
			return true
		}
	}
	return false
}

func (codex) Run(ctx context.Context, s Spec, on func(Event)) (Result, error) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return Result{}, errors.New("没找到 codex 命令")
	}
	args := []string{"exec"}
	if s.Resume != "" {
		args = append(args, "resume")
	}
	args = append(args, "--json")
	// -i 后面能跟多个值，每个后面紧跟一个 -- 开头的参数，免得把末尾的 - 当成图片
	for _, im := range s.Images {
		args = append(args, "-i", im)
	}
	args = append(args, "--skip-git-repo-check")
	// 花名册里写成 codex/<模型> 的是 ChatGPT 订阅经网关的写法，codex 直接用自己的模型
	if m, ok := strings.CutPrefix(s.Model, chatgptVia+"/"); ok && CodexModel(m) {
		s.Model = m
	}
	route, routeEnv, viaPlugin := codexRoute(ctx, s.Model)
	args = append(args, route...)
	if s.Model != "" {
		args = append(args, "-m", s.Model)
	}
	if s.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+s.Effort)
	}
	if s.Ephemeral {
		args = append(args, "--ephemeral")
	}
	// 关掉 codex 自带的多 agent：派活由 morula 管，模型看到 codex 的 send_message 会拿 morula 的 agent 名去调，报错。
	// codex 0.163.0 关掉后仍往会话里写多 agent 的说明，工具有没有去掉没法从记录里看出来；提示词里另外写了不用这些工具。
	args = append(args, "-c", "features.multi_agent=false")
	if s.CompactAt > 0 { // 只对这次调用生效，不改用户的 config.toml
		args = append(args, "-c", "model_auto_compact_token_limit="+strconv.Itoa(s.CompactAt))
	}
	args = append(args, toolArgs(s.Tools)...)
	// 沙箱：resume 不接受 -s 和 --approve-for-me，用 -c 覆盖。
	switch {
	case s.Access == ReadOnly:
		args = append(args, "-c", `sandbox_mode="read-only"`, "-c", `approval_policy="never"`)
	case s.Access == Full:
		args = append(args, "-c", `sandbox_mode="danger-full-access"`, "-c", `approval_policy="never"`)
	case s.Resume == "":
		args = append(args, "--approve-for-me")
	default:
		args = append(args, "-c", `sandbox_mode="workspace-write"`, "-c", `approval_policy="never"`)
	}
	if s.Schema != "" {
		f, err := os.CreateTemp("", "morula-schema-*.json")
		if err != nil {
			return Result{}, err
		}
		defer os.Remove(f.Name())
		f.WriteString(s.Schema)
		f.Close()
		args = append(args, "--output-schema", f.Name())
	}
	if s.Resume != "" {
		args = append(args, s.Resume)
	} else if s.Dir != "" {
		args = append(args, "-C", s.Dir)
	}
	args = append(args, "-") // 提示词从 stdin 读，避免命令行过长

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = s.Dir
	// 停止时杀掉整个进程组：codex 起的 MCP 服务器和命令会继承输出管道，只杀 codex 时
	// 读输出要等它们全部退出，用户点停止后迟迟停不下来。WaitDelay 兜底不再等管道。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 3 * time.Second
	if viaPlugin {
		cmd.Env = append(os.Environ(), routeEnv...)
		if s.Dir != "" {
			cmd.Env = append(cmd.Env, "PWD="+s.Dir) // 理由见 launch
		}
	}
	cmd.Stdin = strings.NewReader(s.Prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("启动 codex 失败：%w", err)
	}
	// 进程组外还有进程拿着输出管道时，停止 3 秒后直接关管道，不再等它。
	defer context.AfterFunc(ctx, func() { time.AfterFunc(3*time.Second, func() { stdout.Close() }) })()
	var res Result
	var failure string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		var m struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
			Error    *struct {
				Message string `json:"message"`
			} `json:"error"`
			Item struct {
				Type     string          `json:"type"`
				Text     string          `json:"text"`
				Command  string          `json:"command"`
				ExitCode *int            `json:"exit_code"`
				Output   string          `json:"aggregated_output"`
				Tool     string          `json:"tool"`
				Args     json.RawMessage `json:"arguments"`
				Status   string          `json:"status"`
				Result   *struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
				Changes []struct {
					Path string `json:"path"`
					Kind string `json:"kind"`
				} `json:"changes"`
			} `json:"item"`
			Usage struct {
				In  int `json:"input_tokens"`
				Out int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Type {
		case "thread.started":
			res.Session = m.ThreadID
		case "item.started":
			switch m.Item.Type {
			case "command_execution":
				on(Event{Kind: "command", Text: bareCommand(m.Item.Command)})
			case "mcp_tool_call":
				on(Event{Kind: "tool", Tool: m.Item.Tool, Args: clip(string(m.Item.Args), 2000)})
			}
		case "item.completed":
			switch m.Item.Type {
			case "agent_message":
				res.Final = m.Item.Text
				on(Event{Kind: "message", Text: m.Item.Text})
			case "command_execution":
				on(Event{Kind: "done", Text: clip(m.Item.Output, 2000), Code: m.Item.ExitCode})
			case "reasoning":
				if t := strings.TrimSpace(m.Item.Text); t != "" {
					on(Event{Kind: "thinking", Text: t})
				}
			case "mcp_tool_call":
				ev := Event{Kind: "tool_done", Tool: m.Item.Tool, Failed: m.Item.Status == "failed" || m.Item.Error != nil}
				if m.Item.Error != nil {
					ev.Text = m.Item.Error.Message
				} else if m.Item.Result != nil {
					var parts []string
					for _, c := range m.Item.Result.Content {
						parts = append(parts, c.Text)
					}
					ev.Text = strings.Join(parts, "\n")
				}
				ev.Text = clip(ev.Text, 2000)
				on(ev)
			case "file_change":
				var paths []string
				for _, c := range m.Item.Changes {
					paths = append(paths, c.Path)
				}
				on(Event{Kind: "file", Text: strings.Join(paths, "\n")})
			}
		case "turn.completed":
			res.Tokens += m.Usage.In + m.Usage.Out
		case "turn.failed", "error":
			if m.Error != nil {
				failure = m.Error.Message
			} else if m.Message != "" {
				failure = m.Message
			}
		}
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if err != nil || failure != "" {
		msg := failure
		if msg == "" {
			msg = strings.TrimSpace(tailLines(stderr.String(), 6))
		}
		if err != nil {
			msg = err.Error() + "：" + msg
		}
		return res, errors.New("codex 运行失败：" + msg)
	}
	if strings.TrimSpace(res.Final) == "" {
		return res, errors.New("codex 没有给出回复")
	}
	if res.Session != "" && !s.Ephemeral {
		if p, err := rolloutPath(res.Session); err == nil {
			res.Context = lastContext(p)
		}
	}
	return res, nil
}

// lastContext 读 rollout 文件里最后一次请求的 token 用量（last_token_usage），读不到时返回 0。
// codex --json 的 turn.completed 只给这一轮所有请求的合计，一轮调了几次工具就会比上下文大好几倍。
func lastContext(p string) int {
	f, err := os.Open(p)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		b := sc.Bytes()
		if !bytes.Contains(b, []byte(`"last_token_usage"`)) {
			continue
		}
		var l struct {
			Payload struct {
				Info struct {
					Last struct {
						In  int `json:"input_tokens"`
						Out int `json:"output_tokens"`
					} `json:"last_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(b, &l) == nil && l.Payload.Info.Last.In > 0 {
			n = l.Payload.Info.Last.In + l.Payload.Info.Last.Out
		}
	}
	return n
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

var rolloutCache sync.Map // 会话 id -> 文件路径

func (codex) sessionFile(session string) string {
	p, _ := rolloutPath(session)
	return p
}

func rolloutPath(session string) (string, error) {
	if p, ok := rolloutCache.Load(session); ok {
		return p.(string), nil
	}
	var found string
	root := filepath.Join(codexHome(), "sessions")
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(p, session+".jsonl") {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	if found == "" {
		return "", ErrNoSession
	}
	rolloutCache.Store(session, found)
	return found, nil
}

// Trajectory 读 codex 保存的 rollout 文件。跳过 codex 自己注入的开发者消息和环境说明。
func (codex) Trajectory(session string) ([]Step, error) {
	p, err := rolloutPath(session)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := []Step{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		var l struct {
			TS      string          `json:"timestamp"`
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Type != "response_item" {
			continue
		}
		var p struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Name    string          `json:"name"`
			Input   string          `json:"input"`
			Args    string          `json:"arguments"`
			Output  json.RawMessage `json:"output"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(l.Payload, &p) != nil {
			continue
		}
		switch p.Type {
		case "message":
			var parts []string
			for _, c := range p.Content {
				parts = append(parts, c.Text)
			}
			text := strings.Join(parts, "\n")
			switch {
			case p.Role == "assistant":
				out = append(out, Step{Role: "agent", Text: text, TS: l.TS})
			case p.Role == "user" && !strings.HasPrefix(text, "# AGENTS.md") && !strings.HasPrefix(text, "<"):
				out = append(out, Step{Role: "user", Text: text, TS: l.TS})
			}
		case "function_call", "custom_tool_call":
			in := p.Input
			if in == "" {
				in = p.Args
			}
			out = append(out, Step{Role: "tool", Text: p.Name + " " + clip(in, 2000), TS: l.TS})
		case "function_call_output", "custom_tool_call_output":
			out = append(out, Step{Role: "output", Text: clip(outputText(p.Output), 4000), TS: l.TS})
		}
	}
	return out, sc.Err()
}

// outputText 读工具输出：可能是字符串，也可能是 [{type,text}] 数组。
func outputText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b []string
		for _, p := range parts {
			b = append(b, p.Text)
		}
		return strings.Join(b, "\n")
	}
	var o struct {
		Output string `json:"output"`
	}
	json.Unmarshal(raw, &o)
	return o.Output
}

// shellWrap 是 codex 给每条命令套的外壳：/bin/zsh -lc '…'。
var shellWrap = regexp.MustCompile(`^/bin/(zsh|bash|sh) -l?c `)

// bareCommand 去掉外壳给人看。里面的命令用一层引号包着、且去掉引号不改意思时把引号也去掉；
// 引号里还有引号、转义或变量时保留原样，免得显示的命令和实际跑的不一样。
func bareCommand(c string) string {
	rest := shellWrap.ReplaceAllString(c, "")
	if rest == c || len(rest) < 2 {
		return c
	}
	in := rest[1 : len(rest)-1]
	switch {
	case rest[0] == '\'' && rest[len(rest)-1] == '\'' && !strings.Contains(in, "'"):
		return in
	case rest[0] == '"' && rest[len(rest)-1] == '"' && !strings.ContainsAny(in, "\"\\$`"):
		return in
	}
	return rest
}
