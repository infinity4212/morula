package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"morula/internal/chat"
	"morula/internal/harness"
	"morula/internal/store"
)

// 首次整理：读本机 codex、opencode 留下的会话，让助理按内容归成项目，用户勾选后建项目。
// 只把会话的标题、第一条消息的开头和目录交给助理的模型，不发轨迹。

// organizeBatch 是一次交给助理的会话数，从最近的往前取；剩下的下次再整理。
const organizeBatch = 200

// Proposal 是助理提议的一个项目。
type Proposal struct {
	Name     string   `json:"name"`
	Summary  string   `json:"summary"`
	Memory   string   `json:"memory"`
	Dir      string   `json:"dir"`
	Existing int64    `json:"existing"` // 并入已有项目时是它的编号
	Sessions []string `json:"sessions"` // 「harness/会话 id」
}

// Organize 是整理的进度，整理页轮询它。
type Organize struct {
	State     string          `json:"state"` // idle、working、ready、failed
	Error     string          `json:"error,omitempty"`
	Model     string          `json:"model,omitempty"`
	Started   string          `json:"started,omitempty"`
	Proposals []Proposal      `json:"proposals"`
	Chats     []string        `json:"chats"`    // 判为闲聊的会话，不建项目
	Sessions  []harness.Local `json:"sessions"` // 这一批交给助理的会话
	Left      int             `json:"left"`     // 还没整理、这一批没带上的会话数
}

// scanCache 存最近一次扫描。启动时就在后台扫，助理上线后问用户时直接用。
type scanCache struct {
	mu   sync.Mutex
	at   time.Time
	list []harness.Local
}

// scanTTL 内不重扫。
const scanTTL = 2 * time.Minute

// Scan 重新扫描本机会话。只读本机文件，不调模型。
func (e *Engine) Scan() []harness.Local {
	e.scan.mu.Lock()
	defer e.scan.mu.Unlock()
	e.scan.list, e.scan.at = harness.LocalSessions(), time.Now()
	return e.scan.list
}

// Scanned 返回最近一次扫描的结果，过期时重扫。
func (e *Engine) Scanned() []harness.Local { return e.scanned() }

func (e *Engine) scanned() []harness.Local {
	e.scan.mu.Lock()
	list, at := e.scan.list, e.scan.at
	e.scan.mu.Unlock()
	if list == nil || time.Since(at) > scanTTL {
		return e.Scan()
	}
	return list
}

// Pending 返回还没整理过的本机会话（已整理的、morula 自己任务用过的都不算），以及跳过了几个。
func (e *Engine) Pending() ([]harness.Local, int, error) {
	seen, err := e.DB.Seen()
	if err != nil {
		return nil, 0, err
	}
	all := e.scanned()
	out := []harness.Local{}
	for _, s := range all {
		if !seen[s.Harness+"/"+s.ID] {
			out = append(out, s)
		}
	}
	return out, len(all) - len(out), nil
}

// orgKey 存整理结果，app 重启后还能接着勾选。
const orgKey = "onboard.result"

func (e *Engine) Organizing() Organize {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.org.State == "" {
		json.Unmarshal([]byte(e.DB.Setting(orgKey, "{}")), &e.org)
	}
	o := e.org
	if o.State == "" {
		o.State = "idle"
	}
	if o.Proposals == nil {
		o.Proposals = []Proposal{}
	}
	if o.Chats == nil {
		o.Chats = []string{}
	}
	if o.Sessions == nil {
		o.Sessions = []harness.Local{}
	}
	return o
}

// StartOrganize 在后台让助理整理最近的一批会话。only 不空时只整理这几个 harness 的会话；
// ids 不空时只整理这几个会话，每个写成「harness/会话 id」。
func (e *Engine) StartOrganize(only, ids []string) error {
	a, err := e.assistant()
	if err != nil {
		return err
	}
	pending, _, err := e.Pending()
	if err != nil {
		return err
	}
	if len(only) > 0 {
		pending = slices.DeleteFunc(pending, func(s harness.Local) bool { return !slices.Contains(only, s.Harness) })
	}
	if len(ids) > 0 {
		pending = slices.DeleteFunc(pending, func(s harness.Local) bool { return !slices.Contains(ids, s.Harness+"/"+s.ID) })
	}
	if len(pending) == 0 {
		return errors.New("没有要整理的对话")
	}
	e.mu.Lock()
	if e.org.State == "working" {
		e.mu.Unlock()
		return errors.New("正在整理")
	}
	batch := pending[:min(len(pending), organizeBatch)]
	e.org = Organize{State: "working", Model: a.Runtime + " · " + a.Model, Started: time.Now().Format(time.RFC3339),
		Sessions: batch, Left: len(pending) - len(batch)}
	e.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		ps, chats, err := e.organize(ctx, a, batch)
		e.mu.Lock()
		defer e.mu.Unlock()
		if err != nil {
			e.org.State, e.org.Error = "failed", err.Error()
			return
		}
		e.org.State, e.org.Proposals, e.org.Chats = "ready", ps, chats
		b, _ := json.Marshal(e.org)
		e.DB.SetSetting(orgKey, string(b))
	}()
	return nil
}

const organizeSchema = `{"type":"object","additionalProperties":false,"required":["projects","chats"],"properties":{
"projects":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["name","summary","memory","dir","existing","sessions"],
"properties":{"name":{"type":"string"},"summary":{"type":"string"},"memory":{"type":"string"},"dir":{"type":"string"},"existing":{"type":"integer"},
"sessions":{"type":"array","items":{"type":"string"}}}}},
"chats":{"type":"array","items":{"type":"string"}}}}`

const organizeRules = `用户第一次用 morula。下面是用户在本机其他 harness 里留下的会话，每行一个：编号、harness、最后更新日期、工作目录、标题、用户第一条消息的开头。
把它们整理成项目：
- 做同一件事的会话归成一个项目，目录不同也归在一起；同一个目录里明显不相干的事分开。
- 和某个已有项目是同一件事：existing 填已有项目编号，name 填该项目的名字。其余 existing 填 0。
- name 简短，不和已有项目重名（并入的除外）。summary 一句话写这个项目在做什么，以后你只靠它判断用户的消息属不属于这个项目。
- memory 写从标题和第一条消息能看出来的事：做过什么、提到的目录和文件。看不出来的不写，不编造。
- dir 原样抄这些会话里最能代表项目的工作目录；看不出来填空串。
- sessions 填会话编号（s 开头）。
- 一次性的提问、闲聊、测试，和只凭一句话看不出要做什么的会话，编号放进 chats，不建项目。
- 每个会话编号只出现一次。`

func (e *Engine) organize(ctx context.Context, a *store.Agent, batch []harness.Local) ([]Proposal, []string, error) {
	defer e.hold(a.Name, "整理以前的对话")()
	prompt, keys, err := e.organizePrompt(a, batch)
	if err != nil {
		return nil, nil, err
	}
	var out struct {
		Projects []Proposal `json:"projects"`
		Chats    []string   `json:"chats"`
	}
	s := spec(a, scratch(), prompt, organizeSchema)
	s.Access, s.Ephemeral = harness.ReadOnly, true
	if _, err := harness.Ask(ctx, a.Runtime, s, &out, nil); err != nil {
		return nil, nil, err
	}
	return e.organized(out.Projects, out.Chats, keys)
}

// organizePrompt 是整理一批会话的提示词，keys 把提示词里的会话编号映射回「harness/会话 id」。
func (e *Engine) organizePrompt(a *store.Agent, batch []harness.Local) (string, map[string]string, error) {
	projects, err := e.DB.Projects()
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	b.WriteString(e.role(a) + "\n\n" + organizeRules + "\n\n# 已有项目\n")
	for _, p := range projects {
		fmt.Fprintf(&b, "- 编号 %d「%s」：%s\n", p.ID, p.Name, oneLine(p.Summary))
	}
	if len(projects) == 0 {
		b.WriteString("（还没有项目）\n")
	}
	b.WriteString("\n# 会话\n")
	keys := map[string]string{}
	for i, s := range batch {
		k := fmt.Sprintf("s%d", i+1)
		keys[k] = s.Harness + "/" + s.ID
		fmt.Fprintf(&b, "%s | %s | %s | %s | %s | %s\n", k, s.Harness, s.Updated.Format("2006-01-02"), s.Dir, oneLine(s.Title), clip(oneLine(s.First), 160))
	}
	return b.String(), keys, nil
}

// organized 把模型的回答换回会话，丢掉对不上的。
func (e *Engine) organized(projects []Proposal, chats []string, keys map[string]string) ([]Proposal, []string, error) {
	// 编号换回会话；模型编出来的编号和重复出现的编号丢掉。
	used := map[string]bool{}
	pick := func(ks []string) []string {
		r := []string{}
		for _, k := range ks {
			if v, ok := keys[strings.TrimSpace(k)]; ok && !used[v] {
				used[v] = true
				r = append(r, v)
			}
		}
		return r
	}
	ps := []Proposal{}
	for _, p := range projects {
		p.Sessions = pick(p.Sessions)
		p.Name = strings.TrimSpace(p.Name)
		if len(p.Sessions) == 0 || p.Name == "" {
			continue
		}
		if p.Dir = strings.TrimSpace(p.Dir); p.Dir != "" && !isDir(p.Dir) {
			p.Dir = "" // 目录已经不在了，建项目时在项目根目录下新建
		}
		if p.Existing != 0 {
			if ex, _ := e.DB.Project(p.Existing); ex == nil {
				p.Existing = 0
			}
		}
		ps = append(ps, p)
	}
	return ps, pick(chats), nil
}

// ApplyOrganize 按用户勾选的提议建项目或并入已有项目，记下这一批里归了项目和判为闲聊的会话。
// 没勾选的提议里的会话不记，下次整理还会出现。返回每个提议的错误，没有错误时对应位置是空串。
func (e *Engine) ApplyOrganize(items []Proposal) ([]string, error) {
	o := e.Organizing()
	if o.State != "ready" {
		return nil, errors.New("还没有整理结果")
	}
	titles := map[string]string{}
	for _, s := range o.Sessions {
		titles[s.Harness+"/"+s.ID] = oneLine(s.Title)
	}
	day := time.Now().Format("2006-01-02")
	errs := make([]string, len(items))
	created := 0
	for i, it := range items {
		var sess []string
		for _, k := range it.Sessions {
			if _, ok := titles[k]; ok {
				sess = append(sess, k)
			}
		}
		if len(sess) == 0 {
			errs[i] = "这一批里没有这个对话"
			continue
		}
		var src strings.Builder
		fmt.Fprintf(&src, "\n\n来源会话（%s 从本机整理）：\n", day)
		for j, k := range sess {
			if j == 20 {
				fmt.Fprintf(&src, "- 另有 %d 个\n", len(sess)-20)
				break
			}
			fmt.Fprintf(&src, "- %s：%s\n", k, titles[k])
		}
		mem := strings.TrimSpace(it.Memory) + src.String()
		var p *store.Project
		var err error
		if it.Existing != 0 {
			if p, err = e.DB.Project(it.Existing); err == nil && p == nil {
				err = errors.New("已有项目不存在")
			}
			if err == nil {
				full := strings.TrimSpace(p.Memory) + "\n\n## 从本机会话整理\n" + mem
				err = e.DB.UpdateProject(p.ID, nil, &full, nil)
			}
		} else {
			if dir := strings.TrimSpace(it.Dir); dir != "" && isDir(dir) {
				p, err = e.DB.AddProject(it.Name, strings.TrimSpace(it.Summary), dir)
			} else {
				p, err = e.CreateProject(it.Name, strings.TrimSpace(it.Summary))
			}
			if err == nil {
				err = e.DB.UpdateProject(p.ID, nil, &mem, nil)
			}
		}
		if err != nil {
			errs[i] = err.Error()
			continue
		}
		for _, k := range sess {
			h, id, _ := strings.Cut(k, "/")
			e.DB.MarkImported(h, id, p.ID)
		}
		created++
	}
	for _, k := range o.Chats {
		h, id, _ := strings.Cut(k, "/")
		e.DB.MarkImported(h, id, 0)
	}
	e.mu.Lock()
	e.org = Organize{State: "idle"}
	e.DB.SetSetting(orgKey, "{}")
	e.mu.Unlock()
	e.offerStaffAfterOrganize(created)
	e.StartBrief()
	return errs, nil
}

// DiscardOrganize 丢掉整理结果，什么都不记。
func (e *Engine) DiscardOrganize() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.org.State != "working" {
		e.org = Organize{State: "idle"}
		e.DB.SetSetting(orgKey, "{}")
	}
}

// askedKey 记用户回答过要不要整理；回答过之后只能从设置进整理页。首次引导的第 3 步问一次。
const askedKey = "onboard.asked"

// introKey 记助理做过自我介绍：首次引导的第 2 步用它验证模型通了，只做一次。
const introKey = "onboard.intro"

const introRules = `用户刚装好 morula，你第一次上线。用三到五句话向用户介绍你自己：你是 morula 的助理，用户把要做的事告诉你，
你判断归到哪个项目、拆成任务派给子 agent，需要用户拍板时在对话里问。最后一句告诉用户可以直接说第一件要做的事。
不用标题和列表，不提模型名。`

const introSchema = `{"type":"object","additionalProperties":false,"required":["message"],"properties":{"message":{"type":"string"}}}`

// Introduce 让助理真实调用一次模型做自我介绍，介绍发到对话里。调用失败时返回错误，什么都不发。
func (e *Engine) Introduce(ctx context.Context) (string, error) {
	a, err := e.assistant()
	if err != nil {
		return "", err
	}
	defer e.hold(a.Name, "自我介绍")()
	s := spec(a, scratch(), e.role(a)+"\n\n"+introRules, introSchema)
	s.Access, s.Ephemeral = harness.ReadOnly, true
	var out struct {
		Message string `json:"message"`
	}
	if _, err := harness.Ask(ctx, a.Runtime, s, &out, nil); err != nil {
		return "", err
	}
	text := strings.TrimSpace(out.Message)
	if text == "" {
		return "", fmt.Errorf("%s · %s 没有回复内容", a.Runtime, a.Model)
	}
	e.DB.SetSetting(introKey, "1")
	e.Chat.EmitIn(ctx, chat.Event{"type": "reply", "text": text, "verdict": "fixed"})
	return text, nil
}

// Cost 是整理一批会话要交给助理模型的量，首次引导用它提醒用户会花 token。
type Cost struct {
	Sessions int    `json:"sessions"` // 这一批的会话数
	Left     int    `json:"left"`     // 这一批带不上的会话数
	Chars    int    `json:"chars"`    // 提示词的字符数，不含 harness 自己加的系统提示
	Model    string `json:"model"`
}

// OrganizeCost 按 StartOrganize 会发的提示词算这一批的量。只读本机文件，不调模型。
func (e *Engine) OrganizeCost() (Cost, error) {
	a, err := e.assistant()
	if err != nil {
		return Cost{}, err
	}
	pending, _, err := e.Pending()
	if err != nil {
		return Cost{}, err
	}
	batch := pending[:min(len(pending), organizeBatch)]
	prompt, _, err := e.organizePrompt(a, batch)
	if err != nil {
		return Cost{}, err
	}
	return Cost{Sessions: len(batch), Left: len(pending) - len(batch), Chars: len([]rune(prompt)), Model: a.Runtime + " · " + a.Model}, nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
