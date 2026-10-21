package work

import (
	"strconv"
	"strings"
	"testing"
)

// 默认项目：第一次用到时建「日常」，只建一次；已有同名项目时用它；指向的项目没了不再重建；列表里标出默认项目。
func TestDefaultProject(t *testing.T) {
	e := staffEngine(t)
	d := e.DefaultProject()
	if d == nil || d.Name != DefaultProjectName {
		t.Fatalf("应建好「%s」，得到 %+v", DefaultProjectName, d)
	}
	if again := e.DefaultProject(); again == nil || again.ID != d.ID {
		t.Fatalf("第二次应返回同一个项目，得到 %+v", again)
	}
	if ps, _ := e.DB.Projects(); len(ps) != 1 {
		t.Fatalf("只应有 1 个项目，得到 %d 个", len(ps))
	}
	if got := ProjectList(e.DB); !strings.Contains(got, "「"+DefaultProjectName+"」（默认项目") {
		t.Errorf("项目列表里没标默认项目：\n%s", got)
	}

	o, err := e.CreateProject("官网", "改官网")
	if err != nil {
		t.Fatal(err)
	}
	e.SetDefaultProject(o.ID)
	if got := e.DefaultProject(); got == nil || got.ID != o.ID {
		t.Fatalf("设成默认后应返回「官网」，得到 %+v", got)
	}
	if got := ProjectList(e.DB); strings.Contains(got, "「"+DefaultProjectName+"」（默认项目") || !strings.Contains(got, "「官网」（默认项目") {
		t.Errorf("默认标记应跟着换：\n%s", got)
	}

	e.SetDefaultProject(o.ID + 100) // 指向的项目不存在
	if got := e.DefaultProject(); got != nil {
		t.Fatalf("默认项目不存在时应返回 nil，不重建，得到 %+v", got)
	}
}

// 已有同名的「日常」时不再建一个，直接用它。
func TestDefaultProjectExisting(t *testing.T) {
	e := staffEngine(t)
	p, err := e.CreateProject(DefaultProjectName, "用户自己建的")
	if err != nil {
		t.Fatal(err)
	}
	d := e.DefaultProject()
	if d == nil || d.ID != p.ID {
		t.Fatalf("应用已有的「%s」，得到 %+v", DefaultProjectName, d)
	}
	if e.DB.Setting("project.default", "") != strconv.FormatInt(p.ID, 10) {
		t.Fatal("设置里应记下已有项目的编号")
	}
}
