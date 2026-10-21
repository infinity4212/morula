package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeLines(t *testing.T, p string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	var b []byte
	for _, l := range lines {
		b = append(b, l+"\n"...)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadChat(t *testing.T) {
	d := t.TempDir()
	codex := filepath.Join(d, "codex.jsonl")
	writeLines(t, codex,
		`{"type":"session_meta","payload":{"id":"c1","cwd":"/proj/a","source":"cli"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>x</environment_context>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"把登录页改成深色"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"shell"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"改好了，还没跑测试"}]}}`)
	c, ok := readChat(codex)
	if !ok || c.ID != "c1" || c.Dir != "/proj/a" || c.Users != 1 || c.First != "把登录页改成深色" || c.LastReply != "改好了，还没跑测试" {
		t.Fatalf("codex: %v %+v", ok, c)
	}

	sub := filepath.Join(d, "sub.jsonl")
	writeLines(t, sub, `{"type":"session_meta","payload":{"id":"c2","source":{"subagent":"x"}}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":"做点事"}}`)
	if _, ok := readChat(sub); ok {
		t.Fatal("子 agent 的会话应跳过")
	}

	cc := filepath.Join(d, "cc.jsonl")
	writeLines(t, cc,
		`{"type":"summary","summary":"整理发票脚本"}`,
		`{"type":"user","sessionId":"s1","cwd":"/proj/b","message":{"role":"user","content":"<system-reminder>规则</system-reminder>写个发票汇总脚本"}}`,
		`{"type":"user","sessionId":"s1","isMeta":true,"message":{"role":"user","content":"元信息"}}`,
		`{"type":"user","sessionId":"s1","toolUseResult":{"x":1},"message":{"role":"user","content":[{"type":"text","text":"工具输出"}]}}`,
		`{"type":"user","sessionId":"s1","message":{"role":"user","content":"[Request interrupted by user]"}}`,
		`{"type":"user","sessionId":"s1","message":{"role":"user","content":"Tool loaded."}}`,
		`{"type":"assistant","sessionId":"s1","message":{"role":"assistant","content":[{"type":"thinking","thinking":"想"},{"type":"text","text":"脚本写好了"}]}}`,
		`{"type":"user","sessionId":"s1","message":{"role":"user","content":"再加按月汇总"}}`)
	c, ok = readChat(cc)
	if !ok || c.ID != "s1" || c.Title != "整理发票脚本" || c.Users != 2 || c.First != "写个发票汇总脚本" ||
		c.LastUser != "再加按月汇总" || c.LastReply != "脚本写好了" {
		t.Fatalf("claude 格式: %v %+v", ok, c)
	}

	wb := filepath.Join(d, "wb.jsonl")
	writeLines(t, wb, `{"role":"user","content":"<user_query>查一下周报</user_query>"}`, `{"role":"assistant","content":"周报在共享盘"}`)
	if c, ok = readChat(wb); !ok || c.First != "查一下周报" || c.Title != "查一下周报" {
		t.Fatalf("顶层格式: %v %+v", ok, c)
	}

	for name, body := range map[string]string{
		"log.jsonl":    `{"level":"info","msg":"started"}`,
		"text.jsonl":   `not json`,
		"morula.jsonl": `{"role":"user","content":"你是 morula 的执行者"}`,
	} {
		p := filepath.Join(d, name)
		writeLines(t, p, body)
		if _, ok := readChat(p); ok {
			t.Fatalf("%s 不应算对话记录", name)
		}
	}
}

func TestRecentChats(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	msg := func(dir, text string) string {
		return `{"role":"user","cwd":"` + dir + `","content":"` + text + `"}`
	}
	writeLines(t, filepath.Join(home, ".codex/sessions/old.jsonl"), msg("/proj/a", "旧的"))
	writeLines(t, filepath.Join(home, "work/new.jsonl"), msg("/proj/b", "新的"))
	writeLines(t, filepath.Join(home, "work/tmp.jsonl"), msg("/private/tmp/x", "临时目录"))
	writeLines(t, filepath.Join(home, "node_modules/x/dep.jsonl"), msg("/proj/c", "依赖"))
	writeLines(t, filepath.Join(home, "data/own.jsonl"), msg("/proj/d", "morula 自己的"))
	now := time.Now()
	os.Chtimes(filepath.Join(home, ".codex/sessions/old.jsonl"), now.Add(-time.Hour), now.Add(-time.Hour))

	got := RecentChats(context.Background(), 10, filepath.Join(home, "data"))
	if len(got) != 2 || got[0].First != "新的" || got[0].Source != "work" || got[1].Source != ".codex" {
		t.Fatalf("%+v", got)
	}
	if got := RecentChats(context.Background(), 1, filepath.Join(home, "data")); len(got) != 1 || got[0].First != "新的" {
		t.Fatalf("n=1: %+v", got)
	}
}

func TestFindChats(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeLines(t, filepath.Join(home, ".codex/sessions/a.jsonl"),
		`{"role":"user","cwd":"/proj/a","content":"帮我看看 Helios 为什么打不开"}`,
		`{"role":"assistant","content":"Helios 的签名过期了"}`)
	writeLines(t, filepath.Join(home, ".codex/sessions/m.jsonl"),
		`{"role":"user","cwd":"/proj/b","content":"你是 morula 团队里的子 agent「执行-1」。把 HELIOS 的图标换掉"}`)
	writeLines(t, filepath.Join(home, ".codex/sessions/x.jsonl"), `{"role":"user","content":"别的事"}`)

	got := FindChats(context.Background(), "helios", "", 10)
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	var morula, plain *Chat
	for i := range got {
		if got[i].Morula {
			morula = &got[i]
		} else {
			plain = &got[i]
		}
	}
	if morula == nil || plain == nil || plain.Dir != "/proj/a" || plain.Hit == "" || morula.Hit == "" || morula.Agent != "执行-1" {
		t.Fatalf("%+v", got)
	}
	for text, want := range map[string]string{
		"你是 morula 里的助理。用户是管理者": "助理", "你是 morula 团队里的验收 agent「验收」。": "验收",
		"你是 morula 团队里的 agent「说人话」。": "说人话", "你是 morula 团队里的子 agent「张三」，项目「x」的项目负责人。": "张三",
	} {
		if m := agentRe.FindStringSubmatch(text); m == nil || orStr(m[2], m[1]) != want {
			t.Errorf("%s：%q", text, m)
		}
	}
	if got := FindChats(context.Background(), "", "执行-1", 10); len(got) != 1 || got[0].Agent != "执行-1" {
		t.Fatalf("按 agent 找：%+v", got)
	}
	if got := FindChats(context.Background(), "helios", "验收", 10); len(got) != 0 {
		t.Fatalf("别的 agent：%+v", got)
	}
	if got := FindChats(context.Background(), "  ", "", 10); got != nil {
		t.Fatalf("空关键词：%+v", got)
	}
}
