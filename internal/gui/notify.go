package gui

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"morula/internal/chat"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

// 系统通知：只在要用户拍板（回答问题、批准危险操作）和安排做完汇报时发，过程中的每一步不发。
// morula 窗口在前台时不发。只有 morula 窗口里有，morula web 不发。
// 两类各有一个开关，存在设置 notify.ask、notify.report 里，默认开。

// notifyKind 返回事件属于哪一类通知，不发通知时返回空串。
func notifyKind(e chat.Event) string {
	switch e["type"] {
	case "ask", "permission":
		return "ask"
	case "reply":
		if b, _ := e["report"].(bool); b {
			return "report"
		}
	}
	return ""
}

func notifyOn(kind string) bool { return db.Setting("notify."+kind, "1") == "1" }

// notifyText 写通知的标题和正文。
func notifyText(e chat.Event) (title, body string) {
	s := func(k string) string { v, _ := e[k].(string); return strings.TrimSpace(v) }
	switch e["type"] {
	case "permission":
		title, body = "要你批准", s("display")
		if in, ok := e["input"].(map[string]string); ok && in["操作"] != "" {
			body += "：" + in["操作"]
		}
	case "ask":
		title, body = "要你回答", s("text")
	default:
		title, body = "安排做完了", s("text")
	}
	if r := []rune(body); len(r) > 140 {
		body = string(r[:140]) + "…"
	}
	return title, body
}

// startNotify 起通知服务，接到会话上。起不来时（例如不在 .app 里运行，没有 bundle id）只记日志。
func startNotify(win *application.WebviewWindow) {
	ns := notifications.New()
	if err := ns.ServiceStartup(context.Background(), application.ServiceOptions{}); err != nil {
		log.Println("morula: 系统通知不可用：", err)
		return
	}
	// 点通知回到 morula 窗口
	ns.OnNotificationResponse(func(notifications.NotificationResult) {
		win.Show()
		win.Focus()
	})
	var ask sync.Once
	var n int
	var mu sync.Mutex
	sess.OnEmit = func(e chat.Event) {
		kind := notifyKind(e)
		if kind == "" || !notifyOn(kind) || win.IsFocused() {
			return
		}
		// 第一次要发时才请求权限，macOS 会弹一次询问
		ask.Do(func() {
			if ok, _ := ns.CheckNotificationAuthorization(); !ok {
				if _, err := ns.RequestNotificationAuthorization(); err != nil {
					log.Println("morula: 请求通知权限失败：", err)
				}
			}
		})
		mu.Lock()
		n++
		id := fmt.Sprint("morula-", n)
		mu.Unlock()
		title, body := notifyText(e)
		if err := ns.SendNotification(notifications.NotificationOptions{ID: id, Title: title, Body: body}); err != nil {
			log.Println("morula: 发通知失败：", err)
		}
	}
}
