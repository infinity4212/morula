package work

import (
	"testing"

	"morula/internal/harness"
	"morula/internal/journal"
)

// 上下文的组成：新会话的提示词分到职责说明、记忆、对话，续跑整段算对话，事件按种类分，思考不算；
// 各块按 harness 给的总数缩放，加起来等于总数；不是当前会话的组成不显示。
func TestCtxMix(t *testing.T) {
	if estTokens("你好abcd") != 3 {
		t.Errorf("estTokens = %d，应为 3", estTokens("你好abcd"))
	}
	m := &CtxMix{}
	m.addPrompt("规则规则记忆记忆记忆问题", "规则规则", "问题", false, false)
	if m.Role != 4 || m.Memory != 6 || m.Chat != 2 {
		t.Errorf("新会话分错了：%+v", m)
	}
	m.addPrompt("续跑", "规则", "续跑", true, false)
	m.addPrompt("规则补发问", "规则", "问", true, true)
	if m.Role != 6 || m.Chat != 2+2+3 {
		t.Errorf("续跑分错了：%+v", m)
	}
	m.addEvent(harness.Event{Kind: "message", Text: "答复"})
	m.addEvent(harness.Event{Kind: "thinking", Text: "想一想想一想"})
	m.addEvent(harness.Event{Kind: "tool", Args: "参数", Text: "结果"})
	if m.Chat != 9 || m.Tools != 4 {
		t.Errorf("事件分错了：%+v", m)
	}

	e := staffEngine(t)
	if e.AssistantMix() != nil {
		t.Error("没有会话时应返回 nil")
	}
	e.DB.SetSetting("assistant.thread", journal.Today()+"|"+roleVer()+"|t1")
	e.DB.SetSetting("assistant.context", "t1|1000")
	e.saveMix(m, "t1")
	mix := e.AssistantMix()
	if mix == nil || mix["role"]+mix["memory"]+mix["chat"]+mix["tools"] != 1000 || mix["role"] != 6*1000/25 {
		t.Errorf("缩放不对：%v", mix)
	}
	e.saveMix(m, "t0")
	if e.AssistantMix() != nil {
		t.Error("记下的是别的会话，不该显示")
	}
}
