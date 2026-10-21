package work

import (
	"context"
	"slices"
	"strconv"
	"testing"

	"morula/internal/store"
)

// 一条进了项目的消息：判断归属 → 安排 → 任务树（父任务拆出子任务）→ 检查汇报 → 审回复 → 你。
func TestFlowsMergeTasks(t *testing.T) {
	e := staffEngine(t)
	p, err := e.DB.AddProject("morula", "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := e.newFlow(context.Background(), "改一下 Agent 页")
	routed := e.addStep(f, "助理", "判断归属", "u")
	e.setStep(f, routed, "done")
	ctx := withFlow(context.Background(), f, routed)
	plan := e.addStep(f, "助理", "安排", routed)
	e.setStep(f, plan, "done")
	run, _ := e.DB.AddRun(p.ID, "改一下 Agent 页")
	f.Run, f.plan = run, plan
	e.setFlowProject(f, p)
	top, _ := e.DB.AddTask(store.Task{RunID: run, Depth: 1, Title: "改界面", Agent: "前端"})
	kid, _ := e.DB.AddTask(store.Task{RunID: run, ParentID: top, Depth: 2, Title: "改样式", Agent: "前端"})
	e.DB.SetTask(kid, store.TaskPatch{Status: store.Ptr("done")})
	other, _ := e.DB.AddTask(store.Task{RunID: run, Depth: 1, Title: "写文档", Agent: "文案"})

	got := e.Flows(p.ID)
	if len(got) != 1 {
		t.Fatalf("flows = %d", len(got))
	}
	by := map[string]Step{}
	for _, s := range got[0].Steps {
		by[s.ID] = s
	}
	id := func(n int64) string { return "t" + itoa(n) }
	if s := by[id(top)]; s.State != "working" || !slices.Equal(s.From, []string{plan}) {
		t.Errorf("父任务 = %+v，要 working、连着安排", s)
	}
	if s := by[id(kid)]; s.State != "done" || !slices.Equal(s.From, []string{id(top)}) {
		t.Errorf("子任务 = %+v，要 done、连着父任务", s)
	}
	if s := by[id(other)]; s.State != "waiting" {
		t.Errorf("没开始的任务 = %+v，要 waiting", s)
	}
	if len(e.Flows(p.ID+1)) != 0 {
		t.Error("别的项目不该看到这条流程")
	}

	// 检查汇报连着所有顶层任务；「你」从它接下去
	finish := e.addStep(f, "助理", finishWhat)
	posOf(ctx).tip = finish
	e.say(ctx, "收到汇报", "做完了", nil)
	e.endFlow(f, "done")
	steps := e.Flows(0)[0].Steps
	last := map[string]Step{}
	for _, s := range steps {
		last[s.What] = s
	}
	if s := last[finishWhat]; !slices.Equal(s.From, []string{id(top), id(other)}) || s.State != "done" {
		t.Errorf("检查汇报 = %+v", s)
	}
	if s := last["收到汇报"]; !slices.Equal(s.From, []string{finish}) || s.State != "done" {
		t.Errorf("收到汇报 = %+v", s)
	}
	for _, s := range steps {
		if s.Task == other && s.State != "waiting" {
			t.Errorf("流程结束后没开始的任务仍是 waiting，得到 %s", s.State)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
