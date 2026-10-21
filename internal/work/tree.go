package work

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"morula/internal/chat"
	"morula/internal/harness"
	"morula/internal/skills"
	"morula/internal/store"
)

// run 是一次安排在执行期间共用的东西。
type run struct {
	id      int64
	project *store.Project
	limits  Limits
	slots   chan struct{} // 同时运行的 harness 进程数；只在调用 harness 时占用，等子任务时不占
	turn    string        // 派活的那条用户消息，任务进展挂在它下面
	inbox   bool          // 验收过程放收件箱（对话结束后的验收），不插进对话
}

func (e *Engine) execRun(ctx context.Context, runID, projectID int64) {
	defer func() {
		e.mu.Lock()
		delete(e.runs, runID)
		e.mu.Unlock()
	}()
	f := e.flowOfRun(runID)
	if f != nil {
		ctx = chat.WithTurn(ctx, f.Turn)
	}
	p, err := e.DB.Project(projectID)
	if err != nil || p == nil {
		e.DB.SetRun(runID, "status", "failed")
		if f != nil {
			e.endFlow(f, "failed")
		}
		return
	}
	// 同一个项目里的安排一次只跑一个，免得两批子 agent 同时改同一个目录；不同项目不受影响。
	e.mu.Lock()
	lane := e.lanes[projectID]
	if lane == nil {
		lane = make(chan struct{}, 1)
		e.lanes[projectID] = lane
	}
	e.mu.Unlock()
	select {
	case lane <- struct{}{}:
	default:
		e.Chat.EmitIn(ctx, chat.Event{"type": "note", "text": "「" + p.Name + "」上一次的安排还在做，这次的安排排在它后面", "project": p.ID, "run": runID})
		select {
		case lane <- struct{}{}:
		case <-ctx.Done():
			e.DB.SetRun(runID, "status", "stopped")
			if f != nil {
				e.endFlow(f, "stopped")
			}
			return
		}
	}
	release := sync.OnceFunc(func() { <-lane })
	defer release()
	r := &run{id: runID, project: p, limits: e.Limits(), turn: chat.TurnOf(ctx)}
	r.slots = make(chan struct{}, r.limits.Parallel)

	tasks, _ := e.DB.Tasks(runID)
	var wg sync.WaitGroup
	for _, t := range tasks {
		if t.ParentID != 0 {
			continue
		}
		wg.Add(1)
		go func(t store.Task) {
			defer wg.Done()
			e.runNode(ctx, r, t, "")
		}(t)
	}
	wg.Wait()
	if ctx.Err() != nil {
		e.DB.SetRun(runID, "status", "stopped")
		e.Chat.EmitIn(ctx, chat.Event{"type": "note", "text": "「" + p.Name + "」的安排已停止", "project": p.ID, "run": runID})
		if f != nil {
			e.endFlow(f, "stopped")
		}
		return
	}
	release() // 验收和汇报不改文件；验收打回后重新派的活不用等这次汇报写完
	e.finish(r, f)
}

// call 占一个并发名额，调用 harness，并把进度写进 live。
func (e *Engine) call(ctx context.Context, r *run, taskID int64, a *store.Agent, s harness.Spec, out any) (harness.Result, error) {
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return harness.Result{}, ctx.Err()
	}
	defer func() { <-r.slots }()
	defer e.setLive(taskID, "")
	defer e.hold(a.Name, "「"+r.project.Name+"」任务 #"+strconv.FormatInt(taskID, 10))()
	return harness.Ask(ctx, a.Runtime, s, out, func(ev harness.Event) {
		switch ev.Kind {
		case "command":
			e.setLive(taskID, "$ "+ev.Text)
		case "message":
			e.setLive(taskID, ev.Text)
		}
	})
}

type report struct {
	Status   string     `json:"status"`
	Summary  string     `json:"summary"`
	Children []taskSpec `json:"children"`
	Approval struct {
		Action string `json:"action"`
		Reason string `json:"reason"`
	} `json:"approval"`
}

// node 是 runNode 里一个节点的可变状态。
type node struct {
	t      store.Task
	a      *store.Agent
	effort string // 这个节点实际用的思考强度，见 taskEffort
	tier   string // 写提示词时的审批档位；续跑时沿用，和提示词里的第 3 条对得上
	tokens int
}

func (e *Engine) set(n *node, p store.TaskPatch) {
	e.DB.SetTask(n.t.ID, p)
}

func (e *Engine) taskEvent(r *run, n *node, text string) {
	e.Chat.EmitIn(chat.WithTurn(context.Background(), r.turn), chat.Event{"type": "task", "project": r.project.ID, "run": r.id, "task": n.t.ID,
		"agent": n.a.Name, "title": n.t.Title, "text": text})
}

// runNode 执行一个节点直到完成或失败。feedback 非空表示被父节点打回后重做。
func (e *Engine) runNode(ctx context.Context, r *run, t store.Task, feedback string) {
	a, err := e.executor(ctx, t.Agent, r.project.ID)
	if err != nil {
		why := err.Error()
		if ctx.Err() != nil { // 等用户回答要不要新建执行者时点了停止
			why = "已停止"
		}
		e.DB.SetTask(t.ID, store.TaskPatch{Status: store.Ptr("failed"), Feedback: &why})
		return
	}
	n := &node{t: t, a: a, effort: taskEffort(a, t.Effort), tokens: t.Tokens}
	model, _ := liveModel(a) // 记实际用的模型：花名册里的模型下架时，这里是换上的那个
	e.set(n, store.TaskPatch{Agent: &a.Name, Runtime: &a.Runtime, Model: &model, Effort: &n.effort})
	if feedback == "" {
		e.taskEvent(r, n, "开始")
	} else {
		e.taskEvent(r, n, "被打回，重做")
	}

	prompt, resume := e.workerPrompt(r, n), ""
	if feedback != "" && t.Session != "" {
		prompt = "上级检查后打回了你的结果：\n" + feedback + "\n\n按要求修改后，按同样的 JSON 格式重新汇报。"
		resume = t.Session
	}
	attempts := t.Attempts
	for {
		attempts++
		e.set(n, store.TaskPatch{Status: store.Ptr("working"), Attempts: &attempts})
		rep, err := e.work(ctx, r, n, prompt, resume)
		if err == nil {
			rep, err = e.approve(ctx, r, n, rep)
		}
		if err != nil {
			e.stopOrFail(ctx, r, n, err)
			return
		}
		resume = n.t.Session
		switch rep.Status {
		case "blocked":
			e.set(n, store.TaskPatch{Status: store.Ptr("failed"), Report: &rep.Summary})
			e.taskEvent(r, n, "做不了："+clip(rep.Summary, 120))
			return
		case "split":
			if n.t.Depth >= r.limits.MaxDepth || len(rep.Children) == 0 {
				prompt = fmt.Sprintf("这是第 %d 层，已经到最大层数，不能再拆。直接完成任务，按同样的 JSON 格式汇报。", n.t.Depth)
				continue
			}
			e.split(ctx, r, n, rep)
			return
		}
		// done
		e.set(n, store.TaskPatch{Report: &rep.Summary})
		if n.t.Check == "" {
			e.set(n, store.TaskPatch{Status: store.Ptr("done")})
			e.taskEvent(r, n, "完成")
			return
		}
		e.set(n, store.TaskPatch{Status: store.Ptr("checking")})
		out, ok := machineCheck(ctx, r.project.Dir, n.t.Check)
		e.set(n, store.TaskPatch{CheckOut: &out})
		if ok {
			e.set(n, store.TaskPatch{Status: store.Ptr("done")})
			e.taskEvent(r, n, "完成，机器检查通过")
			return
		}
		if ctx.Err() != nil {
			e.stopOrFail(ctx, r, n, ctx.Err())
			return
		}
		if attempts > r.limits.Retries {
			e.set(n, store.TaskPatch{Status: store.Ptr("failed"), Feedback: store.Ptr("机器检查没通过，重做次数已用完")})
			e.taskEvent(r, n, "机器检查没通过，重做次数已用完")
			return
		}
		fb := "机器检查 `" + n.t.Check + "` 没通过"
		e.set(n, store.TaskPatch{Status: store.Ptr("rejected"), Feedback: &fb})
		e.taskEvent(r, n, "机器检查没通过，打回重做")
		prompt = fb + "，输出：\n" + out + "\n\n修好后按同样的 JSON 格式重新汇报。"
	}
}

func (e *Engine) stopOrFail(ctx context.Context, r *run, n *node, err error) {
	if ctx.Err() != nil {
		e.set(n, store.TaskPatch{Status: store.Ptr("failed"), Feedback: store.Ptr("已停止")})
		return
	}
	msg := err.Error()
	e.set(n, store.TaskPatch{Status: store.Ptr("failed"), Feedback: &msg})
	e.taskEvent(r, n, "失败："+clip(msg, 160))
}

// work 让子 agent 跑一轮，返回它的汇报。
func (e *Engine) work(ctx context.Context, r *run, n *node, prompt, resume string) (report, error) {
	var rep report
	s := spec(n.a, r.project.Dir, prompt, reportSchema)
	s.Effort, s.Access, s.Resume = n.effort, harness.Workspace, resume
	if n.tier == TierAll { // 清单上的事（例如改工作目录以外的文件、联网）不再上报，沙箱也不能挡
		s.Access = harness.Full
	}
	res, err := e.call(ctx, r, n.t.ID, n.a, s, &rep)
	n.tokens += res.Tokens
	p := store.TaskPatch{Tokens: &n.tokens}
	if res.Session != "" {
		n.t.Session = res.Session
		p.Session = &res.Session
	}
	e.set(n, p)
	return rep, err
}

// approve 处理子 agent 上报的危险操作：发审批卡片给用户，等批准或拒绝，再让子 agent 接着做。
func (e *Engine) approve(ctx context.Context, r *run, n *node, rep report) (report, error) {
	for rep.Status == "approval" {
		desc := rep.Approval.Action + "\n原因：" + rep.Approval.Reason
		e.set(n, store.TaskPatch{Status: store.Ptr("approval"), Approval: &desc})
		ans, err := e.Chat.Wait(ctx, chat.Event{
			"type": "permission", "project": r.project.ID, "run": r.id, "task": n.t.ID,
			"display":     n.a.Name + " 想执行危险操作",
			"description": "项目「" + r.project.Name + "」· 任务「" + n.t.Title + "」",
			"input":       map[string]string{"操作": rep.Approval.Action, "原因": rep.Approval.Reason},
			"free":        true,
		})
		if err != nil {
			return rep, err
		}
		msg := "用户已批准：" + rep.Approval.Action + "\n执行它，然后按同样的 JSON 格式汇报。"
		verdict := "（已批准）"
		if t := strings.TrimSpace(ans.Text); t != "" {
			msg = "用户没有直接批准或拒绝，而是回复：" + t + "\n按用户的意思继续做。仍然需要危险操作时，再 status=approval 并写清楚具体操作。"
			verdict = "（用户回复：" + t + "）"
		} else if !ans.Allow {
			msg = "用户拒绝了：" + rep.Approval.Action + "\n不要执行它。换一种不需要这个操作的做法完成任务；做不到就 status=blocked 并说明。"
			verdict = "（已拒绝）"
		}
		desc += "\n" + verdict
		e.set(n, store.TaskPatch{Status: store.Ptr("working"), Approval: &desc})
		if rep, err = e.work(ctx, r, n, msg, n.t.Session); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func (e *Engine) workerPrompt(r *run, n *node) string {
	var b strings.Builder
	fmt.Fprintf(&b, "你是 morula 团队里的子 agent「%s」。", n.a.Name)
	if n.a.Instructions != "" {
		b.WriteString("你的职责说明：" + n.a.Instructions)
	}
	b.WriteString("\n回答用中文。\n")
	b.WriteString(skillBlock(n.a.Skills))
	b.WriteString("\n" + projectBlock(r.project, true))
	// 父节点的任务是上下文；兄弟节点互不共享状态。
	if n.t.ParentID != 0 {
		if p, _ := e.DB.Task(n.t.ParentID); p != nil {
			fmt.Fprintf(&b, "\n# 上级任务（由「%s」拆给你）\n%s\n%s\n", p.Agent, p.Title, p.Brief)
		}
	}
	more := "还能往下拆"
	if n.t.Depth >= r.limits.MaxDepth {
		more = "已到最大层数，不能再拆"
	}
	fmt.Fprintf(&b, "\n# 你的任务（第 %d 层，最多 %d 层，%s）\n%s\n\n%s\n", n.t.Depth, r.limits.MaxDepth, more, n.t.Title, n.t.Brief)
	if n.t.Depth < r.limits.MaxDepth {
		b.WriteString("\n# 花名册（拆子任务时 agent 填其中合适的名字，没有合适的填空串）\n" + e.roster(r.project.ID))
	}
	if n.t.Check != "" {
		fmt.Fprintf(&b, "\n完成后上级会在工作目录里运行机器检查：`%s`，退出码 0 才算通过。\n", n.t.Check)
	}
	n.tier = e.Tier()
	b.WriteString("\n" + strings.Replace(workerRules, "{{danger}}", tierRule(n.tier), 1))
	return b.String()
}

// split 把节点拆成子节点，并行执行，再由这个节点检查子节点的汇报。
func (e *Engine) split(ctx context.Context, r *run, n *node, rep report) {
	var kids []int64
	for _, c := range rep.Children {
		id, err := e.DB.AddTask(store.Task{RunID: r.id, ParentID: n.t.ID, Depth: n.t.Depth + 1,
			Title: c.Title, Brief: c.Brief, Agent: c.Agent, Check: c.Check, Effort: n.t.Effort})
		if err == nil {
			kids = append(kids, id)
		}
	}
	e.set(n, store.TaskPatch{Status: store.Ptr("waiting"), Report: &rep.Summary})
	e.taskEvent(r, n, fmt.Sprintf("拆成 %d 个子任务", len(kids)))

	todo, feedback := kids, ""
	for round := 0; ; round++ {
		var wg sync.WaitGroup
		for _, id := range todo {
			t, _ := e.DB.Task(id)
			if t == nil {
				continue
			}
			wg.Add(1)
			go func(t store.Task) {
				defer wg.Done()
				e.runNode(ctx, r, t, feedback)
			}(*t)
		}
		wg.Wait()
		if ctx.Err() != nil {
			e.stopOrFail(ctx, r, n, ctx.Err())
			return
		}

		// 父节点检查：机器检查已在子节点完成时跑过，这里看汇报是否满足父任务。
		e.set(n, store.TaskPatch{Status: store.Ptr("checking")})
		var b strings.Builder
		b.WriteString("你拆出的子任务都结束了，下面是它们的汇报和机器检查结果。检查它们合起来是否完成了你的任务。\n")
		b.WriteString("pass：是否通过。不通过时 redo 填要重做的子任务编号，feedback 写具体要改什么。summary：你这个任务的汇报（做了什么、怎么验证的、还缺什么）。只读检查，不要改文件。\n\n")
		for _, id := range kids {
			if t, _ := e.DB.Task(id); t != nil {
				b.WriteString(describe(t))
			}
		}
		var rv struct {
			Pass     bool    `json:"pass"`
			Feedback string  `json:"feedback"`
			Redo     []int64 `json:"redo"`
			Summary  string  `json:"summary"`
		}
		s := spec(n.a, r.project.Dir, b.String(), reviewSchema)
		s.Effort, s.Access, s.Resume = n.effort, harness.ReadOnly, n.t.Session
		res, err := e.call(ctx, r, n.t.ID, n.a, s, &rv)
		n.tokens += res.Tokens
		e.set(n, store.TaskPatch{Tokens: &n.tokens})
		if err != nil {
			e.stopOrFail(ctx, r, n, err)
			return
		}
		todo = nil
		for _, id := range rv.Redo {
			for _, k := range kids {
				if k == id {
					todo = append(todo, id)
				}
			}
		}
		if rv.Pass || len(todo) == 0 || round >= r.limits.Retries {
			st := "done"
			if !rv.Pass {
				st = "failed"
			}
			e.set(n, store.TaskPatch{Status: &st, Report: &rv.Summary, Feedback: &rv.Feedback})
			if rv.Pass {
				e.taskEvent(r, n, "子任务检查通过，完成")
			} else {
				e.taskEvent(r, n, "子任务检查没通过："+clip(rv.Feedback, 120))
			}
			return
		}
		feedback = rv.Feedback
		for _, id := range todo {
			e.DB.SetTask(id, store.TaskPatch{Status: store.Ptr("rejected"), Feedback: &feedback})
		}
		e.taskEvent(r, n, fmt.Sprintf("打回 %d 个子任务", len(todo)))
	}
}

func describe(t *store.Task) string {
	s := fmt.Sprintf("## 子任务 %d「%s」（%s）\n状态：%s\n汇报：%s\n", t.ID, t.Title, t.Agent, t.Status, orEmpty(t.Report))
	if t.Check != "" {
		s += fmt.Sprintf("机器检查（morula 在项目目录里实际运行，不是子 agent 自述）`%s` 输出：\n%s\n", t.Check, orEmpty(clip(t.CheckOut, 1500)))
	}
	if t.Feedback != "" {
		s += "备注：" + t.Feedback + "\n"
	}
	return s + "\n"
}

// finish：助理收齐顶层汇报，检查后更新项目记忆和项目摘要，向用户汇报。
// f 是这次安排所在的流程，可能为 nil（morula 重启前开始的安排）。
func (e *Engine) finish(r *run, f *Flow) {
	ctx := context.Background()
	state := "failed"
	if f != nil {
		ctx = chat.WithTurn(ctx, f.Turn) // 汇报挂在派活的那条消息下面
		defer func() { e.endFlow(f, state) }()
	}
	a, err := e.assistant()
	if err != nil {
		e.Chat.EmitIn(ctx, chat.Event{"type": "error", "text": err.Error()})
		return
	}
	tasks, _ := e.DB.Tasks(r.id)
	failed := false
	var tree strings.Builder
	var walk func(parent int64, depth int)
	walk = func(parent int64, depth int) {
		for i := range tasks {
			t := &tasks[i]
			if t.ParentID != parent {
				continue
			}
			if parent == 0 && t.Status != "done" {
				failed = true
			}
			tree.WriteString(strings.Repeat("  ", depth) + strings.ReplaceAll(describe(t), "\n", "\n"+strings.Repeat("  ", depth)))
			walk(t.ID, depth+1)
		}
	}
	walk(0, 0)
	runs, _ := e.DB.Runs(r.project.ID, 1)
	request, planText := "", ""
	if len(runs) > 0 && runs[0].ID == r.id {
		request, planText = runs[0].Request, runs[0].Plan
	}
	p, _ := e.DB.Project(r.project.ID)
	if p == nil {
		p = r.project
	}

	// 先验收，再写汇报。有顶层任务没完成时不验收，直接汇报失败。
	var v *acceptance
	var verr error
	var from []string // 检查汇报这一步从哪里连进来；空时连着全部顶层任务
	if !failed {
		var as string
		if acc, _ := e.DB.Agent(AcceptName); f != nil && acc != nil && !acc.Off {
			as = e.addStep(f, AcceptName, acceptWhat)
		}
		v, verr = e.accept(ctx, r, p, request, planText, tree.String())
		if as != "" {
			e.setStep(f, as, map[bool]string{true: "done", false: "failed"}[v != nil && v.Pass])
			from = []string{as}
		}
		if verr != nil {
			e.Chat.EmitIn(ctx, chat.Event{"type": "error", "text": "验收失败：" + verr.Error(), "project": p.ID})
		}
		if v != nil && !v.Pass && e.rejected(ctx, r, p, a, request, v) {
			return // 打回给助理重新安排；这条流程到此为止，算失败
		}
	}
	var step string
	if f != nil {
		w := a.Name
		if o := e.owner(p); o != nil {
			w = o.Name
		}
		step = e.addStep(f, w, finishWhat, from...)
		ctx = withFlow(ctx, f, step)
	}
	st := "done"
	if failed || (v != nil && !v.Pass) {
		st = "failed"
	}
	after := ""
	if e.agentMode(a) {
		after = e.takeAfter(r.id)
	}
	afterNote := ""
	switch {
	case after != "" && st == "done":
		afterNote = "\n# 做完后接着要做的事\n" + after + "\n汇报发出后 morula 会交给你接着做，那时你有管理团队等工具。它不是这次安排的活：汇报里不给它写状态（不写未开始、阻塞、未验证），只说接下来会接着做它。\n"
	case after != "":
		afterNote = "\n# 做完后接着要做的事（这次不做）\n" + after + "\n这次安排没做完，这件事不接着做。汇报里说明它没有做、为什么。\n"
	}
	// 有项目负责人时由它写汇报、项目记忆和项目摘要；助理只收到汇报
	w, role, planHd, by := a, e.role(a), "你当时的安排", "morula 检查后"
	if o := e.owner(p); o != nil {
		w, role, planHd, by = o, ownerRole(o, p), "助理的安排", "项目负责人「"+o.Name+"」检查后"
		afterNote = strings.Replace(afterNote, "morula 会交给你接着做，那时你有管理团队等工具", "morula 会交给助理接着做", 1)
	}
	defer e.hold(w.Name, "检查「"+r.project.Name+"」的汇报")()
	prompt := role + "\n\n" + projectBlock(p, true) +
		"\n# 用户的要求\n" + request + "\n\n# " + planHd + "\n" + planText +
		"\n\n# 任务树的汇报（缩进表示子任务）\n" + tree.String() +
		"\n# 验收结果\n" + acceptNote(v, verr, failed) + "\n" + afterNote +
		"\n检查这些汇报，然后：\n" +
		"reply：给用户的汇报。用用户能看懂的话写，不要写汇报里的内部字段名、变量名和 JSON，例如不写 tools=[]，写「没有可用的工具」。第一句是结论；状态只用这些词：已完成、部分完成、未开始、进行中、已验证、未验证、失败、跳过、阻塞、未确认；写明每个结论是否验证、怎么验证的（验收查了什么写在上面）；没执行的检查不写成通过；验收没通过时不写成已完成。" + wordingRule + "\n" +
		"memory：项目记忆的完整新内容，保留原有仍然有效的内容，加上这次的结论、决定、产出文件；删掉过时的。\n" +
		"summary：项目摘要，一句话，说明项目是什么、做什么用。助理以后只靠它判断消息属于哪个项目，项目没变时照抄原来的；进展写进 memory，不写在这里。"
	var out struct {
		Reply   string `json:"reply"`
		Memory  string `json:"memory"`
		Summary string `json:"summary"`
	}
	who := "助理"
	if w != a {
		who = "「" + w.Name + "」"
	}
	x := e.newActivity(ctx, who+"在检查「"+p.Name+"」的汇报", chat.Event{"project": p.ID})
	s := spec(w, scratch(), prompt, finalSchema)
	s.Access, s.Ephemeral = harness.ReadOnly, true
	res, err := harness.Ask(ctx, w.Runtime, s, &out, x.on)
	x.end(map[bool]string{true: "done", false: "failed"}[err == nil], true, res.Tokens) // 最后一句是 JSON，不显示
	if err != nil {
		e.DB.SetRun(r.id, "status", "failed")
		e.Chat.EmitIn(ctx, chat.Event{"type": "error", "text": who + "汇报失败：" + err.Error(), "project": p.ID})
		return
	}
	if f != nil {
		e.setStep(f, step, "done")
	}
	var mem, sum *string
	if strings.TrimSpace(out.Memory) != "" {
		mem = &out.Memory
	}
	if strings.TrimSpace(out.Summary) != "" {
		sum = &out.Summary
	}
	if d := e.DefaultProject(); d != nil && d.ID == p.ID {
		sum = nil // 默认项目的摘要是给路由看的固定说明，不写进展
	}
	e.DB.UpdateProject(p.ID, sum, mem, nil)
	e.DB.SetRun(r.id, "reply", out.Reply)
	e.DB.SetRun(r.id, "status", st)
	ev := chat.Event{"project": p.ID, "run": r.id, "report": true, "activity": x.group}
	if st != "done" {
		ev["failed"] = true
	}
	if w != a {
		ev["agent"] = w.Name
	}
	if e.Chat.Talking() { // 用户正在说话：汇报进收件箱，不插进对话
		ev["inbox"] = true
	}
	e.say(ctx, "收到汇报", out.Reply, ev)
	e.noteMissed(fmt.Sprintf("「%s」安排 %d 的任务树做完了，%s向用户汇报：\n%s", p.Name, r.id, by, out.Reply))
	state = "done" // 流程走完了；任务失败与否看各个任务节点
	if after != "" && st == "done" {
		e.Chat.Begin(chat.TurnOf(ctx), func(ctx context.Context) { e.afterTurn(ctx, a, p, r.id, after, out.Reply) })
	}
	e.reviewStaff(p)
}

// executor 找一个能派活的执行者（关着的不算）：先按名字找；找不到时用常用于这个项目的，再用通用执行者
// （没绑 skill、没有常用项目的「执行」角色），再没有就问用户，同意后按助理的配置新建一个通用执行者。
func (e *Engine) executor(ctx context.Context, name string, project int64) (*store.Agent, error) {
	ok := func(a *store.Agent) bool {
		if a == nil || fixed(a) || a.Off {
			return false
		}
		_, err := harness.Get(a.Runtime)
		return err == nil
	}
	if name != "" {
		if a, _ := e.DB.Agent(name); ok(a) {
			return a, nil
		}
	}
	e.hires.Lock()
	defer e.hires.Unlock()
	list, _ := e.DB.Agents()
	for i := range list {
		if slices.Contains(list[i].Projects, project) && ok(&list[i]) {
			return &list[i], nil
		}
	}
	for i := range list {
		if generalist(&list[i]) && ok(&list[i]) {
			return &list[i], nil
		}
	}
	as, err := e.assistant()
	if err != nil {
		return nil, err
	}
	// 自动新建前先问用户：花名册是用户的，不能悄悄多出人来。全部放行时不问，新建后照样发一条说明。
	name = freeName(list, "执行-")
	if e.Tier() != TierAll {
		ans, err := e.Chat.Wait(ctx, chat.Event{"type": "ask", "text": "要新建一个通用执行者「" + name + "」来做这件事吗？",
			"reason":  "团队里没有合适的执行者。新建的执行者和助理用同一个模型（" + as.Runtime + " · " + as.Model + "），思考强度 " + orDefault(workerEffort(as), "用模型默认") + "，以后在 Agent 页可以改或删。",
			"options": []map[string]string{{"value": "go", "label": "新建「" + name + "」"}, {"value": "no", "label": "不新建，这个任务先不做"}}})
		if err != nil {
			return nil, err
		}
		if ans.Choice != "go" {
			return nil, errors.New("团队里没有合适的执行者，你没有同意新建。可以在 Agent 页加一个执行者后重试")
		}
	}
	a := store.Agent{Name: name, Role: "执行", Runtime: as.Runtime, Model: as.Model, Effort: workerEffort(as),
		Instructions: "通用执行者。由助理在花名册里没有合适执行者时自动新建。"}
	if err := e.DB.AddAgent(a); err != nil {
		return nil, errors.New("团队里没有能干活的执行者，自动新建也失败了：" + err.Error())
	}
	e.Chat.EmitIn(ctx, chat.Event{"type": "note", "text": "团队里没有通用执行者，按助理的模型新建了「" + a.Name + "」（" + a.Runtime + " · " + a.Model + " · 思考强度 " + orDefault(a.Effort, "默认") + "）"})
	return &a, nil
}

// generalist 判断是不是通用执行者：「执行」角色，没绑 skill，也没有常用项目。
func generalist(a *store.Agent) bool {
	return a.Role == "执行" && len(a.Skills) == 0 && len(a.Projects) == 0
}

// freeName 返回 prefix 加最小的未占用序号。
func freeName(list []store.Agent, prefix string) string {
	for i := 1; ; i++ {
		n := fmt.Sprintf("%s%d", prefix, i)
		if !slices.ContainsFunc(list, func(a store.Agent) bool { return a.Name == n }) {
			return n
		}
	}
}

// 机器检查命令由助理写，跑之前拦下明显的危险操作：这类命令要走审批，不能当检查跑。
var dangerous = regexp.MustCompile(`(^|[\s;&|(])(rm|rmdir|sudo|dd|mkfs|shred|chmod|chown|mv|curl|wget|ssh|scp)\s|git\s+(push|reset|clean|rebase|filter-branch)|>\s*/`)

func machineCheck(ctx context.Context, dir, cmd string) (string, bool) {
	if dangerous.MatchString(cmd) {
		return "没有运行：检查命令里有删除、改权限、联网或改 git 历史之类的操作，morula 不把它当机器检查跑。", false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = dir
	out, err := c.CombinedOutput()
	s := clip(strings.TrimSpace(string(out)), 4000)
	if err != nil {
		if s != "" {
			s += "\n"
		}
		s += "（" + err.Error() + "）"
		return s, false
	}
	if s == "" {
		s = "（无输出，退出码 0）"
	}
	return s, true
}

// skillBlock 写出 agent 绑定的 skill。不依赖 harness 自己加载 skill：每个都给出 SKILL.md 的路径，让子 agent 先读。
// 本机已经没有的 skill 跳过。
func skillBlock(names []string) string {
	if len(names) == 0 {
		return ""
	}
	all := skills.List()
	var b strings.Builder
	for _, n := range names {
		if s, ok := skills.Find(all, n); ok {
			fmt.Fprintf(&b, "- %s：%s（读 %s）\n", s.Name, clip(oneLine(s.Description), 200), filepath.Join(s.Path, "SKILL.md"))
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n# 你的 skill\n开始做任务之前，先读下面每个 skill 的 SKILL.md；任务用得上的，按里面的步骤和规则做。\n" + b.String()
}
