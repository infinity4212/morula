package work

import (
	"context"
	"strconv"
	"strings"
	"time"

	"morula/internal/chat"
	"morula/internal/harness"
	"morula/internal/journal"
	"morula/internal/store"
)

// 助理的记忆存在 morula 里，harness 的会话只是缓存。
// 助理一天一个会话：上下文（最后一次请求带的 token，见 harness.Result.ContextTokens）到上限时，
// harness 会压缩的（harness.Compacts）由它自己压缩，morula 不换会话；不会压缩的由 morula 让助理写交接记录，
// 下一条消息换新会话，见 compactAssistant。和 agent 的对话（Direct）按交接记录换会话，见 compactDirect。

// defaultCompactTokens 是会话的上下文到多少 token 后压缩（助理）或换会话（对话）。一轮里多次请求的用量加起来不算：那是上下文的好几倍，
// 按它算的话三四轮就换一次会话，一直在丢上下文。设置项 assistant.compact_tokens 可以改。
const defaultCompactTokens = 250000

func (e *Engine) compactLimit() int {
	if n, err := strconv.Atoi(e.DB.Setting("assistant.compact_tokens", "")); err == nil && n > 0 {
		return n
	}
	return defaultCompactTokens
}

const directHandoffPrompt = `这个会话太长了，morula 要换一个新会话，让你接着和用户对话。新会话也是你，但看不到这个会话里的内容。
写一份交接记录给它：用户要做成什么、已经定下的结论和决定、改过的文件、正在讨论的问题停在哪一步、接下来要问或要做什么。
项目记忆里已经有的不用重复。不超过 1500 字，用短列表。只输出交接记录本身，不要调用工具。`

// compactDirect 在对话的一轮回复用掉的 token 超过上限后，让对话里的 agent 写交接记录，下一条消息换新会话。
// 对话期间结束或换了人时不改。
func (e *Engine) compactDirect(a *store.Agent, p *store.Project, d *Direct, tid string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s := spec(a, p.Dir, directHandoffPrompt, "")
	s.Access, s.Resume = harness.ReadOnly, tid
	res, err := harness.Ask(ctx, a.Runtime, s, nil, nil)
	text := strings.TrimSpace(res.Final)
	if err != nil || text == "" {
		return // 下一轮还超就再试；会话照旧续跑
	}
	if e.updateDirect(d.Agent, d.Since, func(cur *Direct) {
		if cur.Session == tid {
			cur.Handoff, cur.Prev, cur.Session = text, tid, ""
		}
	}) {
		e.Chat.Emit(chat.Event{"type": "notice", "text": "「" + a.Name + "」的对话太长，已记下要点，从下一条消息起重新开始。"})
	}
}

const assistantHandoffPrompt = `这个会话太长了，morula 要换一个新会话，让你接着当助理。新会话也是你，但看不到这个会话里的内容，
只看得到 morula 存着的记忆、今天最近的对话和这份交接记录。写交接记录给它：用户今天要做成什么、已经定下的结论和决定、
派出去还没回来的活（安排编号、项目）、答应了用户还没做的事、正在和用户商量的事停在哪一步。
记忆和项目记忆里已经有的不用重复。不超过 1500 字，用短列表。只输出交接记录本身，不要调用工具。`

// compactAssistant 在助理的会话太长、harness 又不会自己压缩时，续跑这个会话写交接记录，记进 assistant.handoff，
// 清掉 assistant.thread，下一条消息新开会话时带上（agentPrompt）。调用方占着会话，写完再放开。
// 期间清空过对话页（gen 变了）、或会话已经换了时不改。写不出来时照旧续跑，下一轮还超再试。
func (e *Engine) compactAssistant(a *store.Agent, gen int, tid string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s := spec(a, scratch(), assistantHandoffPrompt, "")
	s.Access, s.Resume = harness.ReadOnly, tid
	res, err := harness.Ask(ctx, a.Runtime, s, nil, nil)
	text := strings.TrimSpace(res.Final)
	if err == nil && text != "" && e.saveHandoff(gen, tid, text) {
		e.Chat.Emit(chat.Event{"type": "notice", "text": "助理的对话太长，已记下要点，从下一条消息起重新开始。"})
	}
}

// saveHandoff 记下交接记录，清掉当前会话。gen 变了、或当前会话已经不是 tid 时不改，返回 false。
func (e *Engine) saveHandoff(gen int, tid, text string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := strings.Split(e.DB.Setting("assistant.thread", ""), "|")
	if gen != e.threadGen || len(v) != 3 || v[2] != tid {
		return false
	}
	e.DB.SetSetting("assistant.handoff", v[0]+"|"+tid+"|"+text)
	e.DB.SetSetting("assistant.thread", "")
	e.DB.SetSetting("assistant.context", "")
	return true
}

// handoff 是今天上一个助理会话留下的交接记录；没有、或不是今天的时返回空。
func (e *Engine) handoff() string {
	h := strings.SplitN(e.DB.Setting("assistant.handoff", ""), "|", 3)
	if len(h) != 3 || h[0] != journal.Today() {
		return ""
	}
	return h[2]
}
