package gateway

// codex 登录的 ChatGPT 订阅（见 internal/chatgpt）。后端只认 Responses，而且只流式：
//   - /v1/responses：请求体按 codex CLI 的样子改写后转发，回复原样流回
//   - /v1/chat/completions（opencode、pi 发这种）：请求翻成 Responses，流式回复翻回 Chat chunk；
//     客户端没要流式时攒成一个 chat.completion 再回
//
// 推理内容只能带回摘要文本，加密的推理内容在 Chat 格式里放不下，下一轮不带回去。

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"morula/internal/chatgpt"
	"morula/internal/plugin"
)

// boundCallID 把超过 64 个字符的工具调用 id 换成它的 SHA-256（后端只收 64 个字符以内的）。同一个 id 每次换出来一样。
func boundCallID(id string) string {
	if len(id) <= 64 {
		return id
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(id)))
	return hex.EncodeToString(sum[:])
}

// chatText 取 Chat 消息 content 里的文本：字符串，或 part 数组里 type 为 text 的。
func chatText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, x := range c {
			if p, ok := x.(map[string]any); ok {
				if s, ok := p["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// chatToResponses 把 Chat Completions 请求翻成 Responses 请求（还没经过 chatgpt.Body 改写）。
func chatToResponses(req map[string]any, model string) map[string]any {
	var system []string
	input := []any{}
	msgs, _ := req["messages"].([]any)
	for _, x := range msgs {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			if t := chatText(m["content"]); strings.TrimSpace(t) != "" {
				system = append(system, t)
			}
		case "user":
			var content []any
			switch c := m["content"].(type) {
			case string:
				content = append(content, map[string]any{"type": "input_text", "text": c})
			case []any:
				for _, y := range c {
					p, _ := y.(map[string]any)
					switch p["type"] {
					case "text":
						content = append(content, map[string]any{"type": "input_text", "text": p["text"]})
					case "image_url":
						url := p["image_url"]
						if u, ok := url.(map[string]any); ok {
							url = u["url"]
						}
						content = append(content, map[string]any{"type": "input_image", "image_url": url})
					}
				}
			}
			if len(content) > 0 {
				input = append(input, map[string]any{"type": "message", "role": "user", "content": content})
			}
		case "assistant":
			if t := chatText(m["content"]); t != "" {
				input = append(input, map[string]any{"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": t}}})
			}
			calls, _ := m["tool_calls"].([]any)
			for _, y := range calls {
				c, _ := y.(map[string]any)
				fn, _ := c["function"].(map[string]any)
				id, _ := c["id"].(string)
				args, _ := fn["arguments"].(string)
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": boundCallID(id), "name": fn["name"], "arguments": args})
			}
		case "tool":
			id, _ := m["tool_call_id"].(string)
			input = append(input, map[string]any{"type": "function_call_output", "call_id": boundCallID(id), "output": chatText(m["content"])})
		}
	}
	out := map[string]any{"model": model, "input": input, "instructions": strings.Join(system, "\n\n")}
	var tools []any
	ts, _ := req["tools"].([]any)
	for _, x := range ts {
		t, _ := x.(map[string]any)
		fn, _ := t["function"].(map[string]any)
		if t["type"] != "function" || fn == nil || fn["name"] == nil {
			continue
		}
		params := fn["parameters"]
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, map[string]any{"type": "function", "name": fn["name"], "description": fn["description"],
			"parameters": params, "strict": false})
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if v, ok := req["parallel_tool_calls"]; ok {
		out["parallel_tool_calls"] = v
	}
	if e, _ := req["reasoning_effort"].(string); e != "" {
		out["reasoning"] = map[string]any{"effort": e}
	}
	return out
}

// sink 收 Responses 流里的内容，写成 Chat 格式。
type sink interface {
	text(d string)
	reasoning(d string)
	callStart(i int, id, name string)
	callArgs(i int, d string)
	finish(usage map[string]any)
	fail(msg string)
}

type chatUsage struct {
	Input  int `json:"input_tokens"`
	Output int `json:"output_tokens"`
	Total  int `json:"total_tokens"`
	InputD *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputD *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u chatUsage) chat() map[string]any {
	total := u.Total
	if total == 0 {
		total = u.Input + u.Output
	}
	m := map[string]any{"prompt_tokens": u.Input, "completion_tokens": u.Output, "total_tokens": total}
	if u.InputD != nil {
		m["prompt_tokens_details"] = map[string]any{"cached_tokens": u.InputD.CachedTokens}
	}
	if u.OutputD != nil {
		m["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.OutputD.ReasoningTokens}
	}
	return m
}

// readResponses 读 ChatGPT 后端的 SSE 流，交给 sink。流在 response.completed 之前断了算失败。
func readResponses(r io.Reader, s sink) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	calls := map[string]int{} // item id → 工具调用序号
	streamed := map[int]bool{}
	reasoned := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var e struct {
			Type    string `json:"type"`
			Delta   string `json:"delta"`
			ItemID  string `json:"item_id"`
			Message string `json:"message"`
			Item    struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
			Response struct {
				Usage *chatUsage `json:"usage"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
				Incomplete *struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &e) != nil {
			continue
		}
		switch e.Type {
		case "response.output_text.delta":
			s.text(e.Delta)
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			s.reasoning(e.Delta)
			reasoned = reasoned || e.Delta != ""
		case "response.reasoning_summary_part.added":
			if reasoned {
				s.reasoning("\n\n") // 摘要分段之间空一行
			}
		case "response.output_item.added":
			if e.Item.Type == "function_call" {
				i := len(calls)
				calls[e.Item.ID] = i
				s.callStart(i, e.Item.CallID, e.Item.Name)
			}
		case "response.function_call_arguments.delta":
			if i, ok := calls[e.ItemID]; ok && e.Delta != "" {
				streamed[i] = true
				s.callArgs(i, e.Delta)
			}
		case "response.output_item.done":
			if i, ok := calls[e.Item.ID]; ok && !streamed[i] {
				args := e.Item.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				s.callArgs(i, args)
			}
		case "response.completed", "response.done":
			var u map[string]any
			if e.Response.Usage != nil {
				u = e.Response.Usage.chat()
			}
			s.finish(u)
			return
		case "response.incomplete":
			msg := "回复没写完"
			if e.Response.Incomplete != nil {
				msg += "：" + e.Response.Incomplete.Reason
			}
			s.fail(msg)
			return
		case "response.failed":
			msg := "ChatGPT 后端回复失败"
			if e.Response.Error != nil {
				msg += "：" + e.Response.Error.Message
			}
			s.fail(msg)
			return
		case "error":
			s.fail("ChatGPT 后端出错：" + e.Message)
			return
		}
	}
	if err := sc.Err(); err != nil {
		s.fail("读 ChatGPT 后端的回复出错：" + err.Error())
		return
	}
	s.fail("ChatGPT 后端的回复中途断了")
}

// streamSink 边收边写 Chat 的 SSE chunk。
type streamSink struct {
	w     http.ResponseWriter
	f     http.Flusher
	id    string
	model string
	at    int64
	calls bool
}

func (s *streamSink) chunk(delta map[string]any, finish any, usage map[string]any) {
	c := map[string]any{"id": s.id, "object": "chat.completion.chunk", "created": s.at, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	if usage != nil {
		c["usage"] = usage
	}
	b, _ := json.Marshal(c)
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	if s.f != nil {
		s.f.Flush()
	}
}

func (s *streamSink) text(d string) {
	if d != "" {
		s.chunk(map[string]any{"content": d}, nil, nil)
	}
}

func (s *streamSink) reasoning(d string) {
	if d != "" {
		s.chunk(map[string]any{"reasoning_content": d}, nil, nil)
	}
}

func (s *streamSink) callStart(i int, id, name string) {
	s.calls = true
	s.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": ""}}}}, nil, nil)
}

func (s *streamSink) callArgs(i int, d string) {
	s.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": i, "function": map[string]any{"arguments": d}}}}, nil, nil)
}

func (s *streamSink) finish(usage map[string]any) {
	reason := "stop"
	if s.calls {
		reason = "tool_calls"
	}
	s.chunk(map[string]any{}, reason, usage)
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	if s.f != nil {
		s.f.Flush()
	}
}

func (s *streamSink) fail(msg string) {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "morula_gateway"}})
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	if s.f != nil {
		s.f.Flush()
	}
}

// wholeSink 攒成一个 chat.completion，给没要流式的客户端。
type wholeSink struct {
	content, reason strings.Builder
	calls           []map[string]any
	usage           map[string]any
	err             string
	done            bool
}

func (s *wholeSink) text(d string)      { s.content.WriteString(d) }
func (s *wholeSink) reasoning(d string) { s.reason.WriteString(d) }
func (s *wholeSink) callStart(i int, id, name string) {
	for len(s.calls) <= i {
		s.calls = append(s.calls, map[string]any{"type": "function", "function": map[string]any{"name": "", "arguments": ""}})
	}
	s.calls[i]["id"] = id
	s.calls[i]["function"].(map[string]any)["name"] = name
}
func (s *wholeSink) callArgs(i int, d string) {
	if i < len(s.calls) {
		fn := s.calls[i]["function"].(map[string]any)
		fn["arguments"] = fn["arguments"].(string) + d
	}
}
func (s *wholeSink) finish(usage map[string]any) { s.usage, s.done = usage, true }
func (s *wholeSink) fail(msg string)             { s.err = msg }

// chatgptChat 处理发给 ChatGPT 订阅的 Chat Completions 请求。
func chatgptChat(w http.ResponseWriter, r *http.Request, ref plugin.ModelRef, body []byte) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var req map[string]any
	if dec.Decode(&req) != nil {
		apiError(w, http.StatusBadRequest, "请求不是 JSON")
		return
	}
	stream, _ := req["stream"].(bool)
	res, err := chatgpt.Do(r.Context(), chatgpt.Body(chatToResponses(req, ref.Model.ID)))
	if err != nil {
		apiError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		upstreamError(w, res)
		return
	}
	id, at := "chatcmpl-"+rid(), time.Now().Unix()
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		readResponses(res.Body, &streamSink{w: w, f: f, id: id, model: ref.ID(), at: at})
		return
	}
	s := &wholeSink{}
	readResponses(res.Body, s)
	if !s.done {
		apiError(w, http.StatusBadGateway, s.err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.completion(id, at, ref.ID()))
}

// completion 是攒好的 chat.completion。
func (s *wholeSink) completion(id string, at int64, model string) map[string]any {
	msg := map[string]any{"role": "assistant", "content": s.content.String()}
	if s.reason.Len() > 0 {
		msg["reasoning_content"] = s.reason.String()
	}
	finish := "stop"
	if len(s.calls) > 0 {
		msg["tool_calls"] = s.calls
		finish = "tool_calls"
	}
	out := map[string]any{"id": id, "object": "chat.completion", "created": at, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}}
	if s.usage != nil {
		out["usage"] = s.usage
	}
	return out
}

// chatgptResponses 处理发给 ChatGPT 订阅的 Responses 请求：改写后转发，回复原样流回。
func chatgptResponses(w http.ResponseWriter, r *http.Request, ref plugin.ModelRef, body []byte) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var req map[string]any
	if dec.Decode(&req) != nil {
		apiError(w, http.StatusBadRequest, "请求不是 JSON")
		return
	}
	req["model"] = ref.Model.ID
	res, err := chatgpt.Do(r.Context(), chatgpt.Body(req))
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
	copyFlush(w, res.Body)
}
