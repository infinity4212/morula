package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCodexSessions 用临时的 CODEX_HOME 造几个 rollout，检查哪些会话被列出来。
func TestCodexSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // 没有 opencode.db
	t.Setenv("HOME", t.TempDir())          // 没有 pi、WorkBuddy 的会话
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	day := filepath.Join(home, "sessions", "2026", "10", "01")
	os.MkdirAll(day, 0o755)
	meta := func(id, cwd, source string) string {
		return `{"type":"session_meta","payload":{"id":"` + id + `","cwd":"` + cwd + `","source":` + source + `}}`
	}
	user := func(text string) string {
		return `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}}`
	}
	write := func(id string, lines ...string) {
		os.WriteFile(filepath.Join(day, "rollout-"+id+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	}
	write("a", meta("a", "/work/kit", `"vscode"`), user("# AGENTS.md instructions"), user("<environment_context>"), user("帮我审阅 Kit"))
	write("b", meta("b", "/work/kit", `{"subagent":{"thread_spawn":{}}}`), user("子任务"))
	write("c", meta("c", "/work/x", `"exec"`), user("你是 morula 团队里的子 agent「执行-1」"))
	write("d", meta("d", "/tmp/try", `"cli"`), user("只回复收到"))
	write("e", meta("e", "/work/y", `"cli"`))
	// 同一个会话的第二个 rollout 文件，只该列一次
	os.MkdirAll(filepath.Join(home, "archived_sessions"), 0o755)
	os.WriteFile(filepath.Join(home, "archived_sessions", "rollout-a2.jsonl"), []byte(meta("a", "/work/kit", `"vscode"`)+"\n"+user("帮我审阅 Kit")+"\n"), 0o644)
	os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(`{"id":"a","thread_name":"审阅 Kit"}`+"\n"), 0o644)

	got := LocalSessions()
	if len(got) != 1 {
		t.Fatalf("应只列出 a，得到 %+v", got)
	}
	s := got[0]
	if s.ID != "a" || s.Title != "审阅 Kit" || s.First != "帮我审阅 Kit" || s.Dir != "/work/kit" || s.Harness != "codex" {
		t.Fatalf("会话 a 读错了：%+v", s)
	}
}

// pi 的会话：头一行给 id 和工作目录，第一条用户消息是开头；morula 派出去的会话跳过。
func TestReadPiSession(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.jsonl")
	os.WriteFile(p, []byte(`{"type":"session","id":"s1","cwd":"/w"}
{"type":"message","message":{"role":"assistant","content":"在"}}
{"type":"message","message":{"role":"user","content":[{"type":"text","text":"帮我看日志"}]}}
`), 0o644)
	s, ok := readPiSession(p)
	if !ok || s.ID != "s1" || s.Dir != "/w" || s.First != "帮我看日志" {
		t.Fatalf("读出 %+v %v", s, ok)
	}
	os.WriteFile(p, []byte(`{"type":"session","id":"s2","cwd":"/w"}
{"type":"message","message":{"role":"user","content":"\"你是 morula 的助理"}}
`), 0o644)
	if _, ok := readPiSession(p); ok {
		t.Fatal("morula 派出去的会话应跳过")
	}
}

// WorkBuddy 的会话：标题取 ai-title，去掉 <system-reminder>，取出 <user_query> 里的话，斜杠命令跳过。
func TestReadWorkbuddySession(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "abc.jsonl")
	os.WriteFile(p, []byte(`{"type":"message","role":"user","cwd":"/w","content":[{"type":"input_text","text":"<command-name>/login</command-name>"}]}
{"type":"message","role":"user","cwd":"/w","content":[{"type":"input_text","text":"<system-reminder>规则</system-reminder><user_query>整理周报</user_query>"}]}
{"type":"ai-title","aiTitle":"周报"}
`), 0o644)
	s, ok := readWorkbuddySession(p)
	if !ok || s.ID != "abc" || s.Dir != "/w" || s.Title != "周报" || s.First != "整理周报" {
		t.Fatalf("读出 %+v %v", s, ok)
	}
}

// Chosen 去掉没勾的工具；选过模型的工具只留勾上的模型。
func TestChosen(t *testing.T) {
	list := []Info{{Name: "codex", Models: []Model{{ID: "a"}, {ID: "b"}}}, {Name: "pi", Models: []Model{{ID: "c"}}}, {Name: "hermes"}}
	got := Chosen(list, []string{"codex", "pi"}, map[string][]string{"codex": {"b"}})
	if len(got) != 2 || len(got[0].Models) != 1 || got[0].Models[0].ID != "b" || len(got[1].Models) != 1 {
		t.Fatalf("得到 %+v", got)
	}
	if len(list[0].Models) != 2 {
		t.Fatal("改到了原来的列表")
	}
}
