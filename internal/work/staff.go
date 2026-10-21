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
	"morula/internal/skills"
	"morula/internal/store"
)

// 配置子 agent：助理按项目和本机的 skill 提议新建或修改子 agent，用户勾选后才改花名册。
// 两个时机：整理本机会话建好项目后问一次（全部项目）；之后某个项目的任务总落到通用执行者、
// 或某个 agent 总被打回时，按那个项目的任务问（有冷却期）。
// 只把项目摘要、项目记忆的开头、skill 的名字和说明、花名册交给助理的模型；聚焦提议另带任务的标题、要求和打回意见的开头。

// StaffItem 是助理提议的一项改动。
type StaffItem struct {
	Action       string       `json:"action"` // new 新建，update 修改已有 agent
	Name         string       `json:"name"`
	Role         string       `json:"role"`
	Instructions string       `json:"instructions"`
	Skills       []string     `json:"skills"`
	Projects     []int64      `json:"projects"`
	Reason       string       `json:"reason"`
	Old          *store.Agent `json:"old,omitempty"` // 修改时是改之前的样子
	// 新建时用的 harness、模型、思考强度；空时用助理的配置
	Runtime string `json:"runtime,omitempty"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
}

// Staff 是配置子 agent 的进度，配置页轮询它。
type Staff struct {
	State     string      `json:"state"` // idle、working、ready、failed
	Error     string      `json:"error,omitempty"`
	Model     string      `json:"model,omitempty"`
	Started   string      `json:"started,omitempty"`
	Why       string      `json:"why,omitempty"` // 为什么提议；聚焦某个项目时写触发条件
	Proposals []StaffItem `json:"proposals"`
}

// Focus 让提议只看一个项目：带上触发提议的任务。
type Focus struct {
	Project int64
	Why     string
	Tasks   []store.Task
}

const (
	staffKey    = "staff.result"  // 存提议，app 重启后还能接着勾选
	staffAsked  = "staff.asked"   // 用户回答过要不要按整理出的项目配置子 agent
	snoozeKey   = "staff.snooze." // 加项目编号：这个时间之前不再按任务情况提议
	staffWindow = 14 * 24 * time.Hour
	snoozeFor   = 7 * 24 * time.Hour
	// staffHits 是触发提议的次数：落到通用执行者的任务数，或同一个 agent 被打回、失败的任务数
	staffHits = 3
)

func (e *Engine) Staffing() Staff {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.staff.State == "" {
		json.Unmarshal([]byte(e.DB.Setting(staffKey, "{}")), &e.staff)
	}
	s := e.staff
	if s.State == "" {
		s.State = "idle"
	}
	if s.Proposals == nil {
		s.Proposals = []StaffItem{}
	}
	return s
}

// StartStaff 在后台让助理提议。focus 为 nil 时看全部项目。
func (e *Engine) StartStaff(focus *Focus) error {
	a, err := e.assistant()
	if err != nil {
		return err
	}
	if focus == nil {
		if ps, err := e.DB.Projects(); err != nil {
			return err
		} else if len(ps) == 0 {
			return errors.New("还没有项目，没有可依据的内容")
		}
	}
	e.mu.Lock()
	if e.staff.State == "working" {
		e.mu.Unlock()
		return errors.New("正在提议")
	}
	why := ""
	if focus != nil {
		why = focus.Why
	}
	e.staff = Staff{State: "working", Model: a.Runtime + " · " + a.Model, Started: time.Now().Format(time.RFC3339), Why: why}
	e.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		items, err := e.proposeStaff(ctx, a, focus)
		e.mu.Lock()
		defer e.mu.Unlock()
		if err != nil {
			e.staff.State, e.staff.Error = "failed", err.Error()
			return
		}
		e.staff.State, e.staff.Proposals = "ready", items
		b, _ := json.Marshal(e.staff)
		e.DB.SetSetting(staffKey, string(b))
	}()
	return nil
}

const staffSchema = `{"type":"object","additionalProperties":false,"required":["agents"],"properties":{
"agents":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["action","name","role","instructions","skills","projects","reason"],
"properties":{"action":{"type":"string","enum":["new","update"]},"name":{"type":"string"},"role":{"type":"string"},"instructions":{"type":"string"},
"skills":{"type":"array","items":{"type":"string"}},"projects":{"type":"array","items":{"type":"integer"}},"reason":{"type":"string"}}}}}}`

const staffRules = `按下面的项目和本机的 skill，提议新建或修改子 agent。子 agent 是你派活的执行者。
- 按能力分 agent，不按项目分：几个项目要的是同一种本事，就用一个 agent，在 projects 里列出这些项目的编号。
- 已有 agent 能胜任的，用 update 改它：补 skill、补常用项目、改职责说明。不新建和已有 agent 重复的。
- action 填 new 或 update。update 时 name 原样填已有 agent 的名字，其余字段填改后的完整内容（不是只填改动）。
- new 的 name 简短，不和已有 agent 重名。role 填简短的角色名，例如 开发、写作、调研、测试；不填「助理」。
- instructions 写这个 agent 负责哪类事、做事的标准，几句话；不写项目名和项目编号（写在 projects 里），不写模型和 harness。
- skills 只填下面列出的 skill 名，只绑这个 agent 做事真用得上的。projects 只填下面列出的项目编号。
- reason 一句话写为什么建或改：依据哪些项目、哪些 skill、哪些任务。
- 没必要建或改的不提。可以返回空列表。`

func (e *Engine) proposeStaff(ctx context.Context, a *store.Agent, focus *Focus) ([]StaffItem, error) {
	defer e.hold(a.Name, "提议 agent")()
	var projects []store.Project
	if focus != nil {
		p, err := e.DB.Project(focus.Project)
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, errors.New("项目不存在")
		}
		projects = []store.Project{*p}
	} else {
		list, err := e.DB.Projects()
		if err != nil {
			return nil, err
		}
		for _, p := range list {
			if full, _ := e.DB.Project(p.ID); full != nil {
				projects = append(projects, *full)
			}
		}
	}
	all := skills.List()
	agents := e.staffable()
	var b strings.Builder
	b.WriteString(e.role(a) + "\n\n" + staffRules + "\n\n# 项目\n")
	for _, p := range projects {
		fmt.Fprintf(&b, "## 编号 %d「%s」\n摘要：%s\n", p.ID, p.Name, oneLine(p.Summary))
		if m := strings.TrimSpace(p.Memory); m != "" {
			fmt.Fprintf(&b, "记忆开头：%s\n", clip(oneLine(m), 600))
		}
	}
	b.WriteString("\n# 本机的 skill\n")
	for _, s := range all {
		fmt.Fprintf(&b, "- %s：%s\n", s.Name, clip(oneLine(s.Description), 200))
	}
	if len(all) == 0 {
		b.WriteString("（本机没有 skill，skills 填空列表）\n")
	}
	b.WriteString("\n# 已有的子 agent\n")
	for _, ag := range agents {
		fmt.Fprintf(&b, "- %s（%s）：%s；skill：%s；常用项目编号：%v\n", ag.Name, ag.Role, oneLine(ag.Instructions), strings.Join(ag.Skills, "、"), ag.Projects)
	}
	if len(agents) == 0 {
		b.WriteString("（还没有）\n")
	}
	if focus != nil {
		fmt.Fprintf(&b, "\n# 为什么要提议\n%s\n\n# 相关任务\n", focus.Why)
		for _, t := range focus.Tasks {
			fmt.Fprintf(&b, "- [%s，第 %d 次，%s] %s：%s", t.Agent, t.Attempts, t.Status, oneLine(t.Title), clip(oneLine(t.Brief), 200))
			if t.Feedback != "" {
				b.WriteString("；打回意见：" + clip(oneLine(t.Feedback), 200))
			}
			b.WriteString("\n")
		}
	}
	var out struct {
		Agents []StaffItem `json:"agents"`
	}
	s := spec(a, scratch(), b.String(), staffSchema)
	s.Access, s.Ephemeral = harness.ReadOnly, true
	if _, err := harness.Ask(ctx, a.Runtime, s, &out, nil); err != nil {
		return nil, err
	}
	ids := map[int64]bool{}
	for _, p := range projects {
		ids[p.ID] = true
	}
	return e.checkStaff(out.Agents, all, ids), nil
}

// staffable 是能被提议修改的 agent：除了助理和验收 agent 都是。
func (e *Engine) staffable() []store.Agent {
	list, _ := e.DB.Agents()
	out := []store.Agent{}
	for _, a := range list {
		if !fixed(&a) {
			out = append(out, a)
		}
	}
	return out
}

// checkStaff 去掉不合规的提议：编出来的 skill 和项目编号丢掉；不碰助理和验收 agent；
// update 要改已有的 agent，new 不能和已有的重名；同一个名字只留第一项。
// projects 是这次交给助理的项目；update 时原来绑的、不在这次范围里的项目保留。
func (e *Engine) checkStaff(in []StaffItem, all []skills.Skill, projects map[int64]bool) []StaffItem {
	agents := e.staffable()
	byName := map[string]*store.Agent{}
	for i := range agents {
		byName[agents[i].Name] = &agents[i]
	}
	seen := map[string]bool{}
	out := []StaffItem{}
	for _, it := range in {
		it.Name, it.Role = strings.TrimSpace(it.Name), strings.TrimSpace(it.Role)
		if it.Name == "" || seen[it.Name] || it.Name == AcceptName || it.Role == store.RoleAssistant {
			continue
		}
		if as, _ := e.DB.Assistant(); as != nil && as.Name == it.Name {
			continue
		}
		old := byName[it.Name]
		switch it.Action {
		case "update":
			if old == nil {
				continue
			}
			cp := *old
			it.Old = &cp
		case "new":
			if old != nil {
				continue
			}
			if ag, _ := e.DB.Agent(it.Name); ag != nil {
				continue
			}
			it.Old = nil
		default:
			continue
		}
		if it.Role == "" {
			it.Role = "执行"
		}
		sk := []string{}
		for _, n := range it.Skills {
			if _, ok := skills.Find(all, strings.TrimSpace(n)); ok && !slices.Contains(sk, strings.TrimSpace(n)) {
				sk = append(sk, strings.TrimSpace(n))
			}
		}
		ps := []int64{}
		if old != nil {
			for _, id := range old.Projects {
				if !projects[id] {
					ps = append(ps, id)
				}
			}
		}
		for _, id := range it.Projects {
			if projects[id] && !slices.Contains(ps, id) {
				ps = append(ps, id)
			}
		}
		it.Skills, it.Projects = sk, ps
		it.Instructions, it.Reason = strings.TrimSpace(it.Instructions), strings.TrimSpace(it.Reason)
		it.Runtime, it.Model, it.Effort = "", "", ""
		seen[it.Name] = true
		out = append(out, it)
	}
	return out
}

// ApplyStaff 按用户勾选（可能改过）的提议新建或修改 agent。用户改过的 skill 和项目再查一遍。
// 返回每一项的错误，没有错误时对应位置是空串。
func (e *Engine) ApplyStaff(items []StaffItem) ([]string, error) {
	if e.Staffing().State != "ready" {
		return nil, errors.New("还没有提议")
	}
	as, err := e.assistant()
	if err != nil {
		return nil, err
	}
	all := skills.List()
	ids := map[int64]bool{}
	if ps, err := e.DB.Projects(); err == nil {
		for _, p := range ps {
			ids[p.ID] = true
		}
	}
	errs := make([]string, len(items))
	for i, it := range items {
		checked := e.checkStaff([]StaffItem{it}, all, ids)
		if len(checked) == 0 {
			errs[i] = "这一项不能用：团队里没有这个名字，或者要改的是助理、验收 agent"
			continue
		}
		c := checked[0]
		if c.Action == "new" {
			ag := store.Agent{Name: c.Name, Role: c.Role, Runtime: it.Runtime, Model: it.Model, Effort: it.Effort,
				Instructions: c.Instructions, Skills: c.Skills, Projects: c.Projects}
			if ag.Runtime == "" {
				ag.Runtime, ag.Model, ag.Effort = as.Runtime, as.Model, workerEffort(as)
			}
			err = e.DB.AddAgent(ag)
		} else {
			err = e.DB.UpdateAgent(c.Name, store.Patch{Role: &c.Role, Instructions: &c.Instructions, Skills: &c.Skills, Projects: &c.Projects})
		}
		if err != nil {
			errs[i] = err.Error()
		}
	}
	e.DiscardStaff()
	return errs, nil
}

// DiscardStaff 丢掉提议，不改花名册。
func (e *Engine) DiscardStaff() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.staff.State != "working" {
		e.staff = Staff{State: "idle"}
		e.DB.SetSetting(staffKey, "{}")
	}
}

// offerStaff 问一张卡片，用户选了 go 就开始提议，提议好后回一条带「打开配置页」按钮的消息。
// inbox 为 true 时卡片和结果都放收件箱，不插进对话。key 防止同一件事同时问两张卡片。answered 在用户回答后调用。
func (e *Engine) offerStaff(key, text, reason, yes, no string, focus *Focus, inbox bool, answered func(yes bool)) {
	e.mu.Lock()
	if e.asking[key] {
		e.mu.Unlock()
		return
	}
	e.asking[key] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.asking, key)
		e.mu.Unlock()
	}()
	ans, err := e.Chat.Wait(context.Background(), chat.Event{
		"type": "ask", "inbox": inbox, "text": text, "reason": reason,
		"options": []map[string]string{{"value": "go", "label": yes}, {"value": "no", "label": no}},
	})
	if err != nil {
		return
	}
	ok := ans.Choice == "go"
	answered(ok)
	say := func(text string, show bool) {
		ev := chat.Event{"type": "reply", "inbox": inbox, "text": text, "verdict": "fixed"}
		if show {
			ev["show"] = "staff"
		}
		e.Chat.Emit(ev)
	}
	if !ok {
		say("好的。以后想配置，到「Agent」页点「按项目和 skill 配置」。", false)
		return
	}
	if err := e.StartStaff(focus); err != nil {
		say("没能开始提议："+err.Error(), true)
		return
	}
	e.Chat.Emit(chat.Event{"type": "note", "inbox": inbox, "text": "助理在提议 agent，提议好后在这里告诉你。"})
	for {
		time.Sleep(3 * time.Second)
		s := e.Staffing()
		switch s.State {
		case "working":
			continue
		case "ready":
			if len(s.Proposals) == 0 {
				say("看过了，现有的 agent 够用，没有要新建或修改的。", false)
				e.DiscardStaff()
			} else {
				say(fmt.Sprintf("提议好了：%s。请到配置页勾选要采用的。", countStaff(s.Proposals)), true)
			}
		default:
			say("提议失败："+s.Error+"。可以到配置页重试。", true)
		}
		return
	}
}

func countStaff(items []StaffItem) string {
	n, u := 0, 0
	for _, it := range items {
		if it.Action == "new" {
			n++
		} else {
			u++
		}
	}
	var parts []string
	if n > 0 {
		parts = append(parts, fmt.Sprintf("新建 %d 个", n))
	}
	if u > 0 {
		parts = append(parts, fmt.Sprintf("修改 %d 个", u))
	}
	return strings.Join(parts, "，")
}

// offerStaffAfterOrganize 在整理建好项目后问一次要不要配置子 agent。用户回答过就不再问。
func (e *Engine) offerStaffAfterOrganize(created int) {
	if created == 0 || e.DB.Setting(staffAsked, "") != "" {
		return
	}
	a, err := e.assistant()
	if err != nil {
		return
	}
	n := len(skills.List())
	go e.offerStaff("organize",
		fmt.Sprintf("项目建好了。要我按这些项目和本机的 %d 个 skill，提议新建或修改 agent 吗？", n),
		fmt.Sprintf("提议时把项目摘要、项目记忆的开头、skill 的名字和说明交给助理的模型（%s · %s）。你勾选后才改团队。", a.Runtime, a.Model),
		"提议", "以后再说", nil, false, // 刚整理完、用户在等这一步，放在对话里
		func(bool) { e.DB.SetSetting(staffAsked, "1") })
}

// reviewStaff 在一次安排结束后看这个项目最近的任务：落到通用执行者的多了，或某个 agent 总被打回、失败，
// 就在收件箱里问用户要不要按这些任务提议。用户回答后（不管选什么）这个项目冷却 snoozeFor。
func (e *Engine) reviewStaff(p *store.Project) {
	key := snoozeKey + fmt.Sprint(p.ID)
	if t, err := time.Parse(time.RFC3339, e.DB.Setting(key, "")); err == nil && time.Now().Before(t) {
		return
	}
	if st := e.Staffing().State; st == "working" || st == "ready" {
		return // 还有没处理的提议
	}
	why, tasks := e.staffSignal(p.ID)
	if why == "" {
		return
	}
	go e.offerStaff(key,
		fmt.Sprintf("「%s」%s。要我按这些任务提议新建或修改 agent 吗？", p.Name, why),
		"提议时把这个项目的摘要、记忆的开头、这些任务的标题、要求和打回意见的开头交给助理的模型。你勾选后才改团队。不管选哪个，这个项目 7 天内不再问。",
		"提议", "不用", &Focus{Project: p.ID, Why: why, Tasks: tasks}, true, // 不急，放收件箱
		func(bool) { e.DB.SetSetting(key, time.Now().Add(snoozeFor).Format(time.RFC3339)) })
}

// staffSignal 返回触发提议的原因和相关任务；没到次数时原因是空串。
func (e *Engine) staffSignal(project int64) (string, []store.Task) {
	tasks, err := e.DB.RecentTasks(project, time.Now().Add(-staffWindow))
	if err != nil {
		return "", nil
	}
	agents := map[string]*store.Agent{}
	for _, a := range e.staffable() {
		agents[a.Name] = &a
	}
	var general []store.Task
	bad := map[string][]store.Task{}
	for _, t := range tasks {
		a := agents[t.Agent]
		if a == nil {
			continue
		}
		if generalist(a) {
			general = append(general, t)
		}
		if t.Status == "failed" || t.Attempts > 1 {
			bad[t.Agent] = append(bad[t.Agent], t)
		}
	}
	var parts []string
	var related []store.Task
	if len(general) >= staffHits {
		parts = append(parts, fmt.Sprintf("有 %d 个任务落到了通用执行者", len(general)))
		related = append(related, general...)
	}
	names := []string{}
	for n := range bad {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if len(bad[n]) >= staffHits {
			parts = append(parts, fmt.Sprintf("「%s」有 %d 个任务被打回或失败", n, len(bad[n])))
			for _, t := range bad[n] {
				if !slices.ContainsFunc(related, func(r store.Task) bool { return r.ID == t.ID }) {
					related = append(related, t)
				}
			}
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	if len(related) > 30 {
		related = related[len(related)-30:]
	}
	return "最近 14 天" + strings.Join(parts, "，"), related
}
