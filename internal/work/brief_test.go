package work

import (
	"strings"
	"testing"
	"time"

	"morula/internal/harness"
)

func TestBriefPrompt(t *testing.T) {
	at := time.Date(2026, 10, 18, 9, 30, 0, 0, time.Local)
	p := briefPrompt([]harness.Chat{
		{Source: ".codex", Dir: "/proj/a", Updated: at, Users: 3, Title: "登录页", First: "把登录页\n改成深色", LastUser: "再跑一下测试", LastReply: "测试过了"},
		{Source: ".workbuddy", Updated: at, Users: 1, Title: "周报", First: "查一下周报"},
	})
	for _, want := range []string{"## c1\n来源：.codex；更新：2026-10-18 09:30；目录：/proj/a；用户消息 3 条", "第一条：把登录页 改成深色",
		"最后一条：再跑一下测试", "最后的回复：测试过了", "## c2", "目录：（不知道）", "最后的回复：（没有）", "没看出来有没有做完"} {
		if !strings.Contains(p, want) {
			t.Errorf("提示词里没有 %q", want)
		}
	}
	if strings.Count(p, "最后一条：") != 1 {
		t.Error("只有一条用户消息时不该重复写最后一条")
	}
}
