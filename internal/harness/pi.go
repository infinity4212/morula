package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// pi 驱动本机的 pi（pi -p --mode json），用它自己配好的 provider 和模型。
//
// 不改 ~/.pi/agent/settings.json：模型、思考强度、可用工具都用命令行参数只对这次调用生效。
// pi 没有沙箱，权限同本机用户。只读时只开 read、grep、find、ls 四个工具；可写时开全部工具。
type pi struct{}

func init() { register(pi{}) }

func (pi) Name() string { return "pi" }

func (pi) resumes() bool { return true }

// usesTools：用扩展 piTools 调 pi.registerMcpServer，只对这次调用挂上。
func (pi) usesTools() bool { return true }

func (pi) profile() Profile {
	return Profile{Bin: "pi", Sessions: "~/.pi/agent/sessions", Sandbox: "没有隔离：只读时只能查看文件；可写时能做你在这台 Mac 上能做的任何事", Tools: true, Resume: true}
}

func (pi) Detect() error {
	if _, err := exec.LookPath("pi"); err != nil {
		return errors.New("没找到 pi 命令")
	}
	return nil
}

var piModels struct {
	sync.Mutex
	list []Model
	at   time.Time
	ttl  time.Duration
}

// piEfforts 是 pi --thinking 的档位，off 不列：不传 --thinking 时用 pi 自己的默认值。
var piEfforts = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// Models 是 pi 自己的模型，后面接上网关模型（morula/<来源>/<模型>）。
func (pi) Models() []Model {
	piModels.Lock()
	defer piModels.Unlock()
	if !piModels.at.IsZero() && time.Since(piModels.at) < piModels.ttl {
		return append(slices.Clone(piModels.list), gatewayModels()...)
	}
	list, err := readPiModels()
	piModels.list, piModels.at, piModels.ttl = list, time.Now(), 5*time.Minute
	if err != nil {
		piModels.ttl = time.Minute
	}
	return append(slices.Clone(list), gatewayModels()...)
}

func readPiModels() ([]Model, error) {
	bin, err := exec.LookPath("pi")
	if err != nil {
		return []Model{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, bin, "--offline", "--list-models").Output()
	if err != nil {
		return []Model{}, err
	}
	return parsePiModels(string(b)), nil
}

// parsePiModels 读 pi --list-models 的表格：provider、model、context、max-out、thinking、images。
// 模型 id 写作 provider/model，跑的时候在第一个「/」处拆开。
func parsePiModels(s string) []Model {
	out := []Model{}
	for i, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 5 {
			continue
		}
		id := f[0] + "/" + f[1]
		if !noClaude(id) {
			continue
		}
		md := Model{ID: id, Name: f[1], Efforts: []string{}}
		if f[4] == "yes" {
			md.Efforts = slices.Clone(piEfforts)
		}
		out = append(out, md)
	}
	return out
}

func (pi) Run(ctx context.Context, s Spec, on func(Event)) (Result, error) {
	bin, err := exec.LookPath("pi")
	if err != nil {
		return Result{}, errors.New("没找到 pi 命令")
	}
	args := []string{"-p", "--mode", "json", "--no-approve"}
	var env []string
	if inner, r, ok := gatewayRoute(ctx, s.Model); ok {
		var gw []string
		if gw, env, err = piGateway(inner, r); err != nil {
			return Result{}, errors.New("准备 pi 的网关扩展失败：" + err.Error())
		}
		args = append(args, gw...)
	} else if prov, model, ok := strings.Cut(s.Model, "/"); ok {
		args = append(args, "--provider", prov, "--model", model)
	} else if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	if s.Effort != "" {
		args = append(args, "--thinking", s.Effort)
	}
	if s.Tools != nil {
		ext, tenv, err := piToolArgs(s.Tools)
		if err != nil {
			return Result{}, errors.New("准备 pi 的工具扩展失败：" + err.Error())
		}
		args, env = append(args, ext...), append(env, tenv...)
	}
	if s.Access == ReadOnly {
		// 白名单会把 MCP 工具一起关掉（2026-10-20 实测，和帮助里写的不一样），要写上 morula 的工具
		args = append(args, "--tools", "read,grep,find,ls,mcp__morula_assistant__*")
	}
	switch {
	case s.Resume != "":
		args = append(args, "--session", s.Resume)
	case s.Ephemeral:
		args = append(args, "--no-session")
	default:
		args = append(args, "--session-id", uuid.NewString())
	}
	args = append(args, "--", s.Prompt+schemaNote(s.Schema))
	p, err := launch(ctx, bin, args, s.Dir, "", env)
	if err != nil {
		return Result{}, errors.New("启动 pi 失败：" + err.Error())
	}
	st := piRun{args: map[string]json.RawMessage{}}
	p.lines(func(line []byte) { st.event(line, on) })
	res := st.res
	if err := p.wait(ctx, "pi", st.failure); err != nil {
		return res, err
	}
	if strings.TrimSpace(res.Final) == "" {
		return res, errors.New("pi 没有给出回复")
	}
	return res, nil
}

type piMessage struct {
	Role    string `json:"role"`
	Content json.RawMessage
	Usage   struct {
		Input      int `json:"input"`
		Output     int `json:"output"`
		CacheRead  int `json:"cacheRead"`
		CacheWrite int `json:"cacheWrite"`
	} `json:"usage"`
	StopReason   string `json:"stopReason"`
	ErrorMessage string `json:"errorMessage"`
}

// blocks 取消息里的内容块；用户消息的内容可能是一个字符串。
func (m piMessage) blocks() []piBlock {
	var bs []piBlock
	if json.Unmarshal(m.Content, &bs) != nil {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			bs = []piBlock{{Type: "text", Text: s}}
		}
	}
	return bs
}

type piBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (m piMessage) text() string {
	var b strings.Builder
	for _, c := range m.blocks() {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// piRun 是读 pi --mode json 输出时的状态。工具参数只在 tool_execution_start 里，按调用 id 记到结束。
type piRun struct {
	res     Result
	failure string
	args    map[string]json.RawMessage
}

func (st *piRun) event(line []byte, on func(Event)) {
	res := &st.res
	var e struct {
		Type     string          `json:"type"`
		ID       string          `json:"id"`
		Message  piMessage       `json:"message"`
		CallID   string          `json:"toolCallId"`
		ToolName string          `json:"toolName"`
		Args     json.RawMessage `json:"args"`
		Result   struct {
			Content []piBlock `json:"content"`
		} `json:"result"`
		IsError    bool   `json:"isError"`
		Success    bool   `json:"success"`
		FinalError string `json:"finalError"`
	}
	if json.Unmarshal(line, &e) != nil {
		return
	}
	switch e.Type {
	case "session":
		res.Session = e.ID
	case "tool_execution_start":
		st.args[e.CallID] = e.Args
	case "message_end":
		m := e.Message
		if m.Role != "assistant" {
			break
		}
		n := m.Usage.Input + m.Usage.CacheRead + m.Usage.CacheWrite + m.Usage.Output
		res.Tokens += n
		if n > 0 { // 一条助手消息是一次请求
			res.Context = n
		}
		if m.StopReason == "error" {
			st.failure = m.ErrorMessage
			return
		}
		st.failure = ""
		for _, c := range m.blocks() {
			if c.Type == "thinking" && strings.TrimSpace(c.Thinking) != "" {
				on(Event{Kind: "thinking", Text: strings.TrimSpace(c.Thinking)})
			}
		}
		if t := m.text(); strings.TrimSpace(t) != "" {
			res.Final = t
			on(Event{Kind: "message", Text: t})
		}
	case "tool_execution_end":
		var out strings.Builder
		for _, c := range e.Result.Content {
			out.WriteString(c.Text)
		}
		args := st.args[e.CallID]
		delete(st.args, e.CallID)
		toolEvents(e.ToolName, args, out.String(), e.IsError, on)
	case "auto_retry_end":
		if !e.Success && e.FinalError != "" {
			st.failure = e.FinalError
		}
	}
}

// piSessions 是 pi 存会话的目录，PI_CODING_AGENT_SESSION_DIR 可以改。
func piSessions() string {
	if d := os.Getenv("PI_CODING_AGENT_SESSION_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent", "sessions")
}

// sessionFile 是 <会话目录>/<工作目录>/<时间>_<id>.jsonl。
func (pi) sessionFile(session string) string {
	dir := piSessions()
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*_"+session+".jsonl"))
	more, _ := filepath.Glob(filepath.Join(dir, "*_"+session+".jsonl"))
	if files = append(files, more...); len(files) == 0 {
		return ""
	}
	return files[0]
}

// Trajectory 读会话文件里的消息。
func (p pi) Trajectory(session string) ([]Step, error) {
	path := p.sessionFile(session)
	if path == "" {
		return nil, ErrNoSession
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return piSteps(f)
}

func piSteps(r interface{ Read([]byte) (int, error) }) ([]Step, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	out := []Step{}
	for sc.Scan() {
		var e struct {
			Type      string    `json:"type"`
			Timestamp string    `json:"timestamp"`
			Message   piMessage `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Type != "message" {
			continue
		}
		m := e.Message
		switch m.Role {
		case "user":
			out = append(out, Step{Role: "user", Text: m.text(), TS: e.Timestamp})
		case "assistant":
			for _, c := range m.blocks() {
				switch c.Type {
				case "text":
					if strings.TrimSpace(c.Text) != "" {
						out = append(out, Step{Role: "agent", Text: c.Text, TS: e.Timestamp})
					}
				case "toolCall":
					out = append(out, Step{Role: "tool", Text: c.Name + " " + clip(string(c.Arguments), 2000), TS: e.Timestamp})
				}
			}
			if m.StopReason == "error" && m.ErrorMessage != "" {
				out = append(out, Step{Role: "output", Text: clip(m.ErrorMessage, 4000), TS: e.Timestamp})
			}
		case "toolResult":
			out = append(out, Step{Role: "output", Text: clip(m.text(), 4000), TS: e.Timestamp})
		}
	}
	return out, sc.Err()
}
