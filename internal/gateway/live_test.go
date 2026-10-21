package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestLive 经网关向真实上游发 Chat 和 Responses 请求各一次。要已登录的插件模型，默认跳过：
//
//	MORULA_HOME=<数据目录> MORULA_LIVE_MODEL=workbuddy/glm-5.3-flash go test ./internal/gateway -run Live -v
func TestLive(t *testing.T) {
	model := os.Getenv("MORULA_LIVE_MODEL")
	if model == "" {
		t.Skip("没设 MORULA_LIVE_MODEL")
	}
	g, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	post := func(path, body string) *http.Response {
		req, _ := http.NewRequest("POST", g.BaseURL()+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+g.Key)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// WorkBuddy 只接受流式 Chat 请求（非流式回 400 "Non-stream chat request is currently not supported"）
	t.Run("chat", func(t *testing.T) {
		res := post("/chat/completions", `{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"只回复 OK"}]}`)
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			t.Fatalf("状态 %d，回复 %s", res.StatusCode, b)
		}
		var text strings.Builder
		for _, line := range strings.Split(string(b), "\n") {
			var c chunk
			if strings.HasPrefix(line, "data: ") && json.Unmarshal([]byte(line[6:]), &c) == nil {
				for _, ch := range c.Choices {
					if ch.Delta != nil {
						text.WriteString(ch.Delta.Content)
					}
				}
			}
		}
		if text.Len() == 0 {
			t.Fatalf("流里没有文本：%s", b)
		}
		t.Logf("chat 回复：%q", text.String())
	})

	t.Run("responses", func(t *testing.T) {
		res := post("/responses", `{"model":"`+model+`","stream":true,"instructions":"你是测试助手","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"调用 echo 工具，参数 text 为 hi"}]}],
			"tools":[{"type":"function","name":"echo","description":"回显","parameters":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}],"tool_choice":"auto"}`)
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("状态 %d", res.StatusCode)
		}
		var types []string
		var call map[string]any
		var usage any
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 64*1024), 16<<20)
		for sc.Scan() {
			if !strings.HasPrefix(sc.Text(), "data: ") {
				continue
			}
			var e map[string]any
			json.Unmarshal([]byte(sc.Text()[6:]), &e)
			typ, _ := e["type"].(string)
			if !strings.HasSuffix(typ, ".delta") {
				types = append(types, typ)
			}
			if it, ok := e["item"].(map[string]any); ok && typ == "response.output_item.done" && it["type"] == "function_call" {
				call = it
			}
			if typ == "response.failed" {
				t.Fatalf("失败：%v", e["response"])
			}
			if typ == "response.completed" {
				usage = e["response"].(map[string]any)["usage"]
			}
		}
		t.Logf("事件：%v", types)
		if call == nil {
			t.Fatal("没有 function_call")
		}
		t.Logf("工具调用：%v %v；usage：%v", call["name"], call["arguments"], usage)
	})
}
