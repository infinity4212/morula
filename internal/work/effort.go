package work

import (
	"slices"
	"strings"

	"morula/internal/harness"
	"morula/internal/store"
)

// 思考强度：助理派活时可以给每个任务定思考强度，
// 简单的活用低一档，省时间；不定时用执行者自己的设置。新建的执行者默认用 workerDefaultEffort，
// 不再照抄助理的：助理常设成 max，照抄后装一个 skill 也要几分钟。

const workerDefaultEffort = "medium"

// efforts 返回 a 的模型支持的思考强度，查不到时返回 nil。
func efforts(a *store.Agent) []string {
	if m, ok := pickModel(a); ok && m.ID == a.Model {
		return m.Efforts
	}
	return nil
}

// pickModel 返回 a 这次实际会用的模型：a 的模型在 harness 列表里就是它，不在时是列表里的第一个（见 liveModel）。
// 列表读不到时 ok 为 false。
func pickModel(a *store.Agent) (m harness.Model, ok bool) {
	if a == nil || a.Model == "" {
		return m, false
	}
	ad, err := harness.Get(a.Runtime)
	if err != nil {
		return m, false
	}
	ms := ad.Models()
	for _, x := range ms {
		if x.ID == a.Model {
			return x, true
		}
	}
	if len(ms) == 0 {
		return m, false
	}
	return ms[0], true
}

// workerEffort 是照 as 的模型新建执行者时用的思考强度：模型支持 workerDefaultEffort 就用它，否则用模型默认。
func workerEffort(as *store.Agent) string {
	if slices.Contains(efforts(as), workerDefaultEffort) {
		return workerDefaultEffort
	}
	return ""
}

// taskEffort 是任务实际用的思考强度：派活时定的、执行者的模型也支持，就用它；否则用执行者自己的设置。
// 派活时执行者可能还没定（agent 填空串），所以开工时再核对一次。
func taskEffort(a *store.Agent, want string) string {
	m, ok := pickModel(a) // 模型下架时按换上的模型核对
	if want != "" && ok && slices.Contains(m.Efforts, want) {
		return want
	}
	if a.Effort != "" && ok && !slices.Contains(m.Efforts, a.Effort) {
		return "" // 模型换过，原来的思考强度它不支持
	}
	return a.Effort
}

// checkEffort 核对派活时定的思考强度，不对时返回原因。点了名的执行者按它的模型核对；
// 没点名时执行者开工才定，按助理的模型核对（新建的通用执行者用助理的模型），开工时不支持就用执行者自己的设置。
func (e *Engine) checkEffort(t taskSpec) string {
	want := strings.TrimSpace(t.Effort)
	if want == "" {
		return ""
	}
	a, who := (*store.Agent)(nil), ""
	if t.Agent != "" {
		a, _ = e.DB.Agent(t.Agent)
		who = "执行者「" + t.Agent + "」"
	}
	if a == nil {
		a, _ = e.assistant()
		who = "助理"
	}
	// 模型下架时开工会换成列表里的第一个（见 liveModel），这里按换上的模型核对，和 taskEffort 一致。
	m, ok := pickModel(a)
	if !ok || len(m.Efforts) == 0 {
		return "查不到" + who + "的模型支持哪些思考强度，effort 填空串"
	}
	if !slices.Contains(m.Efforts, want) {
		return who + "的模型（" + m.ID + "）不支持思考强度「" + want + "」，可选：" + strings.Join(m.Efforts, "、")
	}
	return ""
}
