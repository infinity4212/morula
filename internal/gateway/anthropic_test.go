package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestChatToMessages(t *testing.T) {
	b, stream, err := chatToMessages([]byte(`{"model":"glm","stream":true,"reasoning_effort":"high","temperature":0.3,"max_tokens":1000,
		"messages":[{"role":"system","content":"规则"},{"role":"user","content":[{"type":"text","text":"看"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]},
		{"role":"assistant","content":"好","tool_calls":[{"id":"t1","type":"function","function":{"name":"read","arguments":"{\"p\":1}"}},{"id":"t2","type":"function","function":{"name":"ls","arguments":""}}]},
		{"role":"tool","tool_call_id":"t1","content":"一"},{"role":"tool","tool_call_id":"t2","content":"二"},{"role":"user","content":"继续"}],
		"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}],"tool_choice":"required"}`))
	if err != nil || !stream {
		t.Fatal(err, stream)
	}
	var m struct {
		System    string `json:"system"`
		MaxTokens int    `json:"max_tokens"`
		Thinking  map[string]any
		Temp      any `json:"temperature"`
		Messages  []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		}
		Tools      []map[string]any `json:"tools"`
		ToolChoice map[string]any   `json:"tool_choice"`
	}
	json.Unmarshal(b, &m)
	if m.System != "规则" || m.Thinking["budget_tokens"] != float64(16000) || m.MaxTokens != 16000+8192 || m.Temp != nil {
		t.Fatalf("system、thinking 或 max_tokens 不对：%s", b)
	}
	// user、assistant、user（两个 tool_result 和「继续」并成一条）
	if len(m.Messages) != 3 || m.Messages[2].Role != "user" || len(m.Messages[2].Content) != 3 {
		t.Fatalf("消息没有按角色合并：%s", b)
	}
	img := m.Messages[0].Content[1]
	if img["type"] != "image" || img["source"].(map[string]any)["media_type"] != "image/png" || img["source"].(map[string]any)["data"] != "AAA" {
		t.Fatalf("图片不对：%v", img)
	}
	use := m.Messages[1].Content[1]
	if use["type"] != "tool_use" || use["input"].(map[string]any)["p"] != float64(1) || m.Messages[1].Content[2]["input"] == nil {
		t.Fatalf("tool_use 不对：%v", m.Messages[1].Content)
	}
	if m.Messages[2].Content[0]["tool_use_id"] != "t1" || m.Tools[0]["input_schema"] == nil || m.ToolChoice["type"] != "any" {
		t.Fatalf("tool_result、tools 或 tool_choice 不对：%s", b)
	}
}

const messagesSSE = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":7,"cache_read_input_tokens":3,"output_tokens":1}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想"}}

data: {"type":"content_block_start","index":1,"content_block":{"type":"text"}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"你好"}}

data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tu_1","name":"read"}}

data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"p\":"}}

data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"1}"}}

data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}

data: {"type":"message_stop"}

`

func TestMessagesToChatSSE(t *testing.T) {
	var out bytes.Buffer
	messagesToChatSSE(strings.NewReader(messagesSSE), &out, "glm")
	s := &wholeSink{}
	readChatSSE(&out, s)
	c := s.completion("x", 0, "glm")
	msg := c["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls := msg["tool_calls"].([]map[string]any)
	if !s.done || msg["content"] != "你好" || msg["reasoning_content"] != "想" || len(calls) != 1 ||
		calls[0]["id"] != "tu_1" || calls[0]["function"].(map[string]any)["arguments"] != `{"p":1}` {
		t.Fatalf("翻译不对：done=%v %v", s.done, c)
	}
	if s.usage["prompt_tokens"] != float64(10) || s.usage["completion_tokens"] != float64(9) {
		t.Fatalf("usage = %v", s.usage)
	}
	// responses 处理函数用的 readSSE 也读得懂翻好的流
	var w bytes.Buffer
	messagesToChatSSE(strings.NewReader(messagesSSE), &w, "glm")
	if !strings.HasSuffix(w.String(), "data: [DONE]\n\n") {
		t.Fatalf("没有 [DONE]：%s", w.String())
	}
}

func TestMessagesCutOff(t *testing.T) {
	var out bytes.Buffer
	messagesToChatSSE(strings.NewReader(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"半"}}`+"\n\n"), &out, "glm")
	if !strings.Contains(out.String(), "中途断了") {
		t.Fatalf("流断了应报错：%s", out.String())
	}
}

func TestMessageToChat(t *testing.T) {
	b, err := messageToChat([]byte(`{"content":[{"type":"text","text":"好"},{"type":"tool_use","id":"t","name":"ls","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":3}}`), "glm")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Choices []struct {
			Message struct {
				Content   string           `json:"content"`
				ToolCalls []map[string]any `json:"tool_calls"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
	}
	json.Unmarshal(b, &c)
	if c.Choices[0].Message.Content != "好" || len(c.Choices[0].Message.ToolCalls) != 1 || c.Choices[0].Finish != "tool_calls" {
		t.Fatalf("%s", b)
	}
}
