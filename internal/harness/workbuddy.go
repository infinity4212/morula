package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// workbuddy 驱动本机的 CodeBuddy Code（终端版 codebuddy，codebuddy -p --output-format stream-json），用它自己登录的账号和模型。
// codebuddy 和桌面版 WorkBuddy 用同一个账号，morula 里统一叫 WorkBuddy；桌面版自带的命令行要靠桌面版主进程给登录信息，morula 不用它。
//
// 不改 ~/.codebuddy：模型、思考强度、MCP、权限都用命令行参数只对这次调用生效（权限用 --settings 传一段 JSON）。
// codebuddy 没有系统沙箱。只读时 deny 掉写文件、跑命令、开子 agent 的工具；可写时跳过权限确认（-y）。
//
// 2026-10-20 实测 2.170.0：MCP 工具要先经 ToolSearch 找到，再经 DeferExecuteTool 调用。
// --tools 白名单会挡掉 MCP 工具，--permission-mode plan、dontAsk 会拒掉 DeferExecuteTool，所以只读不用这两样，改用 --settings。
type workbuddy struct{}

func init() { register(workbuddy{}) }

func (workbuddy) Name() string { return "workbuddy" }

func (workbuddy) resumes() bool { return true }

func (workbuddy) usesTools() bool { return true }

func (workbuddy) compacts() bool { return true }

func (workbuddy) profile() Profile {
	return Profile{Bin: "codebuddy", Sessions: "~/.codebuddy/projects", Sandbox: "无沙箱隔离：只读时禁止改文件和运行命令；可写时跳过逐项确认", Tools: true, Compact: true, Resume: true}
}

func (workbuddy) Detect() error {
	if _, err := exec.LookPath("codebuddy"); err != nil {
		return errors.New("没找到 codebuddy 命令")
	}
	return nil
}

// cbEfforts 是 --effort 的档位。codebuddy 没有按模型列出支持哪几档，每个模型都列全部。
var cbEfforts = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

var cbModels struct {
	sync.Mutex
	list []Model
	at   time.Time
	ttl  time.Duration
}

// Models 读 codebuddy --help 里 --model 一行列出的模型，第一个是默认模型。
func (workbuddy) Models() []Model {
	cbModels.Lock()
	defer cbModels.Unlock()
	if !cbModels.at.IsZero() && time.Since(cbModels.at) < cbModels.ttl {
		return slices.Clone(cbModels.list)
	}
	list, err := readCodebuddyModels()
	cbModels.list, cbModels.at, cbModels.ttl = list, time.Now(), 5*time.Minute
	if err != nil {
		cbModels.ttl = time.Minute
	}
	return slices.Clone(list)
}

func readCodebuddyModels() ([]Model, error) {
	bin, err := exec.LookPath("codebuddy")
	if err != nil {
		return []Model{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, bin, "--help").Output()
	if err != nil {
		return []Model{}, err
	}
	list := parseCodebuddyModels(string(b))
	priceModels(bin, list)
	return list, nil
}

var cbSupported = regexp.MustCompile(`--model <model>[^\n]*Currently supported: \(([^)]*)\)`)

func parseCodebuddyModels(help string) []Model {
	out := []Model{}
	m := cbSupported.FindStringSubmatch(help)
	if m == nil {
		return out
	}
	for _, id := range strings.Split(m[1], ",") {
		if id = strings.TrimSpace(id); id != "" && noClaude(id) {
			out = append(out, Model{ID: id, Name: id, Efforts: slices.Clone(cbEfforts)})
		}
	}
	return out
}

// cbReadOnlyDeny 是只读时禁掉的工具：写文件、跑命令，以及会另起 agent 的工具。deny 也管经 DeferExecuteTool 调的工具（2026-10-20 实测）。
var cbReadOnlyDeny = []string{"Write", "Edit", "MultiEdit", "NotebookEdit", "Bash", "PowerShell", "Monitor", "Agent", "TeamCreate"}

// codebuddyEnv 是这次调用的环境变量：MCP_TOOL_TIMEOUT（毫秒）管单次工具调用最多等多久，HTTP 传输层也按它等。
func codebuddyEnv(s Spec) []string {
	if s.Tools == nil || s.Tools.Timeout <= 0 {
		return nil
	}
	return []string{"MCP_TOOL_TIMEOUT=" + strconv.Itoa(s.Tools.Timeout*1000)}
}

// codebuddyArgs 是除提示词以外的参数。
func codebuddyArgs(s Spec) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	if s.Effort != "" {
		args = append(args, "--effort", s.Effort)
	}
	perm := map[string][]string{}
	if s.Tools != nil {
		srv := map[string]any{}
		if s.Tools.URL != "" {
			srv["type"], srv["url"] = "http", s.Tools.URL
		} else {
			srv["type"], srv["command"], srv["args"], srv["env"] = "stdio", s.Tools.Command, s.Tools.Args, s.Tools.Env
		}
		b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"morula_assistant": srv}})
		args = append(args, "--mcp-config", string(b))
		perm["allow"] = []string{"ToolSearch", "DeferExecuteTool", "mcp__morula_assistant"}
	}
	if s.Access == ReadOnly {
		perm["deny"] = cbReadOnlyDeny
	} else {
		args = append(args, "-y")
	}
	if len(perm) > 0 {
		b, _ := json.Marshal(map[string]any{"permissions": perm})
		args = append(args, "--settings", string(b))
	}
	if s.CompactAt > 0 { // codebuddy 会夹到 100k 到 1M 之间
		args = append(args, "--autocompact", strconv.Itoa(s.CompactAt))
	}
	switch {
	case s.Resume != "":
		args = append(args, "-r", s.Resume)
	case s.Ephemeral:
		args = append(args, "--no-session-persistence")
	}
	return args
}

func (workbuddy) Run(ctx context.Context, s Spec, on func(Event)) (Result, error) {
	bin, err := exec.LookPath("codebuddy")
	if err != nil {
		return Result{}, errors.New("没找到 codebuddy 命令")
	}
	if _, _, ok := gatewayRoute(ctx, s.Model); ok {
		return Result{}, errors.New("codebuddy 还不能用 morula 网关的模型")
	}
	args := append(codebuddyArgs(s), "--", s.Prompt+schemaNote(s.Schema))
	p, err := launch(ctx, bin, args, s.Dir, "", codebuddyEnv(s))
	if err != nil {
		return Result{}, errors.New("启动 codebuddy 失败：" + err.Error())
	}
	st := cbRun{calls: map[string]cbCall{}}
	p.lines(func(line []byte) { st.event(line, on) })
	res := st.res
	if err := p.wait(ctx, "codebuddy", st.failure); err != nil {
		return res, err
	}
	if strings.TrimSpace(res.Final) == "" {
		return res, errors.New("codebuddy 没有给出回复")
	}
	return res, nil
}

type cbCall struct {
	name string
	args json.RawMessage
}

// cbRun 是读 stream-json 输出时的状态。工具参数在助手消息的 tool_use 里，结果在下一条用户消息的 tool_result 里，按 id 对上。
type cbRun struct {
	res     Result
	failure string
	calls   map[string]cbCall
}

type cbBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type cbUsage struct {
	Input  int `json:"input_tokens"` // 已含缓存命中的输入（2026-10-20 实测 input_tokens = cache_creation + cache_read）
	Output int `json:"output_tokens"`
}

func (st *cbRun) event(line []byte, on func(Event)) {
	var e struct {
		Type    string `json:"type"`
		Session string `json:"session_id"`
		Message struct {
			Content json.RawMessage `json:"content"`
			Usage   cbUsage         `json:"usage"`
		} `json:"message"`
		IsError bool    `json:"is_error"`
		Result  string  `json:"result"`
		Usage   cbUsage `json:"usage"`
	}
	if json.Unmarshal(line, &e) != nil {
		return
	}
	res := &st.res
	if e.Session != "" {
		res.Session = e.Session
	}
	var blocks []cbBlock
	json.Unmarshal(e.Message.Content, &blocks)
	switch e.Type {
	case "assistant":
		if u := e.Message.Usage; u.Input > 0 { // 一次请求的思考和回复分成几条事件，只有一条带用量
			res.Context = u.Input + u.Output
		}
		for _, b := range blocks {
			switch b.Type {
			case "thinking":
				if t := strings.TrimSpace(b.Thinking); t != "" {
					on(Event{Kind: "thinking", Text: t})
				}
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					on(Event{Kind: "message", Text: b.Text})
				}
			case "tool_use":
				st.calls[b.ID] = cbCall{b.Name, b.Input}
			}
		}
	case "user":
		for _, b := range blocks {
			if b.Type != "tool_result" {
				continue
			}
			c := st.calls[b.ToolUseID]
			delete(st.calls, b.ToolUseID)
			if c.name == "ToolSearch" {
				continue // 只是在找工具
			}
			cbToolEvents(c.name, c.args, cbText(b.Content), b.IsError, on)
		}
	case "result":
		res.Tokens = e.Usage.Input + e.Usage.Output
		if e.IsError {
			st.failure = e.Result
		} else {
			res.Final = e.Result
		}
	}
}

// cbToolEvents 把 codebuddy 的工具名换成 toolEvents 认的：DeferExecuteTool 拆出里面真正调的工具。
func cbToolEvents(name string, args json.RawMessage, out string, failed bool, on func(Event)) {
	if name == "DeferExecuteTool" {
		var d struct {
			Tool   string          `json:"toolName"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(args, &d) == nil && d.Tool != "" {
			name, args = d.Tool, d.Params
		}
	}
	switch name {
	case "Bash", "PowerShell":
		toolEvents("bash", args, out, failed, on)
	case "Write", "Edit", "MultiEdit":
		var in struct {
			Path string `json:"file_path"`
		}
		json.Unmarshal(args, &in)
		on(Event{Kind: "file", Text: in.Path})
	default:
		toolEvents(name, args, out, failed, on)
	}
}

// cbText 取工具结果的文字：可能是字符串、一个内容块，或内容块的数组。
func cbText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var bs []cbBlock
	if json.Unmarshal(raw, &bs) != nil {
		var one cbBlock
		json.Unmarshal(raw, &one)
		bs = []cbBlock{one}
	}
	var b strings.Builder
	for _, c := range bs {
		b.WriteString(c.Text)
	}
	return b.String()
}

func codebuddyHome() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codebuddy")
}

// sessionFile 是 ~/.codebuddy/projects/<工作目录>/<会话 id>.jsonl；桌面版 WorkBuddy 的会话在 ~/.workbuddy/projects 下，格式一样。
func (workbuddy) sessionFile(session string) string {
	home, _ := os.UserHomeDir()
	for _, dir := range []string{codebuddyHome(), filepath.Join(home, ".workbuddy")} {
		if files, _ := filepath.Glob(filepath.Join(dir, "projects", "*", session+".jsonl")); len(files) > 0 {
			return files[0]
		}
	}
	return ""
}

// Trajectory 读会话文件：type 为 message 的是用户和助手的话，function_call、function_call_result 是工具调用和输出。
func (c workbuddy) Trajectory(session string) ([]Step, error) {
	p := c.sessionFile(session)
	if p == "" {
		return nil, ErrNoSession
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return codebuddySteps(f)
}

func codebuddySteps(r interface{ Read([]byte) (int, error) }) ([]Step, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	out := []Step{}
	for sc.Scan() {
		var e struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Timestamp json.RawMessage `json:"timestamp"` // 毫秒数，2026-10-20 实测是数字
			Content   json.RawMessage `json:"content"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Output    json.RawMessage `json:"output"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		ts := strings.Trim(string(e.Timestamp), `"`)
		if ms, err := strconv.ParseInt(ts, 10, 64); err == nil {
			ts = time.UnixMilli(ms).Format(time.RFC3339)
		}
		switch e.Type {
		case "message":
			t := strings.TrimSpace(cbText(e.Content))
			if t == "" {
				continue
			}
			role := map[string]string{"user": "user", "assistant": "agent"}[e.Role]
			if role != "" {
				out = append(out, Step{Role: role, Text: t, TS: ts})
			}
		case "function_call":
			out = append(out, Step{Role: "tool", Text: e.Name + " " + clip(e.Arguments, 2000), TS: ts})
		case "function_call_result":
			out = append(out, Step{Role: "output", Text: clip(cbText(e.Output), 4000), TS: ts})
		}
	}
	return out, sc.Err()
}

// priceModels 从 codebuddy 包里的 product.json 补上每个模型扣几倍点数和上下文长度。
// product.json 只和账号的模型列表部分重合，对不上的模型不补。读不到时什么也不做。
func priceModels(bin string, list []Model) {
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(real), "..", "product.json"))
	if err != nil {
		return
	}
	var p struct {
		Models []struct {
			ID      string `json:"id"`
			Credits string `json:"credits"`
			Max     int    `json:"maxInputTokens"`
		} `json:"models"`
	}
	if json.Unmarshal(b, &p) != nil {
		return
	}
	for i := range list {
		for _, m := range p.Models {
			if m.ID != list[i].ID {
				continue
			}
			list[i].Credits = m.Credits
			if list[i].Context == 0 {
				list[i].Context = m.Max
			}
		}
	}
}
