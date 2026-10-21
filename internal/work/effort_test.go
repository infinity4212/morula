package work

import (
	"strings"
	"testing"

	"morula/internal/store"
)

// 思考强度：新建执行者用 medium；派活时定的要模型支持（模型下架时按换上的核对），不支持的拒收；开工时执行者的模型不支持就用它自己的设置。
func TestEffort(t *testing.T) {
	e := staffEngine(t)
	as, _ := e.assistant()
	if got := workerEffort(as); got != "medium" {
		t.Fatalf("workerEffort = %q", got)
	}
	if got := workerEffort(&store.Agent{Runtime: "codex", Model: "没有"}); got != "" {
		t.Fatalf("查不到模型时用模型默认，得到 %q", got)
	}
	e.DB.AddAgent(store.Agent{Name: "甲", Role: "执行", Runtime: "codex", Model: "m", Effort: "high"})
	e.DB.AddAgent(store.Agent{Name: "乙", Role: "执行", Runtime: "codex", Model: "没有", Effort: "high"})
	cases := []struct {
		t    taskSpec
		want string // 拒收原因里要有的字，空表示通过
	}{
		{taskSpec{Agent: "甲"}, ""},
		{taskSpec{Agent: "甲", Effort: "low"}, ""},
		{taskSpec{Agent: "甲", Effort: "max"}, "可选：low、medium、high"},
		{taskSpec{Agent: "乙", Effort: "low"}, ""}, // 乙的模型下架了，按换上的 m 核对
		{taskSpec{Agent: "乙", Effort: "max"}, "（m）不支持"},
		{taskSpec{Effort: " low "}, ""},
		{taskSpec{Agent: "不存在", Effort: "ultra"}, "助理的模型"},
	}
	for _, c := range cases {
		got := e.checkEffort(c.t)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("checkEffort(%+v) = %q，应含 %q", c.t, got, c.want)
		}
	}
	a, _ := e.DB.Agent("甲")
	b, _ := e.DB.Agent("乙")
	for _, c := range []struct {
		a         *store.Agent
		want, got string
	}{{a, "low", "low"}, {a, "", "high"}, {a, "max", "high"}, {b, "low", "low"}, {b, "", "high"}} { // 乙的模型不在列表里，按换上的 m 核对
		if got := taskEffort(c.a, c.want); got != c.got {
			t.Errorf("taskEffort(%s, %q) = %q，应为 %q", c.a.Name, c.want, got, c.got)
		}
	}
}

// 花名册里的模型下架后，这次改用列表里的第一个；不支持的思考强度用模型默认；提示只发一次。
func TestLiveModel(t *testing.T) {
	staffEngine(t)
	var notes []string
	old := onSwap
	onSwap = func(agent, from, to string) { notes = append(notes, agent+":"+from+">"+to) }
	defer func() { onSwap = old }()
	gone := &store.Agent{Name: "丙", Runtime: "codex", Model: "下架了", Effort: "max"}
	for i := 0; i < 2; i++ {
		if m, ef := liveModel(gone); m != "m" || ef != "" {
			t.Fatalf("liveModel = %q %q，应为 m 和空", m, ef)
		}
	}
	if len(notes) != 1 || notes[0] != "丙:下架了>m" {
		t.Errorf("提示应只发一次：%v", notes)
	}
	ok := &store.Agent{Name: "丁", Runtime: "codex", Model: "m", Effort: "high"}
	if m, ef := liveModel(ok); m != "m" || ef != "high" {
		t.Errorf("模型在列表里时照原样：%q %q", m, ef)
	}
	if got := taskEffort(gone, ""); got != "" {
		t.Errorf("换上的模型不支持 max，taskEffort 应为空，得到 %q", got)
	}
}
