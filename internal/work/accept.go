package work

import (
	"context"
	"fmt"
	"strings"

	"morula/internal/chat"
	"morula/internal/harness"
	"morula/internal/store"
)

// 验收：派下去的活做完后，验收 agent 在项目目录里只读地核对
// 活是不是真做完了，先验收，再由助理写汇报。验收没通过时打回给助理重新安排；
// 同一件活被打回 3 次，发卡片问用户。

// AcceptName 是验收 agent 在花名册里的名字。它和助理一样是固定成员，不派活。
const AcceptName = "验收"

// AcceptInstructions 是验收 agent 的默认职责说明。
const AcceptInstructions = "检查派下去的活是不是真做完了：看改动、打开产出、跑只读的检查，对照用户的要求判通过或不通过。不改文件。"

// maxRejects：同一件活被打回这么多次后，不再自动重新安排，问用户。
const maxRejects = 3

const acceptWhat = "验收"

const acceptSchema = `{"type":"object","additionalProperties":false,"required":["pass","problems","evidence"],
"properties":{"pass":{"type":"boolean"},"problems":{"type":"string"},"evidence":{"type":"string"}}}`

type acceptance struct {
	Pass     bool   `json:"pass"`
	Problems string `json:"problems"`
	Evidence string `json:"evidence"`
}

// rework 是验收打回后重新安排的位置：root 是这串安排的第一次，rejects 是已被打回的次数。
type rework struct {
	root    int64
	rejects int
}

// fixed 报告 a 是不是固定成员（助理、验收）：不派活，不能经管理团队删。
func fixed(a *store.Agent) bool {
	return a.Role == store.RoleAssistant || a.Name == AcceptName
}

// accept 让验收 agent 检查一次安排。花名册里没有验收 agent、或它在 Agent 页被关掉时返回 nil, nil。
// 过程显示成一组「验收过程」，结论是这组的最后一行。
func (e *Engine) accept(ctx context.Context, r *run, p *store.Project, request, plan, tree string) (*acceptance, error) {
	a, err := e.DB.Agent(AcceptName)
	if err != nil || a == nil || a.Off {
		return nil, err
	}
	defer e.hold(a.Name, "验收「"+p.Name+"」")()
	var b strings.Builder
	fmt.Fprintf(&b, "你是 morula 团队里的验收 agent「%s」。", a.Name)
	if s := strings.TrimSpace(a.Instructions); s != "" {
		b.WriteString(s)
	}
	fmt.Fprintf(&b, "\n\n项目「%s」的工作目录是 %s，你就在这个目录里。沙箱只读：能读本机任何文件（包括工作目录以外的，例如用户给的来源目录）、跑只读命令，不能写文件。\n\n", p.Name, p.Dir)
	b.WriteString("# 用户的要求\n" + orEmpty(request) + "\n\n# 助理的安排\n" + orEmpty(plan))
	if after := e.peekAfter(r.id); after != "" {
		b.WriteString("\n\n# 这次安排做完后，助理接着做的事（不在这次安排里，不验收）\n" + after)
	}
	b.WriteString("\n\n# 子 agent 的汇报和机器检查（汇报是子 agent 自己说的；机器检查是 morula 实际运行的）\n" + tree + `
# 怎么验收
- 不信汇报，自己核对：看改动（是 git 仓库时看 git status、git diff），打开产出的文件，运行只读的检查命令。
- 对照用户的要求逐条看：要的东西在不在、对不对、能不能用。上面写明了助理接着做的事时，那一部分不验收。
- 沙箱只读，要写文件的检查可能跑不了。说一条检查跑不了之前，先实际跑一次，把命令和输出写进 evidence；没跑过不能说跑不了。
  跑不了的标未验证；只因为跑不了，不判不通过。
- pass：亲自确认用户要的都做到了，才填 true。
- problems：不通过时写没做完或做错的地方，具体到文件、命令和输出，让重新安排的人知道改什么。通过时填空串。
- evidence：你查了什么、怎么查的、结果是什么，一条一行。
` + wordingRule)

	ev := chat.Event{"project": p.ID, "label": "验收过程"}
	if r.inbox {
		ev["inbox"] = true
	}
	x := e.newActivity(ctx, "验收在检查「"+p.Name+"」的活", ev)
	s := spec(a, p.Dir, b.String(), acceptSchema)
	s.Access, s.Ephemeral = harness.ReadOnly, true
	var v acceptance
	res, err := harness.Ask(ctx, a.Runtime, s, &v, x.on)
	if err != nil {
		x.end("failed", false, res.Tokens)
		return nil, err
	}
	verdict := "验收结论：通过"
	if !v.Pass {
		verdict = "验收结论：不通过\n\n问题：\n" + orEmpty(v.Problems)
	}
	x.line(verdict + "\n\n查了什么：\n" + orEmpty(v.Evidence))
	e.DB.SetRun(r.id, "verdict", map[bool]string{true: "pass", false: "fail"}[v.Pass])
	x.end(map[bool]string{true: "done", false: "rejected"}[v.Pass], true, res.Tokens) // 最后一句是 JSON，不显示
	return &v, nil
}

// acceptNote 是交给助理写汇报的验收结果。
func acceptNote(v *acceptance, err error, failed bool) string {
	switch {
	case failed:
		return "没有验收：有顶层任务没完成。"
	case err != nil:
		return "验收失败，没有结论：" + err.Error() + "。汇报里写明未验收。"
	case v == nil:
		return "花名册里没有验收 agent，或它被关掉了，这次没有验收。汇报里写明未验收。"
	case v.Pass:
		return "通过。验收查了：\n" + orEmpty(v.Evidence)
	}
	return "不通过，用户决定先停下，不再重新安排。问题：\n" + orEmpty(v.Problems) + "\n验收查了：\n" + orEmpty(v.Evidence)
}

// rejected 处理验收没通过的安排：打回给助理重新安排时返回 true。助理不是 agent 方式、
// 或打回已到 maxRejects 次且用户选了先停下时返回 false，由 finish 照常汇报。
func (e *Engine) rejected(ctx context.Context, r *run, p *store.Project, a *store.Agent, request string, v *acceptance) bool {
	cur, _ := e.DB.Run(r.id)
	rw := rework{root: r.id, rejects: 1}
	if cur != nil {
		if cur.RedoOf != 0 {
			rw.root = cur.RedoOf
		}
		rw.rejects = cur.Rejects + 1
	}
	if !e.agentMode(a) {
		return false
	}
	note := ""
	if rw.rejects >= maxRejects {
		ans, err := e.Chat.Wait(ctx, chat.Event{
			"type": "ask", "project": p.ID, "run": r.id, "free": true,
			"text":   fmt.Sprintf("「%s」这件活验收打回 %d 次还没通过，要怎么办？", p.Name, rw.rejects),
			"reason": "卡在哪里见上面「验收过程」的最后一条。选再安排一次时，助理换做法或换 agent 再派；也可以在下面写你的意见。",
			"options": []map[string]string{
				{"value": "again", "label": "再安排一次"},
				{"value": "stop", "label": "先停下，汇报现状"},
			},
		})
		note = strings.TrimSpace(ans.Text)
		if err != nil || (note == "" && ans.Choice != "again") {
			return false
		}
	}
	e.DB.SetRun(r.id, "status", "rejected")
	e.Chat.EmitIn(ctx, chat.Event{"type": "note", "project": p.ID, "run": r.id,
		"text": fmt.Sprintf("「%s」验收没通过（第 %d 次），已打回给助理重新安排", p.Name, rw.rejects)})
	after := e.takeAfter(r.id)
	e.Chat.Begin(chat.TurnOf(ctx), func(ctx context.Context) { e.reworkTurn(ctx, a, p, rw, request, v, note, after) })
	return true
}

// reworkTurn 让助理按验收发现的问题重新安排。和用户的消息一样能停止、能补充。
// after 是这次安排记着的后续，空表示没有。
func (e *Engine) reworkTurn(ctx context.Context, a *store.Agent, p *store.Project, rw rework, request string, v *acceptance, note, after string) {
	defer e.hold(a.Name, "重新安排「"+p.Name+"」")()
	f := e.newFlow(ctx, "验收打回："+request)
	e.mu.Lock()
	f.Steps[0].Agent, f.Steps[0].What = AcceptName, "验收打回"
	e.mu.Unlock()
	e.setFlowProject(f, p)
	step := e.addStep(f, a.Name, "重新安排", "u")
	var b strings.Builder
	fmt.Fprintf(&b, "# 验收没通过，打回给你重新安排\n项目「%s」（编号 %d）里的一件活，验收没通过，这是第 %d 次打回。\n\n", p.Name, p.ID, rw.rejects)
	b.WriteString("用户的要求：\n" + orEmpty(request) + "\n\n验收发现的问题：\n" + orEmpty(v.Problems) + "\n\n验收查了什么：\n" + orEmpty(v.Evidence) + "\n\n")
	if note != "" {
		b.WriteString("用户看了以后的意见：\n" + note + "\n\n")
	}
	if after != "" {
		b.WriteString("你上次派活时记下了做完后要接着做的事，还没做：\n" + after + "\n" +
			"重新派活时，仍要等活做完才能做的部分写进 dispatch 的 after；已经做了或用不着等的，不要再写。\n\n")
	}
	b.WriteString("先用 read_project 看这个项目最近的安排，再按问题选一种办法：\n" +
		"- 活本身没做好：换一种做法、或换一个 agent，用 dispatch 重新派原来那件活；brief 里写清楚上次哪里没做到。\n" +
		"- 这件活要和用户来回商量（追问、访谈、一起改稿）：不要再派，用 transfer 转接给合适的 agent。\n" +
		"- 问题是不清楚用户要什么：不要派活，在回复里直接问用户。\n" +
		"不要派「复核验收意见」「核对安排记录」这类活：子 agent 看不到 morula 的安排记录和验收意见，只能做原来那件活。\n" +
		"回复用户：验收没通过、问题是什么、这次怎么办。")
	r := &agentRun{e: e, a: a, text: request, rework: &rw}
	e.agentLoop(withFlow(ctx, f, step), a, r, b.String(), nil)
}
