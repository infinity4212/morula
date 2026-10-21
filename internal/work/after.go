package work

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"morula/internal/store"
)

// 派活后的后续：用户一句话里常有两件事，
// 后一件要等前一件做完，例如「装好这个 skill，再用它建一个 agent」。助理派前一件时把后一件写进
// dispatch 的 after，morula 记在设置 run.after.<安排编号> 里；汇报发出后交回助理接着做。
// 验收打回时取出来交给重新安排的助理：它可能在打回那一轮里已经做了，要不要再记由它定。

func afterKey(runID int64) string { return "run.after." + strconv.FormatInt(runID, 10) }

// setAfter 记下安排 runID 做完后要接着做的事。
func (e *Engine) setAfter(runID int64, text string) {
	if text = strings.TrimSpace(text); text != "" {
		e.DB.SetSetting(afterKey(runID), text)
	}
}

// peekAfter 返回安排 runID 记着的后续，不清掉。
func (e *Engine) peekAfter(runID int64) string {
	return e.DB.Setting(afterKey(runID), "")
}

// takeAfter 取出并清掉安排 runID 记着的后续。
func (e *Engine) takeAfter(runID int64) string {
	s := e.peekAfter(runID)
	if s != "" {
		e.DB.SetSetting(afterKey(runID), "")
	}
	return s
}

// afterTurn 在汇报发出后让助理接着做记下的后续。和用户的消息一样能停止、能补充。
func (e *Engine) afterTurn(ctx context.Context, a *store.Agent, p *store.Project, runID int64, after, report string) {
	defer e.hold(a.Name, "接着做「"+p.Name+"」的后续")()
	f := e.newFlow(ctx, "接着做："+after)
	e.setFlowProject(f, p)
	step := e.addStep(f, a.Name, "接着做", "u")
	msg := fmt.Sprintf("# 派活时记下的后续\n项目「%s」（编号 %d）的安排 %d 做完了，汇报已经发给用户：\n%s\n\n"+
		"你派活时记下了做完后要接着做的事：\n%s\n\n现在接着做。前一件没做成、接不下去时，不要硬做，回复用户卡在哪里。",
		p.Name, p.ID, runID, report, after)
	e.agentLoop(withFlow(ctx, f, step), a, &agentRun{e: e, a: a, text: after}, msg, nil)
}
