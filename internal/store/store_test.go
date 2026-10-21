package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestAgentSkills 检查旧库补上 skills、projects 两列，以及新增、修改时读写这两列。
func TestAgentSkills(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MORULA_HOME", dir)
	old, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "morula.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE agents (name TEXT PRIMARY KEY, role TEXT NOT NULL DEFAULT '执行', runtime TEXT NOT NULL,
		model TEXT NOT NULL, effort TEXT NOT NULL DEFAULT '', instructions TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'idle',
		task TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
		INSERT INTO agents (name, runtime, model, created_at, updated_at) VALUES ('旧', 'codex', 'm', '', '')`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, err := db.Agent("旧")
	if err != nil || a == nil || a.Skills == nil || len(a.Skills) != 0 || len(a.Projects) != 0 {
		t.Fatalf("旧 agent：%+v %v", a, err)
	}
	if err := db.AddAgent(Agent{Name: "新", Runtime: "codex", Model: "m", Skills: []string{"s1"}, Projects: []int64{3}}); err != nil {
		t.Fatal(err)
	}
	sk, ps := []string{"s2", "s3"}, []int64{}
	if err := db.UpdateAgent("新", Patch{Skills: &sk, Projects: &ps}); err != nil {
		t.Fatal(err)
	}
	n, _ := db.Agent("新")
	if !slices.Equal(n.Skills, sk) || len(n.Projects) != 0 {
		t.Fatalf("新 agent：%+v", n)
	}
}

// TestAgentStats 检查按 agent 汇总任务：done 算做完，rejected 和 failed 算打回或失败，没署名的任务不算。
func TestAgentStats(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, _ := db.AddProject("一", "", "")
	run, _ := db.AddRun(p.ID, "做事")
	for _, x := range []struct{ agent, status string }{
		{"甲", "done"}, {"甲", "done"}, {"甲", "rejected"}, {"甲", "working"}, {"乙", "failed"}, {"", "done"},
	} {
		id, _ := db.AddTask(Task{RunID: run, Title: "t", Agent: x.agent})
		st := x.status
		db.SetTask(id, TaskPatch{Status: &st})
	}
	got, err := db.AgentStats()
	if err != nil {
		t.Fatal(err)
	}
	if a := got["甲"]; a.Done != 2 || a.Missed != 1 || a.Total != 4 || a.Last == "" {
		t.Fatalf("甲：%+v", a)
	}
	if b := got["乙"]; b.Done != 0 || b.Missed != 1 || b.Total != 1 {
		t.Fatalf("乙：%+v", b)
	}
	if len(got) != 2 {
		t.Fatalf("没署名的任务不应算进去：%+v", got)
	}
}

// TestRework 检查验收打回的一串安排：新安排的 redo_of、rejects 为 0，SetRework 后 Run 和 Runs 都读得到。
func TestRework(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := db.AddProject("P", "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, _ := db.AddRun(p.ID, "改界面")
	second, _ := db.AddRun(p.ID, "改界面")
	if r, err := db.Run(first); err != nil || r == nil || r.RedoOf != 0 || r.Rejects != 0 {
		t.Fatalf("新安排：%+v %v", r, err)
	}
	if err := db.SetRework(second, first, 1); err != nil {
		t.Fatal(err)
	}
	if r, _ := db.Run(second); r.RedoOf != first || r.Rejects != 1 {
		t.Errorf("SetRework 后：%+v", r)
	}
	rs, _ := db.Runs(p.ID, 10)
	if len(rs) != 2 {
		t.Fatalf("Runs = %d 条", len(rs))
	}
	for _, r := range rs {
		if r.ID == second && (r.RedoOf != first || r.Rejects != 1) {
			t.Errorf("Runs 里的打回字段：%+v", r)
		}
	}
	if r, err := db.Run(9999); r != nil || err != nil {
		t.Errorf("没有的安排应返回 nil, nil：%+v %v", r, err)
	}
}

// TestMemoryVersions 检查改记忆时存下旧版本、内容没变不存、能退回、删项目时一并删掉。
func TestMemoryVersions(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := db.AddProject("记忆", "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"一", "二", "二", "三"} {
		if err := db.UpdateProject(p.ID, nil, &m, nil); err != nil {
			t.Fatal(err)
		}
	}
	sum := "只改摘要"
	if err := db.UpdateProject(p.ID, &sum, nil, nil); err != nil {
		t.Fatal(err)
	}
	vs, err := db.MemoryVersions(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 起初的空记忆不存；「二」写了两次只存一次；只改摘要不存
	if len(vs) != 2 || vs[0].Memory != "二" || vs[1].Memory != "一" {
		t.Fatalf("旧版本：%+v", vs)
	}
	if err := db.RestoreMemory(p.ID, vs[1].ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.Project(p.ID); got.Memory != "一" {
		t.Fatalf("退回后的记忆：%q", got.Memory)
	}
	if vs, _ = db.MemoryVersions(p.ID); len(vs) != 3 || vs[0].Memory != "三" {
		t.Fatalf("退回前的内容要存成旧版本：%+v", vs)
	}
	if err := db.RestoreMemory(p.ID+1, vs[0].ID); err == nil {
		t.Fatal("别的项目的旧版本不能退回")
	}
	if err := db.UpdateProject(p.ID+99, nil, &sum, nil); err == nil {
		t.Fatal("没有这个项目时要报错")
	}
	if err := db.RemoveProject(p.ID); err != nil {
		t.Fatal(err)
	}
	if vs, _ = db.MemoryVersions(p.ID); len(vs) != 0 {
		t.Fatalf("删项目后旧版本还在：%+v", vs)
	}
}

// TestMemoryKeep 检查每个项目只留最近 memoryKeep 条旧版本。
func TestMemoryKeep(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := db.AddProject("上限", "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= memoryKeep+5; i++ {
		m := fmt.Sprint("版本", i)
		if err := db.UpdateProject(p.ID, nil, &m, nil); err != nil {
			t.Fatal(err)
		}
	}
	vs, _ := db.MemoryVersions(p.ID)
	if len(vs) != memoryKeep || vs[0].Memory != fmt.Sprint("版本", memoryKeep+4) {
		t.Fatalf("留了 %d 条，最新一条 %q", len(vs), vs[0].Memory)
	}
}

func TestMerge(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, _ := db.AddProject("甲", "", "")
	b, _ := db.AddProject("乙", "", "")
	am, bm := "甲的结论", "乙的结论"
	db.UpdateProject(a.ID, nil, &am, nil)
	db.UpdateProject(b.ID, nil, &bm, nil)
	r1, _ := db.AddRun(a.ID, "一")
	r2, _ := db.AddRun(a.ID, "二")
	db.MarkImported("codex", "s1", a.ID)

	db.SetRun(r2, "status", "working")
	if _, err := db.MergeProjects(a.ID, b.ID); err == nil {
		t.Fatal("甲还有安排在跑，不该并")
	}
	db.SetRun(r1, "status", "done")
	db.SetRun(r2, "status", "done")
	m, err := db.MergeProjects(a.ID, b.ID)
	if err != nil || m.Runs != 2 {
		t.Fatalf("合并：%+v %v", m, err)
	}
	ps, _ := db.Projects()
	if len(ps) != 1 || ps[0].ID != b.ID {
		t.Fatalf("合并后的项目列表：%+v", ps)
	}
	nb, _ := db.Project(b.ID)
	if !strings.HasPrefix(nb.Memory, bm) || !strings.Contains(nb.Memory, am) {
		t.Fatalf("合并后乙的记忆：%q", nb.Memory)
	}
	if runs, _ := db.Runs(b.ID, 10); len(runs) != 2 {
		t.Fatalf("乙的安排：%d", len(runs))
	}
	var pid int64
	db.db.QueryRow(`SELECT project_id FROM imported WHERE session = 's1'`).Scan(&pid)
	if pid != b.ID {
		t.Fatalf("整理记录没挪：%d", pid)
	}
	if last, _ := db.LastMerge(); last == nil || last.ID != m.ID {
		t.Fatalf("最近一次合并：%+v", last)
	}

	if _, kept, err := db.UndoMerge(m.ID); err != nil || kept {
		t.Fatalf("撤销：%v %v", kept, err)
	}
	if _, _, err := db.UndoMerge(m.ID); err == nil {
		t.Fatal("同一次合并撤销了两次")
	}
	nb, _ = db.Project(b.ID)
	if nb.Memory != bm {
		t.Fatalf("撤销后乙的记忆：%q", nb.Memory)
	}
	if runs, _ := db.Runs(a.ID, 10); len(runs) != 2 {
		t.Fatalf("撤销后甲的安排：%d", len(runs))
	}
	db.db.QueryRow(`SELECT project_id FROM imported WHERE session = 's1'`).Scan(&pid)
	if ps, _ := db.Projects(); len(ps) != 2 || pid != a.ID {
		t.Fatalf("撤销后：%d 个项目，整理记录在 %d", len(ps), pid)
	}
	if last, _ := db.LastMerge(); last != nil {
		t.Fatalf("撤销后还有没撤销的合并：%+v", last)
	}

	// 合并后乙的记忆又被改过：撤销时并进来的那段留着
	m, _ = db.MergeProjects(a.ID, b.ID)
	edited := "改过了"
	db.UpdateProject(b.ID, nil, &edited, nil)
	if _, kept, err := db.UndoMerge(m.ID); err != nil || !kept {
		t.Fatalf("记忆被改过时撤销：%v %v", kept, err)
	}

	if old, err := db.RenameProject(a.ID, "丙"); err != nil || old != "甲" {
		t.Fatalf("改名：%q %v", old, err)
	}
	if _, err := db.RenameProject(a.ID, "乙"); err == nil {
		t.Fatal("改成同名项目没报错")
	}
	db.SetRun(r1, "status", "working")
	if err := db.MoveRuns([]int64{r1}, b.ID); err == nil {
		t.Fatal("还在跑的安排被挪了")
	}
	if err := db.MoveRuns([]int64{r2}, b.ID); err != nil {
		t.Fatal(err)
	}
	if runs, _ := db.Runs(b.ID, 10); len(runs) != 1 || runs[0].ID != r2 {
		t.Fatalf("挪后乙的安排：%+v", runs)
	}
}
