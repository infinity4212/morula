package work

import (
	"strings"
	"testing"

	"morula/internal/store"
)

// 有项目负责人时，助理拆的任务合成一个顶层任务交给它；负责人被删后改回由助理兼任。
func TestOwner(t *testing.T) {
	e := staffEngine(t)
	for _, a := range []store.Agent{
		{Name: "负责人", Role: "执行", Runtime: "codex", Model: "m"},
		{Name: "执行-1", Role: "执行", Runtime: "codex", Model: "m"},
	} {
		if err := e.DB.AddAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	p, err := e.DB.AddProject("官网", "官网改版", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if e.SetOwner(p.ID, AcceptName) == nil || e.SetOwner(p.ID, "没有这个人") == nil {
		t.Fatal("固定成员和不在花名册里的名字不能当负责人")
	}
	if err := e.SetOwner(p.ID, "负责人"); err != nil {
		t.Fatal(err)
	}
	p, _ = e.DB.Project(p.ID)
	o := e.owner(p)
	if o == nil || o.Name != "负责人" {
		t.Fatalf("owner = %+v", o)
	}
	tasks := ownerTask(o, p, "改首页", "先改标题再改配色", []taskSpec{
		{Title: "改标题", Brief: "标题改成 X", Agent: "执行-1", Check: "test -f index.html"},
		{Title: "改配色", Brief: "换成蓝色"},
	})
	if len(tasks) != 1 || tasks[0].Agent != "负责人" {
		t.Fatalf("应合成一个给负责人的任务：%+v", tasks)
	}
	for _, want := range []string{"改标题", "建议派给「执行-1」", "test -f index.html", "改配色", "先改标题再改配色", "split"} {
		if !strings.Contains(tasks[0].Brief, want) {
			t.Errorf("brief 缺「%s」：\n%s", want, tasks[0].Brief)
		}
	}
	if one := ownerTask(o, p, "改首页", "", []taskSpec{{Title: "改", Agent: "负责人"}}); one[0].Title != "改" {
		t.Error("已经派给负责人的单个任务应照原样")
	}
	if !strings.Contains(projectBlock(p, false), "项目负责人：负责人") || !strings.Contains(ProjectList(e.DB), "项目负责人「负责人」") {
		t.Error("项目信息里应写明负责人")
	}
	if err := e.DB.RemoveAgent("负责人"); err != nil {
		t.Fatal(err)
	}
	p, _ = e.DB.Project(p.ID)
	if p.Owner != "" || e.owner(p) != nil {
		t.Errorf("负责人删掉后应改回由助理兼任：%q", p.Owner)
	}
}
