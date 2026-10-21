package work

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"morula/internal/chat"
	"morula/internal/harness"
	"morula/internal/store"
)

// 首次使用时读用户最近的对话记录：本机所有 .jsonl 里最近的 10 个，只取每个的标题、目录、第一条和最后一条用户消息、
// 最后一条回复，让助理列出「最近在做的事 → 停在哪一步 → 依据」，每件事给一条「接着做」时发的消息。只做一次。

// briefChats 是交给助理的对话记录数。
const briefChats = 10

// briefKey 记做过（或开始做）最近在做的事，只做一次。
const briefKey = "onboard.brief"

// Pick 是回复下面的一个按钮：点了把 Text 填进输入框，用户改完自己发。
type Pick struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

const briefSchema = `{"type":"object","additionalProperties":false,"required":["items"],"properties":{
"items":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["what","stop","evidence","chats","next"],
"properties":{"what":{"type":"string"},"stop":{"type":"string"},"evidence":{"type":"string"},"chats":{"type":"array","items":{"type":"string"}},"next":{"type":"string"}}}}}}`

const briefRules = `用户刚开始用 morula。下面是用户本机最近的几个对话记录（来自 codex、WorkBuddy 等工具），每个有编号（c 开头）、
来源、最后更新时间、工作目录、标题、用户发了几条消息、用户的第一条和最后一条消息、最后一条回复。都截断过，你看到的只有这些。
找出用户最近在做的事，每件事填一项：
- what：在做什么，一句话。几个记录是同一件事时合成一项。
- stop：停在哪一步。只写从最后一条用户消息和最后一条回复能看出来的；看不出做没做完，写「没看出来有没有做完」，不猜。
- evidence：依据，写对话的标题和原话片段（引号里照抄，不改字）。
- chats：这件事用到的对话编号。
- next：用户点「接着做」后发给你的消息，以用户的口吻写，单独看也明白要做什么：写明目录、做到哪了、接下来做什么。
  看不出做没做完时，写成先核对现状再决定。
编号只填在 chats 里：what、stop、evidence、next 是给用户看的，用户看不到编号，不写 c1、c2 这类编号，提到某个对话时写它的标题。
一次性的提问、闲聊、测试不列。最多列 5 项，最近的在前。没有在做的事时 items 为空。不编造记录里没有的事。`

// Brief 读最近的对话记录，让助理用只读、不留会话的调用列出最近在做的事，结果发到对话里。
// 已经做过时什么都不做；读不到对话记录时也不调模型。
func (e *Engine) Brief(ctx context.Context) error {
	if e.DB.Setting(briefKey, "") != "" {
		return errors.New("已经读过最近的对话记录")
	}
	a, err := e.assistant()
	if err != nil {
		return err
	}
	e.DB.SetSetting(briefKey, "1")
	var skip []string
	if d, err := store.Dir(); err == nil {
		skip = append(skip, d)
	}
	chats := harness.RecentChats(ctx, briefChats, skip...)
	if len(chats) == 0 {
		return nil
	}
	defer e.hold(a.Name, "读最近的对话记录")()
	var out struct {
		Items []struct {
			What     string   `json:"what"`
			Stop     string   `json:"stop"`
			Evidence string   `json:"evidence"`
			Chats    []string `json:"chats"`
			Next     string   `json:"next"`
		} `json:"items"`
	}
	s := spec(a, scratch(), e.role(a)+"\n\n"+briefPrompt(chats), briefSchema)
	s.Access, s.Ephemeral = harness.ReadOnly, true
	if _, err := harness.Ask(ctx, a.Runtime, s, &out, nil); err != nil {
		e.Chat.Emit(chat.Event{"type": "error", "text": "读最近的对话记录没成功：" + err.Error()})
		return err
	}
	byKey := map[string]harness.Chat{}
	for i, c := range chats {
		byKey[fmt.Sprintf("c%d", i+1)] = c
	}
	var b strings.Builder
	var picks []Pick
	for _, it := range out.Items {
		what, next := oneLine(it.What), strings.TrimSpace(it.Next)
		if what == "" || next == "" {
			continue
		}
		var paths []string
		for _, k := range it.Chats {
			if c, ok := byKey[strings.TrimSpace(k)]; ok {
				paths = append(paths, c.Path)
			}
		}
		fmt.Fprintf(&b, "%d. **%s**\n   - 停在：%s\n   - 依据：%s\n", len(picks)+1, what, orDefault(oneLine(it.Stop), "没看出来有没有做完"), oneLine(it.Evidence))
		if len(paths) > 0 {
			next += "\n\n原来的对话记录：" + strings.Join(paths, "、")
		}
		picks = append(picks, Pick{Label: "接着做：" + clip(what, 20), Text: next})
	}
	var text string
	if len(picks) == 0 {
		text = fmt.Sprintf("我读了你本机最近的 %d 个对话记录，没看出正在做、还没做完的事。直接告诉我第一件要做的事就行。", len(chats))
	} else {
		text = fmt.Sprintf("我读了你本机最近的 %d 个对话记录（每个只看了开头和结尾），你最近在做这些：\n\n%s\n点下面的按钮会把接着做的话填进输入框，改完再发。", len(chats), b.String())
	}
	ev := chat.Event{"type": "reply", "text": text, "verdict": "fixed"}
	if len(picks) > 0 {
		ev["picks"] = picks
	}
	e.Chat.Emit(ev)
	return nil
}

// StartBrief 在后台读最近的对话记录。
func (e *Engine) StartBrief() {
	if e.DB.Setting(briefKey, "") != "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		e.Brief(ctx)
	}()
}

func briefPrompt(chats []harness.Chat) string {
	var b strings.Builder
	b.WriteString(briefRules + "\n\n# 对话记录\n")
	for i, c := range chats {
		fmt.Fprintf(&b, "\n## c%d\n来源：%s；更新：%s；目录：%s；用户消息 %d 条\n标题：%s\n第一条：%s\n",
			i+1, c.Source, c.Updated.Format("2006-01-02 15:04"), orDefault(c.Dir, "（不知道）"), c.Users, c.Title, oneLine(c.First))
		if c.Users > 1 {
			fmt.Fprintf(&b, "最后一条：%s\n", oneLine(c.LastUser))
		}
		fmt.Fprintf(&b, "最后的回复：%s\n", orDefault(oneLine(c.LastReply), "（没有）"))
	}
	return b.String()
}
