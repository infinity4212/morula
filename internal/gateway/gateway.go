// Package gateway 是 morula 的本地模型网关：harness 把它当成一个 OpenAI 兼容的接口地址，
// 网关把请求交给插件的 fetch（见 internal/plugin）。
//
// 只监听 127.0.0.1 的随机端口，key 每次启动随机生成，只通过环境变量交给 morula 自己启动的 harness。
// 每个请求都校验 Host、Origin 和 key：浏览器页面（带 Origin）一律拒绝，防 CSRF 和 DNS rebinding。
//
// 对外提供：
//   - GET  /v1/models
//   - POST /v1/chat/completions：原样转给插件，只把模型名里的 <provider>/ 去掉
//   - POST /v1/responses：转成 Chat Completions 发给插件，再把流式回复转回 Responses 事件（codex 只发这种）
//
// codex 登录的 ChatGPT 订阅（provider codex）不经过插件，见 chatgpt.go；只讲 Anthropic Messages 的插件见 anthropic.go。
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"morula/internal/plugin"
)

// Gateway 是一个运行中的网关。
type Gateway struct {
	Port int
	Key  string
	srv  *http.Server
}

// Start 在 127.0.0.1 的随机端口上起网关。
func Start() (*Gateway, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("网关监听失败：%w", err)
	}
	b := make([]byte, 24)
	rand.Read(b)
	g := &Gateway{Port: ln.Addr().(*net.TCPAddr).Port, Key: "morula-" + hex.EncodeToString(b)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", g.models)
	mux.HandleFunc("POST /v1/chat/completions", g.chat)
	mux.HandleFunc("POST /v1/responses", g.responses)
	g.srv = &http.Server{Handler: g.guard(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := g.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Println("gateway:", err)
		}
	}()
	return g, nil
}

// BaseURL 是给 harness 的接口地址。
func (g *Gateway) BaseURL() string { return "http://127.0.0.1:" + strconv.Itoa(g.Port) + "/v1" }

// Close 停掉网关。
func (g *Gateway) Close() error { return g.srv.Close() }

// guard 校验 Host、Origin 和 key。
func (g *Gateway) guard(next http.Handler) http.Handler {
	port := strconv.Itoa(g.Port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "127.0.0.1:"+port && r.Host != "localhost:"+port {
			apiError(w, http.StatusForbidden, "Host 不对")
			return
		}
		if r.Header.Get("Origin") != "" {
			apiError(w, http.StatusForbidden, "网关不接受浏览器页面的请求")
			return
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "" {
			key = r.Header.Get("X-Api-Key")
		}
		if subtle.ConstantTimeCompare([]byte(key), []byte(g.Key)) != 1 {
			apiError(w, http.StatusUnauthorized, "key 不对")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func apiError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "morula_gateway"}})
}

func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	list, err := plugin.Catalog(r.Context(), false)
	if err != nil && len(list) == 0 {
		apiError(w, http.StatusBadGateway, err.Error())
		return
	}
	data := []map[string]any{}
	for _, p := range list {
		if !p.SignedIn {
			continue
		}
		for _, m := range p.Models {
			data = append(data, map[string]any{"id": p.ID + "/" + m.ID, "object": "model", "owned_by": p.ID})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// readBody 读请求体，限 64 MiB。
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		apiError(w, http.StatusBadRequest, "读不了请求体")
		return nil, false
	}
	return b, true
}

// send 把 Chat Completions 请求交给插件；API key 来源直接发给它的接口。只讲 Messages 的插件见 anthropic.go。
func send(ctx context.Context, ref plugin.ModelRef, body []byte) (*http.Response, error) {
	hd := http.Header{}
	hd.Set("Content-Type", "application/json")
	hd.Set("Accept", "text/event-stream, application/json")
	if ref.Provider.Builtin {
		a, ok := plugin.APIKeyFor(ref.Provider.ID)
		if !ok {
			return nil, errors.New("这个 API 密钥已删除：" + ref.Provider.ID)
		}
		req, err := http.NewRequestWithContext(ctx, "POST", a.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = hd
		req.Header.Set("Authorization", "Bearer "+a.Key)
		return http.DefaultClient.Do(req)
	}
	h, err := plugin.Get(ctx)
	if err != nil {
		return nil, err
	}
	if isAnthropic(ref.Provider) {
		return sendMessages(ctx, h, ref, body)
	}
	return h.Fetch(ctx, plugin.Request{Provider: ref.Provider.ID, Method: "POST", Path: "/chat/completions", Header: hd, Body: body})
}

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
		apiError(w, http.StatusBadRequest, "请求不是 JSON")
		return
	}
	var model string
	json.Unmarshal(req["model"], &model)
	ref, found := plugin.Find(r.Context(), model)
	if !found {
		apiError(w, http.StatusNotFound, "没有这个模型，或它的 provider 还没登录："+model)
		return
	}
	if plugin.IsChatGPT(ref.Provider) {
		chatgptChat(w, r, ref, body)
		return
	}
	req["model"], _ = json.Marshal(ref.Model.ID)
	body, _ = json.Marshal(req)
	res, err := send(r.Context(), ref, body)
	if err != nil {
		apiError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(res.StatusCode)
	copyFlush(w, res.Body)
}

// copyFlush 边读边写边 flush，流式回复不攒着。
func copyFlush(w http.ResponseWriter, r io.Reader) {
	f, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if f != nil {
				f.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// upstreamError 把上游的错误回复原样带上状态码交给客户端。
func upstreamError(w http.ResponseWriter, res *http.Response) {
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	msg := strings.TrimSpace(string(b))
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.StatusCode)
		io.Copy(w, bytes.NewReader(b))
		return
	}
	if msg == "" {
		msg = http.StatusText(res.StatusCode)
	}
	apiError(w, res.StatusCode, "上游回复 "+strconv.Itoa(res.StatusCode)+"："+msg)
}
