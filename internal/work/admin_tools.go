package work

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"morula/internal/chat"
	"morula/internal/store"
)

// 助理管例行事项和删项目的工具。新建、修改不问用户；删除，和把开着的例行事项关掉，先发确认卡片。

type routineArgs struct {
	ID        int64  `json:"id,omitempty" jsonschema:"要改的例行事项编号，list_routines 里列着；新建时填 0"`
	Name      string `json:"name,omitempty" jsonschema:"名字，简短；新建时必填，修改时不改填空串"`
	Prompt    string `json:"prompt,omitempty" jsonschema:"到点时发给助理的原话，写清要做什么、结果怎么写；新建时必填，修改时不改填空串"`
	Days      []int  `json:"days,omitempty" jsonschema:"星期几，0 是星期日，1 到 6 是星期一到星期六；每天填 0 到 6 全部，工作日填 1 到 5。新建时不填算每天，修改时不改填空列表"`
	At        string `json:"at,omitempty" jsonschema:"几点，写成 08:30 这样的 24 小时制；新建时必填，修改时不改填空串"`
	Agent     string `json:"agent,omitempty" jsonschema:"交给哪个 agent 做，填团队名单里的名字；由你自己处理填「助理」。新建时不填算助理，修改时不改填空串"`
	ProjectID int64  `json:"project_id,omitempty" jsonschema:"做完的结果放进哪个项目；不指定填 0。修改时填 0 不改，填 -1 改成不指定"`
	Enabled   string `json:"enabled,omitempty" jsonschema:"on 打开、off 关掉；新建时不填算打开，修改时不改填空串。把开着的关掉会先发确认卡片"`
}

type routineIDArgs struct {
	ID     int64  `json:"id" jsonschema:"例行事项编号，list_routines 里列着"`
	Reason string `json:"reason" jsonschema:"为什么删，一句话，显示在确认卡片上"`
}

type removeProjectArgs struct {
	ProjectID int64  `json:"project_id" jsonschema:"要删的项目编号"`
	Reason    string `json:"reason" jsonschema:"按用户的原意说明为什么删，一句话，显示在确认卡片上"`
}

var weekdays = []string{"日", "一", "二", "三", "四", "五", "六"}

// daysText 把星期几的位写成「每天」「工作日」「周一、周三」。
func daysText(d int) string {
	switch d & 127 {
	case 127:
		return "每天"
	case 62:
		return "工作日"
	case 65:
		return "周末"
	}
	var out []string
	for i, w := range weekdays {
		if d&(1<<i) != 0 {
			out = append(out, "周"+w)
		}
	}
	return strings.Join(out, "、")
}

// RoutineList 是给助理看的例行事项清单。
func RoutineList(e *Engine) string {
	rs, err := e.DB.Routines()
	if err != nil {
		return "读不到例行事项：" + err.Error()
	}
	if len(rs) == 0 {
		return "还没有例行事项"
	}
	var b strings.Builder
	for _, r := range rs {
		st := "开着"
		if !r.Enabled {
			st = "关着"
		}
		who := r.Agent
		if who == "" {
			who = "助理"
		}
		fmt.Fprintf(&b, "- 编号 %d「%s」（%s）：%s %s，交给 %s", r.ID, r.Name, st, daysText(r.Days), r.At, who)
		if r.ProjectID != 0 {
			if p, _ := e.DB.Project(r.ProjectID); p != nil {
				fmt.Fprintf(&b, "，放进项目「%s」（编号 %d）", p.Name, p.ID)
			}
		}
		fmt.Fprintf(&b, "\n  原话：%s\n", clip(oneLine(r.Prompt), 200))
		if r.Last != nil {
			fmt.Fprintf(&b, "  最近一次：%s，%s\n", r.Last.StartedAt, r.Last.Status)
		}
	}
	return b.String()
}

// confirm 发一张确认卡片。用户点了 go 时 ok 为 true；否则 why 是给助理的说明。
func (r *agentRun) confirm(ctx context.Context, text, reason, yes, no string, items []string) (ok bool, why string) {
	ans, err := r.e.Chat.Wait(ctx, chat.Event{"type": "ask", "text": text, "reason": reason, "items": items, "free": true,
		"options": []map[string]string{{"value": "go", "label": yes}, {"value": "cancel", "label": no}}})
	switch {
	case err != nil:
		return false, "卡片已失效，用户没有回答，什么都没改"
	case strings.TrimSpace(ans.Text) != "":
		return false, "用户没确认，写了：" + strings.TrimSpace(ans.Text) + "。什么都没改"
	case ans.Choice != "go":
		return false, "用户选了「" + no + "」，什么都没改"
	}
	return true, ""
}

// emitRoutine 在对话里留一条例行事项的改动记录。
func (r *agentRun) emitRoutine(act string, rt *store.Routine) {
	r.e.Chat.EmitIn(r.ctx, chat.Event{"type": "reorg", "op": "routine", "act": act, "routine": rt.ID, "routineName": rt.Name,
		"when": daysText(rt.Days) + " " + rt.At, "enabled": rt.Enabled})
}

func (r *agentRun) adminTools(s *mcp.Server) {
	e := r.e
	mcp.AddTool(s, &mcp.Tool{Name: "list_routines", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		Description: "列出全部例行事项：编号、名字、开着没有、哪几天几点、交给谁、放进哪个项目、原话和最近一次的结果。"},
		func(context.Context, *mcp.CallToolRequest, noArgs) (*mcp.CallToolResult, any, error) {
			return reply(RoutineList(e)), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "save_routine",
		Description: "新建（id 填 0）或修改一件例行事项：到点由助理照原话处理一件事。新建、修改不问用户，对话里会留一条记录；把开着的事项关掉会先发确认卡片。修改时只填要改的字段。"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in routineArgs) (*mcp.CallToolResult, any, error) {
			var rt store.Routine
			if in.ID != 0 {
				old, _ := e.DB.Routine(in.ID)
				if old == nil {
					return refuse("没有编号为 " + strconv.FormatInt(in.ID, 10) + " 的例行事项，用 list_routines 查编号"), nil, nil
				}
				rt = *old
			} else {
				rt = store.Routine{Days: 127, Enabled: true}
			}
			if t := strings.TrimSpace(in.Name); t != "" {
				rt.Name = t
			}
			if t := strings.TrimSpace(in.Prompt); t != "" {
				rt.Prompt = t
			}
			if t := strings.TrimSpace(in.At); t != "" {
				rt.At = t
			}
			if len(in.Days) > 0 {
				rt.Days = 0
				for _, d := range in.Days {
					if d < 0 || d > 6 {
						return refuse("days 里的 " + strconv.Itoa(d) + " 不对：0 是星期日，1 到 6 是星期一到星期六。什么都没改"), nil, nil
					}
					rt.Days |= 1 << d
				}
			}
			if t := strings.TrimSpace(in.Agent); t != "" {
				if t == "助理" {
					t = ""
				} else if a, _ := e.DB.Agent(t); a == nil {
					return refuse("团队里没有叫「" + t + "」的 agent，用 list_agents 查名字。什么都没改"), nil, nil
				}
				rt.Agent = t
			}
			switch {
			case in.ProjectID == -1:
				rt.ProjectID = 0
			case in.ProjectID != 0:
				p, _ := e.DB.Project(in.ProjectID)
				if p == nil || p.MergedInto != 0 {
					return refuse("没有编号为 " + strconv.FormatInt(in.ProjectID, 10) + " 的项目。什么都没改"), nil, nil
				}
				rt.ProjectID = p.ID
			}
			off := false
			switch strings.TrimSpace(in.Enabled) {
			case "on":
				rt.Enabled = true
			case "off":
				off = rt.Enabled && in.ID != 0
				rt.Enabled = false
			case "":
			default:
				return refuse("enabled 只能填 on、off 或空串。什么都没改"), nil, nil
			}
			if off {
				c, done := r.tie(ctx)
				ok, why := r.confirm(c, "关掉例行事项「"+rt.Name+"」吗？", "", "关掉", "不关",
					[]string{"关掉后到点不再发给助理", "设置都留着，在例行事项页或跟助理说一句就能再打开"})
				done()
				r.step("", "关例行事项")
				if !ok {
					return reply(why), nil, nil
				}
			}
			var saved *store.Routine
			var err error
			act := "update"
			if in.ID == 0 {
				saved, err = e.DB.AddRoutine(rt)
				act = "add"
			} else {
				saved, err = e.DB.UpdateRoutine(rt)
			}
			if err != nil {
				return refuse("没存：" + err.Error()), nil, nil
			}
			if !off {
				r.step("", map[string]string{"add": "新建例行事项", "update": "改例行事项"}[act])
			}
			r.emitRoutine(act, saved)
			return reply(fmt.Sprintf("已存例行事项「%s」，编号 %d：%s %s，%s", saved.Name, saved.ID, daysText(saved.Days), saved.At,
				map[bool]string{true: "开着", false: "关着"}[saved.Enabled])), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "remove_routine",
		Description: "删掉一件例行事项和它的运行记录：先发确认卡片，用户确认后才删，删了不能撤销。只是暂时不想跑时用 save_routine 关掉。"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in routineIDArgs) (*mcp.CallToolResult, any, error) {
			rt, _ := e.DB.Routine(in.ID)
			if rt == nil {
				return refuse("没有编号为 " + strconv.FormatInt(in.ID, 10) + " 的例行事项，用 list_routines 查编号"), nil, nil
			}
			c, done := r.tie(ctx)
			ok, why := r.confirm(c, "删掉例行事项「"+rt.Name+"」吗？", in.Reason, "删掉", "不删",
				[]string{daysText(rt.Days) + " " + rt.At + " 的这件事不再跑", "它的运行记录一起删掉，删了不能撤销", "只想暂时停一下时选「不删」，让助理关掉"})
			done()
			r.step("", "删例行事项")
			if !ok {
				return reply(why), nil, nil
			}
			if err := e.DB.RemoveRoutine(rt.ID); err != nil {
				return refuse("没删：" + err.Error()), nil, nil
			}
			r.emitRoutine("remove", rt)
			return reply("已删掉例行事项「" + rt.Name + "」"), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "delete_project",
		Description: "删掉一个项目在 morula 里的记录（安排、任务、项目记忆）：先发确认卡片，用户确认后才删，删了不能撤销。工作目录里的文件不动。默认项目和有安排在跑的项目删不了。两个项目是一件事时用 merge_projects，不要删。"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in removeProjectArgs) (*mcp.CallToolResult, any, error) {
			p, _ := e.DB.Project(in.ProjectID)
			if p == nil || p.MergedInto != 0 {
				return refuse("没有编号为 " + strconv.FormatInt(in.ProjectID, 10) + " 的项目，用 list_projects 查编号"), nil, nil
			}
			if d := e.DefaultProject(); d != nil && d.ID == p.ID {
				return refuse("「" + p.Name + "」是默认项目，删不了。什么都没删"), nil, nil
			}
			runs, _ := e.DB.Runs(p.ID, 1000)
			for _, x := range runs {
				if x.Status == "working" {
					return refuse(fmt.Sprintf("「%s」的安排 %d 还在跑，等它结束或停掉后再删。什么都没删", p.Name, x.ID)), nil, nil
				}
			}
			items := []string{
				fmt.Sprintf("删掉 morula 里的 %d 次安排、它们的任务和项目记忆，删了不能撤销", len(runs)),
				"项目文件夹 " + p.Dir + " 里的文件不动",
			}
			rs, _ := e.DB.Routines()
			var hit []store.Routine
			for _, x := range rs {
				if x.ProjectID == p.ID {
					hit = append(hit, x)
					items = append(items, "例行事项「"+x.Name+"」原来放进这个项目，删后改成不指定项目")
				}
			}
			c, done := r.tie(ctx)
			ok, why := r.confirm(c, "删掉项目「"+p.Name+"」吗？", in.Reason, "删掉", "不删", items)
			done()
			r.step("", "删项目")
			if !ok {
				return reply(why), nil, nil
			}
			if err := e.DB.RemoveProject(p.ID); err != nil {
				return refuse("没删：" + err.Error()), nil, nil
			}
			for _, x := range hit {
				x.ProjectID = 0
				e.DB.UpdateRoutine(x)
			}
			e.Chat.EmitIn(r.ctx, chat.Event{"type": "reorg", "op": "remove", "project": p.ID, "name": p.Name})
			return reply("已删掉项目「" + p.Name + "」。工作目录 " + p.Dir + " 里的文件还在"), nil, nil
		})
}
