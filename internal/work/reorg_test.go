package work

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"morula/internal/chat"
)

// 两步方式放项目不问用户：编号对就用，新的事直接建，拿不准用第一个候选，都没有时放默认项目。
func TestPlace(t *testing.T) {
	e := staffEngine(t)
	ctx := context.Background()
	d := e.DefaultProject()
	site, _ := e.CreateProject("官网", "改官网")
	ps, _ := e.DB.Projects()
	cases := []struct {
		r    route
		want string
	}{
		{route{Decision: "project", ProjectID: site.ID}, "官网"},
		{route{Decision: "project", ProjectID: 999}, d.Name},
		{route{Decision: "new", Name: "周报脚本", Summary: "每周的周报"}, "周报脚本"},
		{route{Decision: "new", Name: "官网"}, "官网"}, // 同名的已经有了，用它，不报错
		{route{Decision: "ask", Candidates: []int64{999, site.ID}}, "官网"},
		{route{Decision: "ask"}, d.Name},
	}
	for i, c := range cases {
		p := e.place(ctx, c.r, ps)
		if p == nil || p.Name != c.want {
			t.Errorf("第 %d 个：要放进「%s」，得到 %+v", i+1, c.want, p)
		}
	}
	if n, _ := e.DB.Projects(); len(n) != 3 {
		t.Errorf("应有日常、官网、周报脚本 3 个项目，得到 %d 个", len(n))
	}
}

// 助理的整理工具：建项目不问，改名不问，合并要用户确认，撤销合并、挪安排。
func TestReorgTools(t *testing.T) {
	e := staffEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &agentRun{e: e, ctx: ctx}
	ct, st := mcp.NewInMemoryTransports()
	if _, err := r.server().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) (string, bool) {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res.Content[0].(*mcp.TextContent).Text, res.IsError
	}

	// 没有人回答卡片时 create_project 也直接建好
	if out, bad := call("create_project", map[string]any{"name": "官网改版", "summary": "改官网"}); bad || !strings.Contains(out, "已新建项目「官网改版」") {
		t.Fatalf("建项目：%s", out)
	}
	if out, bad := call("create_project", map[string]any{"name": "官网改版", "summary": "又一个"}); !bad {
		t.Fatalf("同名项目应拒收：%s", out)
	}
	ps, _ := e.DB.Projects()
	site := ps[0]
	if out, bad := call("rename_project", map[string]any{"project_id": site.ID, "name": "官网"}); bad || !strings.Contains(out, "「官网改版」改名为「官网」") {
		t.Fatalf("改名：%s", out)
	}
	other, _ := e.CreateProject("落地页", "")
	run, _ := e.DB.AddRun(other.ID, "改落地页")
	e.DB.SetRun(run, "status", "done")
	def := e.DefaultProject()

	merge := map[string]any{"from": other.ID, "into": site.ID, "reason": "落地页就是官网的一部分"}
	ctx1, cancel1 := context.WithCancel(ctx)
	go answerAll(ctx1, e.Chat, chat.Answer{Choice: "cancel"})
	if out, _ := call("merge_projects", merge); !strings.Contains(out, "不合并") {
		t.Fatalf("用户选不合并：%s", out)
	}
	cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go answerAll(ctx2, e.Chat, chat.Answer{Choice: "go"})
	if out, bad := call("merge_projects", map[string]any{"from": def.ID, "into": site.ID, "reason": "试"}); !bad || !strings.Contains(out, "默认项目") {
		t.Fatalf("默认项目不能并：%s", out)
	}
	if out, bad := call("merge_projects", merge); bad || !strings.Contains(out, "挪过去 1 次安排") {
		t.Fatalf("合并：%s", out)
	}
	if runs, _ := e.DB.Runs(site.ID, 10); len(runs) != 1 {
		t.Fatalf("合并后官网的安排：%d", len(runs))
	}
	if out, bad := call("undo_merge", map[string]any{}); bad || !strings.Contains(out, "1 次安排挪回了原项目") {
		t.Fatalf("撤销合并：%s", out)
	}
	if out, bad := call("undo_merge", map[string]any{}); !bad {
		t.Fatalf("没有能撤销的合并时应拒收：%s", out)
	}
	if out, bad := call("move_runs", map[string]any{"run_ids": []int64{run}, "project_id": site.ID}); bad || !strings.Contains(out, "1 次安排挪到「官网」") {
		t.Fatalf("挪安排：%s", out)
	}
	if runs, _ := e.DB.Runs(site.ID, 10); len(runs) != 1 || runs[0].ID != run {
		t.Fatalf("挪后官网的安排：%+v", runs)
	}

	// 对话里留下的记录：新建、改名、合并、撤销、挪安排各一条
	_, evs, _ := e.Chat.Poll(ctx2, 0, 0, 0)
	var ops []string
	for _, ev := range evs {
		switch ev["type"] {
		case "route":
			ops = append(ops, fmt.Sprintf("route:%v", ev["created"]))
		case "reorg":
			ops = append(ops, fmt.Sprint(ev["op"]))
		}
	}
	if got := strings.Join(ops, " "); got != "route:true rename merge unmerge move" {
		t.Errorf("对话里的记录：%s", got)
	}
}
