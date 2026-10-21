package work

import (
	"context"
	"sort"
	"strconv"
	"time"

	"morula/internal/chat"
	"morula/internal/store"
)

// Flow 是一条用户消息从进来到回给用户的经过：助理判断归属、安排、子 agent 干活、助理检查汇报。
// 界面按它画流程图。只存在内存里，morula 重启后清空。
//
// 子 agent 的任务不记在这里：Flows 返回时按 Run 从任务表现读，任务状态因此和项目页一致。
type Flow struct {
	ID          int64      `json:"id"`
	Text        string     `json:"text"`
	Project     int64      `json:"project"`
	ProjectName string     `json:"projectName"`
	Run         int64      `json:"run"`
	Turn        string     `json:"turn,omitempty"` // 起因的那条用户消息，界面点它跳到对话里的那条
	State       string     `json:"state"`          // working / done / failed / stopped
	Started     time.Time  `json:"started"`
	Ended       *time.Time `json:"ended,omitempty"`
	Steps       []Step     `json:"steps"`

	plan string // 安排那一步的 id，顶层任务从它连出去
}

// Step 是流程图上的一个节点。Agent 为空表示用户。From 是连进来的节点。
type Step struct {
	ID    string   `json:"id"`
	Agent string   `json:"agent"`
	What  string   `json:"what"`
	From  []string `json:"from"`
	State string   `json:"state"` // waiting / working / done / failed
	Task  int64    `json:"task,omitempty"`
}

const keepFlows = 20 // 内存里最多留多少条

// flowPos 跟着 ctx 走：属于哪条流程，下一步从哪个节点连出去。
type flowPos struct {
	f   *Flow
	tip string
}

type flowKey struct{}

// withFlow 也把流程的起因那条用户消息放进 ctx，派出去的活、汇报、验收发的事件因此都挂在那条消息下面。
func withFlow(ctx context.Context, f *Flow, tip string) context.Context {
	return context.WithValue(chat.WithTurn(ctx, f.Turn), flowKey{}, &flowPos{f: f, tip: tip})
}

func posOf(ctx context.Context) *flowPos {
	p, _ := ctx.Value(flowKey{}).(*flowPos)
	return p
}

func (e *Engine) newFlow(ctx context.Context, text string) *Flow {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.flowSeq++
	f := &Flow{ID: e.flowSeq, Text: clip(oneLine(text), 80), Turn: chat.TurnOf(ctx), State: "working", Started: time.Now(),
		Steps: []Step{{ID: "u", What: "发来消息", State: "done", From: []string{}}}}
	e.flows = append(e.flows, f)
	if len(e.flows) > keepFlows {
		e.flows = e.flows[len(e.flows)-keepFlows:]
	}
	return f
}

// addStep 加一个正在做的节点，返回它的 id。
func (e *Engine) addStep(f *Flow, agent, what string, from ...string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := "s" + strconv.Itoa(len(f.Steps))
	f.Steps = append(f.Steps, Step{ID: id, Agent: agent, What: what, From: from, State: "working"})
	return id
}

func (e *Engine) setStep(f *Flow, id, state string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range f.Steps {
		if f.Steps[i].ID == id {
			f.Steps[i].State = state
		}
	}
}

// endFlow 结束一条流程。还标着正在做的节点按 state 收尾：完成时记完成，否则记失败。
func (e *Engine) endFlow(f *Flow, state string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if f.Ended != nil {
		return
	}
	now := time.Now()
	f.State, f.Ended = state, &now
	for i := range f.Steps {
		if f.Steps[i].State == "working" {
			f.Steps[i].State = map[bool]string{true: "done", false: "failed"}[state == "done"]
		}
	}
}

// ClearFlows 去掉已结束的流程，还在跑的留着。用户清空对话页时调用。
func (e *Engine) ClearFlows() {
	e.mu.Lock()
	defer e.mu.Unlock()
	keep := e.flows[:0]
	for _, f := range e.flows {
		if f.Ended == nil {
			keep = append(keep, f)
		}
	}
	e.flows = keep
}

// DropFlowsSince 去掉 t 及以后开始、已结束的流程。用户撤回消息时调用；还在跑的留着。
func (e *Engine) DropFlowsSince(t time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	keep := e.flows[:0]
	for _, f := range e.flows {
		if f.Ended == nil || f.Started.Before(t) {
			keep = append(keep, f)
		}
	}
	e.flows = keep
}

func (e *Engine) setFlowProject(f *Flow, p *store.Project) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f.Project, f.ProjectName = p.ID, p.Name
}

func (e *Engine) flowOfRun(run int64) *Flow {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range e.flows {
		if f.Run == run {
			return f
		}
	}
	return nil
}

// say 显示给用户，并在流程上画到「你」为止。
func (e *Engine) say(ctx context.Context, what, text string, extra map[string]any) {
	e.Chat.Say(ctx, text, extra)
	if p := posOf(ctx); p != nil {
		id := e.addStep(p.f, "", what, p.tip)
		e.setStep(p.f, id, "done")
		p.tip = id
	}
}

// Flows 返回最近的流程，新的在前；project 非 0 时只要这个项目的。子 agent 的任务从任务表补进来。
func (e *Engine) Flows(project int64) []Flow {
	e.mu.Lock()
	out := []Flow{}
	for i := len(e.flows) - 1; i >= 0; i-- {
		f := e.flows[i]
		if project != 0 && f.Project != project {
			continue
		}
		c := *f
		c.Steps = append([]Step(nil), f.Steps...)
		out = append(out, c)
	}
	e.mu.Unlock()
	for i := range out {
		if out[i].Run != 0 {
			out[i].Steps = e.withTasks(out[i])
		}
	}
	return out
}

// withTasks 把任务树插在「安排」和「验收」「检查汇报」之间：顶层任务连着安排，子任务连着父任务，
// 验收（没有验收时是检查汇报）连着所有顶层任务。
func (e *Engine) withTasks(f Flow) []Step {
	tasks, err := e.DB.Tasks(f.Run)
	if err != nil || len(tasks) == 0 {
		return f.Steps
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	var tops []string
	parents := map[int64]bool{}
	for _, t := range tasks {
		parents[t.ParentID] = true
	}
	steps := []Step{}
	for _, t := range tasks {
		id := "t" + strconv.FormatInt(t.ID, 10)
		from := f.plan
		if t.ParentID != 0 {
			from = "t" + strconv.FormatInt(t.ParentID, 10)
		} else {
			tops = append(tops, id)
		}
		st := taskState(t.Status)
		if st == "waiting" && parents[t.ID] {
			st = "working" // 拆了子任务的父任务在等子任务，算正在做
		}
		if f.Ended != nil && st == "working" {
			st = "failed" // 流程已结束还没收尾的任务，是被停掉的
		}
		steps = append(steps, Step{ID: id, Agent: t.Agent, What: t.Title, From: []string{from}, State: st, Task: t.ID})
	}
	out := make([]Step, 0, len(f.Steps)+len(steps))
	linked := false
	for _, s := range f.Steps {
		// 验收（没有验收时是检查汇报）连着所有顶层任务
		if !linked && (s.What == acceptWhat || s.What == finishWhat) && len(s.From) == 0 {
			s.From, linked = tops, true
		}
		out = append(out, s)
		if s.ID == f.plan {
			out = append(out, steps...)
		}
	}
	return out
}

const finishWhat = "检查汇报"

func taskState(s string) string {
	switch s {
	case "done":
		return "done"
	case "failed":
		return "failed"
	case "waiting":
		return "waiting"
	}
	return "working" // working、checking、approval、rejected（打回后等重做）
}
