package gateway

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"morula/internal/chatgpt"
	"morula/internal/plugin"
)

func TestChatToResponses(t *testing.T) {
	var req map[string]any
	json.Unmarshal([]byte(`{"model":"codex/m","reasoning_effort":"high","messages":[
		{"role":"system","content":"规则"},
		{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]},
		{"role":"assistant","content":"好","tool_calls":[{"id":"`+strings.Repeat("c", 70)+`","type":"function","function":{"name":"read","arguments":"{\"p\":1}"}}]},
		{"role":"tool","tool_call_id":"`+strings.Repeat("c", 70)+`","content":"内容"}],
		"tools":[{"type":"function","function":{"name":"read","description":"读","parameters":{"type":"object"}}}]}`), &req)
	out := chatToResponses(req, "m")
	in := out["input"].([]any)
	if out["instructions"] != "规则" || len(in) != 4 {
		t.Fatalf("out = %v", out)
	}
	user := in[0].(map[string]any)["content"].([]any)
	if user[1].(map[string]any)["type"] != "input_image" || user[1].(map[string]any)["image_url"] != "data:image/png;base64,AA" {
		t.Fatalf("图片不对：%v", user)
	}
	call, res := in[2].(map[string]any), in[3].(map[string]any)
	if call["type"] != "function_call" || len(call["call_id"].(string)) != 64 || call["call_id"] != res["call_id"] || res["output"] != "内容" {
		t.Fatalf("工具调用不对：%v %v", call, res)
	}
	tool := out["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "read" || tool["type"] != "function" || out["reasoning"].(map[string]any)["effort"] != "high" {
		t.Fatalf("tools 或 reasoning 不对：%v", out)
	}
}

// upstreamSSE 是 ChatGPT 后端一段带推理、文本、工具调用的流。
const upstreamSSE = `event: response.created
data: {"type":"response.created","response":{}}

data: {"type":"response.reasoning_summary_part.added"}

data: {"type":"response.reasoning_summary_text.delta","delta":"想一想"}

data: {"type":"response.output_text.delta","delta":"你"}

data: {"type":"response.output_text.delta","delta":"好"}

data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read"}}

data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"p\":"}

data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"1}"}

data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"p\":1}"}}

data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"ls"}}

data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"ls","arguments":""}}

data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":4}}}}

`

func jwt(exp time.Time) string {
	b, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

// fakeChatGPT 起一个假后端，CODEX_HOME 指向临时目录里的假登录信息。返回收到的请求体。
func fakeChatGPT(t *testing.T, reply string, status int) *[]byte {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"`+jwt(time.Now().Add(time.Hour))+`","refresh_token":"r","account_id":"a"}}`), 0o600)
	os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`{"models":[{"slug":"m","visibility":"list","base_instructions":"You are Codex."}]}`), 0o644)
	got := new([]byte)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	saved := chatgpt.Base
	chatgpt.Base = srv.URL
	t.Cleanup(func() { chatgpt.Base = saved })
	return got
}

var ref = plugin.ModelRef{Provider: plugin.Provider{ID: plugin.ChatGPTID, Builtin: true}, Model: plugin.Model{ID: "m"}}

func TestChatGPTChatStream(t *testing.T) {
	got := fakeChatGPT(t, upstreamSSE, 200)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	chatgptChat(w, r, ref, []byte(`{"model":"codex/m","stream":true,"temperature":0.5,"messages":[{"role":"system","content":"规则"},{"role":"user","content":"hi"}]}`))
	var sent map[string]any
	json.Unmarshal(*got, &sent)
	if sent["instructions"] != "You are Codex." || sent["store"] != false || sent["temperature"] != nil || sent["model"] != "m" {
		t.Fatalf("发给后端的请求不对：%s", *got)
	}
	var content, reason string
	args := map[float64]string{}
	ids := map[float64]string{}
	var finish any
	var usage map[string]any
	done := false
	sc := bufio.NewScanner(w.Body)
	for sc.Scan() {
		d, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if d == "[DONE]" {
			done = true
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
					ToolCalls []struct {
						Index    float64 `json:"index"`
						ID       string  `json:"id"`
						Function struct {
							Name, Arguments string
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				Finish any `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		json.Unmarshal([]byte(d), &c)
		for _, ch := range c.Choices {
			content += ch.Delta.Content
			reason += ch.Delta.Reasoning
			for _, tc := range ch.Delta.ToolCalls {
				args[tc.Index] += tc.Function.Arguments
				if tc.ID != "" {
					ids[tc.Index] = tc.ID
				}
			}
			if ch.Finish != nil {
				finish = ch.Finish
			}
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	if content != "你好" || reason != "想一想" || args[0] != `{"p":1}` || args[1] != "{}" || ids[1] != "call_2" || finish != "tool_calls" || !done {
		t.Fatalf("content=%q reason=%q args=%v ids=%v finish=%v done=%v", content, reason, args, ids, finish, done)
	}
	if usage["prompt_tokens"] != float64(10) || usage["prompt_tokens_details"].(map[string]any)["cached_tokens"] != float64(4) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestChatGPTChatWhole(t *testing.T) {
	fakeChatGPT(t, upstreamSSE, 200)
	w := httptest.NewRecorder()
	chatgptChat(w, httptest.NewRequest("POST", "/", nil), ref, []byte(`{"model":"codex/m","messages":[{"role":"user","content":"hi"}]}`))
	var out struct {
		Choices []struct {
			Message struct {
				Content   string           `json:"content"`
				ToolCalls []map[string]any `json:"tool_calls"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "你好" || len(out.Choices[0].Message.ToolCalls) != 2 || out.Choices[0].Finish != "tool_calls" {
		t.Fatalf("回复不对：%s", w.Body.String())
	}
}

func TestChatGPTCutOff(t *testing.T) {
	fakeChatGPT(t, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"半\"}\n\n", 200)
	w := httptest.NewRecorder()
	chatgptChat(w, httptest.NewRequest("POST", "/", nil), ref, []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "中途断了") {
		t.Fatalf("流断了应报错：%d %s", w.Code, w.Body.String())
	}
}

func TestChatGPTUpstreamError(t *testing.T) {
	fakeChatGPT(t, `{"error":{"message":"Model not found"}}`, 404)
	w := httptest.NewRecorder()
	chatgptChat(w, httptest.NewRequest("POST", "/", nil), ref, []byte(`{"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if w.Code != 404 || !strings.Contains(w.Body.String(), "Model not found") {
		t.Fatalf("上游错误没带回来：%d %s", w.Code, w.Body.String())
	}
}

func TestChatGPTResponsesPassthrough(t *testing.T) {
	got := fakeChatGPT(t, upstreamSSE, 200)
	w := httptest.NewRecorder()
	chatgptResponses(w, httptest.NewRequest("POST", "/", nil), ref, []byte(`{"model":"codex/m","stream":true,"input":"hi","max_output_tokens":5}`))
	var sent map[string]any
	json.Unmarshal(*got, &sent)
	if sent["model"] != "m" || sent["max_output_tokens"] != nil || w.Body.String() != upstreamSSE {
		t.Fatalf("sent=%s body=%q", *got, w.Body.String())
	}
}

// 经网关的完整路径：模型目录里有 codex/m，Chat 请求走 ChatGPT 分支，不经插件。
func TestGatewayRoutesChatGPT(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	got := fakeChatGPT(t, upstreamSSE, 200)
	plugin.Forget()
	defer plugin.Forget()
	g, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	req, _ := http.NewRequest("GET", g.BaseURL()+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+g.Key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), `"codex/m"`) {
		t.Fatalf("模型列表里没有 codex/m：%s", b)
	}
	req, _ = http.NewRequest("POST", g.BaseURL()+"/chat/completions", strings.NewReader(`{"model":"codex/m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+g.Key)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(b), "你好") || !strings.Contains(string(*got), `"store":false`) {
		t.Fatalf("状态 %d：%s；发给后端：%s", res.StatusCode, b, *got)
	}
}
