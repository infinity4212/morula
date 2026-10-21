package work

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"morula/internal/chat"
	"morula/internal/harness"
	"morula/internal/skills"
	"morula/internal/store"
)

// 管理 morula 本身：用户要增删改 agent 时，
// 助理只提出改动，morula 发确认卡片，用户确认后由 morula 自己改花名册。助理和子 agent 都不直接改。

// change 是助理提出的一项改动。不用的字段是空串。
type change struct {
	Op           string   `json:"op"` // agent_add / agent_update / agent_remove
	Target       string   `json:"target"`
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	Instructions string   `json:"instructions"`
	Profile      string   `json:"profile"`
	Skills       []string `json:"skills,omitempty"` // 要绑定的 skill，整体替换；空表示不改
	// 用哪个 harness、模型和思考强度。新建时空的按助理的补，修改时空的表示不改（见 setup）。
	Runtime string `json:"runtime,omitempty"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
}

// setup 定 agent 用的 harness、模型和思考强度，base 是新建时的助理、修改时的原 agent。
// harness 和模型都没填时沿用 base 的，不核对列表：模型下架时开工会换（见 liveModel）。
// 填了任一个时，harness 要本机能用、模型要在它的列表里；只填 harness 时，和 base 不同就用列表里的第一个模型。
// 思考强度填了要模型支持（按 pickModel，和开工时一样）；没填时，修改且没换模型就不变，否则用 workerDefaultEffort（模型支持时）或模型默认。
func setup(c change, base *store.Agent, add bool) (rt, model, effort string, err error) {
	rt, model, effort = strings.TrimSpace(c.Runtime), strings.TrimSpace(c.Model), strings.TrimSpace(c.Effort)
	if rt == "" && model == "" {
		rt, model = base.Runtime, base.Model
	} else {
		if rt == "" {
			rt = base.Runtime
		}
		ad, err := harness.Get(rt)
		if err != nil {
			return "", "", "", errors.New("morula 还不支持 AI 工具「" + rt + "」，支持的是：" + strings.Join(harness.Names(), "、"))
		}
		if err := ad.Detect(); err != nil {
			return "", "", "", errors.New("这台 Mac 上的「" + rt + "」不能用：" + err.Error())
		}
		ms := ad.Models()
		if model == "" {
			if rt == base.Runtime {
				model = base.Model
			} else if len(ms) > 0 {
				model = ms[0].ID
			}
		}
		if !slices.ContainsFunc(ms, func(m harness.Model) bool { return m.ID == model }) {
			var ids []string
			for _, m := range ms {
				ids = append(ids, m.ID)
			}
			if len(ids) == 0 {
				return "", "", "", errors.New("查不到「" + rt + "」能用的模型")
			}
			return "", "", "", errors.New("「" + rt + "」没有模型「" + model + "」，可选：" + clip(strings.Join(ids, "、"), 400))
		}
	}
	m, ok := pickModel(&store.Agent{Runtime: rt, Model: model})
	if effort != "" {
		if !ok || len(m.Efforts) == 0 {
			return "", "", "", errors.New("查不到模型「" + model + "」支持哪些思考强度，effort 填空串")
		}
		if !slices.Contains(m.Efforts, effort) {
			return "", "", "", errors.New("模型（" + m.ID + "）不支持思考强度「" + effort + "」，可选：" + strings.Join(m.Efforts, "、"))
		}
		return rt, model, effort, nil
	}
	if !add && rt == base.Runtime && model == base.Model {
		return rt, model, base.Effort, nil
	}
	if ok && slices.Contains(m.Efforts, workerDefaultEffort) {
		effort = workerDefaultEffort
	}
	return rt, model, effort, nil
}

// setupText 是给用户看的「harness · 模型 · 思考强度」。
func setupText(rt, model, effort string) string {
	return rt + " · " + model + " · 思考强度 " + orDefault(effort, "用模型默认")
}

// checkSkills 去掉空白和重复，确认每个 skill 本机都有。
func checkSkills(names []string) ([]string, error) {
	all := skills.List()
	out := []string{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || slices.Contains(out, n) {
			continue
		}
		if _, ok := skills.Find(all, n); !ok {
			return nil, errors.New("本机没有 skill「" + n + "」。morula 从这些目录读 skill：" + strings.Join(skills.Dirs(), "、"))
		}
		out = append(out, n)
	}
	return out, nil
}

// checkChange 在发确认卡片前检查改动能不能做，返回给用户看的一句话；
// 会丢东西的改动在 warn 里写后果，只放在确认卡片上。
func (e *Engine) checkChange(c change, as *store.Agent) (desc, warn string, err error) {
	switch c.Op {
	case "agent_add":
		name := strings.TrimSpace(c.Name)
		if name == "" {
			return "", "", errors.New("新建 agent 没有名字")
		}
		if a, _ := e.DB.Agent(name); a != nil {
			return "", "", errors.New("已有同名 agent：" + name)
		}
		if c.Role == store.RoleAssistant {
			return "", "", errors.New("助理只能有一个，不能再新建助理")
		}
		role := orDefault(c.Role, "执行")
		rt, model, effort, err := setup(c, as, true)
		if err != nil {
			return "", "", err
		}
		same := ""
		if rt == as.Runtime && model == as.Model {
			same = "，harness 和模型与助理相同"
		}
		s := fmt.Sprintf("新建 agent「%s」（%s，%s%s）", name, role, setupText(rt, model, effort), same)
		if t := strings.TrimSpace(c.Instructions); t != "" {
			s += "，职责：" + clip(oneLine(t), 60)
		}
		sk, err := checkSkills(c.Skills)
		if err != nil {
			return "", "", err
		}
		if len(sk) > 0 {
			s += "，绑定 skill：" + strings.Join(sk, "、")
		}
		return s, "", nil
	case "agent_update", "agent_remove":
		a, _ := e.DB.Agent(strings.TrimSpace(c.Target))
		if a == nil {
			return "", "", errors.New("没有这个 agent：" + c.Target)
		}
		if a.Role == store.RoleAssistant {
			return "", "", errors.New("「" + a.Name + "」是助理。改助理要在 Agent 页或设置里操作")
		}
		if c.Op == "agent_remove" {
			if a.Name == AcceptName {
				return "", "", errors.New("「验收」负责检查派下去的活是不是真做完了，不能删")
			}
			if a.Status == "working" {
				return "", "", errors.New("「" + a.Name + "」正在做事：" + a.Task + "。等它做完再删")
			}
			return "删除 agent「" + a.Name + "」（" + a.Role + "）", "删除后不能恢复，它的配置和介绍会一起去掉", nil
		}
		var parts []string
		if c.Role != "" && c.Role != a.Role {
			if c.Role == store.RoleAssistant {
				return "", "", errors.New("换助理要在设置里操作")
			}
			parts = append(parts, "角色从「"+a.Role+"」改成「"+c.Role+"」")
		}
		if t := strings.TrimSpace(c.Instructions); t != "" && t != a.Instructions {
			parts = append(parts, "职责改成："+clip(oneLine(t), 60))
		}
		if t := strings.TrimSpace(c.Profile); t != "" && t != a.Profile {
			parts = append(parts, "介绍改成："+clip(oneLine(t), 60))
		}
		if len(c.Skills) > 0 { // 空列表当作不改：模型常把不用的字段填成空列表
			sk, err := checkSkills(c.Skills)
			if err != nil {
				return "", "", err
			}
			if !slices.Equal(sk, a.Skills) {
				parts = append(parts, "绑定的 skill 改成："+orDefault(strings.Join(sk, "、"), "不绑定"))
			}
		}
		rt, model, effort, err := setup(c, a, false)
		if err != nil {
			return "", "", err
		}
		if rt != a.Runtime || model != a.Model || effort != a.Effort {
			parts = append(parts, "从「"+setupText(a.Runtime, a.Model, a.Effort)+"」改成「"+setupText(rt, model, effort)+"」")
		}
		if len(parts) == 0 {
			return "", "", errors.New("「" + a.Name + "」没有要改的地方")
		}
		return "修改 agent「" + a.Name + "」：" + strings.Join(parts, "；"), "", nil
	}
	return "", "", errors.New("morula 还不支持这种改动：" + c.Op)
}

func (e *Engine) applyChange(c change, as *store.Agent) error {
	switch c.Op {
	case "agent_add":
		// 不像 Agent 页那样让助理写介绍：这里写介绍要再起一次模型。AgentList 会用职责说明代替。
		sk, _ := checkSkills(c.Skills)
		rt, model, effort, err := setup(c, as, true)
		if err != nil {
			return err
		}
		return e.DB.AddAgent(store.Agent{Name: strings.TrimSpace(c.Name), Role: orDefault(c.Role, "执行"),
			Runtime: rt, Model: model, Effort: effort, Skills: sk,
			Instructions: strings.TrimSpace(c.Instructions), Profile: strings.TrimSpace(c.Profile)})
	case "agent_remove":
		return e.DB.RemoveAgent(strings.TrimSpace(c.Target))
	case "agent_update":
		var p store.Patch
		if c.Role != "" {
			p.Role = &c.Role
		}
		if t := strings.TrimSpace(c.Instructions); t != "" {
			p.Instructions = &t
		}
		if t := strings.TrimSpace(c.Profile); t != "" {
			p.Profile = &t
		}
		if len(c.Skills) > 0 { // 空列表当作不改：模型常把不用的字段填成空列表
			sk, _ := checkSkills(c.Skills)
			p.Skills = &sk
		}
		a, _ := e.DB.Agent(strings.TrimSpace(c.Target))
		if a == nil {
			return errors.New("没有这个 agent：" + c.Target)
		}
		rt, model, effort, err := setup(c, a, false)
		if err != nil {
			return err
		}
		if rt != a.Runtime || model != a.Model || effort != a.Effort {
			p.Runtime, p.Model, p.Effort = &rt, &model, &effort
		}
		return e.DB.UpdateAgent(strings.TrimSpace(c.Target), p)
	}
	return errors.New("不支持的改动：" + c.Op)
}

// teamResult 是一次改团队的经过。Bad 非空时什么都没改，也没发卡片。
type teamResult struct {
	Bad    []string
	Lines  []string // 每项改动给用户看的一句话
	Asked  bool     // 发了确认卡片
	Answer chat.Answer
	Err    error // 等卡片时停止了
	Done   []string
	Failed []string
}

// changeTeam 检查改动、发确认卡片，用户确认后执行。用户写了补充、或没确认时不改。
func (e *Engine) changeTeam(ctx context.Context, as *store.Agent, actions []change, reason string) teamResult {
	var res teamResult
	var items []string
	for _, c := range actions {
		s, warn, err := e.checkChange(c, as)
		if err != nil {
			res.Bad = append(res.Bad, err.Error())
			continue
		}
		res.Lines = append(res.Lines, s)
		if warn != "" {
			s += "。" + warn
		}
		items = append(items, s)
	}
	if len(res.Bad) > 0 || len(res.Lines) == 0 {
		return res
	}
	p := posOf(ctx)
	wait := e.addStep(p.f, "", "确认改动", p.tip)
	p.tip = wait
	res.Asked = true
	res.Answer, res.Err = e.Chat.Wait(ctx, chat.Event{"type": "ask", "text": "确认改团队吗？", "reason": reason,
		"items": items, "free": true,
		"options": []map[string]string{{"value": "go", "label": "确认，执行这些改动"}, {"value": "cancel", "label": "不改了"}}})
	if res.Err != nil {
		e.setStep(p.f, wait, "failed")
		return res
	}
	e.setStep(p.f, wait, "done")
	if strings.TrimSpace(res.Answer.Text) != "" || res.Answer.Choice != "go" {
		return res
	}
	for i, c := range actions {
		if err := e.applyChange(c, as); err != nil {
			res.Failed = append(res.Failed, res.Lines[i]+"：失败，"+err.Error())
		} else {
			res.Done = append(res.Done, res.Lines[i])
		}
	}
	step := e.addStep(p.f, "", "收到结果", wait)
	e.setStep(p.f, step, "done")
	p.tip = step
	return res
}

// text 是执行结果：已完成和失败各列一段。
func (r teamResult) text() string {
	out := ""
	if len(r.Done) > 0 {
		out = "已完成：\n\n- " + strings.Join(r.Done, "\n- ")
	}
	if len(r.Failed) > 0 {
		out += "\n\n失败：\n\n- " + strings.Join(r.Failed, "\n- ")
	}
	return strings.TrimSpace(out)
}

// manage 处理 decision=manage：检查改动、发确认卡片、执行。用户写了补充时返回补充的文字，
// 调用方带着它重新判断。
func (e *Engine) manage(ctx context.Context, as *store.Agent, r route) (extra string, ok bool) {
	res := e.changeTeam(ctx, as, r.Actions, r.Reply)
	switch {
	case len(res.Bad) > 0:
		e.say(ctx, "收到回复", "这次的改动没有执行：\n\n- "+strings.Join(res.Bad, "\n- "), nil)
		return "", true
	case !res.Asked:
		e.say(ctx, "收到回复", orDefault(r.Reply, "没有要改的地方。"), nil)
		return "", true
	case res.Err != nil:
		return "", false
	case strings.TrimSpace(res.Answer.Text) != "":
		return strings.TrimSpace(res.Answer.Text), true
	case res.Answer.Choice != "go":
		e.Chat.EmitIn(ctx, chat.Event{"type": "note", "text": "没有改团队"})
		return "", true
	}
	e.Chat.EmitIn(ctx, chat.Event{"type": "reply", "text": res.text(), "verdict": "fixed", "show": "agents"})
	return "", len(res.Failed) == 0
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
