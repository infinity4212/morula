package work

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"morula/internal/chat"
	"morula/internal/skills"
	"morula/internal/store"
)

func staffEngine(t *testing.T) *Engine {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MORULA_HOME", filepath.Join(home, "morula"))
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "cx"))
	dir := filepath.Join(home, "cx", "skills", "go")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: go-dev\ndescription: 写 Go\n---\n"), 0o644)
	// 模型 m 支持三档思考强度；只写列表，不会调用模型。
	os.WriteFile(filepath.Join(home, "cx", "auth.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(home, "cx", "models_cache.json"), []byte(`{"models":[{"slug":"m","display_name":"m","visibility":"list",`+
		`"default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}]}]}`), 0o644)
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, a := range []store.Agent{
		{Name: "助理", Role: store.RoleAssistant, Runtime: "codex", Model: "m", Effort: "high"},
		{Name: AcceptName, Role: "验收", Runtime: "codex", Model: "m"},
	} {
		if err := db.AddAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	return New(db, chat.New())
}

// answerAll 替用户回答所有卡片，直到 ctx 结束。
func answerAll(ctx context.Context, s *chat.Session, a chat.Answer) {
	var epoch int64
	n := 0
	for ctx.Err() == nil {
		ep, evs, _ := s.Poll(ctx, epoch, n, 200*time.Millisecond)
		if ep != epoch {
			epoch, n = ep, 0
		}
		for _, ev := range evs {
			n++
			// 上一次调用留下的轮询可能在取消后才返回，不能拿旧答案去答下一次的卡片
			if t, _ := ev["type"].(string); (t == "ask" || t == "permission") && ctx.Err() == nil {
				id, _ := ev["id"].(string)
				s.Answer(id, a)
			}
		}
	}
}

// TestExecutor 检查没指定执行者时的顺序：常用于本项目的 → 通用执行者 → 问用户后新建「执行-N」。
func TestExecutor(t *testing.T) {
	e := staffEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go answerAll(ctx, e.Chat, chat.Answer{Choice: "go"})
	pick := func(name string, project int64) string {
		a, err := e.executor(ctx, name, project)
		if err != nil {
			t.Fatal(err)
		}
		return a.Name
	}
	e.DB.AddAgent(store.Agent{Name: "执行-1", Role: "写作", Runtime: "codex", Model: "m"}) // 占掉名字，不是通用执行者
	if n := pick("", 1); n != "执行-2" {
		t.Fatalf("没有通用执行者时应新建 执行-2，得到 %s", n)
	}
	e.DB.AddAgent(store.Agent{Name: "后端", Role: "开发", Runtime: "codex", Model: "m", Projects: []int64{7}})
	if n := pick("", 7); n != "后端" {
		t.Fatalf("项目 7 应派给 后端，得到 %s", n)
	}
	if n := pick("", 8); n != "执行-2" {
		t.Fatalf("项目 8 应派给通用执行者，得到 %s", n)
	}
	if n := pick("执行-1", 7); n != "执行-1" {
		t.Fatalf("指定的执行者应优先，得到 %s", n)
	}
	if n := pick("助理", 8); n != "执行-2" {
		t.Fatalf("不能派给助理，得到 %s", n)
	}
}

// TestStaff 检查提议的过滤和采用：编出来的 skill、项目丢掉；不碰助理和验收 agent；
// 修改时保留范围外的常用项目；新建默认用助理的配置。
func TestStaff(t *testing.T) {
	e := staffEngine(t)
	p1, _ := e.DB.AddProject("一", "", "")
	p2, _ := e.DB.AddProject("二", "", "")
	e.DB.AddAgent(store.Agent{Name: "老", Role: "开发", Runtime: "codex", Model: "x", Projects: []int64{p2.ID}})
	in := []StaffItem{
		{Action: "new", Name: "写手", Role: "写作", Skills: []string{"go-dev", "编的"}, Projects: []int64{p1.ID, 999}},
		{Action: "update", Name: "老", Role: "开发", Skills: []string{"go-dev"}, Projects: []int64{p1.ID}},
		{Action: "update", Name: "没有的", Role: "开发"},
		{Action: "new", Name: "老", Role: "开发"},
		{Action: "update", Name: "助理", Role: "开发"},
		{Action: "update", Name: AcceptName, Role: "验收"},
		{Action: "new", Name: "写手", Role: "写作"},
	}
	out := e.checkStaff(in, skills.List(), map[int64]bool{p1.ID: true})
	if len(out) != 2 {
		t.Fatalf("应留 2 项，得到 %+v", out)
	}
	if !slices.Equal(out[0].Skills, []string{"go-dev"}) || !slices.Equal(out[0].Projects, []int64{p1.ID}) {
		t.Fatalf("新建项：%+v", out[0])
	}
	if out[1].Old == nil || !slices.Equal(out[1].Projects, []int64{p2.ID, p1.ID}) {
		t.Fatalf("修改项：%+v", out[1])
	}
	e.staff = Staff{State: "ready", Proposals: out}
	out[0].Runtime = ""
	errs, err := e.ApplyStaff(out)
	if err != nil || errs[0] != "" || errs[1] != "" {
		t.Fatalf("apply：%v %v", err, errs)
	}
	w, _ := e.DB.Agent("写手")
	if w == nil || w.Model != "m" || w.Effort != workerDefaultEffort || !slices.Equal(w.Skills, []string{"go-dev"}) {
		t.Fatalf("写手：%+v", w)
	}
	o, _ := e.DB.Agent("老")
	if o.Model != "x" || !slices.Equal(o.Skills, []string{"go-dev"}) || len(o.Projects) != 2 {
		t.Fatalf("老：%+v", o)
	}
	if e.Staffing().State != "idle" {
		t.Fatal("采用后提议应清掉")
	}
}

// TestStaffSignal 检查触发条件：落到通用执行者的任务满 3 个，或同一个 agent 被打回、失败满 3 个。
func TestStaffSignal(t *testing.T) {
	e := staffEngine(t)
	p, _ := e.DB.AddProject("一", "", "")
	e.DB.AddAgent(store.Agent{Name: "执行-1", Role: "执行", Runtime: "codex", Model: "m"})
	e.DB.AddAgent(store.Agent{Name: "专", Role: "开发", Runtime: "codex", Model: "m", Projects: []int64{p.ID}})
	run, _ := e.DB.AddRun(p.ID, "做事")
	add := func(agent, status string, attempts int) {
		id, _ := e.DB.AddTask(store.Task{RunID: run, Title: "t", Agent: agent})
		e.DB.SetTask(id, store.TaskPatch{Status: &status, Attempts: &attempts})
	}
	add("执行-1", "done", 1)
	add("执行-1", "done", 1)
	add("专", "failed", 1)
	add("专", "done", 2)
	if why, _ := e.staffSignal(p.ID); why != "" {
		t.Fatalf("没到次数不应触发：%s", why)
	}
	add("执行-1", "done", 1)
	add("专", "rejected", 3)
	why, tasks := e.staffSignal(p.ID)
	if !strings.Contains(why, "3 个任务落到了通用执行者") || !strings.Contains(why, "「专」有 3 个任务被打回或失败") || len(tasks) != 6 {
		t.Fatalf("应两条都触发：%s，%d 个任务", why, len(tasks))
	}
}

// TestSkillBlock 检查派活提示词写出绑定 skill 的 SKILL.md 路径，本机没有的 skill 跳过。
func TestSkillBlock(t *testing.T) {
	staffEngine(t)
	b := skillBlock([]string{"go-dev", "没有的"})
	s, _ := skills.Find(skills.List(), "go-dev")
	if !strings.Contains(b, filepath.Join(s.Path, "SKILL.md")) || strings.Contains(b, "没有的") {
		t.Fatalf("skill 段：%q", b)
	}
	if skillBlock([]string{"没有的"}) != "" || skillBlock(nil) != "" {
		t.Fatal("没有可用的 skill 时应为空")
	}
}
