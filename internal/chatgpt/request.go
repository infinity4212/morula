package chatgpt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Model 是订阅里的一个模型。
type Model struct {
	ID           string
	Name         string
	Context      int
	Default      string
	Efforts      []string
	Instructions string // codex 发这个模型时用的 instructions
}

type cache struct {
	ClientVersion string `json:"client_version"`
	Models        []struct {
		Slug       string `json:"slug"`
		Display    string `json:"display_name"`
		Visibility string `json:"visibility"`
		Context    int    `json:"context_window"`
		Default    string `json:"default_reasoning_level"`
		Base       string `json:"base_instructions"`
		Messages   struct {
			Template string `json:"instructions_template"`
		} `json:"model_messages"`
		Levels []struct {
			Effort string `json:"effort"`
		} `json:"supported_reasoning_levels"`
	} `json:"models"`
}

func readCache() cache {
	var c cache
	if b, err := os.ReadFile(filepath.Join(Home(), "models_cache.json")); err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

// Models 读 codex 缓存的模型列表（models_cache.json），只列它在选择器里显示的，不列 Claude 模型。
func Models() []Model {
	out := []Model{}
	for _, m := range readCache().Models {
		if m.Visibility != "list" || strings.Contains(strings.ToLower(m.Slug), "claude") {
			continue
		}
		md := Model{ID: m.Slug, Name: m.Display, Context: m.Context, Default: m.Default, Efforts: []string{},
			Instructions: m.Messages.Template}
		if md.Instructions == "" {
			md.Instructions = m.Base
		}
		if md.Name == "" {
			md.Name = m.Slug
		}
		for _, l := range m.Levels {
			md.Efforts = append(md.Efforts, l.Effort)
		}
		out = append(out, md)
	}
	return out
}

func instructions(model string) string {
	for _, m := range Models() {
		if m.ID == model {
			return m.Instructions
		}
	}
	return ""
}

var osName = sync.OnceValue(func() string {
	if runtime.GOOS != "darwin" {
		return runtime.GOOS
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "/usr/bin/sw_vers", "-productVersion").Output()
	v := regexp.MustCompile(`\d+(\.\d+)+`).FindString(string(out))
	if v == "" {
		return "Mac OS"
	}
	for strings.Count(v, ".") < 2 {
		v += ".0"
	}
	return "Mac OS " + v
})

// userAgent 写成 codex CLI 的样子。后端只给带 codex User-Agent 的请求开放新模型，不带时回 404。
func userAgent(ver string) string {
	arch := map[string]string{"arm64": "arm64", "amd64": "x86_64"}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	return "codex_cli_rs/" + ver + " (" + osName() + "; " + arch + ") Apple_Terminal/455"
}

// client 只用 HTTP/1.1：codex 自己也是，HTTP/2 下长回复偶尔被中途重置。
var client = &http.Client{Transport: &http.Transport{
	Proxy:             http.ProxyFromEnvironment,
	ForceAttemptHTTP2: false,
	TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
}}

// Do 把一个 Responses 请求体（已经过 Body 改写）签名后发给 ChatGPT 后端，回复是 SSE 流。
func Do(ctx context.Context, body []byte) (*http.Response, error) {
	tok, account, err := Token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", Base+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	ver := readCache().ClientVersion
	if ver == "" {
		ver = "0.163.0"
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("Authorization", "Bearer "+tok)
	if account != "" {
		h.Set("chatgpt-account-id", account)
	}
	h.Set("OpenAI-Beta", "responses=experimental")
	h.Set("originator", "codex_cli_rs")
	h.Set("version", ver)
	h.Set("User-Agent", userAgent(ver))
	var v struct {
		Key string `json:"prompt_cache_key"`
	}
	json.Unmarshal(body, &v)
	if v.Key != "" {
		// 后端按这两个头找会话的提示词缓存，不看请求体里的 key
		h.Set("session-id", v.Key)
		h.Set("thread-id", v.Key)
	}
	return client.Do(req)
}

// Body 把 Responses 请求改成 codex CLI 会发的样子：只流式、不存（store=false）、instructions 用 codex 给这个模型的，
// 客户端自己的 instructions 作为开头的 developer 消息；后端不认的参数去掉。
func Body(m map[string]any) []byte {
	for _, k := range []string{"max_output_tokens", "max_completion_tokens", "temperature", "top_p", "previous_response_id",
		"user", "safety_identifier", "prompt_cache_options"} {
		delete(m, k)
	}
	if t := m["service_tier"]; t != "priority" && t != "ultrafast" {
		delete(m, "service_tier")
	}
	model, _ := m["model"].(string)
	if s, ok := m["input"].(string); ok {
		m["input"] = []any{map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": s}}}}
	}
	in, _ := m["input"].([]any)
	var input []any
	for _, x := range in {
		it, ok := x.(map[string]any)
		if !ok {
			input = append(input, x)
			continue
		}
		if it["type"] == "item_reference" {
			continue // store=false 时没有存下的项可引用
		}
		if it["role"] == "system" {
			it["role"] = "developer" // 后端不收 system
		}
		input = append(input, it)
	}
	own, _ := m["instructions"].(string)
	if k, _ := m["prompt_cache_key"].(string); k == "" {
		if k = conversationKey(own, input); k != "" {
			m["prompt_cache_key"] = k
		}
	}
	codex := instructions(model)
	if strings.TrimSpace(own) != "" {
		if codex == "" || firstLine(own) == firstLine(codex) {
			codex = own
		} else {
			input = append([]any{map[string]any{"type": "message", "role": "developer",
				"content": []any{map[string]any{"type": "input_text", "text": own}}}}, input...)
		}
	}
	if input == nil {
		input = []any{}
	}
	m["input"] = input
	if codex != "" {
		m["instructions"] = codex
	}
	m["store"] = false
	m["stream"] = true
	m["tool_choice"] = "auto"
	if _, ok := m["parallel_tool_calls"]; !ok {
		m["parallel_tool_calls"] = true
	}
	if r, ok := m["reasoning"].(map[string]any); ok {
		if r["effort"] == "ultra" {
			r["effort"] = "max" // codex CLI 里 ultra 发出去是 max
		}
		if _, ok := r["summary"]; !ok && r["effort"] != "none" {
			r["summary"] = "auto"
		}
	}
	b, _ := json.Marshal(m)
	return b
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(l)
}

// conversationKey 给没带 prompt_cache_key 的客户端算一个：按 instructions 和到第一条用户消息为止的输入算，
// 同一个会话的每一轮都相同，后端就能复用提示词缓存。
func conversationKey(instr string, input []any) string {
	h := sha256.New()
	h.Write([]byte(instr))
	user := false
	for _, x := range input {
		b, _ := json.Marshal(x)
		h.Write(b)
		if it, _ := x.(map[string]any); it["role"] == "user" {
			user = true
			break
		}
	}
	if !user {
		return ""
	}
	s := hex.EncodeToString(h.Sum(nil))
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}
