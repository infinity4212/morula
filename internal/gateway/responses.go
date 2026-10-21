package gateway

// Responses ↔ Chat Completions 转换。codex 只发 Responses 请求（POST /v1/responses，流式），
// 插件给的上游大多只有 Chat Completions，所以在这里两头翻译。
//
// 请求方向：
//   - instructions 和所有 system/developer 消息并成开头的一条 system 消息
//   - message 的 input_text/output_text 变成文本，input_image 变成 image_url
//   - function_call 变成 assistant 的 tool_calls（连续的并进同一条，接在前面的 assistant 文本后）
//   - function_call_output 变成 tool 消息
//   - custom 工具（例如 apply_patch 的自由格式）变成只有一个 input 字符串参数的 function 工具，回来时还原
//   - reasoning 的 summary 文本放进下一条 assistant 消息的 reasoning_content
//   - 不认识的工具类型（web_search、local_shell 等）跳过
//
// 回复方向：把 Chat 的流式 chunk 翻成 response.created / output_item.added / output_text.delta /
// reasoning_summary_text.delta / output_item.done / response.completed。

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"morula/internal/plugin"
)

type respReq struct {
	Model             string            `json:"model"`
	Instructions      string            `json:"instructions"`
	Input             json.RawMessage   `json:"input"`
	Tools             []json.RawMessage `json:"tools"`
	ToolChoice        json.RawMessage   `json:"tool_choice"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls"`
	Reasoning         *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	MaxOutputTokens *int     `json:"max_output_tokens"`
	Temperature     *float64 `json:"temperature"`
	TopP            *float64 `json:"top_p"`
	Stream          bool     `json:"stream"`
}

type item struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Summary   []struct {
		Text string `json:"text"`
	} `json:"summary"`
}

type part struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL any    `json:"image_url"`
}

type chatMsg struct {
	Role             string     `json:"role"`
	Content          any        `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []chatCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

type chatCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// parts 把 Responses 的 content（字符串或 part 数组）拆成文本和图片。
func parts(raw json.RawMessage) (text string, images []any) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var ps []part
	json.Unmarshal(raw, &ps)
	var b strings.Builder
	for _, p := range ps {
		switch p.Type {
		case "input_text", "output_text", "text", "summary_text", "refusal":
			b.WriteString(p.Text)
		case "input_image":
			if p.ImageURL != nil {
				images = append(images, p.ImageURL)
			}
		}
	}
	return b.String(), images
}

// toChat 把 Responses 请求翻成 Chat Completions 请求。custom 返回 custom 工具的名字。
func toChat(r respReq, model string) (map[string]any, map[string]bool, error) {
	var items []item
	var s string
	if json.Unmarshal(r.Input, &s) == nil {
		items = []item{{Type: "message", Role: "user", Content: r.Input}}
	} else if err := json.Unmarshal(r.Input, &items); err != nil {
		return nil, nil, fmt.Errorf("input 读不懂：%w", err)
	}

	var system []string
	if strings.TrimSpace(r.Instructions) != "" {
		system = append(system, r.Instructions)
	}
	var msgs []chatMsg
	reasoning := ""
	lastAssistant := func() *chatMsg {
		if n := len(msgs); n > 0 && msgs[n-1].Role == "assistant" {
			return &msgs[n-1]
		}
		return nil
	}
	addCall := func(id, name, args string) {
		c := chatCall{ID: id, Type: "function"}
		c.Function.Name, c.Function.Arguments = name, args
		a := lastAssistant()
		if a == nil {
			msgs = append(msgs, chatMsg{Role: "assistant", Content: nil})
			a = &msgs[len(msgs)-1]
		}
		if reasoning != "" {
			a.ReasoningContent += reasoning
			reasoning = ""
		}
		a.ToolCalls = append(a.ToolCalls, c)
	}
	for _, it := range items {
		if it.Type == "" && it.Role != "" {
			it.Type = "message"
		}
		switch it.Type {
		case "message":
			text, images := parts(it.Content)
			switch it.Role {
			case "system", "developer":
				if strings.TrimSpace(text) != "" {
					system = append(system, text)
				}
			case "assistant":
				m := chatMsg{Role: "assistant", Content: text, ReasoningContent: reasoning}
				reasoning = ""
				msgs = append(msgs, m)
			default:
				if len(images) == 0 {
					msgs = append(msgs, chatMsg{Role: "user", Content: text})
					continue
				}
				content := []map[string]any{}
				if text != "" {
					content = append(content, map[string]any{"type": "text", "text": text})
				}
				for _, img := range images {
					url := img
					if m, ok := img.(map[string]any); ok {
						url = m["url"]
					}
					content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
				msgs = append(msgs, chatMsg{Role: "user", Content: content})
			}
		case "function_call":
			addCall(it.CallID, it.Name, it.Arguments)
		case "custom_tool_call":
			args, _ := json.Marshal(map[string]string{"input": it.Input})
			addCall(it.CallID, it.Name, string(args))
		case "function_call_output", "custom_tool_call_output":
			text, _ := parts(it.Output)
			msgs = append(msgs, chatMsg{Role: "tool", ToolCallID: it.CallID, Content: text})
		case "reasoning":
			for _, sm := range it.Summary {
				reasoning += sm.Text
			}
		}
	}
	if len(system) > 0 {
		msgs = append([]chatMsg{{Role: "system", Content: strings.Join(system, "\n\n")}}, msgs...)
	}

	custom := map[string]bool{}
	var tools []map[string]any
	for _, raw := range r.Tools {
		var t struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      *bool           `json:"strict"`
			Format      *struct {
				Definition string `json:"definition"`
			} `json:"format"`
		}
		if json.Unmarshal(raw, &t) != nil || t.Name == "" {
			continue
		}
		switch t.Type {
		case "function":
			fn := map[string]any{"name": t.Name, "description": t.Description}
			if len(t.Parameters) > 0 && string(t.Parameters) != "null" {
				fn["parameters"] = t.Parameters
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		case "custom":
			custom[t.Name] = true
			desc := t.Description
			if t.Format != nil && t.Format.Definition != "" {
				desc += "\n\ninput 必须符合这个语法：\n" + t.Format.Definition
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": desc,
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"input": map[string]any{"type": "string", "description": "工具的原始输入"}},
					"required":   []string{"input"},
				},
			}})
		}
	}

	out := map[string]any{
		"model":          model,
		"messages":       msgs,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if len(tools) > 0 {
		out["tools"] = tools
		if len(r.ToolChoice) > 0 {
			var tc string
			if json.Unmarshal(r.ToolChoice, &tc) == nil {
				out["tool_choice"] = tc
			}
		}
		if r.ParallelToolCalls != nil {
			out["parallel_tool_calls"] = *r.ParallelToolCalls
		}
	}
	if r.Reasoning != nil && r.Reasoning.Effort != "" {
		out["reasoning_effort"] = r.Reasoning.Effort
	}
	if r.MaxOutputTokens != nil {
		out["max_tokens"] = *r.MaxOutputTokens
	}
	if r.Temperature != nil {
		out["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		out["top_p"] = *r.TopP
	}
	return out, custom, nil
}

func (g *Gateway) responses(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req respReq
	if err := json.Unmarshal(body, &req); err != nil {
		apiError(w, http.StatusBadRequest, "请求不是 JSON")
		return
	}
	if !req.Stream {
		apiError(w, http.StatusBadRequest, "/v1/responses 只支持 stream: true")
		return
	}
	ref, found := plugin.Find(r.Context(), req.Model)
	if !found {
		apiError(w, http.StatusNotFound, "没有这个模型，或它的 provider 还没登录："+req.Model)
		return
	}
	if plugin.IsChatGPT(ref.Provider) {
		chatgptResponses(w, r, ref, body)
		return
	}
	chat, custom, err := toChat(req, ref.Model.ID)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	cb, _ := json.Marshal(chat)
	res, err := send(r.Context(), ref, cb)
	if err != nil {
		apiError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		upstreamError(w, res)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	s := newStream(w, req.Model, custom)
	s.created()
	if strings.Contains(res.Header.Get("Content-Type"), "event-stream") {
		err = s.readSSE(res.Body)
	} else {
		err = s.readJSON(res.Body)
	}
	if err != nil {
		s.failed(err.Error())
		return
	}
	s.completed()
}

// stream 把 Chat chunk 翻成 Responses 事件写给客户端。
type stream struct {
	w      http.ResponseWriter
	f      http.Flusher
	id     string
	model  string
	custom map[string]bool
	at     int64
	seq    int
	index  int // 下一个 output item 的序号

	open    string // 正在写的项："message"、"reasoning" 或空
	openID  string
	openIdx int
	text    strings.Builder
	calls   map[int]*chatCall
	output  []any
	usage   map[string]any
}

func newStream(w http.ResponseWriter, model string, custom map[string]bool) *stream {
	f, _ := w.(http.Flusher)
	return &stream{w: w, f: f, id: "resp_" + rid(), model: model, custom: custom, at: time.Now().Unix(), calls: map[int]*chatCall{}}
}

func rid() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *stream) emit(typ string, v map[string]any) {
	v["type"] = typ
	v["sequence_number"] = s.seq
	s.seq++
	b, _ := json.Marshal(v)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", typ, b)
	if s.f != nil {
		s.f.Flush()
	}
}

func (s *stream) resp(status string) map[string]any {
	r := map[string]any{"id": s.id, "object": "response", "created_at": s.at, "model": s.model, "status": status, "output": s.output}
	if s.output == nil {
		r["output"] = []any{}
	}
	if s.usage != nil {
		r["usage"] = s.usage
	}
	return r
}

func (s *stream) created() {
	s.emit("response.created", map[string]any{"response": s.resp("in_progress")})
}

// begin 开一个文本或推理项；已经开着同类的就接着写。
func (s *stream) begin(kind string) {
	if s.open == kind {
		return
	}
	s.end()
	s.open, s.openIdx = kind, s.index
	s.index++
	s.text.Reset()
	if kind == "message" {
		s.openID = "msg_" + rid()
		s.emit("response.output_item.added", map[string]any{"output_index": s.openIdx, "item": map[string]any{
			"type": "message", "id": s.openID, "role": "assistant", "status": "in_progress", "content": []any{}}})
		s.emit("response.content_part.added", map[string]any{"item_id": s.openID, "output_index": s.openIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
	} else {
		s.openID = "rs_" + rid()
		s.emit("response.output_item.added", map[string]any{"output_index": s.openIdx, "item": map[string]any{
			"type": "reasoning", "id": s.openID, "summary": []any{}}})
		s.emit("response.reasoning_summary_part.added", map[string]any{"item_id": s.openID, "output_index": s.openIdx, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""}})
	}
}

func (s *stream) delta(kind, d string) {
	if d == "" {
		return
	}
	s.begin(kind)
	s.text.WriteString(d)
	if kind == "message" {
		s.emit("response.output_text.delta", map[string]any{"item_id": s.openID, "output_index": s.openIdx, "content_index": 0, "delta": d})
	} else {
		s.emit("response.reasoning_summary_text.delta", map[string]any{"item_id": s.openID, "output_index": s.openIdx, "summary_index": 0, "delta": d})
	}
}

// end 收掉正在写的文本或推理项。
func (s *stream) end() {
	if s.open == "" {
		return
	}
	text := s.text.String()
	var it map[string]any
	if s.open == "message" {
		p := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		s.emit("response.output_text.done", map[string]any{"item_id": s.openID, "output_index": s.openIdx, "content_index": 0, "text": text})
		s.emit("response.content_part.done", map[string]any{"item_id": s.openID, "output_index": s.openIdx, "content_index": 0, "part": p})
		it = map[string]any{"type": "message", "id": s.openID, "role": "assistant", "status": "completed", "content": []any{p}}
	} else {
		p := map[string]any{"type": "summary_text", "text": text}
		s.emit("response.reasoning_summary_text.done", map[string]any{"item_id": s.openID, "output_index": s.openIdx, "summary_index": 0, "text": text})
		s.emit("response.reasoning_summary_part.done", map[string]any{"item_id": s.openID, "output_index": s.openIdx, "summary_index": 0, "part": p})
		it = map[string]any{"type": "reasoning", "id": s.openID, "summary": []any{p}}
	}
	s.emit("response.output_item.done", map[string]any{"output_index": s.openIdx, "item": it})
	s.output = append(s.output, it)
	s.open = ""
}

// flushCalls 把攒好的工具调用作为 function_call 或 custom_tool_call 项发出去。
func (s *stream) flushCalls() {
	if len(s.calls) == 0 {
		return
	}
	s.end()
	keys := make([]int, 0, len(s.calls))
	for k := range s.calls {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		c := s.calls[k]
		if c.ID == "" {
			c.ID = "call_" + rid()
		}
		var it map[string]any
		if s.custom[c.Function.Name] {
			var a struct {
				Input string `json:"input"`
			}
			input := c.Function.Arguments
			if json.Unmarshal([]byte(input), &a) == nil {
				input = a.Input
			}
			it = map[string]any{"type": "custom_tool_call", "id": "ctc_" + rid(), "call_id": c.ID, "name": c.Function.Name, "input": input, "status": "completed"}
		} else {
			args := c.Function.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			it = map[string]any{"type": "function_call", "id": "fc_" + rid(), "call_id": c.ID, "name": c.Function.Name, "arguments": args, "status": "completed"}
		}
		idx := s.index
		s.index++
		added := map[string]any{}
		for k, v := range it {
			added[k] = v
		}
		added["status"] = "in_progress"
		s.emit("response.output_item.added", map[string]any{"output_index": idx, "item": added})
		s.emit("response.output_item.done", map[string]any{"output_index": idx, "item": it})
		s.output = append(s.output, it)
	}
	s.calls = map[int]*chatCall{}
}

type chunk struct {
	Choices []struct {
		Delta   *chunkMsg `json:"delta"`
		Message *chunkMsg `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type chunkMsg struct {
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content"`
	Reasoning        string     `json:"reasoning"`
	ToolCalls        []chatCall `json:"tool_calls"`
}

func (s *stream) take(c chunk) error {
	if c.Error != nil {
		return fmt.Errorf("上游出错：%s", c.Error.Message)
	}
	for _, ch := range c.Choices {
		m := ch.Delta
		if m == nil {
			m = ch.Message
		}
		if m == nil {
			continue
		}
		s.delta("reasoning", m.ReasoningContent+m.Reasoning)
		s.delta("message", m.Content)
		for i, tc := range m.ToolCalls {
			k := i
			if tc.Index != nil {
				k = *tc.Index
			}
			cur := s.calls[k]
			if cur == nil {
				cur = &chatCall{}
				s.calls[k] = cur
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Function.Name != "" {
				cur.Function.Name = tc.Function.Name
			}
			cur.Function.Arguments += tc.Function.Arguments
		}
	}
	if u := c.Usage; u != nil {
		total := u.TotalTokens
		if total == 0 {
			total = u.PromptTokens + u.CompletionTokens
		}
		s.usage = map[string]any{"input_tokens": u.PromptTokens, "output_tokens": u.CompletionTokens, "total_tokens": total}
		if u.PromptTokensDetails != nil {
			s.usage["input_tokens_details"] = map[string]any{"cached_tokens": u.PromptTokensDetails.CachedTokens}
		}
		if u.CompletionTokensDetails != nil {
			s.usage["output_tokens_details"] = map[string]any{"reasoning_tokens": u.CompletionTokensDetails.ReasoningTokens}
		}
	}
	return nil
}

func (s *stream) readSSE(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var c chunk
		if json.Unmarshal([]byte(data), &c) != nil {
			continue
		}
		if err := s.take(c); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *stream) readJSON(r io.Reader) error {
	var c chunk
	if err := json.NewDecoder(r).Decode(&c); err != nil {
		return fmt.Errorf("上游回复读不懂：%w", err)
	}
	return s.take(c)
}

func (s *stream) completed() {
	s.end()
	s.flushCalls()
	s.emit("response.completed", map[string]any{"response": s.resp("completed")})
}

func (s *stream) failed(msg string) {
	s.end()
	r := s.resp("failed")
	r["error"] = map[string]any{"code": "upstream_error", "message": msg}
	s.emit("response.failed", map[string]any{"response": r})
}
