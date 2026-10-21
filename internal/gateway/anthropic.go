package gateway

// 只讲 Anthropic Messages 的插件 provider（配置里 npm 是 @ai-sdk/anthropic，例如 ZCode）。
// 网关收到的 Chat Completions 请求在这里翻成 Messages 请求，发到插件 fetch 的 /messages；
// 回复再翻回 Chat：流式的边读边翻成 chunk，非流式的翻成一个 chat.completion。
// 翻好的回复交给 chat 和 responses 两个处理函数，和讲 Chat 的插件走同一条路。
//
// 推理内容只带回文本，Anthropic 的 thinking 签名在 Chat 格式里放不下，下一轮不带回去。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"morula/internal/plugin"
)

func isAnthropic(p plugin.Provider) bool { return !p.Builtin && p.NPM == "@ai-sdk/anthropic" }

// 思考强度对应的 thinking 预算，和 ZCode 插件给 @ai-sdk/anthropic 的一样。
var thinkBudget = map[string]int{"minimal": 1024, "low": 4096, "medium": 8192, "high": 16000, "xhigh": 24000, "max": 31999}

// imageBlock 把 Chat 的 image_url（data URL 或网址）翻成 Messages 的 image 块。
func imageBlock(url string) map[string]any {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		meta, data, _ := strings.Cut(rest, ",")
		media, _, _ := strings.Cut(meta, ";")
		return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": media, "data": data}}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}}
}

// chatToMessages 把 Chat Completions 请求翻成 Messages 请求，返回请求体和客户端要不要流式。
func chatToMessages(body []byte) ([]byte, bool, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var req map[string]any
	if err := dec.Decode(&req); err != nil {
		return nil, false, err
	}
	stream, _ := req["stream"].(bool)
	var system []string
	var msgs []map[string]any
	// add 把内容块接到同一角色的上一条后面：Messages 要求 user、assistant 轮流
	add := func(role string, blocks ...map[string]any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(msgs); n > 0 && msgs[n-1]["role"] == role {
			msgs[n-1]["content"] = append(msgs[n-1]["content"].([]map[string]any), blocks...)
			return
		}
		msgs = append(msgs, map[string]any{"role": role, "content": blocks})
	}
	list, _ := req["messages"].([]any)
	for _, x := range list {
		m, _ := x.(map[string]any)
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			if t := chatText(m["content"]); strings.TrimSpace(t) != "" {
				system = append(system, t)
			}
		case "user":
			var blocks []map[string]any
			switch c := m["content"].(type) {
			case string:
				if c != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": c})
				}
			case []any:
				for _, y := range c {
					p, _ := y.(map[string]any)
					switch p["type"] {
					case "text":
						if s, _ := p["text"].(string); s != "" {
							blocks = append(blocks, map[string]any{"type": "text", "text": s})
						}
					case "image_url":
						url, _ := p["image_url"].(string)
						if u, ok := p["image_url"].(map[string]any); ok {
							url, _ = u["url"].(string)
						}
						if url != "" {
							blocks = append(blocks, imageBlock(url))
						}
					}
				}
			}
			add("user", blocks...)
		case "assistant":
			var blocks []map[string]any
			if t := chatText(m["content"]); t != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": t})
			}
			calls, _ := m["tool_calls"].([]any)
			for _, y := range calls {
				c, _ := y.(map[string]any)
				fn, _ := c["function"].(map[string]any)
				args, _ := fn["arguments"].(string)
				var input any = map[string]any{}
				if strings.TrimSpace(args) != "" {
					json.Unmarshal([]byte(args), &input)
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": c["id"], "name": fn["name"], "input": input})
			}
			add("assistant", blocks...)
		case "tool":
			add("user", map[string]any{"type": "tool_result", "tool_use_id": m["tool_call_id"], "content": chatText(m["content"])})
		}
	}
	out := map[string]any{"model": req["model"], "messages": msgs, "stream": stream}
	if msgs == nil {
		out["messages"] = []any{}
	}
	if len(system) > 0 {
		out["system"] = strings.Join(system, "\n\n")
	}
	maxTok := 32000
	for _, k := range []string{"max_completion_tokens", "max_tokens"} {
		if n, ok := req[k].(json.Number); ok {
			if v, err := n.Int64(); err == nil && v > 0 {
				maxTok = int(v)
			}
		}
	}
	if e, _ := req["reasoning_effort"].(string); e != "" {
		if e == "none" {
			out["thinking"] = map[string]any{"type": "disabled"}
		} else {
			b := thinkBudget[e]
			if b == 0 {
				b = 16000
			}
			out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": b}
			maxTok = max(maxTok, b+8192) // max_tokens 要比 thinking 预算大
		}
	}
	out["max_tokens"] = maxTok
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := req[k]; ok && out["thinking"] == nil {
			out[k] = v // 开了 thinking 时 Anthropic 不收这两个
		}
	}
	switch s := req["stop"].(type) {
	case string:
		out["stop_sequences"] = []string{s}
	case []any:
		out["stop_sequences"] = s
	}
	var tools []map[string]any
	ts, _ := req["tools"].([]any)
	for _, x := range ts {
		t, _ := x.(map[string]any)
		fn, _ := t["function"].(map[string]any)
		if t["type"] != "function" || fn == nil || fn["name"] == nil {
			continue
		}
		schema := fn["parameters"]
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, map[string]any{"name": fn["name"], "description": fn["description"], "input_schema": schema})
	}
	if len(tools) > 0 {
		out["tools"] = tools
		switch tc := req["tool_choice"].(type) {
		case string:
			out["tool_choice"] = map[string]any{"type": map[string]string{"required": "any", "none": "none"}[tc]}
			if tc != "required" && tc != "none" {
				out["tool_choice"] = map[string]any{"type": "auto"}
			}
		case map[string]any:
			if fn, ok := tc["function"].(map[string]any); ok {
				out["tool_choice"] = map[string]any{"type": "tool", "name": fn["name"]}
			}
		}
	}
	b, err := json.Marshal(out)
	return b, stream, err
}

var stopReason = map[string]string{"end_turn": "stop", "stop_sequence": "stop", "tool_use": "tool_calls", "max_tokens": "length", "refusal": "content_filter"}

type msgUsage struct {
	Input       int `json:"input_tokens"`
	Output      int `json:"output_tokens"`
	CacheRead   int `json:"cache_read_input_tokens"`
	CacheCreate int `json:"cache_creation_input_tokens"`
}

func (u msgUsage) chat() map[string]any {
	in := u.Input + u.CacheRead + u.CacheCreate
	return map[string]any{"prompt_tokens": in, "completion_tokens": u.Output, "total_tokens": in + u.Output,
		"prompt_tokens_details": map[string]any{"cached_tokens": u.CacheRead}}
}

// messagesToChatSSE 把 Messages 的 SSE 流翻成 Chat 的 SSE 流写进 w。
func messagesToChatSSE(r io.Reader, w io.Writer, model string) {
	id, at := "chatcmpl-"+rid(), time.Now().Unix()
	write := func(delta map[string]any, finish any, usage map[string]any) {
		c := map[string]any{"id": id, "object": "chat.completion.chunk", "created": at, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage != nil {
			c["usage"] = usage
		}
		b, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	fail := func(msg string) {
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "morula_gateway"}})
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	tools := map[int]int{} // 内容块序号 → 工具调用序号
	var usage msgUsage
	finish := ""
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var e struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage msgUsage `json:"usage"`
			} `json:"message"`
			Block struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				Thinking   string `json:"thinking"`
				JSON       string `json:"partial_json"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage *msgUsage `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &e) != nil {
			continue
		}
		switch e.Type {
		case "message_start":
			usage = e.Message.Usage
		case "content_block_start":
			if e.Block.Type == "tool_use" {
				i := len(tools)
				tools[e.Index] = i
				write(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": e.Block.ID, "type": "function",
					"function": map[string]any{"name": e.Block.Name, "arguments": ""}}}}, nil, nil)
			}
		case "content_block_delta":
			switch e.Delta.Type {
			case "text_delta":
				if e.Delta.Text != "" {
					write(map[string]any{"content": e.Delta.Text}, nil, nil)
				}
			case "thinking_delta":
				if e.Delta.Thinking != "" {
					write(map[string]any{"reasoning_content": e.Delta.Thinking}, nil, nil)
				}
			case "input_json_delta":
				if i, ok := tools[e.Index]; ok && e.Delta.JSON != "" {
					write(map[string]any{"tool_calls": []any{map[string]any{"index": i, "function": map[string]any{"arguments": e.Delta.JSON}}}}, nil, nil)
				}
			}
		case "message_delta":
			if e.Delta.StopReason != "" {
				finish = stopReason[e.Delta.StopReason]
				if finish == "" {
					finish = "stop"
				}
			}
			if e.Usage != nil {
				if e.Usage.Output > 0 {
					usage.Output = e.Usage.Output
				}
				if e.Usage.Input > 0 {
					usage.Input = e.Usage.Input
				}
			}
		case "message_stop":
			if finish == "" {
				finish = "stop"
			}
			write(map[string]any{}, finish, usage.chat())
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		case "error":
			fail("上游出错：" + e.Error.Message)
			return
		}
	}
	if err := sc.Err(); err != nil {
		fail("读上游回复出错：" + err.Error())
		return
	}
	fail("上游的回复中途断了")
}

// messageToChat 把非流式的 Messages 回复翻成 chat.completion。
func messageToChat(b []byte, model string) ([]byte, error) {
	var m struct {
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string   `json:"stop_reason"`
		Usage      msgUsage `json:"usage"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("上游回复读不懂：%w", err)
	}
	var text, think strings.Builder
	var calls []map[string]any
	for _, c := range m.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "thinking":
			think.WriteString(c.Thinking)
		case "tool_use":
			args := string(c.Input)
			if args == "" || args == "null" {
				args = "{}"
			}
			calls = append(calls, map[string]any{"id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": args}})
		}
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	if think.Len() > 0 {
		msg["reasoning_content"] = think.String()
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	finish := stopReason[m.StopReason]
	if finish == "" {
		finish = "stop"
	}
	return json.Marshal(map[string]any{"id": "chatcmpl-" + rid(), "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}, "usage": m.Usage.chat()})
}

// sendMessages 把 Chat 请求翻成 Messages 发给插件，回复翻回 Chat 格式。
func sendMessages(ctx context.Context, h *plugin.Host, ref plugin.ModelRef, chat []byte) (*http.Response, error) {
	body, stream, err := chatToMessages(chat)
	if err != nil {
		return nil, err
	}
	hd := http.Header{}
	hd.Set("Content-Type", "application/json")
	hd.Set("Accept", "text/event-stream, application/json")
	hd.Set("anthropic-version", "2023-06-01")
	res, err := h.Fetch(ctx, plugin.Request{Provider: ref.Provider.ID, Method: "POST", Path: "/messages", Header: hd, Body: body})
	if err != nil || res.StatusCode >= 400 {
		return res, err
	}
	model := ref.Model.ID
	if !strings.Contains(res.Header.Get("Content-Type"), "event-stream") {
		// 上游没按流式回：读完整个回复再翻
		b, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		out, err := messageToChat(b, model)
		if err != nil {
			return nil, err
		}
		return chatResponse(out, "application/json"), nil
	}
	pr, pw := io.Pipe()
	up := res.Body
	go func() {
		defer up.Close()
		messagesToChatSSE(up, pw, model)
		pw.Close()
	}()
	if stream {
		res.Body = pr
		res.Header = http.Header{"Content-Type": {"text/event-stream"}}
		res.ContentLength = -1
		return res, nil
	}
	// 客户端没要流式、上游却回了流：把翻好的 Chat 流攒成一个 chat.completion
	whole := &wholeSink{}
	readChatSSE(pr, whole)
	pr.Close()
	if !whole.done {
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": whole.err, "type": "morula_gateway"}})
		return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(bytes.NewReader(b))}, nil
	}
	out, _ := json.Marshal(whole.completion("chatcmpl-"+rid(), time.Now().Unix(), model))
	return chatResponse(out, "application/json"), nil
}

func chatResponse(b []byte, ct string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}, "Content-Length": {strconv.Itoa(len(b))}},
		Body: io.NopCloser(bytes.NewReader(b)), ContentLength: int64(len(b))}
}

// readChatSSE 读 Chat 的 SSE 流交给 wholeSink。
func readChatSSE(r io.Reader, s *wholeSink) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		d, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "data:")
		if !ok {
			continue
		}
		d = strings.TrimSpace(d)
		if d == "[DONE]" {
			break
		}
		var c struct {
			Choices []struct {
				Delta  *chunkMsg `json:"delta"`
				Finish string    `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(d), &c) != nil {
			continue
		}
		if c.Error != nil {
			s.fail(c.Error.Message)
			return
		}
		for _, ch := range c.Choices {
			if ch.Delta != nil {
				s.reasoning(ch.Delta.ReasoningContent)
				s.text(ch.Delta.Content)
				for _, tc := range ch.Delta.ToolCalls {
					i := 0
					if tc.Index != nil {
						i = *tc.Index
					}
					if tc.ID != "" || tc.Function.Name != "" {
						s.callStart(i, tc.ID, tc.Function.Name)
					}
					s.callArgs(i, tc.Function.Arguments)
				}
			}
			if ch.Finish != "" {
				s.finish(c.Usage)
			}
		}
	}
	if !s.done && s.err == "" {
		s.fail("上游的回复中途断了")
	}
}
