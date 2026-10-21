package plugin

import "morula/internal/chatgpt"

// ChatGPTID 是 codex 登录的 ChatGPT 订阅在网关里的 provider id，模型写作 codex/<模型>。
// 和「模型来源」里 codex 一行用同一个 id，取消勾选的模型两边一起生效。
const ChatGPTID = "codex"

// chatgptProvider 是 codex 登录的 ChatGPT 订阅。codex 没登录 ChatGPT 账号时不列。
func chatgptProvider() []Provider {
	if !chatgpt.SignedIn() {
		return nil
	}
	p := Provider{ID: ChatGPTID, Name: "ChatGPT", API: chatgpt.Base, SignedIn: true, AuthType: "codex", Builtin: true, Methods: []Method{}}
	for _, m := range chatgpt.Models() {
		p.Models = append(p.Models, Model{ID: m.ID, Name: m.Name, Context: m.Context, Reasoning: len(m.Efforts) > 0, Efforts: m.Efforts})
	}
	if len(p.Models) == 0 {
		return nil
	}
	return []Provider{p}
}

// IsChatGPT 判断 provider 是不是 codex 登录的 ChatGPT 订阅。
func IsChatGPT(p Provider) bool { return p.Builtin && p.ID == ChatGPTID }
