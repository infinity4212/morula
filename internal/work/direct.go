package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"morula/internal/chat"
	"morula/internal/harness"
	"morula/internal/store"
)

// 对话：用户不经过助理，直接和某个 agent 来回说话。
// 对话期间用户的每条消息都交给这个 agent，续跑它的 harness 会话。结束时它写纪要，追加到项目记忆末尾，
// 项目负责人和助理都读得到；对话里改了文件时，结束后验收一次。

// directKey 是存对话状态的设置项。换日、重启后对话还在。
const directKey = "chat.direct"

// Direct 是正在进行的对话。
type Direct struct {
	Agent   string   `json:"agent"`
	Project int64    `json:"project"`
	Session string   `json:"session,omitempty"` // harness 的会话 id；空表示还没说过话
	Brief   string   `json:"brief,omitempty"`   // 助理转接时写的交接说明，第一条消息带上
	Handoff string   `json:"handoff,omitempty"` // 会话太长换掉前，agent 自己写的交接记录，新会话第一条消息带上
	Prev    string   `json:"prev,omitempty"`    // 换掉的旧会话 id：换掉后还没说话就结束时，用它写纪要
	Files   []string `json:"files,omitempty"`   // 对话里改过的文件
	Since   string   `json:"since"`

	ProjectName string `json:"projectName,omitempty"` // 只给界面看，不存
}

func (e *Engine) direct() *Direct {
	e.dmu.Lock()
	defer e.dmu.Unlock()
	return e.loadDirect()
}

// loadDirect 读对话状态。调用方须持有 e.dmu。
func (e *Engine) loadDirect() *Direct {
	var d Direct
	if json.Unmarshal([]byte(e.DB.Setting(directKey, "")), &d) != nil || d.Agent == "" {
		return nil
	}
	return &d
}

func (e *Engine) saveDirect(d *Direct) {
	if d == nil {
		e.DB.SetSetting(directKey, "")
		return
	}
	b, _ := json.Marshal(d)
	e.DB.SetSetting(directKey, string(b))
}

// updateDirect 在对话还是 agent 这一场时改它的状态，返回 false 表示对话已经结束或换了人。
func (e *Engine) updateDirect(agent, since string, fn func(d *Direct)) bool {
	e.dmu.Lock()
	defer e.dmu.Unlock()
	d := e.loadDirect()
	if d == nil || d.Agent != agent || d.Since != since {
		return false
	}
	fn(d)
	e.saveDirect(d)
	return true
}

// DirectState 是界面显示用的对话状态；没有对话时返回 nil。
func (e *Engine) DirectState() *Direct {
	d := e.direct()
	if d == nil {
		return nil
	}
	if p, _ := e.DB.Project(d.Project); p != nil {
		d.ProjectName = p.Name
	}
	d.Session, d.Brief, d.Handoff, d.Prev = "", "", "", ""
	return d
}

// talkable 报告能不能和 a 对话：不是固定成员（助理、验收），harness 接入了、能续跑会话。
func talkable(a *store.Agent) error {
	switch {
	case a == nil:
		return errors.New("团队里没有这个 agent")
	case fixed(a):
		return fmt.Errorf("「%s」是固定成员，不能单独对话", a.Name)
	case !harness.Resumes(a.Runtime):
		return fmt.Errorf("「%s」用的 AI 工具 %s 现在不能用，或不支持连续对话，没法和它对话", a.Name, a.Runtime)
	}
	return nil
}

// directProject 是对话所在的项目：指定了就用它，否则用 agent 常用的第一个项目，再没有用默认项目。
func (e *Engine) directProject(a *store.Agent, id int64) *store.Project {
	if id != 0 {
		if p, _ := e.DB.Project(id); p != nil {
			return p
		}
	}
	for _, id := range a.Projects {
		if p, _ := e.DB.Project(id); p != nil {
			return p
		}
	}
	return e.DefaultProject()
}

// StartDirect 开始和 name 的对话。已经在和它对话时返回那一场；在和别人对话时先结束那一场。
// brief 是助理转接时写的交接说明，用户自己开始时为空。
func (e *Engine) StartDirect(name string, project int64, brief string) (*Direct, error) {
	a, _ := e.DB.Agent(name)
	if err := talkable(a); err != nil {
		return nil, err
	}
	if cur := e.direct(); cur != nil {
		if cur.Agent == name && brief == "" {
			return cur, nil
		}
		e.EndDirect()
	}
	p := e.directProject(a, project)
	if p == nil {
		return nil, errors.New("找不到对话要放进的项目")
	}
	d := &Direct{Agent: name, Project: p.ID, Brief: strings.TrimSpace(brief), Since: time.Now().Format(time.RFC3339Nano)}
	e.dmu.Lock()
	e.saveDirect(d)
	e.dmu.Unlock()
	how := "开始"
	if d.Brief != "" {
		how = "助理转接，开始"
	}
	e.Chat.Emit(chat.Event{"type": "direct", "on": true, "agent": name, "project": p.ID,
		"text": fmt.Sprintf("%s和「%s」对话（项目「%s」）。接下来的消息直接交给它，写 @助理 或点「结束」回到助理。", how, name, p.Name)})
	return d, nil
}

// EndDirect 结束对话。说过话时在后台让那个 agent 写纪要，写好后放进收件箱。
func (e *Engine) EndDirect() error {
	e.dmu.Lock()
	d := e.loadDirect()
	e.saveDirect(nil)
	e.dmu.Unlock()
	if d == nil {
		return errors.New("现在没有在和谁对话")
	}
	text := fmt.Sprintf("和「%s」的对话结束了，回到助理。", d.Agent)
	if d.Session != "" || d.Prev != "" {
		text += "「" + d.Agent + "」在写纪要，写好后放进收件箱，并写进项目记忆。"
		go e.minutes(*d)
	}
	e.Chat.Emit(chat.Event{"type": "direct", "on": false, "agent": d.Agent, "project": d.Project, "text": text})
	return nil
}

// mention 认出消息开头的「@名字」，返回名字和后面的话。
func mention(text string) (name, rest string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "@") && !strings.HasPrefix(t, "＠") {
		return "", "", false
	}
	_, t, _ = strings.Cut(t, string([]rune(t)[:1]))
	name, rest, _ = strings.Cut(t, " ")
	if n, r, cut := strings.Cut(name, "\n"); cut {
		name, rest = n, r+" "+rest
	}
	return strings.TrimSpace(name), strings.TrimSpace(rest), name != ""
}

// routeDirect 在助理处理之前看这条消息是不是给某个 agent 的：开头写了 @名字，或正在和某个 agent 对话。
// 返回 true 表示已经处理了；text 可能被改成去掉 @助理 之后的话。
func (e *Engine) routeDirect(ctx context.Context, text *string, files []chat.File) bool {
	if name, rest, ok := mention(*text); ok {
		a, _ := e.DB.Agent(name)
		switch {
		case name == "助理" || a != nil && a.Role == store.RoleAssistant:
			if e.direct() != nil {
				e.EndDirect()
			}
			if rest == "" && len(files) == 0 {
				return true
			}
			*text = rest
			return false
		case a != nil:
			d, err := e.StartDirect(a.Name, 0, "")
			if err != nil {
				e.Chat.EmitIn(ctx, chat.Event{"type": "error", "text": err.Error()})
				return true
			}
			if rest != "" || len(files) > 0 {
				e.directTurn(ctx, d, rest, files)
			}
			return true
		}
		// 不认识的名字：照普通消息交给助理
	}
	if d := e.direct(); d != nil {
		e.directTurn(ctx, d, *text, files)
		return true
	}
	return false
}

const directRules = `
# 规则
- 要用户回答的问题直接写在回复里，写完就停下等用户回答；用户的回答会作为下一条消息交给你。
- 你在项目的工作目录里，能读写这个目录。删除文件、git push、git reset --hard、改工作目录以外的文件这类危险操作，先在回复里说明要做什么、为什么，用户同意后再做。
- 工作目录里的 .agents、.codex、.git 三个目录在沙箱里只读。要写这几个目录时，按 harness 的方式申请越过沙箱再跑命令。
- 说一件活的状态时只用这些词：已完成、部分完成、未开始、进行中、已验证、未验证、失败、跳过、阻塞、未确认。没执行的检查不写成通过。
- 用户结束对话后，morula 会让你写一份纪要，写进项目记忆。
`

func (e *Engine) directPrompt(a *store.Agent, p *store.Project, d *Direct) string {
	var b strings.Builder
	fmt.Fprintf(&b, "你是 morula 团队里的 agent「%s」。", a.Name)
	if s := strings.TrimSpace(a.Instructions); s != "" {
		b.WriteString("你的职责说明：" + s)
	}
	b.WriteString("\n\n用户（你的领导）现在不经过助理，直接和你对话：你的每条回复用户直接看到，用户的下一条消息也直接交给你。回答用中文。\n")
	b.WriteString(skillBlock(a.Skills))
	b.WriteString("\n" + projectBlock(p, true))
	if d.Brief != "" {
		b.WriteString("\n# 助理转接时写的交接说明（助理看过之前的对话，你没看过）\n" + d.Brief + "\n")
	}
	if d.Handoff != "" {
		b.WriteString("\n# 交接记录（这场对话之前的会话太长换掉前，你自己写的）\n" + d.Handoff + "\n")
	}
	b.WriteString(directRules + wordingRule)
	return b.String()
}

// directTurn 把一条消息交给对话里的 agent。text 为空、也没有附件时是转接后的第一句：让 agent 先开口。
func (e *Engine) directTurn(ctx context.Context, d *Direct, text string, files []chat.File) {
	a, _ := e.DB.Agent(d.Agent)
	if err := talkable(a); err != nil {
		e.Chat.EmitIn(ctx, chat.Event{"type": "error", "text": err.Error() + "。对话已结束，回到助理。"})
		e.dmu.Lock()
		e.saveDirect(nil)
		e.dmu.Unlock()
		return
	}
	p := e.directProject(a, d.Project)
	if p == nil {
		e.Chat.EmitIn(ctx, chat.Event{"type": "error", "text": "找不到对话所在的项目"})
		return
	}
	defer e.hold(a.Name, "和你对话")()
	title := text
	if title == "" {
		title = "转接给「" + a.Name + "」"
	}
	f := e.newFlow(ctx, title)
	e.setFlowProject(f, p)
	step := e.addStep(f, a.Name, "对话", "u")
	ctx = withFlow(ctx, f, step)
	state := "failed"
	defer func() {
		e.setStep(f, step, map[bool]string{true: "done", false: "failed"}[state == "done"])
		e.endFlow(f, state)
	}()

	msg := "# 用户的消息\n" + text + attachNote(files)
	if strings.TrimSpace(text) == "" && len(files) == 0 {
		msg = "# 现在\n助理刚把用户转接给你。先用一两句话说你是谁、接下来怎么做，然后开始（例如提出第一个问题）。"
	}
	prompt, resume := msg, d.Session
	if resume == "" {
		prompt = e.directPrompt(a, p, d) + "\n\n" + msg
	}
	images := imagesOf(files)
	var changed []string
	x := e.newActivity(ctx, "", chat.Event{"agent": a.Name})
	on := func(ev harness.Event) {
		x.on(ev)
		if ev.Kind == "file" {
			for _, f := range strings.Split(ev.Text, "\n") {
				if f = strings.TrimSpace(f); f != "" && !slices.Contains(changed, f) {
					changed = append(changed, f)
				}
			}
		}
	}
	for {
		s := spec(a, p.Dir, prompt, "")
		s.Access, s.Resume, s.Images = harness.Workspace, resume, images
		var res harness.Result
		more, err := interruptible(ctx, func(c context.Context) error {
			var err error
			res, err = harness.Ask(c, a.Runtime, s, nil, on)
			return err
		})
		if res.Session != "" {
			resume = res.Session
		}
		e.updateDirect(d.Agent, d.Since, func(cur *Direct) {
			if resume != "" {
				cur.Session = resume
			}
			for _, f := range changed {
				if !slices.Contains(cur.Files, f) {
					cur.Files = append(cur.Files, f)
				}
			}
		})
		if more != "" { // 用户补充了：停掉这次，带着补充重来
			text += "\n\n用户补充：" + more
			if resume != "" {
				prompt = "刚才那次被用户的补充打断了，按补充后的要求重新回答。\n\n# 用户的消息\n" + text
				images = nil
			} else {
				prompt = e.directPrompt(a, p, d) + "\n\n# 用户的消息\n" + text + attachNote(files)
			}
			continue
		}
		if err != nil {
			state = map[bool]string{true: "stopped", false: "failed"}[ctx.Err() != nil]
			x.end(state, false, res.Tokens)
			if ctx.Err() == nil {
				e.Chat.EmitIn(ctx, chat.Event{"type": "error", "text": "「" + a.Name + "」回复失败：" + err.Error()})
			}
			return
		}
		x.end("done", true, res.Tokens)
		e.say(ctx, "收到回复", res.Final, chat.Event{"agent": a.Name, "project": p.ID})
		state = "done"
		if resume != "" && res.ContextTokens() > e.compactLimit() {
			e.compactDirect(a, p, d, resume)
		}
		return
	}
}

const minutesPrompt = `用户结束了这次对话。写一份纪要，给项目负责人和助理看，他们没看过这段对话：
- 结论和决定
- 待办：谁做、做什么
- 改了的文件（路径）；没改写「没改文件」
- 没解决的问题
只写纪要本身，不写开场白，不写客套话。不写纪要的大标题；四部分的小标题用加粗，不用 # 标题。`

// minutes 让对话里的 agent 写纪要，追加到项目记忆末尾，记进助理的「会话外发生的事」，放进收件箱。
// 对话里改了文件时验收一次，结论随纪要显示。
func (e *Engine) minutes(d Direct) {
	ctx := context.Background()
	a, _ := e.DB.Agent(d.Agent)
	if a == nil {
		return
	}
	p := e.directProject(a, d.Project)
	if p == nil {
		return
	}
	defer e.hold(a.Name, "写对话纪要")()
	s := spec(a, p.Dir, minutesPrompt+"\n"+wordingRule, "")
	s.Access, s.Resume = harness.ReadOnly, orDefault(d.Session, d.Prev)
	res, err := harness.Ask(ctx, a.Runtime, s, nil, nil)
	text := demote(strings.TrimSpace(res.Final))
	if err != nil || text == "" {
		why := "没有写出内容"
		if err != nil {
			why = err.Error()
		}
		e.Chat.Emit(chat.Event{"type": "error", "inbox": true, "project": p.ID,
			"text": "「" + a.Name + "」写对话纪要失败：" + why + "。项目记忆没有改。"})
		return
	}
	p, _ = e.DB.Project(p.ID) // 对话期间项目记忆可能被别的安排改过，读最新的
	if p == nil {
		return
	}
	mem := strings.TrimSpace(p.Memory) + fmt.Sprintf("\n\n## 纪要：用户和「%s」的对话（%s）\n", a.Name, time.Now().Format("2006-01-02 15:04")) + text
	e.DB.UpdateProject(p.ID, nil, &mem, nil)
	e.noteMissed(fmt.Sprintf("用户直接和「%s」对话了（项目「%s」），纪要已写进项目记忆：\n%s", a.Name, p.Name, text))

	out := fmt.Sprintf("和「%s」的对话纪要，已写进「%s」的项目记忆。\n\n%s", a.Name, p.Name, text)
	failed := false
	if len(d.Files) > 0 {
		out += "\n\n" + e.acceptDirect(ctx, a, p, d, text, &failed)
	}
	e.Chat.Emit(chat.Event{"type": "reply", "inbox": true, "minutes": true, "failed": failed, "agent": a.Name, "project": p.ID, "text": out})
}

// acceptDirect 验收对话里改的文件，记成项目里的一次安排，返回给用户看的结论。
func (e *Engine) acceptDirect(ctx context.Context, a *store.Agent, p *store.Project, d Direct, minutes string, failed *bool) string {
	runID, err := e.DB.AddRun(p.ID, "用户和「"+a.Name+"」的对话")
	if err != nil {
		*failed = true
		return "验收：未验证，记录这次对话失败：" + err.Error()
	}
	e.DB.SetRun(runID, "plan", minutes)
	tree := "（对话，没有任务树）\n「" + a.Name + "」在对话里改了这些文件：\n- " + strings.Join(d.Files, "\n- ") + "\n"
	v, err := e.accept(ctx, &run{id: runID, project: p, inbox: true}, p, "用户在和「"+a.Name+"」的对话里要做的事，见下面的纪要。", minutes, tree)
	st, note := "done", ""
	switch {
	case err != nil:
		st, note = "failed", "验收：未验证，验收失败："+err.Error()
	case v == nil:
		note = "验收：未验证，团队里没有验收 agent。"
	case v.Pass:
		note = "验收：已验证，通过。查了：\n" + orEmpty(v.Evidence)
	default:
		st, note = "failed", "验收：没通过。问题：\n"+orEmpty(v.Problems)
	}
	*failed = st == "failed"
	e.DB.SetRun(runID, "reply", note)
	e.DB.SetRun(runID, "status", st)
	return note
}

// demote 把纪要里的 Markdown 标题降到项目记忆「## 纪要」这一节之下：开头的一级标题（纪要的大标题）去掉，
// 其余标题至少降成三级。不然纪要里的一级、二级标题会把项目记忆后面的结构切断。
func demote(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && strings.HasPrefix(lines[0], "# ") {
		lines = lines[1:]
	}
	fence := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fence = !fence
		}
		if fence || !strings.HasPrefix(l, "#") {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, "#"))
		if n < 3 && len(l) > n && l[n] == ' ' {
			lines[i] = strings.Repeat("#", 3-n) + l
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
