package work

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"morula/internal/store"
)

// 第一层名单是名字、配置和介绍：助理和验收单列为固定成员，没写介绍的用职责说明顶上；
// 每个 agent 写明 harness · 模型 · 思考强度，模型下架时写明开工会换成哪个。
func TestAgentList(t *testing.T) {
	e := staffEngine(t)
	e.DB.AddAgent(store.Agent{Name: "前端", Role: "开发", Runtime: "codex", Model: "m", Instructions: "改界面\n写样式", Profile: ""})
	e.DB.AddAgent(store.Agent{Name: "文案", Role: "写作", Runtime: "codex", Model: "下架了", Effort: "low", Skills: []string{"go-dev"}, Instructions: "很长的职责说明", Profile: "替你写说明文档"})
	e.DB.AddAgent(store.Agent{Name: "旧的", Role: "执行", Runtime: "hermes", Model: "x"})
	got := AgentList(e.DB)
	for _, want := range []string{"固定成员：\n- 助理（助理，codex · m · 思考强度 high）：就是你", "- 验收（验收，codex · m · 思考强度 用模型默认）：", "子 agent（派活时从这里选）：\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("名单里缺 %q：\n%s", want, got)
		}
	}
	for _, bad := range []string{"很长的职责说明"} {
		if strings.Contains(got, bad) {
			t.Errorf("名单里不该有 %q：\n%s", bad, got)
		}
	}
	for _, want := range []string{"- 前端（开发，codex · m · 思考强度 用模型默认）：改界面 写样式",
		"- 文案（写作，codex · 下架了 · 思考强度 low，模型已不在列表里，开工时换成 m，skill：go-dev）：替你写说明文档",
		"- 旧的（执行，hermes · x · 思考强度 用模型默认，harness 未接入，不能派活）"} {
		if !strings.Contains(got, want) {
			t.Errorf("名单里缺 %q：\n%s", want, got)
		}
	}
	e.DB.AddProject("morula", "agent 团队的桌面工作台", t.TempDir())
	if p := ProjectList(e.DB); !strings.Contains(p, "「morula」：agent 团队的桌面工作台") {
		t.Errorf("项目清单 = %q", p)
	}
}

// harness 名单：能用的列出能力和模型（思考强度、默认档），不能用的写原因；模型多时只列前面的，full 时列全部。
func TestHarnessList(t *testing.T) {
	e := staffEngine(t)
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", bin)
	got := HarnessList(e.DB, false)
	for _, want := range []string{"- codex：能用，能接 morula 的工具", "1 个模型：\n  - m：思考强度 low、medium、high，默认 medium\n",
		"- opencode：不能用（没找到 opencode 命令）", "- hermes：不能用（未接入）"} {
		if !strings.Contains(got, want) {
			t.Errorf("harness 名单里缺 %q：\n%s", want, got)
		}
	}
}
