package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestToChat(t *testing.T) {
	in := `{"model":"workbuddy/m","instructions":"sys","stream":true,
	"reasoning":{"effort":"high"},"tool_choice":"auto","parallel_tool_calls":false,
	"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}},
	         {"type":"custom","name":"apply_patch","format":{"definition":"G"}},
	         {"type":"web_search"}],
	"input":[
	 {"type":"message","role":"developer","content":[{"type":"input_text","text":"dev"}]},
	 {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
	 {"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},
	 {"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
	 {"type":"custom_tool_call","call_id":"c2","name":"apply_patch","input":"*** Begin"},
	 {"type":"function_call_output","call_id":"c1","output":"a.txt"},
	 {"type":"custom_tool_call_output","call_id":"c2","output":"ok"},
	 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}
	]}`
	var r respReq
	if err := json.Unmarshal([]byte(in), &r); err != nil {
		t.Fatal(err)
	}
	out, custom, err := toChat(r, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !custom["apply_patch"] {
		t.Error("apply_patch 应记为 custom 工具")
	}
	msgs := out["messages"].([]chatMsg)
	if len(msgs) != 6 {
		t.Fatalf("消息数 %d，应为 6：%+v", len(msgs), msgs)
	}
	if msgs[0].Role != "system" || msgs[0].Content != "sys\n\ndev" {
		t.Errorf("system 消息不对：%+v", msgs[0])
	}
	a := msgs[2]
	if a.Role != "assistant" || len(a.ToolCalls) != 2 || a.ReasoningContent != "think" {
		t.Errorf("assistant 工具调用不对：%+v", a)
	}
	if a.ToolCalls[1].Function.Arguments != `{"input":"*** Begin"}` {
		t.Errorf("custom 调用参数不对：%s", a.ToolCalls[1].Function.Arguments)
	}
	if msgs[3].Role != "tool" || msgs[3].ToolCallID != "c1" || msgs[3].Content != "a.txt" {
		t.Errorf("tool 消息不对：%+v", msgs[3])
	}
	if n := len(out["tools"].([]map[string]any)); n != 2 {
		t.Errorf("工具数 %d，应为 2（web_search 跳过）", n)
	}
	if out["reasoning_effort"] != "high" || out["tool_choice"] != "auto" || out["parallel_tool_calls"] != false {
		t.Errorf("参数没带上：%v %v %v", out["reasoning_effort"], out["tool_choice"], out["parallel_tool_calls"])
	}
}

func TestStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"想"}}]}`,
		`data: {"choices":[{"delta":{"content":"你"}}]}`,
		`data: {"choices":[{"delta":{"content":"好"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"shell","arguments":"{\"c"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"md\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"apply_patch","arguments":"{\"input\":\"P\"}"}}]}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
	}, "\n\n")
	w := httptest.NewRecorder()
	s := newStream(w, "workbuddy/m", map[string]bool{"apply_patch": true})
	s.created()
	if err := s.readSSE(strings.NewReader(sse)); err != nil {
		t.Fatal(err)
	}
	s.completed()

	var done []map[string]any
	var last map[string]any
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e map[string]any
		json.Unmarshal([]byte(line[6:]), &e)
		if e["type"] == "response.output_item.done" {
			done = append(done, e["item"].(map[string]any))
		}
		last = e
	}
	if len(done) != 4 {
		t.Fatalf("完成的项 %d 个，应为 4（推理、文本、两个工具调用）：%v", len(done), done)
	}
	if done[0]["type"] != "reasoning" || done[1]["type"] != "message" {
		t.Errorf("前两项类型不对：%v %v", done[0]["type"], done[1]["type"])
	}
	if txt := done[1]["content"].([]any)[0].(map[string]any)["text"]; txt != "你好" {
		t.Errorf("文本 %v，应为 你好", txt)
	}
	if done[2]["type"] != "function_call" || done[2]["arguments"] != `{"cmd":1}` || done[2]["call_id"] != "call_a" {
		t.Errorf("function_call 不对：%v", done[2])
	}
	if done[3]["type"] != "custom_tool_call" || done[3]["input"] != "P" {
		t.Errorf("custom_tool_call 不对：%v", done[3])
	}
	if last["type"] != "response.completed" {
		t.Fatalf("最后一个事件 %v，应为 response.completed", last["type"])
	}
	u := last["response"].(map[string]any)["usage"].(map[string]any)
	if u["input_tokens"] != 10.0 || u["output_tokens"] != 5.0 {
		t.Errorf("usage 不对：%v", u)
	}
}
