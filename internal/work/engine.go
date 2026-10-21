// Package work 是助理和多子 agent 执行系统。
//
//  1. 路由：助理只看全部项目的项目摘要，判断消息属于哪个项目；拿不准就问用户。
//  2. 安排：选定项目后才加载项目记忆，拆成顶层任务，派给花名册里的子 agent。
//  3. 执行：任务树逐层往下拆；子节点互不共享状态，做完向父节点汇报。
//  4. 检查：父节点先跑机器检查，再让模型判断，不通过就打回重做。
//  5. 汇报：助理收齐顶层汇报，更新项目记忆和项目摘要，向用户汇报。
//
// 每个 agent 都按「模型 + harness + skill」组装，经 internal/harness 启动。
// 审批默认 auto：harness 在工作目录里自主执行；危险操作由子 agent 先上报，用户批准后才做。
package work

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"morula/internal/chat"
	"morula/internal/harness"
	"morula/internal/journal"
	"morula/internal/store"
)

type Engine struct {
	DB   *store.Store
	Chat *chat.Session

	mu     sync.Mutex
	runs   map[int64]context.CancelFunc // 在跑的安排
	lanes  map[int64]chan struct{}      // 项目 id -> 执行权：同一个项目同时只跑一次安排，后来的排队
	live   map[int64]string             // 任务 id -> 最近一步在做什么
	busy   map[string]int               // agent 名字 -> 正在跑的 harness 进程数，用来更新花名册上的状态
	hires  sync.Mutex                   // 自动新建执行者时串行，免得建出两个
	org    Organize                     // 首次整理的进度（onboard.go）
	scan   scanCache                    // 本机会话的扫描结果（onboard.go）
	staff  Staff                        // 配置子 agent 的进度（staff.go）
	asking map[string]bool              // 正在对话里问的配置卡片，免得同一件事问两张
	// 正在写介绍的 agent，和上一次没写成的原因（describe.go）
	describing  map[string]bool
	describeErr map[string]string
	// 最近的流程（flow.go）
	flows   []*Flow
	flowSeq int64
	// agent 方式的助理（agent.go）：会话是否被占着、清空或撤回的次数、会话外发生的事、工具服务
	threadBusy bool
	threadGen  int
	threadDay  string // takeThread 时是哪一天；跨过换日的一轮，会话仍记在开始的那天
	missed     []string
	lastProj   int64 // 上一条回复标的项目：这一轮没用到项目时（接着上一句说），回复沿用它
	tools      toolHost
	dmu        sync.Mutex // 改对话状态（direct.go）时串行
}

func New(db *store.Store, c *chat.Session) *Engine {
	e := &Engine{DB: db, Chat: c, runs: map[int64]context.CancelFunc{}, lanes: map[int64]chan struct{}{}, live: map[int64]string{}, busy: map[string]int{}, asking: map[string]bool{},
		describing: map[string]bool{}, describeErr: map[string]string{}}
	if d, err := store.Dir(); err == nil {
		harness.SetExportDir(filepath.Join(d, "sessions")) // harness 不把会话存成文件时（opencode），翻旧会话读这里导出的
	}
	c.Turn = e.Turn
	c.Summarize = e.Summarize
	c.BriefAt = func() string { return db.Setting("assistant.brief_at", "") }
	c.InDirect = func() bool { return e.direct() != nil }
	onSwap = func(agent, from, to string) {
		// 不急，放收件箱，不插进对话
		c.Emit(chat.Event{"type": "note", "inbox": true, "text": "「" + agent + "」的模型 " + from + " 已经不能用了，这次改用 " + to + "。到 Agent 页给它换一个模型，这条提示就不再出现"})
	}
	return e
}

// Limits 是任务树的限制，在设置里改。
type Limits struct {
	MaxDepth int    `json:"maxDepth"` // 最大层数
	Parallel int    `json:"parallel"` // 同时运行的 harness 进程
	Retries  int    `json:"retries"`  // 单个节点被打回后最多重做几次
	Root     string `json:"root"`     // 新项目的工作目录放在哪里
}

func (e *Engine) Limits() Limits {
	n := func(k string, def, lo, hi int) int {
		v, err := strconv.Atoi(e.DB.Setting(k, ""))
		if err != nil || v < lo || v > hi {
			return def
		}
		return v
	}
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, "morula-projects")
	return Limits{
		MaxDepth: n("work.maxDepth", 3, 1, 5),
		Parallel: n("work.parallel", 3, 1, 16),
		Retries:  n("work.retries", 1, 0, 5),
		Root:     e.DB.Setting("projects.root", root),
	}
}

func (e *Engine) SetLimits(l Limits) error {
	if l.MaxDepth < 1 || l.MaxDepth > 5 || l.Parallel < 1 || l.Parallel > 16 || l.Retries < 0 || l.Retries > 5 {
		return errors.New("层数 1 到 5，并发 1 到 16，重做 0 到 5")
	}
	root := expand(strings.TrimSpace(l.Root))
	if !filepath.IsAbs(root) {
		return errors.New("项目根目录要写绝对路径")
	}
	e.DB.SetSetting("work.maxDepth", strconv.Itoa(l.MaxDepth))
	e.DB.SetSetting("work.parallel", strconv.Itoa(l.Parallel))
	e.DB.SetSetting("work.retries", strconv.Itoa(l.Retries))
	e.DB.SetSetting("projects.root", root)
	return nil
}

func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// Live 返回每个在跑的任务最近一步在做什么。
func (e *Engine) Live() map[int64]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[int64]string, len(e.live))
	for k, v := range e.live {
		out[k] = v
	}
	return out
}

func (e *Engine) setLive(id int64, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if text == "" {
		delete(e.live, id)
	} else {
		e.live[id] = clip(strings.TrimSpace(text), 200)
	}
}

// hold 在花名册上把 agent 标成工作中，返回的函数把它放回空闲（同一 agent 的所有进程都结束后）。
func (e *Engine) hold(name, task string) func() {
	e.mu.Lock()
	e.busy[name]++
	e.mu.Unlock()
	e.DB.SetStatus(name, "working", task)
	return func() {
		e.mu.Lock()
		e.busy[name]--
		n := e.busy[name]
		e.mu.Unlock()
		if n == 0 {
			e.DB.SetStatus(name, "idle", "")
		}
	}
}

// Running 返回在跑的安排 id。
func (e *Engine) Running() []int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []int64{}
	for id := range e.runs {
		out = append(out, id)
	}
	return out
}

// StopAll 停掉所有在跑的安排。
func (e *Engine) StopAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.runs {
		c()
	}
}

func (e *Engine) StopRun(id int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.runs[id]
	if !ok {
		return errors.New("这次安排没有在跑")
	}
	c()
	return nil
}

// assistant 返回助理；花名册里没有时报错。
func (e *Engine) assistant() (*store.Agent, error) {
	a, err := e.DB.Assistant()
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, errors.New("团队里没有助理。到「设置」把助理角色指给一个 agent")
	}
	return a, nil
}

// scratch 是助理只读运行时的工作目录：一个 morula 自己的空目录，避免读到某个项目的说明文件。
func scratch() string {
	d, err := store.Dir()
	if err != nil {
		d = os.TempDir()
	}
	d = filepath.Join(d, "assistant")
	os.MkdirAll(d, 0o755)
	return d
}

func spec(a *store.Agent, dir, prompt, schema string) harness.Spec {
	model, effort := liveModel(a)
	// 「用模型默认」时写明模型自己的默认强度。不写的话 codex 用用户全局配置里的 model_reasoning_effort（例如 low），不是模型默认。
	if m, ok := pickModel(a); ok && effort == "" {
		effort = m.Default
	}
	return harness.Spec{Dir: dir, Model: model, Effort: effort, Prompt: prompt, Schema: schema}
}

// 模型下架（例如 codex 换了可用的模型列表）后，花名册里还写着旧模型，一跑就报「not supported」。
// liveModel 在 a 的模型不在 harness 当前的列表里时，这次改用列表里的第一个，思考强度不支持时用模型默认。
// 花名册不改，由用户在 Agent 页换；每个 agent 的每个旧模型只在对话里提示一次（onSwap）。
// 列表读不到（例如没有缓存）时照原样跑。
var (
	swapMu  sync.Mutex
	swapped = map[string]bool{}
	onSwap  func(agent, from, to string)
)

func liveModel(a *store.Agent) (model, effort string) {
	to, ok := pickModel(a)
	if !ok || to.ID == a.Model {
		return a.Model, a.Effort
	}
	if a.Effort != "" && !slices.Contains(to.Efforts, a.Effort) {
		effort = ""
	} else {
		effort = a.Effort
	}
	swapMu.Lock()
	key := a.Name + "\x00" + a.Model
	first := !swapped[key]
	swapped[key] = true
	swapMu.Unlock()
	if first && onSwap != nil {
		onSwap(a.Name, a.Model, to.ID)
	}
	return to.ID, effort
}

func (e *Engine) role(a *store.Agent) string {
	s := assistantRole
	if strings.TrimSpace(a.Instructions) != "" {
		s += "\n\n你的职责说明：" + a.Instructions
	}
	return s
}

func recent() string {
	t, _ := journal.Recent(journal.Today(), 4000)
	if strings.TrimSpace(t) == "" {
		return "（今天还没有对话）"
	}
	return t
}

type route struct {
	Decision   string   `json:"decision"`
	ProjectID  int64    `json:"project_id"`
	Name       string   `json:"name"`
	Summary    string   `json:"summary"`
	Candidates []int64  `json:"candidates"`
	Reason     string   `json:"reason"`
	Quote      string   `json:"quote"`
	Reply      string   `json:"reply"`
	Actions    []change `json:"actions"`
}

// attachNote 把附件写成提示词里的一段。子 agent 看不到用户的消息，派活时要把路径写进任务说明。
func attachNote(files []chat.File) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n用户随消息发来的附件（已存到本机，可以直接读）：\n")
	for _, f := range files {
		kind := "文件"
		if f.Image {
			kind = "图片"
		}
		fmt.Fprintf(&b, "- %s（%s，%d 字节）：%s\n", f.Name, kind, f.Size, f.Path)
	}
	b.WriteString("派活时，把任务要用的附件路径写进任务说明，子 agent 看不到这条消息。")
	return b.String()
}

func imagesOf(files []chat.File) []string {
	var out []string
	for _, f := range files {
		if f.Image {
			out = append(out, f.Path)
		}
	}
	return out
}

// Turn 处理一条用户消息。开头写了 @名字、或正在和某个 agent 对话时交给那个 agent（direct.go）；
// 否则交给助理：助理能当 agent 时交给 agentTurn，不能时路由到项目，再安排。
// 附件写进提示词，图片同时交给 harness。
func (e *Engine) Turn(ctx context.Context, text string, files []chat.File) {
	if e.routeDirect(ctx, &text, files) {
		return
	}
	a, err := e.assistant()
	if err != nil {
		e.Chat.EmitIn(ctx, chat.Event{"type": "error", "text": err.Error()})
		return
	}
	defer e.hold(a.Name, "处理用户的消息")()
	if e.agentMode(a) {
		e.agentTurn(ctx, a, text, files)
		return
	}
	title := text
	if strings.TrimSpace(title) == "" && len(files) > 0 {
		title = "发来附件 " + files[0].Name
	}
	f := e.newFlow(ctx, title)
	ok := false
	defer func() {
		// 转进任务树的流程由 execRun 收尾
		if f.Run == 0 {
			e.endFlow(f, map[bool]string{true: "done", false: map[bool]string{true: "stopped", false: "failed"}[ctx.Err() != nil]}[ok])
		}
	}()
	if p := e.pinned(ctx); p != nil { // 在项目页里发的：不判断归属，直接在这个项目里安排
		e.setFlowProject(f, p)
		if strings.TrimSpace(text) == "" {
			text = title
		}
		ok = e.plan(ctx, a, p, text, files)
		return
	}
	projects, err := e.DB.Projects()
	if err != nil {
		e.Chat.EmitIn(ctx, chat.Event{"type": "error", "text": err.Error()})
		return
	}
	mem := strings.TrimSpace(journal.Memory())
	if mem == "" {
		mem = "（空）"
	}
	// 项目和执行者只给名字和介绍：harness 能接工具时让助理自己查，不能时直接写进提示词。
	tools := directoryTools(a.Runtime)
	directory := "\n\n" + toolsNote
	if tools == nil {
		directory = "\n\n# 已有项目（只有项目摘要）\n" + ProjectList(e.DB) +
			"\n# 团队里的 agent\n" + AgentList(e.DB)
	}
	// morula mcp --assistant 是另一个进程，接不到插件的模型（harness.Plugins 只在主进程里设），所以写进提示词。
	directory += "\n\n# 本机的 harness 和模型\n" + clip(HarnessList(e.DB, false), 6000)
	routed := e.addStep(f, a.Name, "判断归属", "u")
	ctx = withFlow(ctx, f, routed)
	var p *store.Project
	// 用户在改团队的确认卡片里写了补充、没选选项时，带着补充重新判断。最多补充 3 次。
	// 用户把排队的消息补充进来时也重新判断，这种不计次数。
	for round := 0; ; {
		prompt := e.role(a) + "\n\n# 全局记忆（用户偏好）\n" + mem + directory +
			"\n\n# 今天最近的对话\n" + recent() +
			"\n\n# 用户的新消息\n" + text + attachNote(files) + "\n\n" + routeRules

		e.Chat.EmitIn(ctx, chat.Event{"type": "progress", "text": "助理在判断这条消息属于哪个项目…"})
		var r route
		s := spec(a, scratch(), prompt, routeSchema)
		s.Access, s.Ephemeral, s.Images, s.Tools = harness.ReadOnly, true, imagesOf(files), tools
		more, err := interruptible(ctx, func(ctx context.Context) error {
			_, err := harness.Ask(ctx, a.Runtime, s, &r, nil)
			return err
		})
		if more != "" {
			e.setStep(f, routed, "done")
			text += "\n\n用户补充：" + more
			routed = e.addStep(f, a.Name, "重新判断", routed)
			posOf(ctx).tip = routed
			continue
		}
		if err != nil {
			e.setStep(f, routed, "failed")
			e.fail(ctx, "路由", err)
			return
		}
		e.setStep(f, routed, "done")

		extra := ""
		switch r.Decision {
		case "chat":
			var extra chat.Event
			if d := e.DefaultProject(); d != nil { // 闲聊算默认项目
				e.setFlowProject(f, d)
				extra = chat.Event{"project": d.ID}
			}
			e.say(ctx, "收到回复", r.Reply, extra)
			ok = true
			return
		case "manage":
			extra, ok = e.manage(ctx, a, r)
			if extra == "" {
				return
			}
		default:
			wait := e.addStep(f, "", "放进项目", posOf(ctx).tip)
			p = e.place(ctx, r, projects)
			e.setStep(f, wait, map[bool]string{true: "done", false: "failed"}[p != nil])
			posOf(ctx).tip = wait
			if p == nil {
				return
			}
		}
		if extra == "" {
			break
		}
		if round++; round == 3 {
			e.Chat.EmitIn(ctx, chat.Event{"type": "note", "text": "补充了 3 次还没定下来，这条消息先不处理。可以把要求写全后重新发一条"})
			ok = true
			return
		}
		text += "\n\n用户补充：" + extra
		routed = e.addStep(f, a.Name, "重新判断", posOf(ctx).tip)
		posOf(ctx).tip = routed
	}
	e.setFlowProject(f, p)
	if strings.TrimSpace(text) == "" {
		text = title
	}
	ok = e.plan(ctx, a, p, text, files)
}

// place 按助理的判断把消息放进项目，不问用户：拿不准时用第一个候选项目，没有候选时用默认项目；
// 新的事直接建项目。放进了哪个项目在对话里留一条记录，用户觉得不对再说。返回 nil 表示出错，已报给用户。
func (e *Engine) place(ctx context.Context, r route, projects []store.Project) *store.Project {
	byID := map[int64]*store.Project{}
	for i := range projects {
		byID[projects[i].ID] = &projects[i]
	}
	emit := func(p *store.Project, created bool, reason, quote string) *store.Project {
		e.Chat.EmitIn(ctx, chat.Event{"type": "route", "project": p.ID, "name": p.Name, "created": created, "reason": reason, "quote": quote})
		return p
	}
	if r.Decision == "project" {
		if p := byID[r.ProjectID]; p != nil {
			return emit(p, false, r.Reason, r.Quote)
		}
		r.Reason = "助理给出的项目编号不存在"
	}
	if name := strings.TrimSpace(r.Name); r.Decision == "new" && name != "" {
		for _, p := range projects { // 同名的项目已经有了时用它
			if p.Name == name {
				return emit(&p, false, r.Reason, "")
			}
		}
		p, err := e.CreateProject(name, r.Summary)
		if err == nil {
			return emit(p, true, r.Summary, "")
		}
		e.Chat.EmitIn(ctx, chat.Event{"type": "error", "text": "新建项目「" + name + "」失败：" + err.Error() + "。先放进默认项目"})
	}
	for _, id := range r.Candidates {
		if p := byID[id]; p != nil {
			return emit(p, false, orDefault(r.Reason, "拿不准，先放这里"), "")
		}
	}
	if d := e.DefaultProject(); d != nil {
		return emit(d, false, orDefault(r.Reason, "不像哪个已有项目，先放默认项目"), "")
	}
	e.Chat.EmitIn(ctx, chat.Event{"type": "error", "text": "没有默认项目，这条消息没地方放"})
	return nil
}

// CreateProject 建项目，并在项目根目录下建它的工作目录。
func (e *Engine) CreateProject(name, summary string) (*store.Project, error) {
	name = strings.TrimSpace(name)
	dirName := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r < ' ' {
			return '-'
		}
		return r
	}, name)
	dirName = strings.TrimLeft(dirName, ".")
	if dirName == "" {
		return nil, errors.New("项目名不能为空")
	}
	dir := filepath.Join(e.Limits().Root, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return e.DB.AddProject(name, summary, dir)
}

type taskSpec struct {
	Title  string `json:"title"`
	Brief  string `json:"brief"`
	Agent  string `json:"agent"`
	Check  string `json:"check"`
	Effort string `json:"effort,omitempty"` // 只有 agent 方式的 dispatch 填；拆出的子任务沿用父任务的
}

// roster 列出能派活的执行者（关着的不列），写明绑定的 skill 和常用于哪些项目；常用于 project 的排在前面。
func (e *Engine) roster(project int64) string {
	list, _ := e.DB.Agents()
	names := map[int64]string{}
	if ps, err := e.DB.Projects(); err == nil {
		for _, p := range ps {
			names[p.ID] = p.Name
		}
	}
	var mine, rest strings.Builder
	for _, a := range list {
		if fixed(&a) || a.Off {
			continue
		}
		b := &rest
		if slices.Contains(a.Projects, project) {
			b = &mine
		}
		fmt.Fprintf(b, "- %s（%s）：%s · %s · 思考强度 %s", a.Name, a.Role, a.Runtime, a.Model, orDefault(a.Effort, "默认"))
		if a.Instructions != "" {
			b.WriteString("；" + oneLine(a.Instructions))
		}
		if len(a.Skills) > 0 {
			b.WriteString("；skill：" + strings.Join(a.Skills, "、"))
		}
		var ps []string
		for _, id := range a.Projects {
			if n := names[id]; n != "" {
				ps = append(ps, "「"+n+"」")
			}
		}
		if len(ps) > 0 {
			b.WriteString("；常用于 " + strings.Join(ps, ""))
		}
		if _, err := harness.Get(a.Runtime); err != nil {
			b.WriteString("（harness 未接入，不能派活）")
		}
		b.WriteString("\n")
	}
	if mine.Len()+rest.Len() == 0 {
		return "（还没有执行者，agent 字段填空串即可）\n"
	}
	out := ""
	if mine.Len() > 0 {
		out += "常用于这个项目的：\n" + mine.String()
	}
	if rest.Len() > 0 {
		if mine.Len() > 0 {
			out += "其他：\n"
		}
		out += rest.String()
	}
	return out
}

func projectBlock(p *store.Project, withMemory bool) string {
	s := fmt.Sprintf("# 项目「%s」（编号 %d）\n工作目录：%s\n", p.Name, p.ID, p.Dir)
	if p.Owner != "" {
		s += "项目负责人：" + p.Owner + "（派到这个项目的活整件交给它，由它拆、检查和汇报）\n"
	}
	s += "\n## 项目摘要\n" + orEmpty(p.Summary) + "\n"
	if withMemory {
		s += "\n## 项目记忆\n" + orEmpty(p.Memory) + "\n"
	}
	return s
}

// plan 选定项目后加载项目记忆，拆成顶层任务并开始执行。
// 返回 false 表示安排失败。
func (e *Engine) plan(ctx context.Context, a *store.Agent, p *store.Project, text string, files []chat.File) bool {
	e.Chat.EmitIn(ctx, chat.Event{"type": "progress", "text": "助理读了「" + p.Name + "」的项目记忆，正在安排…", "project": p.ID})
	prompt := e.role(a) + "\n\n" + projectBlock(p, true) +
		"\n# 花名册（可以派活的执行者）\n" + e.roster(p.ID) +
		"\n# 今天最近的对话\n" + recent() +
		"\n\n# 用户的新消息\n" + text + attachNote(files) + "\n\n" + planRules
	var out struct {
		Reply string     `json:"reply"`
		Tasks []taskSpec `json:"tasks"`
	}
	s := spec(a, scratch(), prompt, planSchema)
	s.Access, s.Ephemeral, s.Images = harness.ReadOnly, true, imagesOf(files)
	fp := posOf(ctx)
	step := e.addStep(fp.f, a.Name, "安排", fp.tip)
	fp.tip = step
	more, err := interruptible(ctx, func(ctx context.Context) error {
		_, err := harness.Ask(ctx, a.Runtime, s, &out, nil)
		return err
	})
	if more != "" { // 用户补充了要求：带着补充重新安排
		e.setStep(fp.f, step, "done")
		return e.plan(ctx, a, p, text+"\n\n用户补充："+more, files)
	}
	if err != nil {
		e.setStep(fp.f, step, "failed")
		e.fail(ctx, "安排", err)
		return false
	}
	e.setStep(fp.f, step, "done")
	if len(out.Tasks) == 0 {
		e.say(ctx, "收到回复", out.Reply, chat.Event{"project": p.ID})
		return true
	}
	runID, err := e.startRun(fp, step, p, text, out.Reply, out.Tasks, nil)
	if err != nil {
		e.Chat.EmitIn(ctx, chat.Event{"type": "error", "text": err.Error()})
		return false
	}
	e.say(ctx, "收到安排", out.Reply, chat.Event{"project": p.ID, "run": runID})
	return true
}

// startRun 记下一次安排和它的顶层任务，在后台开始执行，不占着对话：用户可以接着说别的事。
// 流程还没接任务树时，任务树接在 step 这个节点后面。
// rw 非 nil 表示这次安排是验收打回后重新安排的。
func (e *Engine) startRun(fp *flowPos, step string, p *store.Project, request, plan string, tasks []taskSpec, rw *rework) (int64, error) {
	runID, err := e.DB.AddRun(p.ID, request)
	if err != nil {
		return 0, err
	}
	e.DB.SetRun(runID, "plan", plan)
	if rw != nil {
		e.DB.SetRework(runID, rw.root, rw.rejects)
	}
	if o := e.owner(p); o != nil { // 有项目负责人时整件交给它
		tasks = ownerTask(o, p, request, plan, tasks)
	}
	for _, t := range tasks {
		e.DB.AddTask(store.Task{RunID: runID, Depth: 1, Title: t.Title, Brief: t.Brief, Agent: t.Agent, Check: t.Check,
			Effort: strings.TrimSpace(t.Effort)})
	}
	rctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.runs[runID] = cancel
	if fp != nil && fp.f.Run == 0 {
		fp.f.Run, fp.f.plan = runID, step
	}
	e.mu.Unlock()
	go e.execRun(rctx, runID, p.ID)
	return runID, nil
}

// interruptible 跑 fn；用户把排队的消息补充进这条时停掉 fn，返回补充的文字，调用方带着它重来。
// 补充来得太晚、fn 已经跑完时，补充留在通道里，由下一个能读它的地方处理。
func interruptible(ctx context.Context, fn func(context.Context) error) (string, error) {
	sup := chat.Supplements(ctx)
	if sup == nil {
		return "", fn(ctx)
	}
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn(c) }()
	select {
	case err := <-done:
		return "", err
	case t := <-sup:
		cancel()
		<-done
		return t, nil
	}
}

func (e *Engine) fail(ctx context.Context, step string, err error) {
	if ctx.Err() != nil { // 用户点了停止，chat.Session.Stop 已经告诉用户
		return
	}
	e.Chat.EmitIn(ctx, chat.Event{"type": "error", "text": "助理" + step + "失败：" + err.Error()})
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return clip(s, 200)
}

func orEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "（空）"
	}
	return strings.TrimSpace(s)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// dayCheckPrompt 是换日后让助理在当天的会话里盘点的提示词。
const dayCheckPrompt = `现在换日了，今天的对话到此结束，明天 morula 会开一个新会话，新会话看不到这个会话。
盘点今天，明天早上发给用户。
summary：今天的盘点（Markdown，第一行一句话概括）。分这几部分，没有的部分不写：
**做完了**、**没做完**（做到哪一步）、**答应了还没做**、**等用户拍板**、**明天接着做**。
名字、路径、数字、安排编号原样保留；没验证过的写明未验证。只写这个会话里实际发生的，不编。
memory：全局记忆的完整新内容，只放以后还用得上的用户偏好和约定；项目里的事不放这里，已有项目记忆。没有变化就原样返回。
不要调用工具。`

// Summarize 给某天写总结（盘点），同时更新全局记忆（用户偏好）。
// 助理这天用 agent 方式开过会话时，续跑这天最后一个会话来盘点；会话续不上时按当天的记录写。
func (e *Engine) Summarize(ctx context.Context, day, transcript string) (string, error) {
	a, err := e.assistant()
	if err != nil {
		return "", err
	}
	if ids := dayThreads(e.DB, day); len(ids) > 0 && harness.Agentic(a.Runtime) {
		prompt := dayCheckPrompt + "\n\n# 当前全局记忆\n" + orEmpty(journal.Memory())
		if len(ids) > 1 { // 清空过对话页，前面的会话里还有这天早些时候的事
			var b strings.Builder
			for _, id := range ids[:len(ids)-1] {
				if p := harness.SessionFile(a.Runtime, id); p != "" {
					b.WriteString("- " + p + "\n")
				}
			}
			if b.Len() > 0 {
				prompt += "\n\n# 这天早些时候的会话文件（清空对话页前的，盘点时一起算进去，可以直接读）\n" + b.String()
			}
		}
		if text, err := e.summarize(ctx, a, prompt, ids[len(ids)-1]); err == nil && text != "" {
			return text, nil
		}
	}
	prompt := e.role(a) + "\n\n现在是换日时间。下面是你和用户在 " + day + " 的完整记录。\n" +
		"summary：这一天的总结（Markdown，第一行一句话概括；写做了什么、做了哪些决定、还没做完和等用户拍板的事；名字、路径、数字原样保留）。\n" +
		"memory：全局记忆的完整新内容，只放以后还用得上的用户偏好和约定；项目里的事不放这里，已有项目记忆。没有变化就原样返回。\n\n" +
		"# 当前全局记忆\n" + orEmpty(journal.Memory()) + "\n\n# " + day + " 的记录\n" + transcript
	return e.summarize(ctx, a, prompt, "")
}

// summarize 跑一次写总结。resume 非空时续跑那个会话，盘点留在那天的会话里；空时开一次性的会话。
func (e *Engine) summarize(ctx context.Context, a *store.Agent, prompt, resume string) (string, error) {
	var out struct {
		Summary string `json:"summary"`
		Memory  string `json:"memory"`
	}
	s := spec(a, scratch(), prompt, summarySchema)
	s.Access, s.Resume, s.Ephemeral = harness.ReadOnly, resume, resume == ""
	if resume != "" {
		s.CompactAt = e.compactLimit()
	}
	if _, err := harness.Ask(ctx, a.Runtime, s, &out, nil); err != nil {
		return "", err
	}
	if m := strings.TrimSpace(out.Memory); m != "" && m != "（空）" {
		journal.SaveMemory(m + "\n")
	}
	return strings.TrimSpace(out.Summary), nil
}

// Trajectory 读一个任务在 harness 里的轨迹。
func (e *Engine) Trajectory(taskID int64) ([]harness.Step, error) {
	t, err := e.DB.Task(taskID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, errors.New("没有这个任务")
	}
	if t.Session == "" {
		return nil, errors.New("这个任务还没开始，没有记录")
	}
	a, err := harness.Get(t.Runtime)
	if err != nil {
		return nil, err
	}
	return a.Trajectory(t.Session)
}
