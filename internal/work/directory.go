package work

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"morula/internal/harness"
	"morula/internal/store"
)

// 助理分层看团队和项目：判断归属时只看名字和介绍，
// 选定项目、要派活时才加载项目记忆和执行者的详细说明（plan 里的 projectBlock 与 roster）。

// AgentList 是第一层的团队名单：名字、角色、harness · 模型 · 思考强度、绑定的 skill 和一句话介绍。
// 助理和验收单列在前面。
func AgentList(db *store.Store) string {
	list, err := db.Agents()
	if err != nil {
		return "读花名册失败：" + err.Error()
	}
	// 助理和验收也是团队成员，用户问团队里有谁时要算上。它们各有固定的活，派活时不从它们里选，单列在前面。
	var fixed, b, off strings.Builder
	for _, a := range list {
		if a.Role == store.RoleAssistant {
			fmt.Fprintf(&fixed, "- %s（%s，%s）：就是你。和用户对话，自己出方案、回答问题；要改文件或要分几步的活安排给子 agent，检查后向用户汇报\n", a.Name, a.Role, agentSetup(&a))
			continue
		}
		if a.Name == AcceptName {
			if a.Off {
				fmt.Fprintf(&fixed, "- %s（%s，%s）：用户在 Agent 页把它关了，这段时间派下去的活不验收\n", a.Name, a.Role, agentSetup(&a))
				continue
			}
			fmt.Fprintf(&fixed, "- %s（%s，%s）：派下去的活做完后，先经它核对是不是真做完了；没通过就打回给助理重新安排\n", a.Name, a.Role, agentSetup(&a))
			continue
		}
		if a.Off {
			fmt.Fprintf(&off, "- %s（%s）\n", a.Name, a.Role)
			continue
		}
		about := strings.TrimSpace(a.Profile)
		if about == "" {
			about = clip(oneLine(a.Instructions), 80)
		}
		if about == "" {
			about = "（没有介绍）"
		}
		sk := ""
		if len(a.Skills) > 0 {
			sk = "，skill：" + strings.Join(a.Skills, "、")
		}
		fmt.Fprintf(&b, "- %s（%s，%s%s）：%s\n", a.Name, a.Role, agentSetup(&a), sk, oneLine(about))
	}
	if b.Len() == 0 {
		b.WriteString("（还没有子 agent。派活时 agent 字段填空串，morula 会安排一个通用执行者）\n")
	}
	out := "固定成员：\n" + fixed.String() + "子 agent（派活时从这里选）：\n" + b.String()
	if off.Len() > 0 {
		out += "关着的 agent（用户在 Agent 页关掉了，不接新活，派活时不选）：\n" + off.String()
	}
	return out
}

// agentSetup 是「harness · 模型 · 思考强度」。模型不在列表里时写明开工会换成哪个（见 liveModel），
// harness 没接入时写明不能派活。
func agentSetup(a *store.Agent) string {
	s := setupText(a.Runtime, a.Model, a.Effort)
	if _, err := harness.Get(a.Runtime); err != nil {
		return s + "，harness 未接入，不能派活"
	}
	if m, ok := pickModel(a); ok && m.ID != a.Model {
		s += "，模型已不在列表里，开工时换成 " + m.ID
	}
	return s
}

// harnessModels 是每个 harness 在名单里最多列几个模型。opencode 这类 harness 能列出上百个。
const harnessModels = 40

// HarnessList 是本机的 harness 和模型：哪些能派活、各有什么能力、有哪些模型和思考强度。
// 助理新建或修改 agent 时按它选 harness、模型和思考强度。full 为 false 时每个 harness 只列前 harnessModels 个模型。
// 用户在启动流程里没勾的工具和模型不列。
func HarnessList(db *store.Store, full bool) string {
	var b strings.Builder
	on, pick := db.Choice()
	for _, h := range harness.Chosen(harness.List(store.Runtimes), on, pick) {
		if !h.Ready {
			fmt.Fprintf(&b, "- %s：不能用（%s）\n", h.Name, oneLine(h.Reason))
			continue
		}
		var can []string
		if p := h.Profile; p != nil {
			if p.Tools {
				can = append(can, "能接 morula 的工具")
			}
			if p.Resume {
				can = append(can, "能续跑会话")
			}
			if p.Images {
				can = append(can, "能附图片")
			}
		}
		if len(h.Models) == 0 {
			fmt.Fprintf(&b, "- %s：本机装了，但查不到模型，不能派活\n", h.Name)
			continue
		}
		fmt.Fprintf(&b, "- %s：能用", h.Name)
		if len(can) > 0 {
			b.WriteString("，" + strings.Join(can, "、"))
		}
		fmt.Fprintf(&b, "；%d 个模型：\n", len(h.Models))
		for i, m := range h.Models {
			if !full && i == harnessModels {
				fmt.Fprintf(&b, "  - 另有 %d 个，用 list_harnesses 查全部\n", len(h.Models)-i)
				break
			}
			b.WriteString("  - " + m.ID)
			if m.Name != "" && m.Name != m.ID {
				b.WriteString("（" + m.Name + "）")
			}
			if len(m.Efforts) > 0 {
				b.WriteString("：思考强度 " + strings.Join(m.Efforts, "、"))
				if m.Default != "" {
					b.WriteString("，默认 " + m.Default)
				}
			}
			if m.Via != "" {
				b.WriteString("；经插件或 API key「" + m.Via + "」")
			}
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		return "（本机没有可选的 harness）\n"
	}
	return b.String()
}

// ProjectList 是第一层的项目清单：编号、名字和项目摘要，不含项目记忆。
func ProjectList(db *store.Store) string {
	ps, err := db.Projects()
	if err != nil {
		return "读项目失败：" + err.Error()
	}
	def, _ := strconv.ParseInt(db.Setting("project.default", ""), 10, 64)
	var b strings.Builder
	for _, p := range ps {
		mark := ""
		if p.ID == def {
			mark = "（默认项目：闲聊、问答和零散小事归这里）"
		}
		if p.Owner != "" {
			mark += "（项目负责人「" + p.Owner + "」）"
		}
		fmt.Fprintf(&b, "- 编号 %d「%s」%s：%s\n", p.ID, p.Name, mark, oneLine(p.Summary))
	}
	if b.Len() == 0 {
		return "（还没有项目）\n"
	}
	return b.String()
}

// directoryTools 是判断归属时挂给助理的两个工具（morula mcp --assistant）。harness 不支持时返回 nil，
// 调用方改为把两份清单直接写进提示词。
func directoryTools(runtime string) *harness.Tools {
	if !harness.UsesTools(runtime) {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	dir, err := store.Dir()
	if err != nil {
		return nil
	}
	return &harness.Tools{Command: exe, Args: []string{"mcp", "--assistant"},
		Env: map[string]string{"MORULA_HOME": dir}, Names: []string{"list_projects", "list_agents"}}
}
