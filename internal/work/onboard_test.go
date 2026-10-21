package work

import (
	"strings"
	"testing"
	"time"

	"morula/internal/harness"
)

// 首次引导估算的字数要和整理真正发出的提示词一致，编号要能换回会话。
func TestOrganizePrompt(t *testing.T) {
	e := staffEngine(t)
	a, err := e.assistant()
	if err != nil {
		t.Fatal(err)
	}
	batch := []harness.Local{
		{Harness: "codex", ID: "c1", Title: "修登录", First: "登录页报错", Dir: "/w/app", Updated: time.Now()},
		{Harness: "opencode", ID: "o2", Title: "闲聊", First: "你好", Updated: time.Now()},
	}
	p, keys, err := e.organizePrompt(a, batch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, "s1 | codex |") || !strings.Contains(p, "修登录 | 登录页报错") || !strings.Contains(p, "（还没有项目）") {
		t.Fatalf("提示词缺内容：\n%s", p)
	}
	if keys["s1"] != "codex/c1" || keys["s2"] != "opencode/o2" {
		t.Fatalf("keys = %v", keys)
	}
	ps, chats, err := e.organized([]Proposal{{Name: "登录", Sessions: []string{"s1", "s9"}}, {Name: "空", Sessions: []string{"s1"}}}, []string{"s2", "s2"}, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || len(ps[0].Sessions) != 1 || ps[0].Sessions[0] != "codex/c1" || len(chats) != 1 {
		t.Fatalf("ps = %+v chats = %v", ps, chats)
	}
	// 本机没有会话时（HOME 指到临时目录）估算为 0 个
	c, err := e.OrganizeCost()
	if err != nil || c.Sessions != 0 {
		t.Fatalf("cost = %+v err = %v", c, err)
	}
}
