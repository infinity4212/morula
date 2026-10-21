package harness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// opencode 驱动本机的 OpenCode（opencode run --format json），用它自己配好的 provider 和模型。
//
// 不改 ~/.config/opencode：模型、思考强度（--variant）、权限都用命令行参数只对这次调用生效。
// opencode 没有系统沙箱。只读用它内置的 plan agent（不能改文件），可写用 --auto 自动批准权限请求。
// 不用 --dir：2026-10-17 实测带 --dir 时进程 3 分钟没有输出，改成在工作目录里启动。
type opencode struct{}

func init() { register(opencode{}) }

func (opencode) Name() string { return "opencode" }

func (opencode) resumes() bool { return true }

// usesTools：MCP 服务器写进 OPENCODE_CONFIG_CONTENT 的 mcp 段，只对这次调用生效。plan agent 里也能调（2026-10-20 实测 1.19.3）。
func (opencode) usesTools() bool { return true }

func (opencode) profile() Profile {
	return Profile{Bin: "opencode", Sessions: "~/.local/share/opencode/opencode.db", Sandbox: "没有隔离：只读时不能改文件；可写时自动同意它的权限请求", Tools: true, Resume: true}
}

func (opencode) Detect() error {
	if _, err := exec.LookPath("opencode"); err != nil {
		return errors.New("没找到 opencode 命令")
	}
	return nil
}

// 模型列表要跑一次 opencode models（几秒），记 5 分钟；读失败时 1 分钟后再试。
var ocModels struct {
	sync.Mutex
	list []Model
	at   time.Time
	ttl  time.Duration
}

// Models 是 opencode 自己的模型，后面接上网关模型（morula/<来源>/<模型>）。
func (opencode) Models() []Model {
	ocModels.Lock()
	defer ocModels.Unlock()
	if !ocModels.at.IsZero() && time.Since(ocModels.at) < ocModels.ttl {
		return append(slices.Clone(ocModels.list), gatewayModels()...)
	}
	list, err := readOpencodeModels()
	ocModels.list, ocModels.at, ocModels.ttl = list, time.Now(), 5*time.Minute
	if err != nil {
		ocModels.ttl = time.Minute
	}
	return append(slices.Clone(list), gatewayModels()...)
}

// ocHeader 是 opencode models --verbose 里每个模型前面那行：provider/model。
var ocHeader = regexp.MustCompile(`(?m)^(\S+/\S+)\n\{`)

func readOpencodeModels() ([]Model, error) {
	bin, err := exec.LookPath("opencode")
	if err != nil {
		return []Model{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, bin, "models", "--verbose").Output()
	if err != nil {
		return []Model{}, err
	}
	return parseOpencodeModels(string(b)), nil
}

func parseOpencodeModels(s string) []Model {
	out := []Model{}
	idx := ocHeader.FindAllStringSubmatchIndex(s, -1)
	for i, m := range idx {
		id := s[m[2]:m[3]]
		end := len(s)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		var d struct {
			Name     string                     `json:"name"`
			Status   string                     `json:"status"`
			Variants map[string]json.RawMessage `json:"variants"`
		}
		json.NewDecoder(strings.NewReader(s[m[3]+1 : end])).Decode(&d)
		if !noClaude(id) || d.Status == "deprecated" {
			continue
		}
		md := Model{ID: id, Name: d.Name, Efforts: []string{}}
		for _, e := range effortOrder {
			if _, ok := d.Variants[e]; ok {
				md.Efforts = append(md.Efforts, e)
			}
		}
		out = append(out, md)
	}
	return out
}

func (opencode) Run(ctx context.Context, s Spec, on func(Event)) (Result, error) {
	bin, err := exec.LookPath("opencode")
	if err != nil {
		return Result{}, errors.New("没找到 opencode 命令")
	}
	args := []string{"run", "--format", "json"}
	model, env := opencodeEnv(ctx, s)
	if model != "" {
		args = append(args, "-m", model)
	}
	if s.Effort != "" {
		args = append(args, "--variant", s.Effort)
	}
	if s.Access == ReadOnly {
		args = append(args, "--agent", "plan")
	} else {
		args = append(args, "--auto")
	}
	if s.Resume != "" {
		args = append(args, "-s", s.Resume)
	}
	args = append(args, "--", s.Prompt+schemaNote(s.Schema))
	p, err := launch(ctx, bin, args, s.Dir, "", env)
	if err != nil {
		return Result{}, errors.New("启动 opencode 失败：" + err.Error())
	}
	var res Result
	var failure string
	p.lines(func(line []byte) {
		var m struct {
			Type    string          `json:"type"`
			Session string          `json:"sessionID"`
			Error   json.RawMessage `json:"error"`
			Part    struct {
				Type  string `json:"type"`
				Text  string `json:"text"`
				Tool  string `json:"tool"`
				State struct {
					Status string          `json:"status"`
					Input  json.RawMessage `json:"input"`
					Output string          `json:"output"`
					Error  string          `json:"error"`
				} `json:"state"`
				Tokens struct {
					Total  int `json:"total"`
					Input  int `json:"input"`
					Output int `json:"output"`
				} `json:"tokens"`
			} `json:"part"`
		}
		if json.Unmarshal(line, &m) != nil {
			return
		}
		if m.Session != "" {
			res.Session = m.Session
		}
		switch m.Type {
		case "text":
			if t := strings.TrimSpace(m.Part.Text); t != "" {
				res.Final = m.Part.Text
				on(Event{Kind: "message", Text: m.Part.Text})
			}
		case "reasoning":
			if t := strings.TrimSpace(m.Part.Text); t != "" {
				on(Event{Kind: "thinking", Text: t})
			}
		case "tool_use":
			toolEvents(m.Part.Tool, m.Part.State.Input, m.Part.State.Output+m.Part.State.Error, m.Part.State.Status == "error", on)
		case "step_finish": // 一步是一次请求；total 含缓存命中的输入，是这次请求的上下文大小
			t := m.Part.Tokens
			if t.Total == 0 {
				t.Total = t.Input + t.Output
			}
			res.Tokens += t.Total
			res.Context = t.Total
		case "error":
			failure = errText(m.Error)
		}
	})
	if err := p.wait(ctx, "opencode", failure); err != nil {
		return res, err
	}
	if strings.TrimSpace(res.Final) == "" {
		return res, errors.New("opencode 没有给出回复")
	}
	return res, nil
}

// opencodeEnv 返回 -m 的值和这次调用的环境变量。网关模型的 provider 和 morula 的 MCP 服务器写进同一个 OPENCODE_CONFIG_CONTENT，
// 只对这次调用生效，不写 ~/.config/opencode。
func opencodeEnv(ctx context.Context, s Spec) (model string, env []string) {
	cfg := map[string]any{}
	model = s.Model
	if inner, r, ok := gatewayRoute(ctx, s.Model); ok {
		cfg["provider"], env, model = opencodeProvider(inner, r), []string{"MORULA_GATEWAY_KEY=" + r.Key}, "morula/"+inner
	}
	if s.Tools != nil {
		cfg["mcp"] = map[string]any{"morula_assistant": opencodeMCP(s.Tools)}
	}
	if len(cfg) > 0 {
		b, _ := json.Marshal(cfg)
		env = append(env, "OPENCODE_CONFIG_CONTENT="+string(b))
	}
	return model, env
}

// opencodeMCP 是 opencode 配置里的一个 MCP 服务器。timeout 是毫秒，列工具和调用工具都按它等。
func opencodeMCP(t *Tools) map[string]any {
	m := map[string]any{"enabled": true}
	if t.URL != "" {
		m["type"], m["url"] = "remote", t.URL
	} else {
		m["type"], m["command"] = "local", append([]string{t.Command}, t.Args...)
		if len(t.Env) > 0 {
			m["environment"] = t.Env
		}
	}
	if t.Timeout > 0 {
		m["timeout"] = t.Timeout * 1000
	}
	return m
}

// toolEvents 把一次已经结束的工具调用换成界面上的事件：命令显示成命令和输出，改文件显示成文件，其他显示成工具。
func toolEvents(tool string, input json.RawMessage, output string, failed bool, on func(Event)) {
	var in struct {
		Command  string `json:"command"`
		FilePath string `json:"filePath"`
		Path     string `json:"path"`
	}
	json.Unmarshal(input, &in)
	switch tool {
	case "bash":
		on(Event{Kind: "command", Text: in.Command})
		code := 0
		if failed {
			code = 1
		}
		on(Event{Kind: "done", Text: clip(output, 2000), Code: &code})
	case "edit", "write", "patch", "multiedit":
		on(Event{Kind: "file", Text: in.FilePath + in.Path})
	default:
		tool = bareTool(tool)
		on(Event{Kind: "tool", Tool: tool, Args: clip(string(input), 2000)})
		on(Event{Kind: "tool_done", Tool: tool, Text: clip(output, 2000), Failed: failed})
	}
}

// bareTool 去掉工具名前面的服务器名：pi、codebuddy 写作 mcp__morula_assistant__<工具>，opencode 写作 morula_assistant_<工具>。
func bareTool(tool string) string {
	for _, p := range []string{"mcp__morula_assistant__", "morula_assistant_"} {
		if t, ok := strings.CutPrefix(tool, p); ok {
			return t
		}
	}
	return tool
}

// errText 从 harness 的错误事件里取一句话：可能是字符串，也可能是 {message} 或 {data:{message}}。
func errText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var e struct {
		Name    string `json:"name"`
		Message string `json:"message"`
		Data    struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	json.Unmarshal(raw, &e)
	for _, v := range []string{e.Data.Message, e.Message, e.Name} {
		if v != "" {
			return v
		}
	}
	return clip(string(raw), 600)
}

func opencodeDB() string {
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		home, _ := os.UserHomeDir()
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, "opencode", "opencode.db")
}

// Trajectory 只读打开 opencode.db，按时间读这个会话的消息和各个部分。
func (opencode) Trajectory(session string) ([]Step, error) {
	p := opencodeDB()
	if _, err := os.Stat(p); err != nil {
		return nil, ErrNoSession
	}
	db, err := sql.Open("sqlite", "file:"+p+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT json_extract(m.data, '$.role'), pt.data, m.time_created FROM message m JOIN part pt ON pt.message_id = m.id
		WHERE m.session_id = ? ORDER BY m.time_created, pt.id`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Step{}
	for rows.Next() {
		var role, data string
		var ms int64
		if rows.Scan(&role, &data, &ms) != nil {
			continue
		}
		var pt struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			Tool  string `json:"tool"`
			State struct {
				Input  json.RawMessage `json:"input"`
				Output string          `json:"output"`
			} `json:"state"`
		}
		if json.Unmarshal([]byte(data), &pt) != nil {
			continue
		}
		ts := time.UnixMilli(ms).UTC().Format(time.RFC3339)
		switch {
		case pt.Type == "text" && role == "user":
			out = append(out, Step{Role: "user", Text: pt.Text, TS: ts})
		case pt.Type == "text":
			out = append(out, Step{Role: "agent", Text: pt.Text, TS: ts})
		case pt.Type == "tool":
			out = append(out, Step{Role: "tool", Text: pt.Tool + " " + clip(string(pt.State.Input), 2000), TS: ts},
				Step{Role: "output", Text: clip(pt.State.Output, 4000), TS: ts})
		}
	}
	if len(out) == 0 {
		return nil, ErrNoSession
	}
	return out, rows.Err()
}
