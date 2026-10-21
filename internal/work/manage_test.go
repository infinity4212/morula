package work

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"morula/internal/chat"
	"morula/internal/skills"
	"morula/internal/store"
)

// TestManage 检查管理团队：确认后才改花名册；不能删助理和验收；名字不存在时不改；用户补充时交回补充。
func TestManage(t *testing.T) {
	e := staffEngine(t)
	e.DB.AddAgent(store.Agent{Name: "形象制作", Role: "设计", Runtime: "codex", Model: "m"})
	as, _ := e.assistant()
	run := func(ans chat.Answer, cs ...change) (string, bool) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go answerAll(ctx, e.Chat, ans)
		return e.manage(withFlow(ctx, e.newFlow(context.Background(), "t"), "u"), as, route{Decision: "manage", Actions: cs})
	}
	has := func(name string) bool { a, _ := e.DB.Agent(name); return a != nil }

	run(chat.Answer{Choice: "cancel"}, change{Op: "agent_remove", Target: "形象制作"})
	if !has("形象制作") {
		t.Fatal("用户选了不改，却删了")
	}
	if extra, _ := run(chat.Answer{Text: "先别删，改成只做头像"}, change{Op: "agent_remove", Target: "形象制作"}); extra != "先别删，改成只做头像" || !has("形象制作") {
		t.Fatalf("补充应交回且不执行，得到 %q", extra)
	}
	if _, ok := run(chat.Answer{Choice: "go"}, change{Op: "agent_remove", Target: "形象制作"}); !ok || has("形象制作") {
		t.Fatal("确认后应删掉")
	}
	e.DB.AddAgent(store.Agent{Name: AcceptName, Role: "执行", Runtime: "codex", Model: "m"})
	for _, name := range []string{"助理", AcceptName} {
		run(chat.Answer{Choice: "go"}, change{Op: "agent_remove", Target: name})
		if !has(name) {
			t.Fatalf("不能删固定成员「%s」", name)
		}
	}
	run(chat.Answer{Choice: "go"}, change{Op: "agent_add", Name: "文案", Instructions: "写说明"},
		change{Op: "agent_remove", Target: "没有这个人"})
	if has("文案") {
		t.Fatal("一项改动不能做时，整批都不执行")
	}
	run(chat.Answer{Choice: "go"}, change{Op: "agent_add", Name: "文案", Instructions: "写说明"})
	if a, _ := e.DB.Agent("文案"); a == nil || a.Role != "执行" || a.Runtime != as.Runtime || a.Instructions != "写说明" {
		t.Fatalf("新建的 agent 不对：%+v", a)
	}
	run(chat.Answer{Choice: "go"}, change{Op: "agent_update", Target: "文案", Role: "写作", Skills: []string{}})
	if a, _ := e.DB.Agent("文案"); a.Role != "写作" || a.Instructions != "写说明" {
		t.Fatalf("修改应只改角色：%+v", a)
	}
}

// 管理团队能绑 skill：本机没有的 skill 整批不改；项目目录里装的 skill 也认。
func TestManageSkills(t *testing.T) {
	e := staffEngine(t)
	proj := t.TempDir()
	dir := filepath.Join(proj, ".agents", "skills", "grill-me")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: grill-me\ndescription: 追问方案\n---\n"), 0o644)
	skills.ExtraDirs = func() []string { return []string{filepath.Join(proj, ".agents", "skills")} }
	t.Cleanup(func() { skills.ExtraDirs = nil })
	as, _ := e.assistant()
	run := func(cs ...change) teamResult {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go answerAll(ctx, e.Chat, chat.Answer{Choice: "go"})
		return e.changeTeam(withFlow(ctx, e.newFlow(context.Background(), "t"), "u"), as, cs, "")
	}
	if res := run(change{Op: "agent_add", Name: "追问", Skills: []string{"编的"}}); len(res.Bad) != 1 || !strings.Contains(res.Bad[0], "编的") {
		t.Fatalf("没有的 skill 应拒绝：%+v", res)
	}
	run(change{Op: "agent_add", Name: "追问", Skills: []string{"grill-me", " grill-me"}})
	if a, _ := e.DB.Agent("追问"); a == nil || !slices.Equal(a.Skills, []string{"grill-me"}) {
		t.Fatalf("新建时应绑上 skill：%+v", a)
	}
	run(change{Op: "agent_update", Target: "追问", Skills: []string{"go-dev"}})
	if a, _ := e.DB.Agent("追问"); !slices.Equal(a.Skills, []string{"go-dev"}) {
		t.Fatalf("修改时应整体替换 skill：%+v", a)
	}
}

// 管理团队能定 harness、模型和思考强度：只能选本机能用的 harness、它列出的模型和模型支持的档；
// 不填时新建的和助理相同，修改的不变。
func TestManageSetup(t *testing.T) {
	e := staffEngine(t)
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", bin)
	as, _ := e.assistant()
	run := func(cs ...change) teamResult {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go answerAll(ctx, e.Chat, chat.Answer{Choice: "go"})
		return e.changeTeam(withFlow(ctx, e.newFlow(context.Background(), "t"), "u"), as, cs, "")
	}
	for _, c := range []struct {
		c    change
		want string
	}{
		{change{Op: "agent_add", Name: "甲", Model: "编的"}, "没有模型「编的」，可选：m"},
		{change{Op: "agent_add", Name: "甲", Runtime: "hermes"}, "还不支持"},
		{change{Op: "agent_add", Name: "甲", Runtime: "opencode"}, "不能用"},
		{change{Op: "agent_add", Name: "甲", Effort: "max"}, "不支持思考强度「max」，可选：low、medium、high"},
	} {
		if res := run(c.c); len(res.Bad) != 1 || !strings.Contains(res.Bad[0], c.want) {
			t.Errorf("%+v 应拒绝并含 %q：%+v", c.c, c.want, res.Bad)
		}
	}
	res := run(change{Op: "agent_add", Name: "甲", Runtime: "codex", Model: "m", Effort: "low"})
	if a, _ := e.DB.Agent("甲"); a == nil || a.Model != "m" || a.Effort != "low" || !strings.Contains(strings.Join(res.Lines, ""), "codex · m · 思考强度 low") {
		t.Fatalf("新建的 agent 不对：%+v，%v", a, res.Lines)
	}
	run(change{Op: "agent_add", Name: "乙"})
	if a, _ := e.DB.Agent("乙"); a == nil || a.Runtime != as.Runtime || a.Model != as.Model || a.Effort != "medium" {
		t.Fatalf("不填时应和助理相同、思考强度 medium：%+v", a)
	}
	res = run(change{Op: "agent_update", Target: "甲", Effort: "high"})
	if a, _ := e.DB.Agent("甲"); a.Effort != "high" || !strings.Contains(strings.Join(res.Lines, ""), "改成「codex · m · 思考强度 high」") {
		t.Fatalf("应只改思考强度：%+v，%v", a, res.Lines)
	}
	if res := run(change{Op: "agent_update", Target: "甲", Runtime: "codex", Model: "m", Effort: "high"}); len(res.Bad) != 1 {
		t.Fatalf("和原来一样时应报没有要改的地方：%+v", res)
	}
}
