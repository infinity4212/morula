package work

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"unicode"

	"morula/internal/harness"
	"morula/internal/journal"
)

// 助理会话的上下文由哪几块组成。harness 只给总数，各块是 morula 按自己发出去、收回来的内容估的：
// 开新会话时的规则算职责说明，记忆、盘点、交接记录、今天最近的对话算记忆，每轮的用户消息和助理的话算对话，
// 工具调用的参数和结果算工具结果。harness 自己压缩过会话后，比例不再准，总数仍是 harness 给的。
// 存在设置 assistant.mix（JSON），只对 Session 那个会话有效。

// CtxMix 是一个会话里各块的估算 token 数。
type CtxMix struct {
	mu      sync.Mutex
	Session string `json:"session"`
	Role    int    `json:"role"`
	Memory  int    `json:"memory"`
	Chat    int    `json:"chat"`
	Tools   int    `json:"tools"`
}

// estTokens 粗估 token 数：中日韩文字一个字算一个，其他字符四个算一个。
func estTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + other/4
}

// mixFor 读 tid 这个会话的组成；tid 为空（新会话）或记下的不是它时从零开始。
func (e *Engine) mixFor(tid string) *CtxMix {
	m := &CtxMix{}
	if tid != "" && json.Unmarshal([]byte(e.DB.Setting("assistant.mix", "")), m) == nil && m.Session == tid {
		return m
	}
	return &CtxMix{}
}

func (e *Engine) saveMix(m *CtxMix, session string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Session = session
	b, _ := json.Marshal(m)
	e.DB.SetSetting("assistant.mix", string(b))
}

// addPrompt 记下这一轮发出去的提示词。新会话时 rules 那段算职责说明，msg 算对话，其余算记忆；
// 续跑时整段算对话，规则更新后补发的规则（stale）算职责说明。
func (m *CtxMix) addPrompt(prompt, role, msg string, resume, stale bool) {
	p, r, c := estTokens(prompt), estTokens(role), estTokens(msg)
	switch {
	case !resume:
		m.Role += r
		m.Chat += c
		m.Memory += max(0, p-r-c)
	case stale:
		m.Role += r
		m.Chat += max(0, p-r)
	default:
		m.Chat += p
	}
}

// addEvent 记下 harness 的一个事件：助理的话算对话，工具的参数和结果算工具结果。
func (m *CtxMix) addEvent(ev harness.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch ev.Kind {
	case "message":
		m.Chat += estTokens(ev.Text)
	case "thinking": // 思考过程不一定留在上下文里，不算
	default:
		m.Tools += estTokens(ev.Args) + estTokens(ev.Text)
	}
}

// AssistantMix 是助理当前会话的组成，各块按 AssistantContext 的总数等比缩放。还没有会话、或没记下组成时返回 nil。
func (e *Engine) AssistantMix() map[string]int {
	total := e.AssistantContext()
	m := e.mixFor(e.threadID())
	sum := m.Role + m.Memory + m.Chat + m.Tools
	if total == 0 || sum == 0 {
		return nil
	}
	scale := func(n int) int { return n * total / sum }
	out := map[string]int{"role": scale(m.Role), "memory": scale(m.Memory), "tools": scale(m.Tools)}
	out["chat"] = total - out["role"] - out["memory"] - out["tools"]
	return out
}

// threadID 是今天助理会话的 id，没有时返回空串。
func (e *Engine) threadID() string {
	v := strings.Split(e.DB.Setting("assistant.thread", ""), "|")
	if len(v) == 3 && v[0] == journal.Today() {
		return v[2]
	}
	return ""
}

// HandoffNow 是上下文弹层里的「写交接记录，换新会话」：助理续跑当前会话写交接记录，下一条消息新开会话。
// 助理在忙时不做，免得和正在跑的那一轮抢同一个会话。
func (e *Engine) HandoffNow() error {
	a, err := e.assistant()
	if err != nil {
		return err
	}
	tid, fork, gen, _ := e.takeThread()
	if fork {
		return errors.New("助理在忙，等它答完再试")
	}
	defer e.releaseThread()
	if tid == "" {
		return errors.New("今天还没和助理对话，不用重新开始")
	}
	e.compactAssistant(a, gen, tid)
	if e.threadID() == tid {
		return errors.New("要点没记下来，还没重新开始。可以再试一次")
	}
	return nil
}
