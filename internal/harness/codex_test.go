package harness

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// 工具用 -c 只对这次调用挂上，路径里的引号和反斜杠要转义，免得 TOML 解析错。
func TestToolArgs(t *testing.T) {
	got := toolArgs(&Tools{Command: `/Apps/a "b"/morula`, Args: []string{"mcp", "--assistant"},
		Env: map[string]string{"MORULA_HOME": `/x\y`}, Names: []string{"list_projects", "list_agents"}})
	for _, want := range []string{
		`mcp_servers.morula_assistant.command="/Apps/a \"b\"/morula"`,
		`mcp_servers.morula_assistant.args=["mcp","--assistant"]`,
		`mcp_servers.morula_assistant.env={MORULA_HOME="/x\\y"}`,
		`mcp_servers.morula_assistant.enabled_tools=["list_projects","list_agents"]`,
		`mcp_servers.morula_assistant.default_tools_approval_mode="approve"`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("缺 %s\n得到 %q", want, got)
		}
	}
	if toolArgs(nil) != nil {
		t.Error("没有工具时不该加参数")
	}
}

// 助理的工具走进程内的 HTTP 服务：只给 url，不给 command，带上工具调用的超时。
func TestToolArgsURL(t *testing.T) {
	got := toolArgs(&Tools{URL: "http://127.0.0.1:1/mcp/t", Names: []string{"dispatch"}, Timeout: 3600})
	for _, want := range []string{
		`mcp_servers.morula_assistant.url="http://127.0.0.1:1/mcp/t"`,
		`mcp_servers.morula_assistant.enabled_tools=["dispatch"]`,
		`mcp_servers.morula_assistant.tool_timeout_sec=3600`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("缺 %s\n得到 %q", want, got)
		}
	}
	for _, a := range got {
		if strings.Contains(a, "command=") {
			t.Errorf("给了 url 就不该再给 command：%q", got)
		}
	}
}

// 命令行去掉 /bin/zsh -lc 外壳；引号里还有引号或变量时保留引号。
func TestBareCommand(t *testing.T) {
	for in, want := range map[string]string{
		`/bin/zsh -lc pwd`:                           `pwd`,
		`/bin/zsh -lc 'nl -ba README.md'`:            `nl -ba README.md`,
		`/bin/zsh -lc "nl -ba a.md | sed -n '1,9p'"`: `nl -ba a.md | sed -n '1,9p'`,
		`/bin/bash -lc "echo $HOME"`:                 `"echo $HOME"`,
		`/bin/zsh -lc 'a '"'"'b'"'"''`:               `'a '"'"'b'"'"''`,
		`ls -la`:                                     `ls -la`,
	} {
		if got := bareCommand(in); got != want {
			t.Errorf("bareCommand(%q) = %q，应为 %q", in, got, want)
		}
	}
}

func TestLastContext(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout.jsonl")
	lines := `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":90000,"output_tokens":900},"last_token_usage":{"input_tokens":30000,"output_tokens":300}}}}
{"type":"event_msg","payload":{"type":"token_count","info":null}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":125000,"output_tokens":1400},"last_token_usage":{"input_tokens":35000,"output_tokens":500}}}}
`
	if err := os.WriteFile(p, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lastContext(p); got != 35500 {
		t.Fatalf("lastContext = %d，要 35500", got)
	}
	if got := lastContext(filepath.Join(t.TempDir(), "none.jsonl")); got != 0 {
		t.Fatalf("没有文件时 = %d", got)
	}
	if got := (Result{Tokens: 342000, Context: 32000}).ContextTokens(); got != 32000 {
		t.Fatalf("ContextTokens = %d", got)
	}
	if got := (Result{Tokens: 1000}).ContextTokens(); got != 1000 {
		t.Fatalf("没有上下文大小时 = %d", got)
	}
}
