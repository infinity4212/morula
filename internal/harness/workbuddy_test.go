package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseCodebuddyModels(t *testing.T) {
	help := "  --model <model>    Model for the current session. Please provide the model ID. Currently supported: (hy4-preview, claude-x, glm-5.3)\n" +
		"  --fallback-model <model>  Enable automatic fallback\n"
	got := parseCodebuddyModels(help)
	if len(got) != 2 || got[0].ID != "hy4-preview" || got[1].ID != "glm-5.3" || !slices.Equal(got[0].Efforts, cbEfforts) {
		t.Fatalf("要两个模型（去掉 Claude），得到 %+v", got)
	}
}

func TestCodebuddyArgs(t *testing.T) {
	ro := strings.Join(codebuddyArgs(Spec{Model: "m", Effort: "low", Access: ReadOnly, Resume: "s1", CompactAt: 250000,
		Tools: &Tools{URL: "http://127.0.0.1:1/mcp", Timeout: 3600}}), " ")
	for _, want := range []string{"--model m", "--effort low", "-r s1", "--autocompact 250000",
		`--mcp-config {"mcpServers":{"morula_assistant":{"type":"http","url":"http://127.0.0.1:1/mcp"}}}`,
		`"allow":["ToolSearch","DeferExecuteTool","mcp__morula_assistant"]`, `"deny":["Write",`} {
		if !strings.Contains(ro, want) {
			t.Errorf("只读参数里没有 %s：%s", want, ro)
		}
	}
	if strings.Contains(ro, " -y") || strings.Contains(ro, "--tools") || strings.Contains(ro, "--permission-mode") {
		t.Errorf("只读不能跳过权限，也不能用 --tools、--permission-mode（会挡掉 MCP 工具）：%s", ro)
	}
	rw := strings.Join(codebuddyArgs(Spec{Access: Workspace, Ephemeral: true}), " ")
	if !strings.Contains(rw, " -y") || !strings.Contains(rw, "--no-session-persistence") || strings.Contains(rw, "deny") {
		t.Errorf("可写参数不对：%s", rw)
	}
	if env := codebuddyEnv(Spec{Tools: &Tools{Timeout: 3600}}); !slices.Equal(env, []string{"MCP_TOOL_TIMEOUT=3600000"}) {
		t.Errorf("env = %v", env)
	}
}

// 下面几行取自 2026-10-20 真跑 codebuddy 2.170.0 的输出，只删了无关字段、截短了文字。
const cbStream = `{"type": "system", "subtype": "init", "session_id": "01a15963-1b10"}
{"type": "assistant", "session_id": "01a15963-1b10", "message": {"content": [{"type": "text", "text": "I'll look up the tool schema first."}], "usage": {"input_tokens": 0, "output_tokens": 0}}}
{"type": "assistant", "session_id": "01a15963-1b10", "message": {"content": [{"type": "tool_use", "id": "a1", "name": "ToolSearch", "input": {"tool_names": ["mcp__morula_assistant__list_agents"]}}], "usage": {"input_tokens": 22843, "output_tokens": 35}}}
{"type": "user", "session_id": "01a15963-1b10", "message": {"content": [{"type": "tool_result", "tool_use_id": "a1", "content": [{"type": "text", "text": "Found 1 tool(s). Use DeferExecuteTool to invoke them."}], "is_error": false}]}}
{"type": "assistant", "session_id": "01a15963-1b10", "message": {"content": [{"type": "tool_use", "id": "a2", "name": "DeferExecuteTool", "input": {"toolName": "mcp__morula_assistant__list_agents", "params": {}}}], "usage": {"input_tokens": 23004, "output_tokens": 32}}}
{"type": "user", "session_id": "01a15963-1b10", "message": {"content": [{"type": "tool_result", "tool_use_id": "a2", "content": [{"type": "text", "text": "固定成员："}], "is_error": false}]}}
{"type": "assistant", "session_id": "01a15963-1b10", "message": {"content": [{"type": "text", "text": "返回了 2 个固定成员。"}], "usage": {"input_tokens": 23084, "output_tokens": 24}}}
{"type": "result", "session_id": "01a15963-1b10", "is_error": false, "result": "返回了 2 个固定成员。", "usage": {"input_tokens": 68931, "output_tokens": 91}, "subtype": "success"}`

func TestCodebuddyEvents(t *testing.T) {
	st := cbRun{calls: map[string]cbCall{}}
	var evs []Event
	for _, l := range strings.Split(cbStream, "\n") {
		st.event([]byte(l), func(e Event) { evs = append(evs, e) })
	}
	r := st.res
	if r.Session != "01a15963-1b10" || r.Final != "返回了 2 个固定成员。" || r.Context != 23108 || r.Tokens != 69022 {
		t.Fatalf("结果不对：%+v", r)
	}
	var tools []string
	for _, e := range evs {
		if e.Kind == "tool" {
			tools = append(tools, e.Tool)
		}
	}
	if !slices.Equal(tools, []string{"list_agents"}) { // ToolSearch 不显示；DeferExecuteTool 拆出真正的工具，去掉服务器名
		t.Fatalf("工具事件 %v", tools)
	}
}

// 取自同一次真跑的会话文件，只留了用到的字段。
const cbSession = `{"type": "message", "role": "user", "timestamp": 1792501051867, "content": [{"type": "input_text", "text": "调用 list_agents 工具"}]}
{"type": "file-history-snapshot", "timestamp": "1792501051914"}
{"type": "function_call", "timestamp": 1792501060942, "name": "DeferExecuteTool", "arguments": "{\"params\": {}, \"toolName\": \"mcp__morula_assistant__list_agents\"}"}
{"type": "function_call_result", "timestamp": 1792501060980, "name": "DeferExecuteTool", "output": {"type": "text", "text": "固定成员："}}
{"type": "message", "role": "assistant", "timestamp": 1792501088465, "content": [{"type": "output_text", "text": "返回了 0 个 agent"}]}`

func TestCodebuddySteps(t *testing.T) {
	steps, err := codebuddySteps(strings.NewReader(cbSession))
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, s := range steps {
		roles = append(roles, s.Role)
	}
	if !slices.Equal(roles, []string{"user", "tool", "output", "agent"}) || steps[2].Text != "固定成员：" || steps[0].TS == "" {
		t.Fatalf("轨迹不对：%+v", steps)
	}
}

func TestBareTool(t *testing.T) {
	for in, want := range map[string]string{"morula_assistant_dispatch": "dispatch", "mcp__morula_assistant__dispatch": "dispatch", "read": "read"} {
		if got := bareTool(in); got != want {
			t.Errorf("bareTool(%q) = %q", in, got)
		}
	}
}

func TestPiToolArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	args, env, err := piToolArgs(&Tools{Command: "/bin/morula", Args: []string{"mcp", "--assistant"}, Env: map[string]string{"MORULA_HOME": "/d"}, Timeout: 60})
	if err != nil || len(args) != 2 || args[0] != "-e" || len(env) != 1 {
		t.Fatalf("%v %v %v", args, env, err)
	}
	for _, want := range []string{`"command":"/bin/morula"`, `"args":["mcp","--assistant"]`, `"MORULA_HOME":"/d"`, `"timeout":60`} { // pi 的 timeout 按秒（pi docs/mcp.md）
		if !strings.Contains(env[0], want) {
			t.Errorf("MORULA_TOOLS 里没有 %s：%s", want, env[0])
		}
	}
}

// fakeTraj 不存会话文件，只给轨迹，像 opencode。
type fakeTraj struct{ opencode }

func (fakeTraj) Name() string { return "fake-traj" }
func (fakeTraj) Trajectory(id string) ([]Step, error) {
	return []Step{{Role: "user", Text: "问"}, {Role: "agent", Text: "答", TS: "2026-10-20T10:00:00Z"}}, nil
}

func TestSessionFileExport(t *testing.T) {
	register(fakeTraj{})
	defer delete(adapters, "fake-traj")
	dir := t.TempDir()
	SetExportDir(dir)
	defer SetExportDir("")
	if p := SessionFile("fake-traj", "../x"); p != "" {
		t.Errorf("会话 id 带斜杠时不能导出：%s", p)
	}
	p := SessionFile("fake-traj", "s1")
	b, err := os.ReadFile(p)
	if err != nil || p != filepath.Join(dir, "fake-traj", "s1.md") || !strings.Contains(string(b), "## 回复（2026-10-20T10:00:00Z）\n\n答") {
		t.Fatalf("%s %v\n%s", p, err, b)
	}
}

func TestLaunchPWD(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PWD", "/somewhere/else")
	for _, env := range [][]string{nil, {"X=1"}} {
		p, err := launch(context.Background(), "/bin/sh", []string{"-c", `echo "$PWD"`}, dir, "", env)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		p.lines(func(b []byte) { got = string(b) })
		p.cmd.Wait()
		if got != dir {
			t.Errorf("env %v：PWD = %q，应为 %q", env, got, dir)
		}
	}
}
