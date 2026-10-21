package work

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"morula/internal/journal"
	"morula/internal/store"
)

// 新会话开场带上全局记忆、前几天的总结；续跑时不带，规则改过时把新规则带上。
func TestCompactPrompt(t *testing.T) {
	e := staffEngine(t)
	journal.SaveMemory("用户偏好：先说结论\n")
	journal.SaveSummary("2000-01-01", "旧的一天：装了 grill-me")
	dir, _ := store.Dir()
	os.WriteFile(filepath.Join(dir, "days", "2000-01-01.jsonl"), []byte("{}\n"), 0o644) // Days 按记录文件列出日子
	a := &store.Agent{Name: "助理", Role: "助理", Instructions: "管好团队"}
	p := e.agentPrompt(a, false, false, "你好")
	for _, want := range []string{"先说结论", "装了 grill-me", "# 今天最近的对话", "管好团队"} {
		if !strings.Contains(p, want) {
			t.Errorf("新会话的提示词里没有 %q", want)
		}
	}
	p = e.agentPrompt(a, true, false, "你好")
	if strings.Contains(p, "先说结论") || strings.Contains(p, "管好团队") {
		t.Error("续跑时不该再带记忆和职责")
	}
	p = e.agentPrompt(a, true, true, "你好")
	if !strings.HasPrefix(p, "# 你的规则更新了") || !strings.Contains(p, agentRole) || !strings.Contains(p, "管好团队") {
		t.Error("规则改过时续跑应先带上新规则")
	}
	if e.compactLimit() != defaultCompactTokens {
		t.Error("没设时应用默认上限")
	}
	e.DB.SetSetting("assistant.compact_tokens", "1000")
	if e.compactLimit() != 1000 {
		t.Error("设置项没生效")
	}
}
