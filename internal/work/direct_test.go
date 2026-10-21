package work

import (
	"context"
	"strings"
	"testing"

	"morula/internal/chat"
	"morula/internal/store"
)

func TestMention(t *testing.T) {
	for in, want := range map[string][2]string{
		"@执行-1 看一下":    {"执行-1", "看一下"},
		"＠助理":          {"助理", ""},
		"  @方案追问\n第一行": {"方案追问", "第一行"},
	} {
		name, rest, ok := mention(in)
		if !ok || name != want[0] || rest != want[1] {
			t.Errorf("mention(%q) = %q %q %v", in, name, rest, ok)
		}
	}
	for _, in := range []string{"看一下 @执行-1", "@", "邮件 a@b.c"} {
		if _, _, ok := mention(in); ok {
			t.Errorf("mention(%q) 不该认出名字", in)
		}
	}
}

// 只发 @名字、不带话时开始或结束对话，不调用模型；固定成员和不认识的名字不开始对话。
func TestDirectRoute(t *testing.T) {
	e := staffEngine(t)
	if err := e.DB.AddAgent(store.Agent{Name: "执行-1", Role: "执行", Runtime: "codex", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := e.StartDirect(AcceptName, 0, ""); err == nil {
		t.Error("验收是固定成员，不该能对话")
	}
	text := "@不存在 你好"
	if e.routeDirect(ctx, &text, nil) || e.direct() != nil {
		t.Fatal("不认识的名字应交给助理")
	}
	if e.Chat.Talking() {
		t.Error("没有对话、没人说话时不算正在说话")
	}

	text = "@执行-1"
	if !e.routeDirect(ctx, &text, nil) {
		t.Fatal("@执行-1 应开始对话")
	}
	d := e.direct()
	if d == nil || d.Agent != "执行-1" || d.Project == 0 {
		t.Fatalf("对话 = %+v", d)
	}
	if !e.Chat.Talking() {
		t.Error("对话中应算正在说话，汇报进收件箱")
	}
	if st := e.DirectState(); st.ProjectName == "" || st.Session != "" {
		t.Errorf("给界面的状态 = %+v", st)
	}
	// 同一个人再 @ 一次不新开
	again, err := e.StartDirect("执行-1", 0, "")
	if err != nil || again.Since != d.Since {
		t.Errorf("再 @ 同一个人应沿用：%+v %v", again, err)
	}

	text = "@助理 回来了"
	if e.routeDirect(ctx, &text, nil) || text != "回来了" {
		t.Fatalf("@助理 后面的话应交给助理，得到 %q", text)
	}
	if e.direct() != nil {
		t.Fatal("@助理 应结束对话")
	}
	if e.EndDirect() == nil {
		t.Error("没有对话时结束应报错")
	}

	var on, off int
	_, evs, _ := e.Chat.Poll(ctx, -1, 0, 0)
	for _, ev := range evs {
		if ev["type"] == "direct" {
			if ev["on"] == true {
				on++
			} else {
				off++
			}
		}
	}
	if on != 1 || off != 1 {
		t.Errorf("开始 %d 次、结束 %d 次，应各 1 次", on, off)
	}
}

// 对话状态存在设置里：重开引擎后还在。
func TestDirectPersist(t *testing.T) {
	e := staffEngine(t)
	if err := e.DB.AddAgent(store.Agent{Name: "执行-1", Role: "执行", Runtime: "codex", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartDirect("执行-1", 0, "交接说明"); err != nil {
		t.Fatal(err)
	}
	e2 := New(e.DB, chat.New())
	d := e2.direct()
	if d == nil || d.Agent != "执行-1" || d.Brief != "交接说明" {
		t.Fatalf("重开后 = %+v", d)
	}
	if !e2.Chat.Talking() {
		t.Error("重开后的对话也应算正在说话")
	}
}

func TestDemote(t *testing.T) {
	in := "# 访谈纪要\n\n## 结论\n- a\n### 细节\n```\n# 代码注释\n```\n#不是标题"
	want := "### 结论\n- a\n### 细节\n```\n# 代码注释\n```\n#不是标题"
	if got := demote(in); got != want {
		t.Errorf("demote =\n%s", got)
	}
}

// 对话的会话换掉后：新会话的开场带交接记录，给界面的状态不带。
func TestDirectHandoff(t *testing.T) {
	e := staffEngine(t)
	if err := e.DB.AddAgent(store.Agent{Name: "执行-1", Role: "执行", Runtime: "codex", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	d, err := e.StartDirect("执行-1", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	e.updateDirect(d.Agent, d.Since, func(cur *Direct) { cur.Handoff, cur.Prev, cur.Session = "停在第三个问题", "old", "" })
	d = e.direct()
	a, _ := e.DB.Agent("执行-1")
	if p := e.directPrompt(a, e.directProject(a, d.Project), d); !strings.Contains(p, "停在第三个问题") {
		t.Error("新会话的开场应带交接记录")
	}
	if st := e.DirectState(); st.Handoff != "" || st.Prev != "" {
		t.Errorf("给界面的状态不该带交接记录：%+v", st)
	}
}
