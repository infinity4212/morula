package gui

import (
	"strings"
	"testing"

	"morula/internal/chat"
)

func TestNotifyKind(t *testing.T) {
	for _, c := range []struct {
		e    chat.Event
		want string
	}{
		{chat.Event{"type": "ask", "text": "用哪个库？"}, "ask"},
		{chat.Event{"type": "permission", "display": "前端 想执行危险操作"}, "ask"},
		{chat.Event{"type": "reply", "text": "做完了", "report": true}, "report"},
		{chat.Event{"type": "reply", "text": "好的"}, ""},
		{chat.Event{"type": "activity"}, ""},
		{chat.Event{"type": "task"}, ""},
	} {
		if got := notifyKind(c.e); got != c.want {
			t.Errorf("%v: 得到 %q，应为 %q", c.e, got, c.want)
		}
	}
	title, body := notifyText(chat.Event{"type": "permission", "display": "前端 想执行危险操作", "input": map[string]string{"操作": "rm -rf dist", "原因": "清理"}})
	if title != "要你批准" || body != "前端 想执行危险操作：rm -rf dist" {
		t.Errorf("批准通知：%q %q", title, body)
	}
	_, body = notifyText(chat.Event{"type": "reply", "report": true, "text": strings.Repeat("长", 200)})
	if n := len([]rune(body)); n != 141 {
		t.Errorf("正文应截到 140 字加省略号，得到 %d 字", n)
	}
}
