package work

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"morula/internal/chat"
	"morula/internal/harness"
)

// activity 把助理一次处理里 harness 报出的每一步发成对话里的 activity 事件，界面一条一条显示，
// 回复到了就折叠。
//
// 助理的话先压着：运行结束时最后一句就是回复，不显示成过程；
// 下一件事来了，或压了 holdFor 还没有下文，压着的那句才是过程里的一行。按时间放出来的那句
// 后来成了回复时，结束事件带 drop，界面去掉这一行。
type activity struct {
	e     *Engine
	group string
	ctx   context.Context // 只用来带所属的用户消息（chat.WithTurn），不用来取消

	mu    sync.Mutex
	held  string
	timer *time.Timer
	tail  bool // 最后发出的一行是按时间放出来的那句，之后还没有别的事
	steps int
	ended bool
	inbox bool // 这组过程放收件箱：之后的每一步都带上 inbox，界面不画进对话
}

// holdFor：助理的一句话压这么久还没有下文，就先显示。模型想下一步时常要十几秒，不等它。
var holdFor = 3 * time.Second

var activitySeq struct {
	sync.Mutex
	n int
}

// newActivity 开一组过程，马上发一条 start，界面先显示 what（空时显示「正在思考」）。
// 组号写进当天记录，要跨重启唯一。
func (e *Engine) newActivity(ctx context.Context, what string, extra chat.Event) *activity {
	activitySeq.Lock()
	activitySeq.n++
	g := fmt.Sprintf("a%d-%d", time.Now().UnixMilli(), activitySeq.n)
	activitySeq.Unlock()
	x := &activity{e: e, group: g, ctx: ctx, inbox: extra["inbox"] == true}
	ev := chat.Event{"type": "activity", "group": g, "kind": "start", "text": what}
	for k, v := range extra {
		ev[k] = v
	}
	e.Chat.EmitIn(ctx, ev)
	return x
}

// on 收 harness 的一步。
func (x *activity) on(ev harness.Event) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.ended {
		return
	}
	if ev.Kind == "message" {
		x.flush()
		x.held = strings.TrimSpace(ev.Text)
		held := x.held
		x.timer = time.AfterFunc(holdFor, func() {
			x.mu.Lock()
			defer x.mu.Unlock()
			if !x.ended && x.held == held && held != "" {
				x.flush()
				x.tail = true
			}
		})
		return
	}
	x.flush()
	e := chat.Event{"kind": ev.Kind}
	if ev.Text != "" {
		e["text"] = clip(ev.Text, 600)
	}
	if ev.Tool != "" {
		e["tool"] = ev.Tool
	}
	if ev.Args != "" {
		e["args"] = clip(ev.Args, 600)
	}
	if ev.Failed {
		e["failed"] = true
	}
	if ev.Code != nil {
		e["code"] = *ev.Code
	}
	x.emit(e)
}

// line 在运行结束后加一行 morula 自己的话，例如验收的结论。压着的那句是 harness 的最后回答
// （验收时是 JSON），由这行代替，不显示。
func (x *activity) line(text string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.ended {
		return
	}
	x.held = ""
	x.emit(chat.Event{"kind": "message", "text": clip(text, 4000)})
}

// flush 把压着的那句话发成一行过程。调用方须持有 x.mu。
func (x *activity) flush() {
	if x.timer != nil {
		x.timer.Stop()
		x.timer = nil
	}
	if x.held == "" {
		return
	}
	x.emit(chat.Event{"kind": "message", "text": clip(x.held, 2000)})
	x.held = ""
}

func (x *activity) emit(e chat.Event) {
	e["type"], e["group"] = "activity", x.group
	x.tail = false
	if e["kind"] != "done" && e["kind"] != "tool_done" {
		x.steps++
	}
	if x.inbox {
		e["inbox"] = true
	}
	x.e.Chat.EmitIn(x.ctx, e)
}

// end 收尾这组过程。state 是 done、failed、stopped，或 rejected（验收没通过）。reply 为 true 表示压着的那句是回复，丢掉不显示；
// 失败或停止时它只是过程里的一句话，照常显示。
func (x *activity) end(state string, reply bool, tokens int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.ended {
		return
	}
	if !reply {
		x.flush()
	} else if x.timer != nil {
		x.timer.Stop()
	}
	x.held = ""
	x.ended = true
	e := chat.Event{"type": "activity", "group": x.group, "kind": "end", "state": state, "steps": x.steps}
	if reply && x.tail { // 先显示出来的那句其实是回复
		e["drop"] = true
		e["steps"] = x.steps - 1
	}
	if tokens > 0 {
		e["tokens"] = tokens
	}
	if x.inbox {
		e["inbox"] = true
	}
	x.e.Chat.EmitIn(x.ctx, e)
}
