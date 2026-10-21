package work

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"morula/internal/chat"
	"morula/internal/harness"
	"morula/internal/journal"
	"morula/internal/store"
)

func activityOf(s *chat.Session, group string) []chat.Event {
	var out []chat.Event
	_, evs, _ := s.Poll(context.Background(), -1, 0, 0)
	for _, e := range evs {
		if e["type"] == "activity" && e["group"] == group {
			out = append(out, e)
		}
	}
	return out
}

// 助理的话先压着：后面还有步骤时才显示成过程；成功收尾时最后一句是回复，不显示；失败时照常显示。
func TestActivity(t *testing.T) {
	e := staffEngine(t)
	x := e.newActivity(context.Background(), "", nil)
	x.on(harness.Event{Kind: "message", Text: "我先查一下。"})
	x.on(harness.Event{Kind: "tool", Tool: "list_projects", Args: "{}"})
	x.on(harness.Event{Kind: "tool_done", Tool: "list_projects", Text: "无"})
	x.on(harness.Event{Kind: "message", Text: "这是回复"})
	x.end("done", true, 0)
	x.on(harness.Event{Kind: "thinking", Text: "结束后不该再记"})
	var kinds []string
	for _, ev := range activityOf(e.Chat, x.group) {
		kinds = append(kinds, ev["kind"].(string))
		if ev["text"] == "这是回复" {
			t.Error("回复不该显示成过程")
		}
	}
	if got := len(kinds); got != 5 || kinds[1] != "message" || kinds[4] != "end" {
		t.Fatalf("过程 = %v", kinds)
	}
	if end := activityOf(e.Chat, x.group)[4]; end["steps"] != 2 || end["state"] != "done" {
		t.Errorf("结束 = %v，步数应为 2（tool_done 不算）", end)
	}

	y := e.newActivity(context.Background(), "", nil)
	y.on(harness.Event{Kind: "message", Text: "做到一半"})
	y.end("failed", false, 0)
	evs := activityOf(e.Chat, y.group)
	if len(evs) != 3 || evs[1]["text"] != "做到一半" || evs[2]["state"] != "failed" {
		t.Errorf("失败时压着的话应显示：%v", evs)
	}
}

// 会话：续跑同一个；被占着时另开一次性的一路；清空或撤回后（gen 变了）不记旧会话，下次新开；换日后新开。
func TestThread(t *testing.T) {
	e := staffEngine(t)
	id, fork, gen, _ := e.takeThread()
	if id != "" || fork {
		t.Fatalf("第一次应新开：%q %v", id, fork)
	}
	if _, fork2, _, _ := e.takeThread(); !fork2 {
		t.Error("会话被占着时应另开一路")
	}
	e.saveThread(gen, "t1")
	e.releaseThread()
	if id, _, _, stale := e.takeThread(); id != "t1" || stale {
		t.Errorf("应续跑 t1，得到 %q %v", id, stale)
	}
	e.releaseThread()
	_, _, gen, _ = e.takeThread()
	e.noteMissed("另一路")
	e.ResetThread()
	e.saveThread(gen, "t2")
	e.releaseThread()
	if id, _, _, _ := e.takeThread(); id != "" {
		t.Errorf("清空后应新开，得到 %q", id)
	}
	if e.takeMissed() != "" {
		t.Error("清空后不该再带会话外的事")
	}
	e.saveThread(e.threadGen, "t4")
	e.saveThread(e.threadGen, "t4")
	e.releaseThread()
	if got := dayThreads(e.DB, journal.Today()); !slices.Equal(got, []string{"t1", "t4"}) {
		t.Errorf("当天的会话列表 = %q，应为 [t1 t4]", got)
	}
	// 一轮跨过换日：会话记在开始那天，换日后的下一条消息新开
	_, _, gen, _ = e.takeThread()
	e.threadDay = "2000-01-02"
	e.saveThread(gen, "t5")
	e.releaseThread()
	if id, _, _, _ := e.takeThread(); id != "" {
		t.Errorf("跨过换日的一轮之后应新开，得到 %q", id)
	}
	e.releaseThread()
	if got := dayThreads(e.DB, "2000-01-02"); !slices.Equal(got, []string{"t5"}) {
		t.Errorf("跨过换日的会话应记在开始那天，得到 %q", got)
	}
	e.DB.SetSetting("assistant.thread", "2000-01-01|old")
	if id, _, _, _ := e.takeThread(); id != "" {
		t.Errorf("换日后应新开，得到 %q", id)
	}
	e.releaseThread()
	type want struct {
		id    string
		stale bool
	}
	for in, w := range map[string]want{
		journal.Today() + "|" + roleVer() + "|t3": {"t3", false},
		journal.Today() + "|旧规则|t3":               {"t3", true}, // 规则改过：照旧续跑，补发新规则
		journal.Today() + "|t3":                   {"t3", true}, // 升级前的格式没记规则版本
	} {
		e.DB.SetSetting("assistant.thread", in)
		if id, _, _, stale := e.takeThread(); id != w.id || stale != w.stale {
			t.Errorf("%q 续跑 %q %v，应为 %q %v", in, id, stale, w.id, w.stale)
		}
		e.releaseThread()
	}
}

// 验收的结论代替 harness 的最后回答（JSON），JSON 不显示成过程。
func TestActivityLine(t *testing.T) {
	e := staffEngine(t)
	x := e.newActivity(context.Background(), "", nil)
	x.on(harness.Event{Kind: "tool", Tool: "git diff"})
	x.on(harness.Event{Kind: "message", Text: `{"pass":false}`})
	x.line("验收结论：不通过")
	x.end("rejected", true, 0)
	evs := activityOf(e.Chat, x.group)
	for _, ev := range evs {
		if ev["text"] == `{"pass":false}` {
			t.Error("最后回答的 JSON 不该显示")
		}
	}
	if n := len(evs); n != 4 || evs[2]["text"] != "验收结论：不通过" || evs[3]["state"] != "rejected" {
		t.Errorf("过程 = %v", evs)
	}
}

func TestAcceptNote(t *testing.T) {
	cases := []struct {
		v      *acceptance
		err    error
		failed bool
		want   string
	}{
		{nil, nil, true, "没有验收"},
		{nil, errors.New("超时"), false, "超时"},
		{nil, nil, false, "没有验收 agent"},
		{&acceptance{Pass: true, Evidence: "跑了测试"}, nil, false, "通过。验收查了：\n跑了测试"},
		{&acceptance{Problems: "没改样式"}, nil, false, "没改样式"},
	}
	for _, c := range cases {
		if got := acceptNote(c.v, c.err, c.failed); !strings.Contains(got, c.want) {
			t.Errorf("acceptNote(%+v, %v, %v) = %q，应含 %q", c.v, c.err, c.failed, got, c.want)
		}
	}
}

// 助理的一句话压了 holdFor 没有下文就先显示；后来它成了回复时，结束事件带 drop。
func TestActivityHoldTimeout(t *testing.T) {
	old := holdFor
	holdFor = 20 * time.Millisecond
	defer func() { holdFor = old }()
	e := staffEngine(t)
	x := e.newActivity(context.Background(), "", nil)
	x.on(harness.Event{Kind: "message", Text: "我读一下这个方案。"})
	time.Sleep(80 * time.Millisecond)
	if evs := activityOf(e.Chat, x.group); len(evs) != 2 || evs[1]["text"] != "我读一下这个方案。" {
		t.Fatalf("压久了应先显示：%v", evs)
	}
	x.on(harness.Event{Kind: "tool", Tool: "read_project"})
	x.on(harness.Event{Kind: "message", Text: "这是回复"})
	time.Sleep(80 * time.Millisecond)
	x.end("done", true, 0)
	evs := activityOf(e.Chat, x.group)
	end := evs[len(evs)-1]
	if end["drop"] != true || end["steps"] != 2 {
		t.Errorf("回复先显示过，结束应带 drop、步数 2：%v", end)
	}

	y := e.newActivity(context.Background(), "", nil)
	y.on(harness.Event{Kind: "message", Text: "马上就结束"})
	y.end("done", true, 0)
	time.Sleep(80 * time.Millisecond)
	if evs := activityOf(e.Chat, y.group); len(evs) != 2 || evs[1]["drop"] != nil {
		t.Errorf("没到时间就结束，回复不显示也不带 drop：%v", evs)
	}
}

// harness 不会自己压缩时，交接记录换会话：下一条消息新开，带上交接记录；新会话记下后交接记录清掉。
func TestHandoff(t *testing.T) {
	e := staffEngine(t)
	_, _, gen, _ := e.takeThread()
	e.saveThread(gen, "t1")
	if e.saveHandoff(gen+1, "t1", "x") || e.saveHandoff(gen, "t0", "x") {
		t.Fatal("清空过对话页、或会话已经换了时不该记交接记录")
	}
	if !e.saveHandoff(gen, "t1", "用户要做成 A") {
		t.Fatal("应记下交接记录")
	}
	e.releaseThread()
	if id, _, _, _ := e.takeThread(); id != "" {
		t.Fatalf("写了交接记录后应新开，得到 %q", id)
	}
	if e.handoff() != "用户要做成 A" {
		t.Fatalf("交接记录 = %q", e.handoff())
	}
	a := &store.Agent{Name: "助理"}
	if p := e.agentPrompt(a, false, false, "下一条"); !strings.Contains(p, "# 上一个会话的交接记录") || !strings.Contains(p, "用户要做成 A") {
		t.Error("新会话的提示词里没有交接记录")
	}
	e.saveThread(gen, "t2")
	e.releaseThread()
	if e.handoff() != "" {
		t.Error("新会话记下后应清掉交接记录")
	}
	if !slices.Equal(dayThreads(e.DB, journal.Today()), []string{"t1", "t2"}) {
		t.Error("两个会话都要留在当天的列表里，翻旧会话用")
	}
	e.DB.SetSetting("assistant.handoff", journal.Today()+"|t2|y")
	e.ResetThread()
	if e.handoff() != "" {
		t.Error("清空对话页后不该再带交接记录")
	}
}
