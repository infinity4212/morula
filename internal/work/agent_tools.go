package work

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"morula/internal/chat"
	"morula/internal/harness"
	"morula/internal/journal"
	"morula/internal/store"
)

// toolHost 是 morula 进程里的 MCP 服务（streamable HTTP，只监听 127.0.0.1）。每次运行登记一个随机地址，
// 运行结束后作废。工具调用因此直接进正在跑的 morula：能发卡片、建项目、派活。
type toolHost struct {
	mu      sync.Mutex
	base    string
	servers map[string]*mcp.Server
}

// add 登记一个服务器，返回 harness 要连的地址和作废它的函数。第一次调用时开始监听。
func (h *toolHost) add(s *mcp.Server) (string, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.base == "" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", nil, fmt.Errorf("开不了给助理用的工具服务：%w", err)
		}
		h.servers = map[string]*mcp.Server{}
		handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.servers[strings.TrimPrefix(r.URL.Path, "/mcp/")] // 没登记的地址得到 400
		}, nil)
		go http.Serve(ln, handler)
		h.base = "http://" + ln.Addr().String()
	}
	b := make([]byte, 16)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	h.servers[tok] = s
	return h.base + "/mcp/" + tok, func() {
		h.mu.Lock()
		delete(h.servers, tok)
		h.mu.Unlock()
	}, nil
}

// agentRun 是 agent 方式助理处理的一条消息，工具都挂在它上面。
type agentRun struct {
	e      *Engine
	a      *store.Agent
	ctx    context.Context // 这条消息的处理：用户点停止时取消，带着流程图的位置
	text   string          // 用户的消息，含补充；派活时记成这次安排的要求
	rework *rework         // 验收打回后重新安排时非 nil：这次派的活接在那串安排后面

	mu      sync.Mutex
	project int64 // 最近一次用到的项目和派出的安排，回复上标出来
	run     int64
}

func (r *agentRun) where() (int64, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.project, r.run
}

func (r *agentRun) touch(p *store.Project, run int64) {
	r.mu.Lock()
	r.project = p.ID
	if run != 0 {
		r.run = run
	}
	r.mu.Unlock()
	if fp := posOf(r.ctx); fp != nil {
		r.e.setFlowProject(fp.f, p)
	}
}

// tie 是工具里用的 ctx：用户点停止或 harness 断开连接时都结束。不收补充，补充由 agentTurn 停掉整次运行。
func (r *agentRun) tie(req context.Context) (context.Context, func()) {
	c, cancel := context.WithCancel(chat.WithoutSupplements(r.ctx))
	stop := context.AfterFunc(req, cancel)
	return c, func() { stop(); cancel() }
}

// step 在流程图上加一个节点并标成完成。
func (r *agentRun) step(agent, what string) {
	fp := posOf(r.ctx)
	if fp == nil {
		return
	}
	id := r.e.addStep(fp.f, agent, what, fp.tip)
	r.e.setStep(fp.f, id, "done")
	fp.tip = id
}

func reply(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func refuse(s string) *mcp.CallToolResult {
	r := reply(s)
	r.IsError = true
	return r
}

type noArgs struct{}

type projectArgs struct {
	ProjectID int64 `json:"project_id" jsonschema:"项目编号，用 list_projects 查"`
}

type askArgs struct {
	Question string   `json:"question" jsonschema:"要问用户的问题，一句话"`
	Options  []string `json:"options,omitempty" jsonschema:"给用户选的答案，2 到 5 个；用户也可以不选、自己写"`
	Reason   string   `json:"reason,omitempty" jsonschema:"为什么要问，一句话，显示在问题下面"`
}

type createArgs struct {
	Name    string `json:"name" jsonschema:"项目名，按这件事长期要做的工作起，简短"`
	Summary string `json:"summary" jsonschema:"一句话的项目摘要：项目是什么、要做成什么"`
}

type renameArgs struct {
	ProjectID int64  `json:"project_id" jsonschema:"项目编号"`
	Name      string `json:"name" jsonschema:"新名字"`
}

type mergeArgs struct {
	From   int64  `json:"from" jsonschema:"要并掉的项目编号，合并后从项目列表里收起来"`
	Into   int64  `json:"into" jsonschema:"并进去的项目编号，合并后留着"`
	Reason string `json:"reason" jsonschema:"为什么这两个项目是一件事，一句话，显示在确认卡片上"`
}

type undoMergeArgs struct {
	MergeID int64 `json:"merge_id,omitempty" jsonschema:"合并编号，merge_projects 返回的；填 0 时撤销最近一次合并"`
}

type moveArgs struct {
	RunIDs    []int64 `json:"run_ids" jsonschema:"要挪的安排编号，read_project 里列着"`
	ProjectID int64   `json:"project_id" jsonschema:"挪到哪个项目"`
}

type dispatchArgs struct {
	ProjectID int64      `json:"project_id" jsonschema:"项目编号。新项目先用 create_project 建"`
	Plan      string     `json:"plan" jsonschema:"这次安排的说明：要做成什么、为什么这样拆、由谁做"`
	Tasks     []taskSpec `json:"tasks" jsonschema:"1 到 5 个顶层任务。title 一句话；brief 写清目标、产出、验收标准和要用的附件路径；agent 填执行者名字或空串；check 填检查命令或空串；effort 填这个任务的思考强度（low、medium、high 等，要是执行者的模型支持的），用执行者自己的设置时填空串"`
	After     string     `json:"after,omitempty" jsonschema:"这批任务做完后你要接着做的事，例如用装好的 skill 建 agent；morula 汇报后交回给你接着做。没有填空串"`
}

type transferArgs struct {
	Agent     string `json:"agent" jsonschema:"要转接的 agent 名字，从团队名单的子 agent 里选，不能是你自己和验收"`
	ProjectID int64  `json:"project_id,omitempty" jsonschema:"这件事属于的项目编号；填 0 时用这个 agent 常用的第一个项目，没有时用默认项目"`
	Brief     string `json:"brief" jsonschema:"交接说明：用户要做什么、你已经知道的背景和决定、要用的 skill 和文件路径。那个 agent 没看过这段对话，要写到它不用再问你"`
}

type teamArgs struct {
	Actions []change `json:"actions" jsonschema:"逐项写改动。op 是 agent_add、agent_update 或 agent_remove；agent_add 填 name、role（默认「执行」）、instructions，要绑定 skill 时填 skills（skill 的名字）；agent_update 填 target 和要改的 role、instructions、profile、skills（整体替换；不改填空列表）；agent_remove 填 target。agent_add、agent_update 可以填 runtime（harness 名）、model（模型 id）、effort（思考强度），要用 list_harnesses 里能用的 harness、它列出的模型和这个模型支持的思考强度；新建时不填就和助理相同，修改时不填就不改。不用的字段填空串"`
	Reason  string   `json:"reason" jsonschema:"按用户的原意说明为什么这样改，一句话，显示在确认卡片上"`
}

type memoryArgs struct {
	Memory string `json:"memory" jsonschema:"全局记忆的完整新内容，整体替换旧内容：在旧内容上增删，保留仍然有效的，删掉过时或错的"`
	Drop   bool   `json:"drop,omitempty" jsonschema:"这次有意删掉一大半旧内容（例如用户要求清掉）时填 true；平常填 false"`
}

type searchArgs struct {
	Query string `json:"query,omitempty" jsonschema:"要找的关键词，一个词或一个短语，例如项目名、文件名、用户说过的话；只按 agent 找时填空串"`
	Agent string `json:"agent,omitempty" jsonschema:"只找这个 agent 跑过的会话时填它的名字（例如问某个 agent 做到哪了）；不限时填空串"`
}

type updateArgs struct {
	ProjectID int64  `json:"project_id" jsonschema:"项目编号"`
	Memory    string `json:"memory,omitempty" jsonschema:"项目记忆的完整新内容，整体替换旧内容；保留仍然有效的，删掉过时的。不改填空串"`
	Summary   string `json:"summary,omitempty" jsonschema:"项目摘要的新内容，一句话：项目是什么、做什么用，助理靠它判断消息归不归这个项目。进展写进 memory，不写在这里。不改填空串"`
}

// server 是这条消息的工具。每次运行一个新的服务器，工具里的卡片和派活都算在这条消息上。
func (r *agentRun) server() *mcp.Server {
	e := r.e
	s := mcp.NewServer(&mcp.Implementation{Name: "morula-assistant", Version: "1"}, nil)
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(s, &mcp.Tool{Name: "list_projects", Annotations: ro,
		Description: "列出用户的全部项目：编号、名字和项目摘要。判断一条消息属于哪个项目前先调用它。"},
		func(context.Context, *mcp.CallToolRequest, noArgs) (*mcp.CallToolResult, any, error) {
			return reply(ProjectList(e.DB)), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_agents", Annotations: ro,
		Description: "列出团队里的全部 agent：名字、角色和一句话介绍。固定成员（你自己和验收）单列在前面；派活时从后面的子 agent 里选。"},
		func(context.Context, *mcp.CallToolRequest, noArgs) (*mcp.CallToolResult, any, error) {
			return reply(AgentList(e.DB)), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_harnesses", Annotations: ro,
		Description: "列出本机的 harness：哪些能用、能接工具和续跑吗，以及每个 harness 的全部模型和支持的思考强度。新建或修改 agent、要选 harness 和模型时调用它。"},
		func(context.Context, *mcp.CallToolRequest, noArgs) (*mcp.CallToolResult, any, error) {
			return reply(HarnessList(e.DB, true)), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "read_project", Annotations: ro,
		Description: "读一个项目：工作目录、项目摘要、项目记忆、能派活的执行者，以及最近几次安排和结果。要在这个项目里干活前先读。"},
		func(_ context.Context, _ *mcp.CallToolRequest, in projectArgs) (*mcp.CallToolResult, any, error) {
			p, _ := e.DB.Project(in.ProjectID)
			if p == nil {
				return refuse("没有编号为 " + strconv.FormatInt(in.ProjectID, 10) + " 的项目，用 list_projects 查编号"), nil, nil
			}
			r.touch(p, 0)
			out := projectBlock(p, true) + "\n## 花名册（可以派活的执行者）\n" + e.roster(p.ID)
			n := 3
			if d := e.DefaultProject(); d != nil && d.ID == p.ID {
				n = 10 // 默认项目多列几次，助理看得出同一类小事是不是常来
			}
			if runs, _ := e.DB.Runs(p.ID, n); len(runs) > 0 {
				out += "\n## 最近几次安排（新的在前）\n"
				for _, x := range runs {
					out += fmt.Sprintf("- 安排 %d（%s）：%s\n  结果：%s\n", x.ID, x.Status, clip(oneLine(x.Request), 120), clip(oneLine(orEmpty(x.Reply)), 300))
				}
			}
			return reply(out), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "ask_user",
		Description: "发一张问题卡片问用户，等用户回答后返回回答。用户可以选一个选项，也可以不选、自己写一段话。只在要用户拍板时用；拿不准属于哪个项目时不问，放进最可能的项目。"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in askArgs) (*mcp.CallToolResult, any, error) {
			if strings.TrimSpace(in.Question) == "" {
				return refuse("question 不能为空"), nil, nil
			}
			c, done := r.tie(ctx)
			defer done()
			opts := []map[string]string{}
			for i, o := range in.Options {
				if o = strings.TrimSpace(o); o != "" {
					opts = append(opts, map[string]string{"value": "o" + strconv.Itoa(i), "label": o})
				}
			}
			ans, err := e.Chat.Wait(c, chat.Event{"type": "ask", "text": in.Question, "reason": in.Reason, "options": opts, "free": true})
			r.step("", "问用户")
			if err != nil {
				return refuse("卡片已失效，用户没有回答"), nil, nil
			}
			if t := strings.TrimSpace(ans.Text); t != "" {
				return reply("用户没选选项，写了：" + t), nil, nil
			}
			for _, o := range opts {
				if o["value"] == ans.Choice {
					return reply("用户选了：" + o["label"]), nil, nil
				}
			}
			return reply("用户没有选"), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "create_project",
		Description: "新建一个项目，并建好它的工作目录，不问用户。对话里会显示「新建项目 X」，用户觉得不对会说。返回项目编号。"},
		func(_ context.Context, _ *mcp.CallToolRequest, in createArgs) (*mcp.CallToolResult, any, error) {
			name := strings.TrimSpace(in.Name)
			if name == "" {
				return refuse("name 不能为空"), nil, nil
			}
			p, err := e.CreateProject(name, in.Summary)
			if err != nil {
				return refuse("新建项目失败：" + err.Error() + "。同名的项目已经有了时直接用它（list_projects 查编号），或换个名字"), nil, nil
			}
			e.Chat.EmitIn(r.ctx, chat.Event{"type": "route", "project": p.ID, "name": p.Name, "created": true, "reason": in.Summary})
			r.step("", "新建项目")
			r.touch(p, 0)
			return reply(fmt.Sprintf("已新建项目「%s」，编号 %d，工作目录 %s", p.Name, p.ID, p.Dir)), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "rename_project",
		Description: "给项目改名，不问用户。只改 morula 里的名字，工作目录不动。对话里会显示改名记录，用户点一下能改回。"},
		func(_ context.Context, _ *mcp.CallToolRequest, in renameArgs) (*mcp.CallToolResult, any, error) {
			old, err := e.RenameProject(r.ctx, in.ProjectID, in.Name)
			if err != nil {
				return refuse("没改名：" + err.Error()), nil, nil
			}
			r.step("", "改项目名")
			if p, _ := e.DB.Project(in.ProjectID); p != nil {
				r.touch(p, 0)
			}
			return reply("已把「" + old + "」改名为「" + strings.TrimSpace(in.Name) + "」"), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "merge_projects",
		Description: "把两个其实是一件事的项目合并：发确认卡片，用户确认后把 from 的安排和项目记忆并进 into，from 从项目列表里收起来。工作目录不动，合并能撤销。返回合并编号，或用户的回答。"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in mergeArgs) (*mcp.CallToolResult, any, error) {
			c, done := r.tie(ctx)
			defer done()
			ans, err := e.proposeMerge(c, in.From, in.Into, in.Reason)
			r.step("", "合并项目")
			switch {
			case err != nil:
				return refuse("没合并：" + err.Error()), nil, nil
			case ans.Err != nil:
				return refuse("卡片已失效，用户没有回答，没合并"), nil, nil
			case ans.Text != "":
				return reply("用户没确认，写了：" + ans.Text + "。没合并"), nil, nil
			case ans.Merge == nil:
				return reply("用户选了不合并"), nil, nil
			}
			if p, _ := e.DB.Project(in.Into); p != nil {
				r.touch(p, 0)
			}
			return reply(fmt.Sprintf("已合并，合并编号 %d，挪过去 %d 次安排。用户要撤销时用 undo_merge", ans.Merge.ID, ans.Merge.Runs)), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "undo_merge",
		Description: "撤销一次合并：挪过去的安排挪回来，并进去的项目记忆删掉，被并掉的项目回到列表。用户说合并错了时用。"},
		func(_ context.Context, _ *mcp.CallToolRequest, in undoMergeArgs) (*mcp.CallToolResult, any, error) {
			id := in.MergeID
			if id == 0 {
				m, _ := e.DB.LastMerge()
				if m == nil {
					return refuse("没有能撤销的合并"), nil, nil
				}
				id = m.ID
			}
			m, kept, err := e.UndoMerge(r.ctx, id)
			if err != nil {
				return refuse("没撤销：" + err.Error()), nil, nil
			}
			r.step("", "撤销合并")
			if p, _ := e.DB.Project(m.Src); p != nil {
				r.touch(p, 0)
			}
			out := fmt.Sprintf("已撤销合并 %d，%d 次安排挪回了原项目", id, m.Runs)
			if kept {
				out += "。并进去的那段项目记忆合并后被改过，留着没删，要删时用 update_project"
			}
			return reply(out), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "move_runs",
		Description: "把几次安排挪到另一个项目，不问用户。用户说放错了项目、或要把默认项目里同一类的活收进新项目时用。还在跑的安排挪不了。工作目录里的文件不动。"},
		func(_ context.Context, _ *mcp.CallToolRequest, in moveArgs) (*mcp.CallToolResult, any, error) {
			p, err := e.MoveRuns(r.ctx, in.RunIDs, in.ProjectID)
			if err != nil {
				return refuse("没挪：" + err.Error()), nil, nil
			}
			r.step("", "挪安排")
			r.touch(p, 0)
			return reply(fmt.Sprintf("已把 %d 次安排挪到「%s」。做出来的文件还在原项目的工作目录里", len(in.RunIDs), p.Name)), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "dispatch",
		Description: "在一个已有项目里派一批顶层任务给子 agent，马上返回安排编号，不等做完。子 agent 做完后 morula 会检查并另外向用户汇报。同一个项目同时只跑一次安排，后来的排队。"},
		func(_ context.Context, _ *mcp.CallToolRequest, in dispatchArgs) (*mcp.CallToolResult, any, error) {
			p, _ := e.DB.Project(in.ProjectID)
			if p == nil {
				return refuse("没有编号为 " + strconv.FormatInt(in.ProjectID, 10) + " 的项目。用 list_projects 查编号，新项目先用 create_project 建"), nil, nil
			}
			if len(in.Tasks) == 0 || len(in.Tasks) > 5 {
				return refuse("tasks 要有 1 到 5 个任务"), nil, nil
			}
			for i, t := range in.Tasks {
				if strings.TrimSpace(t.Title) == "" || strings.TrimSpace(t.Brief) == "" {
					return refuse(fmt.Sprintf("第 %d 个任务缺 title 或 brief", i+1)), nil, nil
				}
				if msg := e.checkEffort(t); msg != "" {
					return refuse(fmt.Sprintf("第 %d 个任务：%s。什么都没派", i+1, msg)), nil, nil
				}
			}
			fp := posOf(r.ctx)
			var step string
			if fp != nil {
				step = e.addStep(fp.f, r.a.Name, "派活", fp.tip)
				e.setStep(fp.f, step, "done")
				fp.tip = step
			}
			run, err := e.startRun(fp, step, p, r.text, in.Plan, in.Tasks, r.rework)
			if err != nil {
				return refuse("派活失败：" + err.Error()), nil, nil
			}
			r.touch(p, run)
			e.setAfter(run, in.After)
			var b strings.Builder
			fmt.Fprintf(&b, "已派出，安排编号 %d，项目「%s」：\n", run, p.Name)
			for _, t := range in.Tasks {
				fmt.Fprintf(&b, "- %s（%s", t.Title, orDefault(t.Agent, "morula 安排的通用执行者"))
				if eff := strings.TrimSpace(t.Effort); eff != "" {
					b.WriteString("，思考强度 " + eff)
				}
				b.WriteString("）\n")
			}
			b.WriteString("活还没做完。子 agent 做完后 morula 会检查并向用户汇报。不要等它，也不要用 read_project 查进度：现在就回复用户。")
			if after := e.peekAfter(run); after != "" {
				b.WriteString("\n做完后交回给你接着做：" + after)
			}
			return reply(b.String()), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "transfer",
		Description: "把要和用户来回商量才能做完的事（追问方案、访谈、一起改稿，例如用 skill grill-me）转接给一个 agent：morula 开始用户和它的对话，它读了交接说明后先开口。之后用户的消息直接交给它，用户写 @助理 或点结束才回到你；结束时它写纪要进项目记忆。"},
		func(_ context.Context, _ *mcp.CallToolRequest, in transferArgs) (*mcp.CallToolResult, any, error) {
			if strings.TrimSpace(in.Brief) == "" {
				return refuse("brief 不能为空：那个 agent 没看过这段对话"), nil, nil
			}
			d, err := e.StartDirect(strings.TrimSpace(in.Agent), in.ProjectID, in.Brief)
			if err != nil {
				return refuse("没转接：" + err.Error() + "。用 list_agents 查名字"), nil, nil
			}
			r.step(d.Agent, "转接")
			if p, _ := e.DB.Project(d.Project); p != nil {
				r.touch(p, 0)
			}
			e.Chat.Begin(chat.TurnOf(r.ctx), func(ctx context.Context) { e.directTurn(ctx, d, "", nil) })
			return reply("已转接给「" + d.Agent + "」，它马上开口。用户接下来的消息直接交给它。现在用一句话告诉用户转给了谁、为什么；这件事不要自己接着回答，也不要再派活。"), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "propose_team_change",
		Description: "增删改团队里的 agent。morula 检查改动后发确认卡片，用户确认后由 morula 改，返回结果。先用 list_agents 确认名字。"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in teamArgs) (*mcp.CallToolResult, any, error) {
			c, done := r.tie(ctx)
			defer done()
			res := e.changeTeam(c, r.a, in.Actions, in.Reason)
			switch {
			case len(res.Bad) > 0:
				return refuse("这些改动不能做，什么都没改：\n- " + strings.Join(res.Bad, "\n- ")), nil, nil
			case !res.Asked:
				return reply("没有要改的地方"), nil, nil
			case res.Err != nil:
				return refuse("卡片已失效，用户没有回答，什么都没改"), nil, nil
			case strings.TrimSpace(res.Answer.Text) != "":
				return reply("用户没确认，写了：" + strings.TrimSpace(res.Answer.Text) + "。什么都没改"), nil, nil
			case res.Answer.Choice != "go":
				return reply("用户选了不改，什么都没改"), nil, nil
			}
			return reply(res.text()), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "run_command",
		Description: "一两条命令能做完的小事（打开应用、去掉隔离标记、建一个目录、复制一个文件、结束一个进程）：由 morula 执行，返回退出码和输出。按用户选的审批档位，有的命令直接执行，有的先发审批卡片等用户批准；删除文件、改写 git 历史、清数据库的命令总要批准。改代码、改好几个文件、要分几步的活用 dispatch。"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in commandArgs) (*mcp.CallToolResult, any, error) {
			c, done := r.tie(ctx)
			defer done()
			out, bad := e.askCommand(c, in)
			r.step("", "执行命令")
			if bad {
				return refuse(out), nil, nil
			}
			return reply(out), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "update_memory",
		Description: "改全局记忆：跨项目都适用的用户偏好、约定和习惯。每条消息开头附了当前的全局记忆；传完整新内容，整体替换旧内容。只属于一个项目的事用 update_project 写进项目记忆。"},
		func(_ context.Context, _ *mcp.CallToolRequest, in memoryArgs) (*mcp.CallToolResult, any, error) {
			m := strings.TrimSpace(in.Memory)
			if m == "" {
				return refuse("memory 是空的，什么都没改"), nil, nil
			}
			old := strings.TrimSpace(journal.Memory())
			if n := len([]rune(old)); !in.Drop && len([]rune(m))*2 < n {
				return refuse(fmt.Sprintf("新内容 %d 字，旧内容 %d 字，会删掉一大半旧内容，什么都没改。memory 要传完整新内容：旧内容照抄，再增删。"+
					"确实要删时 drop 填 true。旧内容：\n%s", len([]rune(m)), n, old)), nil, nil
			}
			if err := journal.SaveMemory(m + "\n"); err != nil {
				return refuse("改全局记忆失败：" + err.Error()), nil, nil
			}
			r.step("", "改全局记忆")
			return reply(fmt.Sprintf("已更新全局记忆，共 %d 字", len([]rune(m)))), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "search_chats", Annotations: ro,
		Description: "按关键词找以前的对话：morula 里和用户的对话（按天记），以及本机的对话记录文件（codex、WorkBuddy 等工具的会话，也包括 morula 派给子 agent 干活的会话）。" +
			"返回命中的原话和文件路径；要看全文时用只读命令读那个文件。问某个 agent 跑过的会话、做到哪了时填 agent，只找它跑的会话。"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in searchArgs) (*mcp.CallToolResult, any, error) {
			q, who := strings.TrimSpace(in.Query), strings.TrimSpace(in.Agent)
			if q == "" && who == "" {
				return refuse("query 和 agent 都是空的"), nil, nil
			}
			var b strings.Builder
			if hits := journal.Search(q, 10); who == "" && len(hits) > 0 {
				b.WriteString("# morula 里的对话（从新到旧）\n")
				for _, h := range hits {
					fmt.Fprintf(&b, "- %s：%s\n", h.Day, clip(oneLine(h.Line), 240))
				}
				fmt.Fprintf(&b, "按天的记录在 %s 下，文件名是日期。\n\n", journal.DaysDir())
			}
			// 助理自己这次的会话也会命中（里面有用户刚问的话），去掉
			own := strings.SplitN(e.DB.Setting("assistant.thread", ""), "|", 3)
			var chats []harness.Chat
			for _, c := range harness.FindChats(ctx, q, who, 9) {
				if len(own) != 3 || !strings.HasSuffix(c.ID, own[2]) {
					chats = append(chats, c)
				}
			}
			if len(chats) > 0 {
				b.WriteString("# 本机的对话记录文件（从新到旧）\n命中的可能只是提到了这个词，会话是谁跑的看括号里写的 agent。\n")
				for _, c := range chats {
					kind := c.Source
					if c.Morula {
						kind += "，morula 派出去的会话，agent「" + orDefault(c.Agent, "不知道") + "」"
					}
					fmt.Fprintf(&b, "- %s（%s；%s；目录 %s）\n  标题：%s\n  命中：%s\n", c.Path, kind, c.Updated.Format("2006-01-02 15:04"), orDefault(c.Dir, "不知道"), c.Title, c.Hit)
				}
			}
			if b.Len() == 0 {
				return reply("没找到对话（关键词「" + q + "」，agent「" + who + "」）。换个关键词再找，或问用户在哪个工具里聊的"), nil, nil
			}
			return reply(b.String()), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "update_project",
		Description: "改一个项目的项目记忆或项目摘要。记忆和摘要都是完整新内容，整体替换旧内容；不改的填空串。"},
		func(_ context.Context, _ *mcp.CallToolRequest, in updateArgs) (*mcp.CallToolResult, any, error) {
			p, _ := e.DB.Project(in.ProjectID)
			if p == nil {
				return refuse("没有编号为 " + strconv.FormatInt(in.ProjectID, 10) + " 的项目"), nil, nil
			}
			var mem, sum *string
			if strings.TrimSpace(in.Memory) != "" {
				mem = &in.Memory
			}
			if strings.TrimSpace(in.Summary) != "" {
				sum = &in.Summary
			}
			if mem == nil && sum == nil {
				return refuse("memory 和 summary 都是空的，什么都没改"), nil, nil
			}
			if d := e.DefaultProject(); sum != nil && d != nil && d.ID == p.ID {
				return refuse("「" + p.Name + "」是默认项目，摘要是固定的说明，不能改；要记的事写进 memory。什么都没改"), nil, nil
			}
			if err := e.DB.UpdateProject(p.ID, sum, mem, nil); err != nil {
				return refuse("改项目失败：" + err.Error()), nil, nil
			}
			r.touch(p, 0)
			return reply("已更新「" + p.Name + "」"), nil, nil
		})
	r.adminTools(s)
	return s
}
