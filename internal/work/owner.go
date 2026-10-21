package work

import (
	"fmt"
	"strings"

	"morula/internal/harness"
	"morula/internal/store"
)

// owner 返回项目负责人；没指定、agent 不在了、是固定成员或 harness 接不上时返回 nil，由助理兼任。
func (e *Engine) owner(p *store.Project) *store.Agent {
	if p == nil || p.Owner == "" {
		return nil
	}
	a, _ := e.DB.Agent(p.Owner)
	if a == nil || fixed(a) {
		return nil
	}
	if _, err := harness.Get(a.Runtime); err != nil {
		return nil
	}
	return a
}

// ownerTask 把助理拆好的任务合成一个顶层任务交给项目负责人：助理的拆法只作建议，由负责人决定自己做还是往下拆。
// 只有一个任务、已经派给负责人时照原样交给它。
func ownerTask(o *store.Agent, p *store.Project, request, plan string, tasks []taskSpec) []taskSpec {
	if len(tasks) == 1 && tasks[0].Agent == o.Name {
		return tasks
	}
	var b strings.Builder
	fmt.Fprintf(&b, "你是项目「%s」的项目负责人。这件活整件交给你：你可以自己做，也可以按下面花名册拆给子 agent（汇报里写 split），子任务做完由你检查。\n", p.Name)
	b.WriteString("\n# 用户的要求\n" + request + "\n")
	if strings.TrimSpace(plan) != "" {
		b.WriteString("\n# 助理的安排\n" + plan + "\n")
	}
	b.WriteString("\n# 助理建议的拆法（可以改）\n")
	for i, t := range tasks {
		fmt.Fprintf(&b, "%d. %s", i+1, t.Title)
		if t.Agent != "" {
			b.WriteString("（建议派给「" + t.Agent + "」）")
		}
		b.WriteString("\n   " + strings.ReplaceAll(strings.TrimSpace(t.Brief), "\n", "\n   ") + "\n")
		if t.Check != "" {
			b.WriteString("   机器检查：`" + t.Check + "`\n")
		}
	}
	return []taskSpec{{Title: oneLine(request), Brief: b.String(), Agent: o.Name}}
}

// ownerRole 是项目负责人写汇报、项目记忆和项目摘要时的身份。
func ownerRole(o *store.Agent, p *store.Project) string {
	s := fmt.Sprintf("你是 morula 团队里的子 agent「%s」，项目「%s」的项目负责人。派到这个项目的活由你负责：做完后由你向用户汇报、维护项目记忆和项目摘要。助理只转交用户的要求、收你的汇报。回答用中文。", o.Name, p.Name)
	if strings.TrimSpace(o.Instructions) != "" {
		s += "\n\n你的职责说明：" + o.Instructions
	}
	return s
}

// SetOwner 指定项目负责人；name 为空表示改回由助理兼任。固定成员（助理、验收）不能当。
func (e *Engine) SetOwner(project int64, name string) error {
	name = strings.TrimSpace(name)
	if name != "" {
		a, _ := e.DB.Agent(name)
		switch {
		case a == nil:
			return fmt.Errorf("花名册里没有「%s」", name)
		case fixed(a):
			return fmt.Errorf("「%s」是固定成员，不能当项目负责人", name)
		}
	}
	return e.DB.SetOwner(project, name)
}
