package work

import (
	"context"
	"fmt"
	"strings"
	"time"

	"morula/internal/harness"
	"morula/internal/skills"
	"morula/internal/store"
)

const describeRules = `给上面这个 agent 写一段介绍，放在 morula 的 Agent 页卡片上给用户看。用户不懂 skill，只想知道这个 agent 能替他做什么事。
- 一到两句，不超过 60 个字。
- 写它能帮用户做的具体事情，按用户能理解的说法写。
- 不写 skill 名、文件名、路径、模型名、harness 名，不写「我」。
- 不用标题、列表和引号。`

const describeSchema = `{"type":"object","additionalProperties":false,"required":["profile"],"properties":{"profile":{"type":"string"}}}`

// describePrompt 把 agent 的角色、职责说明、常用项目和绑定 skill 的说明交给助理模型。
func (e *Engine) describePrompt(a *store.Agent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Agent「%s」\n角色：%s\n", a.Name, a.Role)
	if s := strings.TrimSpace(a.Instructions); s != "" {
		fmt.Fprintf(&b, "职责说明：%s\n", s)
	}
	if len(a.Projects) > 0 {
		names := []string{}
		for _, id := range a.Projects {
			if p, _ := e.DB.Project(id); p != nil {
				names = append(names, p.Name)
			}
		}
		if len(names) > 0 {
			fmt.Fprintf(&b, "常用于项目：%s\n", strings.Join(names, "、"))
		}
	}
	all := skills.List()
	b.WriteString("\n# 它加载的 skill\n")
	n := 0
	for _, name := range a.Skills {
		if s, ok := skills.Find(all, name); ok {
			fmt.Fprintf(&b, "- %s：%s\n", s.Name, oneLine(s.Description))
			n++
		}
	}
	if n == 0 {
		b.WriteString("（没有）\n")
	}
	return b.String() + "\n" + describeRules
}

// Describe 让助理按 agent 加载的 skill 写一段给用户看的介绍，写回 agent 的 profile。
// 同一个 agent 正在写时直接返回。失败时 profile 不动。
func (e *Engine) Describe(ctx context.Context, name string) error {
	e.mu.Lock()
	if e.describing[name] {
		e.mu.Unlock()
		return nil
	}
	e.describing[name] = true
	delete(e.describeErr, name)
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.describing, name)
		e.mu.Unlock()
	}()
	a, err := e.DB.Agent(name)
	if err != nil || a == nil {
		return fmt.Errorf("没有这个 Agent：%s", name)
	}
	as, err := e.assistant()
	if err != nil {
		return err
	}
	defer e.hold(as.Name, "给「"+name+"」写介绍")()
	s := spec(as, scratch(), e.describePrompt(a), describeSchema)
	s.Access, s.Ephemeral = harness.ReadOnly, true
	var out struct {
		Profile string `json:"profile"`
	}
	if _, err := harness.Ask(ctx, as.Runtime, s, &out, nil); err != nil {
		return err
	}
	text := strings.TrimSpace(out.Profile)
	if text == "" {
		return fmt.Errorf("%s · %s 没有写出介绍", as.Runtime, as.Model)
	}
	return e.DB.UpdateAgent(name, store.Patch{Profile: &text})
}

// DescribeLater 在后台写介绍，最多等 3 分钟。界面每 4 秒刷新 Agent 页，写好后自然显示。
func (e *Engine) DescribeLater(name string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := e.Describe(ctx, name); err != nil {
			e.mu.Lock()
			e.describeErr[name] = err.Error()
			e.mu.Unlock()
		}
	}()
}

// Describing 返回 agent 的介绍是否正在写，以及上一次没写成的原因（写成了是空）。
func (e *Engine) Describing(name string) (bool, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.describing[name], e.describeErr[name]
}
